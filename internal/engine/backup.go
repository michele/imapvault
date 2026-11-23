package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/mail"
	"github.com/michele/imapvault/internal/objects"
	"github.com/michele/imapvault/internal/plan"
	"github.com/michele/imapvault/internal/repo"
	"github.com/michele/imapvault/internal/seal"
)

// While a folder's bodies are fetched they are committed to the index in
// batches, so a run that dies keeps what it already downloaded. Vars so tests
// can force several batches.
var (
	commitEvery = 100       // bodies per index commit
	commitBytes = int64(64) // MiB of raw bodies per index commit
)

// BackupOptions tune a run. The zero value is fine.
type BackupOptions struct {
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
	// Logf receives progress lines. They contain folder names, UIDs and hashes
	// only: never subjects, bodies or credentials.
	Logf func(format string, args ...any)
	// Full forces a full rescan of every folder: no CONDSTORE shortcuts.
	Full bool
}

// FolderReport is what one folder's backup changed.
type FolderReport struct {
	Name      string `json:"name"`
	Mode      string `json:"mode"`     // unchanged, incremental or full
	Messages  int    `json:"messages"` // open entries of the folder after this run
	New       int    `json:"new"`
	Closed    int    `json:"closed"`
	Reflagged int    `json:"reflagged"`
}

// BackupReport describes a finished run.
type BackupReport struct {
	Snapshot         int64          `json:"snapshot"`
	Status           string         `json:"status"` // complete or partial
	Folders          []FolderReport `json:"folders"`
	NewFolders       int            `json:"new_folders"`
	GoneFolders      int            `json:"gone_folders"`
	Messages         int            `json:"messages"` // in the snapshot
	New              int            `json:"new"`
	Closed           int            `json:"closed"`
	Reflagged        int            `json:"reflagged"`
	Fetched          int            `json:"fetched"`            // bodies downloaded
	SkippedByEmailID int            `json:"skipped_by_emailid"` // new messages whose body was not downloaded
	BlobsAdded       int            `json:"blobs_added"`        // object files written
	BlobsReused      int            `json:"blobs_reused"`       // new entries that needed no new blob
	BytesRaw         int64          `json:"bytes_raw"`          // of the blobs added
	Warnings         []string       `json:"warnings,omitempty"`
}

// Partial reports whether the run stopped before every folder was verified.
func (r *BackupReport) Partial() bool { return r.Status == index.RunPartial }

// errFolderSkipped marks a folder the server refused to open (it vanished
// between LIST and EXAMINE): the run goes on and ends partial.
var errFolderSkipped = errors.New("folder skipped")

