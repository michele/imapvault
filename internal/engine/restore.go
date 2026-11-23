package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/imapsource"
	"github.com/michele/imapvault/internal/imaptarget"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/mail"
	"github.com/michele/imapvault/internal/objects"
	"github.com/michele/imapvault/internal/plan"
)

// Restore is the one place that writes to a server, so it is split in two that
// cannot be mixed up:
//
//   - PlanRestore only reads (the repository, and the target through LIST,
//     EXAMINE and BODY.PEEK) and returns a plan with a token;
//   - ApplyRestore takes a plan.ApprovedRestore, which only plan.Approve can
//     make from a token the user confirmed, and writes exactly that plan through
//     a writer fenced to it.
//
// This file is the only one that imports internal/imaptarget (a test enforces it).

// DefaultRestorePrefix is where messages go unless --into or --in-place says
// otherwise: a separate folder tree, never the user's own folders.
const DefaultRestorePrefix = "Restored/{snapshot}"

// ErrRestoreNotAllowed is returned when the target account's config does not
// set [restore] allow_append.
var ErrRestoreNotAllowed = errors.New(`writing to this account is not enabled: add "allow_append = true" under [restore] in its config`)

// DialTarget connects to the target account. Nothing is changed by connecting;
// writes need a plan.ApprovedRestore.
func DialTarget(ctx context.Context, cfg *config.Config) (mail.Target, error) {
	return imaptarget.Dial(ctx, imapsource.Options{
		Host:     cfg.IMAP.Host,
		Port:     cfg.IMAP.Port,
		TLS:      cfg.IMAP.TLS,
		Username: cfg.IMAP.Username,
		Password: cfg.IMAP.Password,
	})
}

// RestoreOptions selects what to restore and where to.
type RestoreOptions struct {
	Snapshot     string        // "latest" (default), a number, latest~N, or a time
	DeletedSince time.Duration // > 0: restore what was deleted within this window; exclusive with Snapshot
	Folder       string        // only this original folder
	Message      string        // only the message with this SHA-256 prefix (at least 8 hex digits)
	Into         string        // target folder prefix, "{snapshot}" is replaced; default DefaultRestorePrefix
	InPlace      bool          // append into the original folders
	Now          time.Time     // zero: time.Now()
	// Log gets progress lines: folder names, hashes and counts, never message content.
	Log func(format string, args ...any)
}

func (o RestoreOptions) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// RestorePreview is a plan and what the user needs to judge it.
type RestorePreview struct {
	Plan  plan.RestorePlan
	Token string
	// TargetAccount and RepoAccount are "user@host". DifferentAccount is set when
	// they differ: the mail is going to an account the repository was not made from.
	TargetAccount    string
	RepoAccount      string
	DifferentAccount bool
	// AllowAppend is the target config's [restore] allow_append. A preview is
	// still produced without it, so the user sees what would happen; applying is
	// refused.
	AllowAppend bool
	Warnings    []string
}

func targetOf(cfg *config.Config) plan.RestoreTarget {
	return plan.RestoreTarget{Host: cfg.IMAP.Host, Port: cfg.IMAP.Port, Username: cfg.IMAP.Username}
}

// PlanRestore decides what a restore would write, and writes nothing. repoCfg
// locates the repository (and holds the private key); tgtCfg is the account whose
// server would be written to, and is the same config unless --to was used.
func PlanRestore(ctx context.Context, repoCfg, tgtCfg *config.Config, tgt mail.Target, opts RestoreOptions) (*RestorePreview, error) {
	if err := checkRestoreOptions(opts); err != nil {
		return nil, err
	}
	rd, err := openReaderMode(ctx, repoCfg, keyRequired)
	if err != nil {
		return nil, err
	}
	defer rd.close()

	sel, err := rd.selectRestore(ctx, opts)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if !opts.InPlace {
		prefix = strings.Trim(strings.ReplaceAll(orDefault(opts.Into, DefaultRestorePrefix), "{snapshot}", sel.label), "/")
		if prefix == "" {
			return nil, errors.New("--into gives an empty folder prefix")
		}
	}

	folders, err := tgt.ListFolders(ctx)
	if err != nil {
		return nil, err
	}
	in := plan.RestoreInput{
		Target:   targetOf(tgtCfg),
		RepoID:   rd.r.Meta.ID,
		Source:   sel.source,
		InPlace:  opts.InPlace,
		Prefix:   prefix,
		Existing: map[string][]plan.PresentMessage{},
		Messages: sel.messages,
	}
	byName := map[string]mail.Folder{}
	for _, f := range folders {
		byName[f.Name] = f
		if in.TgtDelim == "" {
			in.TgtDelim = f.Delim
		}
	}

	// Which target folders matter, and what is already in the ones that exist.
	want := map[string]bool{}
	for _, m := range sel.messages {
		name, err := plan.TargetFolderName(prefix, m.SrcDelim, in.TgtDelim, m.Folder)
		if err != nil {
			return nil, err
		}
		want[name] = true
	}
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f, exists := byName[name]
		if !exists {
			continue
		}
		if f.NoSelect {
			return nil, fmt.Errorf("target folder %q exists but cannot hold mail (\\Noselect): choose another --into", name)
		}
		present, err := tgt.Present(ctx, name)
		if err != nil {
			return nil, err
		}
		opts.log("target folder %q has %d message(s)", name, len(present))
		in.Existing[name] = toPresent(present)
	}

	p, err := plan.Restore(in)
	if err != nil {
		return nil, err
	}
	repoAcct := rd.r.Meta.Account
	prev := &RestorePreview{
		Plan:          p,
		Token:         p.Token(),
		TargetAccount: tgtCfg.IMAP.Username + "@" + tgtCfg.IMAP.Host,
		RepoAccount:   repoAcct.Username + "@" + repoAcct.Host,
		AllowAppend:   tgtCfg.Restore.AllowAppend,
		Warnings:      append(rd.warnings, sel.warnings...),
	}
	prev.DifferentAccount = !strings.EqualFold(repoAcct.Host, tgtCfg.IMAP.Host) || repoAcct.Username != tgtCfg.IMAP.Username
	return prev, nil
}

