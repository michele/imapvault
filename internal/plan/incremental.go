package plan

import (
	"errors"
	"fmt"
	"slices"
)

// Mode says how much of a folder a backup run has to read.
type Mode int

const (
	// ModeFull lists the whole folder and diffs it against the open entries.
	// It is the authority: every other mode falls back to it when in doubt.
	ModeFull Mode = iota
	// ModeIncremental lists only new UIDs, asks the server for flag changes
	// since the stored MODSEQ, and finds expunges by counting.
	ModeIncremental
	// ModeUnchanged reads nothing beyond EXAMINE.
	ModeUnchanged
)

func (m Mode) String() string {
	switch m {
	case ModeFull:
		return "full"
	case ModeIncremental:
		return "incremental"
	case ModeUnchanged:
		return "unchanged"
	}
	return fmt.Sprintf("mode(%d)", int(m))
}

// FolderSync is what the index remembers about a folder.
type FolderSync struct {
	UIDValidity uint32
	LastUID     uint32
	ModSeq      uint64
	// OpenCurrent counts open entries whose UIDVALIDITY equals the server's;
	// OpenOther counts open entries of any other generation.
	OpenCurrent int
	OpenOther   int
}

// ServerFolder is what EXAMINE reported.
type ServerFolder struct {
	UIDValidity   uint32
	UIDNext       uint32
	NumMessages   uint32
	HighestModSeq uint64
}

// ChooseMode picks the cheapest mode that is still correct.
//
// Unchanged additionally requires UIDNEXT and the message count to match, so it
// does not rely on servers bumping HIGHESTMODSEQ on EXPUNGE (plain CONDSTORE does
// not promise that; QRESYNC does).
func ChooseMode(known FolderSync, srv ServerFolder, condstore, force bool) Mode {
	if force || !condstore || known.ModSeq == 0 || srv.HighestModSeq == 0 ||
		known.UIDValidity != srv.UIDValidity || known.OpenOther > 0 {
		return ModeFull
	}
	if known.ModSeq == srv.HighestModSeq && srv.UIDNext > 0 && srv.UIDNext-1 == known.LastUID &&
		int64(srv.NumMessages) == int64(known.OpenCurrent) {
		return ModeUnchanged
	}
	return ModeIncremental
}

// SplitGenerations separates the open entries of the server's UIDVALIDITY
// generation from the rest. The rest is returned as entry ids to close: after a
// UIDVALIDITY change the old UIDs mean nothing. The rule also covers a reset that
// a killed run left half-done, where entries of both generations are open.
func SplitGenerations(open []OpenEntry, uidValidity uint32) (current []OpenEntry, closeIDs []int64) {
	for _, e := range open {
		if e.UIDValidity == uidValidity {
			current = append(current, e)
		} else {
			closeIDs = append(closeIDs, e.EntryID)
		}
	}
	slices.Sort(closeIDs)
	return current, closeIDs
}

// ErrInconsistent means the incremental view disagrees with the index. The
// caller must fall back to a full rescan before committing anything.
var ErrInconsistent = errors.New("server state does not match the index")

// IncrementalInput is the incremental view of one folder (one generation).
type IncrementalInput struct {
	Open        []OpenEntry // open entries of the current generation
	LastUID     uint32      // folder's stored last_uid
	NewRange    []ServerMsg // listing of LastUID+1:* (may include UIDs <= LastUID, they are ignored)
	Changed     []ServerMsg // UID FETCH 1:LastUID CHANGEDSINCE (may include UIDs > LastUID, ignored)
	NumMessages uint32      // from EXAMINE, same moment as the listings
}

// IncrementalPlan is a MessagePlan that may still need the server's UID list to
// find expunged messages.
type IncrementalPlan struct {
	MessagePlan
	// NeedUIDs is true when the counts show that old messages are gone. The
	// caller then fetches the UIDs 1:LastUID and calls WithExpunges.
	NeedUIDs bool

	expected int
	old      []OpenEntry
	lastUID  uint32
}