// Backup runs one read-only backup of src into the repository and writes a
// snapshot. It never resolves or uses a private key (safety rule 8): everything
// it reads back from the index is plaintext, everything descriptive is written
// sealed. Folders are committed one by one, after their blobs are durable (rule
// 2); the bodies of a big folder are committed in batches on the way.
//
// If the run stops after at least one folder was committed (a folder failed, the
// connection dropped, Ctrl-C) it is recorded as partial: the snapshot holds what
// was verified, folders it did not reach keep their previous entries open, and no
// folder is closed as gone. If nothing was committed the run fails and writes no
// snapshot. Either way the next run resumes from what is stored.
//
// Consistency is per folder: IMAP has no account-wide atomic view.
func Backup(ctx context.Context, cfg *config.Config, src mail.Source, opts BackupOptions) (*BackupReport, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	acct := accountOf(cfg)
	r, err := repo.Open(cfg.Repository.Path, &acct)
	if err != nil {
		return nil, err
	}
	if err := checkRecipients(r, cfg.Encryption.Recipients); err != nil {
		return nil, err
	}
	sealer, err := sealerOf(r)
	if err != nil {
		return nil, err
	}
	folderKey, err := r.FolderKey()
	if err != nil {
		return nil, err
	}
	// No identity, ever: the opener is empty.
	store, err := openStore(r, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()

	unlock, err := r.Lock(repo.Exclusive)
	if err != nil {
		return nil, err
	}
	defer unlock()
	db, err := index.Open(r.IndexPath(), index.Options{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	caps, err := src.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	run, err := db.BeginRun(opts.Now(), caps.String())
	if err != nil {
		return nil, err
	}
	b := &backup{cfg: cfg, src: src, opts: opts, db: db, store: store, sealer: sealer, folderKey: folderKey, run: run,
		condstore: caps.Has("CONDSTORE"), rep: &BackupReport{}}
	if err := b.execute(ctx); err != nil {
		_ = db.FinishRun(run, index.RunFailed, opts.Now())
		return nil, err
	}
	status := index.RunComplete
	if b.partial {
		status = index.RunPartial
	}
	if err := db.CompleteRun(run, status, opts.Now(), nil); err != nil {
		_ = db.FinishRun(run, index.RunFailed, opts.Now())
		return nil, err
	}
	b.rep.Snapshot = run
	b.rep.Status = status
	snaps, err := db.Snapshots()
	if err == nil && len(snaps) > 0 {
		b.rep.Messages = snaps[len(snaps)-1].Messages
	}
	return b.rep, nil
}

type backup struct {
	cfg       *config.Config
	src       mail.Source
	opts      BackupOptions
	db        *index.DB
	store     *objects.Store
	sealer    *seal.Sealer
	folderKey []byte
	run       int64
	condstore bool
	rep       *BackupReport

	committed int  // folders verified and committed (or found unchanged) this run
	partial   bool // the run did not verify every folder
}

type target struct {
	folder mail.Folder
	hash   [32]byte
	known  *index.Folder // nil for a folder first seen in this run
}

func (b *backup) warn(format string, args ...any) {
	b.rep.Warnings = append(b.rep.Warnings, fmt.Sprintf(format, args...))
}

func (b *backup) execute(ctx context.Context) error {
	listed, err := b.src.ListFolders(ctx)
	if err != nil {
		return err
	}
	var selected []mail.Folder
	var lf []plan.ListedFolder
	for _, f := range listed {
		if f.NoSelect || !b.cfg.Folders.Selected(f.Name) {
			continue
		}
		selected = append(selected, f)
		lf = append(lf, plan.ListedFolder{NameHash: seal.FolderHash(b.folderKey, f.Name), Name: f.Name})
	}
	live, err := b.db.LiveFolders()
	if err != nil {
		return err
	}
	var kf []plan.KnownFolder
	knownByHash := map[[32]byte]*index.Folder{}
	for i := range live {
		kf = append(kf, plan.KnownFolder{ID: live[i].ID, NameHash: live[i].NameHash, UIDValidity: live[i].UIDValidity})
		knownByHash[live[i].NameHash] = &live[i]
	}
	fp := plan.DiffFolders(kf, lf)

	var targets []target
	for _, f := range selected {
		h := seal.FolderHash(b.folderKey, f.Name)
		targets = append(targets, target{folder: f, hash: h, known: knownByHash[h]})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].folder.Name < targets[j].folder.Name })

	var stop error
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			stop = err
			break
		}
		err := b.backupFolder(ctx, t)
		if errors.Is(err, errFolderSkipped) {
			b.warn("folder %q is no longer available on the server; skipped", t.folder.Name)
			b.partial = true
			continue
		}
		if err != nil {
			stop = fmt.Errorf("folder %q: %w", t.folder.Name, err)
			break
		}
		b.committed++
	}
	if len(targets) > 0 && b.committed == 0 {
		if stop == nil {
			stop = errors.New("no folder could be read")
		}
		return stop
	}
	if stop != nil {
		b.partial = true
		b.warn("run stopped before every folder was read: %v", stop)
	}
	if b.partial {
		if len(fp.Gone) > 0 {
			b.warn("%d folder(s) missing from the server were not closed: the run is partial", len(fp.Gone))
		}
		return nil
	}
	// Folders that vanished are closed last, once every folder that is still
	// there has been read: a failed or partial run never records a deletion.
	for _, g := range fp.Gone {
		if err := b.db.MarkFolderGone(g.ID, b.run); err != nil {
			return err
		}
		b.rep.GoneFolders++
		b.opts.Logf("folder gone: id %d", g.ID)
	}
	return nil
}

func toPlanEntries(open []index.OpenEntry) ([]plan.OpenEntry, map[int64][32]byte) {
	po := make([]plan.OpenEntry, len(open))
	blobOf := make(map[int64][32]byte, len(open))
	for i, e := range open {
		po[i] = plan.OpenEntry{EntryID: e.ID, UID: e.UID, UIDValidity: e.UIDValidity, Flags: e.Flags}
		blobOf[e.ID] = e.Blob
	}
	return po, blobOf
}