func toPresent(in []mail.Present) []plan.PresentMessage {
	out := make([]plan.PresentMessage, len(in))
	for i, p := range in {
		out[i] = plan.PresentMessage{MessageID: p.MessageID, Size: p.Size, InternalDate: p.InternalDate}
	}
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func checkRestoreOptions(o RestoreOptions) error {
	switch {
	case o.DeletedSince < 0:
		return errors.New("--deleted-since must be a positive duration")
	case o.DeletedSince > 0 && o.Snapshot != "":
		return errors.New("--deleted-since and --snapshot cannot be combined: pick one source")
	case o.InPlace && o.Into != "":
		return errors.New("--in-place and --into cannot be combined")
	case o.Message != "" && len(o.Message) < minHashPrefix:
		return fmt.Errorf("a hash prefix needs at least %d hex characters (got %d)", minHashPrefix, len(o.Message))
	}
	return nil
}

// restoreSelection is what the user asked for, read from the repository.
type restoreSelection struct {
	messages []plan.RestoreMessage
	source   string // labels the plan
	label    string // replaces {snapshot} in the prefix
	warnings []string
}

// selectRestore reads the messages to restore: a snapshot's, or the deleted ones.
func (rd *reader) selectRestore(ctx context.Context, opts RestoreOptions) (*restoreSelection, error) {
	var blobFilter *[32]byte
	if opts.Message != "" {
		rows, err := rd.db.BlobsByPrefix(strings.ToLower(opts.Message), 2)
		if err != nil {
			return nil, err
		}
		switch len(rows) {
		case 0:
			return nil, fmt.Errorf("no message has a hash starting with %s", opts.Message)
		case 2:
			return nil, fmt.Errorf("the prefix %s matches more than one message; give more characters", opts.Message)
		}
		blobFilter = &rows[0].SHA
	}

	names := map[int64]string{}
	delims := map[int64]string{}
	nameOf := func(f index.Folder) (string, error) {
		if n, ok := names[f.ID]; ok {
			return n, nil
		}
		b, err := rd.opener.Open(f.NameSealed)
		if err != nil {
			return "", fmt.Errorf("folder %d: %w", f.ID, err)
		}
		names[f.ID], delims[f.ID] = string(b), f.Delimiter
		return string(b), nil
	}
	build := func(f index.Folder, uid uint32, blob [32]byte, flags string, size int64, metaSealed []byte) (plan.RestoreMessage, error) {
		name, err := nameOf(f)
		if err != nil {
			return plan.RestoreMessage{}, err
		}
		m, err := openMeta(rd.opener, metaSealed)
		if err != nil {
			return plan.RestoreMessage{}, fmt.Errorf("message %x: %w", blob[:8], err)
		}
		return plan.RestoreMessage{
			Folder: name, SrcDelim: f.Delimiter, UID: uid, Blob: blob, Size: size,
			Flags: strings.Fields(flags), InternalDate: m.InternalDate, MessageID: m.MessageID,
		}, nil
	}

	sel := &restoreSelection{}
	if opts.DeletedSince > 0 {
		now := opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		cutoff := now.Add(-opts.DeletedSince)
		sel.source = "deleted within " + opts.DeletedSince.String()
		sel.label = "deleted-" + now.UTC().Format("2006-01-02")
		deleted, err := rd.db.DeletedSince(cutoff)
		if err != nil {
			return nil, err
		}
		all, err := rd.db.AllFolders()
		if err != nil {
			return nil, err
		}
		folders := map[int64]index.Folder{}
		for _, f := range all {
			folders[f.ID] = f
		}
		for _, d := range deleted {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if blobFilter != nil && d.Blob != *blobFilter {
				continue
			}
			f, ok := folders[d.FolderID]
			if !ok {
				return nil, fmt.Errorf("deleted message %x lists folder %d, which the index does not have", d.Blob[:8], d.FolderID)
			}
			m, err := build(f, d.UID, d.Blob, d.Flags, d.Size, d.MetaSealed)
			if err != nil {
				return nil, err
			}
			if opts.Folder != "" && m.Folder != opts.Folder {
				continue
			}
			sel.messages = append(sel.messages, m)
		}
		return sel, nil
	}

	id, err := rd.snapshotID(orDefault(opts.Snapshot, "latest"))
	if err != nil {
		return nil, err
	}
	sel.source = fmt.Sprintf("snapshot %d", id)
	sel.label = fmt.Sprintf("%d", id)
	folders, err := rd.db.SnapshotFolders(id)
	if err != nil {
		return nil, err
	}
	sawFolder := false
	for _, f := range folders {
		name, err := nameOf(f)
		if err != nil {
			return nil, err
		}
		if opts.Folder != "" && name != opts.Folder {
			continue
		}
		sawFolder = true
		entries, err := rd.db.SnapshotEntries(id, f.ID)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if blobFilter != nil && e.Blob != *blobFilter {
				continue
			}
			m, err := build(f, e.UID, e.Blob, e.Flags, e.Size, e.MetaSealed)
			if err != nil {
				return nil, err
			}
			sel.messages = append(sel.messages, m)
		}
	}
	if opts.Folder != "" && !sawFolder {
		return nil, fmt.Errorf("snapshot %d has no folder %q", id, opts.Folder)
	}
	if blobFilter != nil && len(sel.messages) == 0 {
		return nil, fmt.Errorf("snapshot %d does not contain the message %s", id, opts.Message)
	}
	return sel, nil
}