// DiffIncremental works out new mail, flag changes and (by counting) whether
// expunges happened. Entries above LastUID were committed by an interrupted run;
// they are diffed against the listing like a fresh folder would be.
func DiffIncremental(in IncrementalInput) (IncrementalPlan, error) {
	var old, recent []OpenEntry
	for _, e := range in.Open {
		if e.UID <= in.LastUID {
			old = append(old, e)
		} else {
			recent = append(recent, e)
		}
	}
	var newRange []ServerMsg
	for _, m := range in.NewRange {
		if m.UID > in.LastUID {
			newRange = append(newRange, m)
		}
	}
	mp, err := DiffMessages(recent, newRange)
	if err != nil {
		return IncrementalPlan{}, err
	}

	byUID := make(map[uint32]OpenEntry, len(old))
	for _, e := range old {
		if _, dup := byUID[e.UID]; dup {
			return IncrementalPlan{}, fmt.Errorf("index has two open entries for uid %d", e.UID)
		}
		byUID[e.UID] = e
	}
	seen := map[uint32]bool{}
	for _, m := range in.Changed {
		if m.UID > in.LastUID {
			continue
		}
		if seen[m.UID] {
			return IncrementalPlan{}, fmt.Errorf("server listed uid %d twice", m.UID)
		}
		seen[m.UID] = true
		e, ok := byUID[m.UID]
		if !ok {
			return IncrementalPlan{}, fmt.Errorf("%w: uid %d changed but has no open entry", ErrInconsistent, m.UID)
		}
		if nf := NormalizeFlags(m.Flags); nf != e.Flags {
			mp.Reflag = append(mp.Reflag, Reflag{EntryID: e.EntryID, UID: m.UID, Flags: nf})
		}
	}
	slices.SortFunc(mp.Reflag, func(a, b Reflag) int { return compareInt64(a.EntryID, b.EntryID) })

	expected := len(old) + len(newRange) - int(in.NumMessages)
	if expected < 0 {
		return IncrementalPlan{}, fmt.Errorf("%w: server holds %d messages, index and new mail account for only %d",
			ErrInconsistent, in.NumMessages, len(old)+len(newRange))
	}
	if expected > len(old) {
		return IncrementalPlan{}, fmt.Errorf("%w: server holds %d messages, fewer than the %d new ones alone",
			ErrInconsistent, in.NumMessages, len(newRange))
	}
	return IncrementalPlan{MessagePlan: mp, NeedUIDs: expected > 0, expected: expected, old: old, lastUID: in.LastUID}, nil
}

// WithExpunges finishes the plan with the server's UIDs in 1:LastUID: old entries
// whose UID is missing are closed. The number closed must equal the number the
// counts predicted, and the server must not hold a UID the index lacks; either
// mismatch is ErrInconsistent.
func (p IncrementalPlan) WithExpunges(uids []uint32) (MessagePlan, error) {
	if !p.NeedUIDs {
		return p.MessagePlan, nil
	}
	onServer := make(map[uint32]bool, len(uids))
	for _, u := range uids {
		if u <= p.lastUID {
			onServer[u] = true
		}
	}
	have := make(map[uint32]bool, len(p.old))
	var closed []int64
	gone := map[int64]bool{}
	for _, e := range p.old {
		have[e.UID] = true
		if !onServer[e.UID] {
			closed = append(closed, e.EntryID)
			gone[e.EntryID] = true
		}
	}
	for u := range onServer {
		if !have[u] {
			return MessagePlan{}, fmt.Errorf("%w: server holds uid %d, the index does not", ErrInconsistent, u)
		}
	}
	if len(closed) != p.expected {
		return MessagePlan{}, fmt.Errorf("%w: counts predicted %d expunged messages, UID list shows %d", ErrInconsistent, p.expected, len(closed))
	}
	out := p.MessagePlan
	out.Close = append(slices.Clone(out.Close), closed...)
	slices.Sort(out.Close)
	// A message flagged and then expunged shows up in both lists: the close wins.
	out.Reflag = slices.DeleteFunc(slices.Clone(out.Reflag), func(r Reflag) bool { return gone[r.EntryID] })
	return out, nil
}

// KnownBlob is a stored blob an EMAILID may stand for.
type KnownBlob struct {
	SHA  [32]byte
	Size int64
}

// MatchEmailID returns the stored blob to reuse for a new UID, or false when the
// body has to be fetched. EMAILID (RFC 8474) is only a hint: a match also needs
// the same RFC822.SIZE, and the blob key stays the SHA-256 of the raw bytes.
func MatchEmailID(emailID string, size int64, known map[string]KnownBlob) (KnownBlob, bool) {
	if emailID == "" {
		return KnownBlob{}, false
	}
	b, ok := known[emailID]
	if !ok || b.Size != size {
		return KnownBlob{}, false
	}
	return b, true
}