func (b *backup) backupFolder(ctx context.Context, t target) error {
	name := t.folder.Name
	state, err := b.src.Examine(ctx, name)
	if errors.Is(err, mail.ErrFolderGone) {
		return errFolderSkipped
	}
	if err != nil {
		return err
	}

	folderID := int64(0)
	var open []index.OpenEntry
	if t.known == nil {
		sealedName, err := b.sealer.Seal([]byte(name))
		if err != nil {
			return err
		}
		folderID, err = b.db.InsertFolder(b.run, index.NewFolder{
			NameHash: t.hash, NameSealed: sealedName, Delimiter: t.folder.Delim,
			SpecialUse: t.folder.SpecialUse, UIDValidity: state.UIDValidity,
		})
		if err != nil {
			return err
		}
		b.rep.NewFolders++
	} else {
		folderID = t.known.ID
		if open, err = b.db.OpenEntries(folderID); err != nil {
			return err
		}
	}
	po, blobOf := toPlanEntries(open)
	// Entries of any other UIDVALIDITY generation close: their UIDs mean nothing
	// now. The same rule resumes a reset that a killed run left half done.
	cur, closeGen := plan.SplitGenerations(po, state.UIDValidity)
	if len(closeGen) > 0 {
		b.opts.Logf("%s: UIDVALIDITY is now %d, closing %d entries of an older generation", name, state.UIDValidity, len(closeGen))
	}

	sync := plan.FolderSync{OpenCurrent: len(cur), OpenOther: len(closeGen)}
	if t.known != nil {
		sync.UIDValidity, sync.LastUID, sync.ModSeq = t.known.UIDValidity, t.known.LastUID, t.known.HighestModSeq
	}
	mode := plan.ChooseMode(sync,
		plan.ServerFolder{UIDValidity: state.UIDValidity, UIDNext: state.UIDNext, NumMessages: state.NumMessages, HighestModSeq: state.HighestModSeq},
		b.condstore, b.opts.Full)

	if mode == plan.ModeUnchanged {
		b.rep.Folders = append(b.rep.Folders, FolderReport{Name: name, Mode: mode.String(), Messages: len(cur)})
		b.opts.Logf("%s: unchanged (%d messages)", name, len(cur))
		return nil
	}

	var (
		mp     plan.MessagePlan
		infos  map[uint32]mail.MessageInfo
		maxUID uint32
	)
	if mode == plan.ModeIncremental {
		mp, infos, maxUID, err = b.incremental(ctx, name, state, t.known, cur)
		if errors.Is(err, plan.ErrInconsistent) {
			// The cheap view disagrees with the index: the full rescan decides.
			b.opts.Logf("%s: incremental view inconsistent (%v); rescanning in full", name, err)
			mode = plan.ModeFull
		} else if err != nil {
			return err
		}
	}
	if mode == plan.ModeFull {
		if mp, infos, maxUID, err = b.full(ctx, name, cur); err != nil {
			return err
		}
	}

	messages := len(cur) - len(mp.Close)
	closed := len(mp.Close) + len(closeGen)
	allClose := append(slices.Clone(mp.Close), closeGen...)
	slices.Sort(allClose)

	pend, err := b.fetchNew(ctx, name, folderID, state.UIDValidity, mp.Fetch, infos)
	if err != nil {
		return err
	}
	ch := index.FolderChanges{
		UIDValidity: state.UIDValidity, HighestModSeq: state.HighestModSeq, LastUID: maxUID,
		Blobs: pend.blobs, Open: pend.opens, Close: allClose,
	}
	if state.UIDNext > 0 && state.UIDNext-1 > ch.LastUID {
		ch.LastUID = state.UIDNext - 1
	}
	for _, rf := range mp.Reflag {
		ch.Reflag = append(ch.Reflag, index.Reflag{EntryID: rf.EntryID, UID: rf.UID, Blob: blobOf[rf.EntryID], Flags: rf.Flags})
	}
	if err := b.db.ApplyFolder(b.run, folderID, ch); err != nil {
		return err
	}
	opened := len(mp.Fetch) // every fetched or EMAILID-matched UID opened exactly one entry
	fr := FolderReport{Name: name, Mode: mode.String(), Messages: messages + opened, New: opened, Closed: closed, Reflagged: len(ch.Reflag)}
	b.rep.Folders = append(b.rep.Folders, fr)
	b.rep.New += fr.New
	b.rep.Closed += fr.Closed
	b.rep.Reflagged += fr.Reflagged
	b.opts.Logf("%s: %s, %d messages, +%d -%d ~%d", name, fr.Mode, fr.Messages, fr.New, fr.Closed, fr.Reflagged)
	return nil
}

