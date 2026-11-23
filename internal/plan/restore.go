package plan

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Restore is the only operation that writes to a server, so its plan is the
// unit of consent: the user approves exactly this plan, by its token, and
// nothing outside it can be written (the writer in internal/imaptarget is fenced
// to it). Everything here is pure; the engine gathers the inputs and applies the
// result.

// RestoreTarget identifies the server a plan is for. It is part of the token, so
// a plan approved for one server cannot be applied to another.
type RestoreTarget struct {
	Host     string
	Port     int
	Username string
}

// RestoreMessage is one message the user asked to restore.
type RestoreMessage struct {
	Folder       string // original folder name on the source server
	UID          uint32 // original UID: only orders messages, so UIDs on the target ascend with the original's
	SrcDelim     string // that server's hierarchy delimiter
	Blob         [32]byte
	Size         int64
	Flags        []string
	InternalDate time.Time
	MessageID    string // "" when the message has none
}

// PresentMessage is a message already in a target folder.
type PresentMessage struct {
	MessageID    string
	Size         int64
	InternalDate time.Time
}

// RestoreInput is everything Restore decides from.
type RestoreInput struct {
	Target RestoreTarget
	RepoID string
	// Source says what is being restored ("snapshot 12", "deleted since ...").
	// It only labels the plan, but it is part of the token.
	Source   string
	InPlace  bool
	Prefix   string // used when !InPlace, "/"-separated, e.g. "Restored/12"
	TgtDelim string // the target server's hierarchy delimiter
	// Existing holds the folders that exist on the target, by name. A key with an
	// empty list is an empty folder; a missing key is a folder to create.
	Existing map[string][]PresentMessage
	Messages []RestoreMessage
}

// RestoreItem is one message to append.
type RestoreItem struct {
	Blob         [32]byte
	Size         int64
	Flags        []string // already filtered: never \Deleted or \Recent
	InternalDate time.Time
}

// RestoreFolder is the work for one target folder. A folder with nothing to
// append is not in the plan at all.
type RestoreFolder struct {
	Name             string
	Exists           bool // already on the target (otherwise it is created)
	ExistingMessages int
	Append           []RestoreItem
	Skipped          int // messages already present
	Bytes            int64
}

// RestorePlan is what will be written. Its Token is what the user approves.
type RestorePlan struct {
	Target             RestoreTarget
	RepoID             string
	Source             string
	InPlace            bool
	Prefix             string
	Create             []string // folders to create, sorted
	Folders            []RestoreFolder
	Skipped            int // messages already present, over all folders
	Appends            int
	Bytes              int64
	DeletedFlagDropped int // messages that had \Deleted, which is not restored
}

// Empty reports whether there is nothing to write.
func (p RestorePlan) Empty() bool { return len(p.Folders) == 0 }

// ErrFolderName is returned when an original folder name cannot be mapped to a
// target name without guessing.
var ErrFolderName = errors.New("folder name cannot be mapped to the target server")

// TargetFolderName maps an original folder name to its name on the target: the
// prefix (when not in place), then the original path, with the target's
// delimiter between components. It refuses names it would have to guess about.
func TargetFolderName(prefix, srcDelim, tgtDelim, folder string) (string, error) {
	var parts []string
	if prefix != "" {
		parts = append(parts, strings.Split(prefix, "/")...)
	}
	if srcDelim != "" {
		parts = append(parts, strings.Split(folder, srcDelim)...)
	} else {
		parts = append(parts, folder)
	}
	for _, c := range parts {
		switch {
		case c == "":
			return "", fmt.Errorf("%w: %q has an empty path component", ErrFolderName, folder)
		case tgtDelim != "" && strings.Contains(c, tgtDelim) && srcDelim != tgtDelim:
			return "", fmt.Errorf("%w: a component of %q contains the target delimiter %q", ErrFolderName, folder, tgtDelim)
		case strings.ContainsAny(c, "*%"):
			return "", fmt.Errorf("%w: %q contains * or %%", ErrFolderName, folder)
		}
	}
	if len(parts) > 1 && tgtDelim == "" {
		return "", fmt.Errorf("%w: the target has no hierarchy, but %q needs one", ErrFolderName, folder)
	}
	return strings.Join(parts, tgtDelim), nil
}

// presentMatches reports whether an existing message is the same message as m.
// With a Message-ID on both sides they must agree, and the size must agree too.
// Otherwise size and internal date (to the second) decide.
func presentMatches(p PresentMessage, m RestoreMessage) bool {
	if p.Size != m.Size {
		return false
	}
	if p.MessageID != "" && m.MessageID != "" {
		return p.MessageID == m.MessageID
	}
	return p.InternalDate.Unix() == m.InternalDate.Unix()
}

// restoreFlags is the flag list that is appended: the original flags without
// \Recent (the server's) and \Deleted (which would queue the message for the next
// EXPUNGE of any client). dropped says whether \Deleted was removed.
func restoreFlags(flags []string) (out []string, dropped bool) {
	norm := NormalizeFlags(flags)
	if norm == "" {
		return nil, false
	}
	for _, f := range strings.Split(norm, " ") {
		if f == `\Deleted` {
			dropped = true
			continue
		}
		out = append(out, f)
	}
	return out, dropped
}