// RestoreResult is what ApplyRestore did.
type RestoreResult struct {
	Created  []string
	Appended int
	Bytes    int64
	Skipped  int // already present when the plan was made
}

// ApplyRestore writes an approved plan, and nothing else. It re-checks what the
// plan cannot: that the target account enables writing, that it is the server
// the plan was made for, and that it is the repository the plan was made from.
// The writer it uses is fenced to the plan, so even a bug here cannot append to
// another folder or append more than approved.
//
// It stops at the first error. Re-running restore afterwards plans again from the
// server as it is then, so what was already appended is not appended twice.
func ApplyRestore(ctx context.Context, repoCfg, tgtCfg *config.Config, tgt mail.Target, approved plan.ApprovedRestore, opts RestoreOptions) (*RestoreResult, error) {
	p := approved.Plan()
	res := &RestoreResult{Skipped: p.Skipped}
	if p.Empty() {
		return res, nil
	}
	if !tgtCfg.Restore.AllowAppend {
		return res, ErrRestoreNotAllowed
	}
	if got := targetOf(tgtCfg); got != p.Target {
		return res, fmt.Errorf("the plan was made for %s@%s:%d, not for this account", p.Target.Username, p.Target.Host, p.Target.Port)
	}
	rd, err := openReaderMode(ctx, repoCfg, keyRequired)
	if err != nil {
		return res, err
	}
	defer rd.close()
	if rd.r.Meta.ID != p.RepoID {
		return res, errors.New("the plan was made from a different repository")
	}
	st, err := openStore(rd.r, rd.identity)
	if err != nil {
		return res, err
	}
	defer func() { _ = st.Close() }()

	fence := mail.Fence{Create: append([]string(nil), p.Create...), Append: map[string]int{}}
	for _, f := range p.Folders {
		fence.Append[f.Name] = len(f.Append)
	}
	w := tgt.Writer(fence)

	for _, name := range p.Create {
		if err := w.CreateFolder(ctx, name); err != nil {
			return res, err
		}
		opts.log("created folder %q", name)
		res.Created = append(res.Created, name)
	}
	for _, f := range p.Folders {
		for _, it := range f.Append {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			raw, err := st.Get(objects.Key(it.Blob))
			if err != nil {
				return res, fmt.Errorf("message %x: %w", it.Blob[:8], err)
			}
			if int64(len(raw)) != it.Size {
				return res, fmt.Errorf("message %x: stored size does not match the index", it.Blob[:8])
			}
			if err := w.Append(ctx, f.Name, mail.AppendMessage{Flags: it.Flags, InternalDate: it.InternalDate, Raw: raw}); err != nil {
				return res, err
			}
			opts.log("appended %x to %q", it.Blob[:8], f.Name)
			res.Appended++
			res.Bytes += it.Size
		}
	}
	return res, nil
}