// full lists the whole folder and diffs it against the open entries of the
// current generation. It is the authority every other mode falls back to.
func (b *backup) full(ctx context.Context, name string, cur []plan.OpenEntry) (plan.MessagePlan, map[uint32]mail.MessageInfo, uint32, error) {
	infos := map[uint32]mail.MessageInfo{}
	var server []plan.ServerMsg
	var maxUID uint32
	if err := b.src.ListMessages(ctx, name, 1, func(batch []mail.MessageInfo) error {
		for _, m := range batch {
			infos[m.UID] = m
			server = append(server, plan.ServerMsg{UID: m.UID, Flags: m.Flags})
			maxUID = max(maxUID, m.UID)
		}
		return nil
	}); err != nil {
		return plan.MessagePlan{}, nil, 0, err
	}
	mp, err := plan.DiffMessages(cur, server)
	return mp, infos, maxUID, err
}

// incremental lists only UIDs above the folder's last_uid, asks the server for
// flag changes since the stored mod-sequence, and finds expunges by counting
// (asking for the UID list only when the counts say something is gone). It
// returns plan.ErrInconsistent, with nothing committed, when the pieces do not
// add up.
func (b *backup) incremental(ctx context.Context, name string, state mail.FolderState, known *index.Folder, cur []plan.OpenEntry) (plan.MessagePlan, map[uint32]mail.MessageInfo, uint32, error) {
	infos := map[uint32]mail.MessageInfo{}
	var newRange, changed []plan.ServerMsg
	maxUID := known.LastUID
	if err := b.src.ListMessages(ctx, name, known.LastUID+1, func(batch []mail.MessageInfo) error {
		for _, m := range batch {
			if m.UID <= known.LastUID {
				continue
			}
			infos[m.UID] = m
			newRange = append(newRange, plan.ServerMsg{UID: m.UID, Flags: m.Flags})
			maxUID = max(maxUID, m.UID)
		}
		return nil
	}); err != nil {
		return plan.MessagePlan{}, nil, 0, err
	}
	if err := b.src.ChangedFlags(ctx, name, known.LastUID, known.HighestModSeq, func(batch []mail.MessageInfo) error {
		for _, m := range batch {
			changed = append(changed, plan.ServerMsg{UID: m.UID, Flags: m.Flags})
		}
		return nil
	}); err != nil {
		return plan.MessagePlan{}, nil, 0, err
	}
	ip, err := plan.DiffIncremental(plan.IncrementalInput{
		Open: cur, LastUID: known.LastUID, NewRange: newRange, Changed: changed, NumMessages: state.NumMessages,
	})
	if err != nil {
		return plan.MessagePlan{}, nil, 0, err
	}
	mp := ip.MessagePlan
	if ip.NeedUIDs {
		uids, err := b.src.UIDs(ctx, name, known.LastUID)
		if err != nil {
			return plan.MessagePlan{}, nil, 0, err
		}
		if mp, err = ip.WithExpunges(uids); err != nil {
			return plan.MessagePlan{}, nil, 0, err
		}
	}
	return mp, infos, maxUID, nil
}

// batch is the blobs and entries fetched since the last index commit.
type batch struct {
	blobs  []index.NewBlob
	opens  []index.NewEntry
	bytes  int64
	hashes map[[32]byte]bool // blob rows this batch adds
}

func (p *batch) reset() {
	p.blobs, p.opens, p.bytes = nil, nil, 0
	p.hashes = map[[32]byte]bool{}
}

func (b *backup) flush(folderID int64, uidValidity uint32, p *batch) error {
	if err := b.db.ApplyBatch(b.run, folderID, uidValidity, p.blobs, p.opens); err != nil {
		return err
	}
	p.reset()
	return nil
}