// Restore decides what to write. It never plans anything that is already on the
// target: existing messages are matched one for one, so two identical messages
// in the snapshot need two on the server before both count as present. Applying
// a plan and planning again therefore gives an empty plan.
func Restore(in RestoreInput) (RestorePlan, error) {
	p := RestorePlan{Target: in.Target, RepoID: in.RepoID, Source: in.Source, InPlace: in.InPlace}
	if !in.InPlace {
		p.Prefix = in.Prefix
	}
	if in.InPlace && in.Prefix != "" {
		return p, errors.New("a prefix and in-place cannot both be set")
	}
	if !in.InPlace && in.Prefix == "" {
		return p, errors.New("restoring needs a prefix unless it is in place")
	}

	byFolder := map[string][]RestoreMessage{}
	for _, m := range in.Messages {
		name, err := TargetFolderName(in.Prefix, m.SrcDelim, in.TgtDelim, m.Folder)
		if err != nil {
			return p, err
		}
		byFolder[name] = append(byFolder[name], m)
	}
	names := make([]string, 0, len(byFolder))
	for n := range byFolder {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		msgs := byFolder[name]
		sort.SliceStable(msgs, func(i, j int) bool { return lessMessage(msgs[i], msgs[j]) })
		present, exists := in.Existing[name]
		used := make([]bool, len(present))
		f := RestoreFolder{Name: name, Exists: exists, ExistingMessages: len(present)}
		for _, m := range msgs {
			if matchOne(present, used, m) {
				f.Skipped++
				continue
			}
			flags, dropped := restoreFlags(m.Flags)
			if dropped {
				p.DeletedFlagDropped++
			}
			f.Append = append(f.Append, RestoreItem{Blob: m.Blob, Size: m.Size, Flags: flags, InternalDate: m.InternalDate})
			f.Bytes += m.Size
		}
		p.Skipped += f.Skipped
		if len(f.Append) == 0 {
			continue
		}
		if !f.Exists {
			p.Create = append(p.Create, name)
		}
		p.Appends += len(f.Append)
		p.Bytes += f.Bytes
		p.Folders = append(p.Folders, f)
	}
	return p, nil
}

// matchOne consumes the first unused existing message equal to m.
func matchOne(present []PresentMessage, used []bool, m RestoreMessage) bool {
	for i, e := range present {
		if !used[i] && presentMatches(e, m) {
			used[i] = true
			return true
		}
	}
	return false
}

// lessMessage orders messages the way they arrived: internal date, then original
// UID, then (to make the order total) blob and flags. Appending in this order
// gives the restored folder ascending UIDs that follow the original's.
func lessMessage(a, b RestoreMessage) bool {
	if !a.InternalDate.Equal(b.InternalDate) {
		return a.InternalDate.Before(b.InternalDate)
	}
	if a.UID != b.UID {
		return a.UID < b.UID
	}
	if c := strings.Compare(string(a.Blob[:]), string(b.Blob[:])); c != 0 {
		return c < 0
	}
	return NormalizeFlags(a.Flags) < NormalizeFlags(b.Flags)
}

// Token is a short fingerprint of everything the plan will write: the target,
// the repository, the source, the mode, the folders to create and every append
// (folder, message, flags, internal date). Changing any of them changes the
// token. It is not a secret: it proves that the plan being applied is the plan
// the user saw. Shown as two groups of five hex digits.
func (p RestorePlan) Token() string {
	h := sha256.New()
	str := func(s string) {
		_ = binary.Write(h, binary.BigEndian, uint64(len(s)))
		_, _ = h.Write([]byte(s))
	}
	num := func(n int64) { _ = binary.Write(h, binary.BigEndian, n) }

	str("imapvault restore plan v1")
	str(p.Target.Host)
	num(int64(p.Target.Port))
	str(p.Target.Username)
	str(p.RepoID)
	str(p.Source)
	if p.InPlace {
		num(1)
	} else {
		num(0)
	}
	str(p.Prefix)
	num(int64(len(p.Create)))
	for _, c := range p.Create {
		str(c)
	}
	num(int64(len(p.Folders)))
	for _, f := range p.Folders {
		str(f.Name)
		if f.Exists {
			num(1)
		} else {
			num(0)
		}
		num(int64(len(f.Append)))
		for _, it := range f.Append {
			_, _ = h.Write(it.Blob[:])
			num(it.Size)
			str(NormalizeFlags(it.Flags))
			num(it.InternalDate.UnixNano())
		}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return sum[:5] + "-" + sum[5:10]
}

// ApprovedRestore is a plan whose token the user has confirmed. Its field is
// unexported, so the only way to get a non-empty one is Approve: the engine's
// apply function takes this type and cannot be handed an unapproved plan.
type ApprovedRestore struct {
	plan RestorePlan
}

// Plan returns the approved plan.
func (a ApprovedRestore) Plan() RestorePlan { return a.plan }

// ErrNotApproved is returned by Approve when the token does not match the plan.
var ErrNotApproved = errors.New("the token does not match this plan")

// Approve accepts the plan only if token is its token (case, spaces and the dash
// are ignored). An empty plan cannot be approved: there is nothing to write.
func Approve(p RestorePlan, token string) (ApprovedRestore, error) {
	if p.Empty() {
		return ApprovedRestore{}, errors.New("nothing to restore")
	}
	norm := func(s string) string {
		return strings.ToLower(strings.NewReplacer("-", "", " ", "", "\t", "", "\n", "", "\r", "").Replace(s))
	}
	want, got := norm(p.Token()), norm(token)
	if len(got) != len(want) || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		return ApprovedRestore{}, ErrNotApproved
	}
	return ApprovedRestore{plan: p}, nil
}