// fetchNew gets the new messages of a folder: bodies the index can already
// account for through EMAILID are not downloaded, the rest are fetched, hashed
// and written as blobs (durable before any row names them, rule 2). Full
// batches are committed on the way; the remainder is returned for ApplyFolder
// to commit together with the folder's closes and flag changes. If the fetch
// fails, what was already downloaded is committed so the next run starts after it.
func (b *backup) fetchNew(ctx context.Context, name string, folderID int64, uidValidity uint32, uids []uint32, infos map[uint32]mail.MessageInfo) (*batch, error) {
	p := &batch{}
	p.reset()
	if len(uids) == 0 {
		return p, nil
	}

	// EMAILID (RFC 8474) is a download-avoidance hint: with a matching id and
	// size the new UID points at a blob we already hold.
	var ids []string
	for _, u := range uids {
		if id := infos[u].EmailID; id != "" {
			ids = append(ids, id)
		}
	}
	byID := map[string]plan.KnownBlob{}
	if len(ids) > 0 {
		found, err := b.db.BlobsByEmailID(ids)
		if err != nil {
			return nil, err
		}
		for id, eb := range found {
			byID[id] = plan.KnownBlob{SHA: eb.SHA, Size: eb.Size}
		}
	}
	var fetch []uint32
	for _, u := range uids {
		info := infos[u]
		if kb, ok := plan.MatchEmailID(info.EmailID, info.Size, byID); ok {
			p.opens = append(p.opens, index.NewEntry{UID: u, Blob: kb.SHA, Flags: plan.NormalizeFlags(info.Flags)})
			b.rep.SkippedByEmailID++
			b.rep.BlobsReused++
			b.opts.Logf("%s uid %d sha256 %x (EMAILID match, body not downloaded)", name, u, kb.SHA[:8])
			continue
		}
		fetch = append(fetch, u)
	}
	if len(fetch) == 0 {
		return p, nil
	}

	got := 0
	err := b.src.FetchBodies(ctx, name, fetch, func(uid uint32, raw []byte) error {
		info := infos[uid]
		sha := sha256.Sum256(raw)
		got++
		b.rep.Fetched++
		if int64(len(raw)) != info.Size {
			b.warn("folder %q uid %d: RFC822.SIZE says %d bytes, fetched %d (sha256 %x)", name, uid, info.Size, len(raw), sha[:8])
		}
		p.opens = append(p.opens, index.NewEntry{UID: uid, Blob: sha, Flags: plan.NormalizeFlags(info.Flags)})
		p.bytes += int64(len(raw))
		if err := b.storeBlob(name, uid, raw, sha, info, p); err != nil {
			return err
		}
		if len(p.opens) >= commitEvery || p.bytes >= commitBytes<<20 {
			return b.flush(folderID, uidValidity, p)
		}
		return nil
	})
	if err != nil {
		// Keep what arrived: the objects are durable, so committing them is safe,
		// and the next run will not download them again.
		if ferr := b.flush(folderID, uidValidity, p); ferr != nil {
			b.opts.Logf("%s: could not commit the bodies fetched before the failure: %v", name, ferr)
		}
		return nil, err
	}
	if got != len(fetch) {
		return nil, errors.New("the server did not return every requested message")
	}
	return p, nil
}

// storeBlob makes the object durable and queues its index row, unless the index
// already has the blob (or this batch is about to add it).
func (b *backup) storeBlob(name string, uid uint32, raw []byte, sha [32]byte, info mail.MessageInfo, p *batch) error {
	if p.hashes[sha] {
		b.rep.BlobsReused++
		return nil
	}
	have, err := b.db.HasBlob(sha)
	if err != nil {
		return err
	}
	if have {
		b.rep.BlobsReused++
		return nil
	}
	// Durable on disk (fsync + rename) before any index row names it.
	_, stored, added, err := b.store.Put(raw)
	if err != nil {
		return err
	}
	meta, err := sealMeta(b.sealer, parseMeta(raw, info.InternalDate))
	if err != nil {
		return err
	}
	p.blobs = append(p.blobs, index.NewBlob{SHA: sha, StoredSHA: stored, Size: int64(len(raw)), MetaSealed: meta, EmailID: info.EmailID})
	p.hashes[sha] = true
	if added {
		b.rep.BlobsAdded++
		b.rep.BytesRaw += int64(len(raw))
	} else {
		b.rep.BlobsReused++ // an orphan from an interrupted run; now indexed
	}
	b.opts.Logf("%s uid %d sha256 %x", name, uid, sha[:8])
	return nil
}
