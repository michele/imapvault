package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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

// BackupOptions tune a run. The zero value is fine.
type BackupOptions struct {
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
	// Logf receives progress lines. They contain folder names, UIDs and hashes
	// only: never subjects, bodies or credentials.
	Logf func(format string, args ...any)
}

// FolderReport is what one folder's backup changed.
type FolderReport struct {
	Name      string `json:"name"`
	Messages  int    `json:"messages"` // on the server after this run
	New       int    `json:"new"`
	Closed    int    `json:"closed"`
	Reflagged int    `json:"reflagged"`
}

// BackupReport describes a finished run.
type BackupReport struct {
	Snapshot    int64          `json:"snapshot"`
	Folders     []FolderReport `json:"folders"`
	NewFolders  int            `json:"new_folders"`
	GoneFolders int            `json:"gone_folders"`
	Messages    int            `json:"messages"` // in the snapshot
	New         int            `json:"new"`
	Closed      int            `json:"closed"`
	Reflagged   int            `json:"reflagged"`
	BlobsAdded  int            `json:"blobs_added"`  // object files written
	BlobsReused int            `json:"blobs_reused"` // new entries that needed no new blob
	BytesRaw    int64          `json:"bytes_raw"`    // of the blobs added
	Warnings    []string       `json:"warnings,omitempty"`
}

// Backup runs one read-only backup of src into the repository and writes a
// snapshot. It never resolves or uses a private key (safety rule 8): everything
// it reads back from the index is plaintext, everything descriptive is written
// sealed. Folders are committed one by one, each in a single transaction, after
// their blobs are durable (rule 2). If the run fails the folders already
// committed stay valid and no snapshot is written.
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
	b := &backup{cfg: cfg, src: src, opts: opts, db: db, store: store, sealer: sealer, folderKey: folderKey, run: run, rep: &BackupReport{}}
	if err := b.execute(ctx); err != nil {
		_ = db.FinishRun(run, index.RunFailed, opts.Now())
		return nil, err
	}
	if err := db.CompleteRun(run, opts.Now()); err != nil {
		_ = db.FinishRun(run, index.RunFailed, opts.Now())
		return nil, err
	}
	b.rep.Snapshot = run
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
	rep       *BackupReport
}

type target struct {
	folder mail.Folder
	hash   [32]byte
	known  *index.Folder // nil for a folder first seen in this run
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

	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.backupFolder(ctx, t); err != nil {
			return fmt.Errorf("folder %q: %w", t.folder.Name, err)
		}
	}
	// Folders that vanished are closed last, once every folder that is still
	// there has been read: a failed run never records a deletion.
	for _, g := range fp.Gone {
		if err := b.db.MarkFolderGone(g.ID, b.run); err != nil {
			return err
		}
		b.rep.GoneFolders++
		b.opts.Logf("folder gone: id %d", g.ID)
	}
	return nil
}

func (b *backup) backupFolder(ctx context.Context, t target) error {
	name := t.folder.Name
	state, err := b.src.Examine(ctx, name)
	if err != nil {
		return err
	}
	folderID := int64(0)
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
		if t.known.UIDValidity != 0 && t.known.UIDValidity != state.UIDValidity {
			return fmt.Errorf("UIDVALIDITY changed (%d -> %d); handling that is milestone M3, nothing was changed for this folder", t.known.UIDValidity, state.UIDValidity)
		}
	}

	infos := map[uint32]mail.MessageInfo{}
	var server []plan.ServerMsg
	if err := b.src.ListMessages(ctx, name, 1, func(batch []mail.MessageInfo) error {
		for _, m := range batch {
			infos[m.UID] = m
			server = append(server, plan.ServerMsg{UID: m.UID, Flags: m.Flags})
		}
		return nil
	}); err != nil {
		return err
	}
	open, err := b.db.OpenEntries(folderID)
	if err != nil {
		return err
	}
	po := make([]plan.OpenEntry, len(open))
	blobOf := map[int64][32]byte{}
	for i, e := range open {
		po[i] = plan.OpenEntry{EntryID: e.ID, UID: e.UID, Flags: e.Flags}
		blobOf[e.ID] = e.Blob
	}
	mp, err := plan.DiffMessages(po, server)
	if err != nil {
		return err
	}

	ch := index.FolderChanges{UIDValidity: state.UIDValidity, HighestModSeq: state.HighestModSeq, Close: mp.Close}
	if state.UIDNext > 0 {
		ch.LastUID = state.UIDNext - 1
	}
	for _, rf := range mp.Reflag {
		ch.Reflag = append(ch.Reflag, index.Reflag{EntryID: rf.EntryID, UID: rf.UID, Blob: blobOf[rf.EntryID], Flags: rf.Flags})
	}

	pending := map[[32]byte]bool{} // blob rows this folder adds
	if len(mp.Fetch) > 0 {
		err := b.src.FetchBodies(ctx, name, mp.Fetch, func(uid uint32, raw []byte) error {
			info := infos[uid]
			sha := sha256.Sum256(raw)
			if int64(len(raw)) != info.Size {
				b.rep.Warnings = append(b.rep.Warnings, fmt.Sprintf("folder %q uid %d: RFC822.SIZE says %d bytes, fetched %d (sha256 %x)", name, uid, info.Size, len(raw), sha[:8]))
			}
			ch.Open = append(ch.Open, index.NewEntry{UID: uid, Blob: sha, Flags: plan.NormalizeFlags(info.Flags)})
			if pending[sha] {
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
			ch.Blobs = append(ch.Blobs, index.NewBlob{SHA: sha, StoredSHA: stored, Size: int64(len(raw)), MetaSealed: meta, EmailID: info.EmailID})
			pending[sha] = true
			if added {
				b.rep.BlobsAdded++
				b.rep.BytesRaw += int64(len(raw))
			} else {
				b.rep.BlobsReused++ // an orphan from an interrupted run; now indexed
			}
			b.opts.Logf("%s uid %d sha256 %x", name, uid, sha[:8])
			return nil
		})
		if err != nil {
			return err
		}
		if len(ch.Open) != len(mp.Fetch) {
			return errors.New("the server did not return every requested message")
		}
	}
	if err := b.db.ApplyFolder(b.run, folderID, ch); err != nil {
		return err
	}
	fr := FolderReport{Name: name, Messages: len(server), New: len(ch.Open), Closed: len(ch.Close), Reflagged: len(ch.Reflag)}
	b.rep.Folders = append(b.rep.Folders, fr)
	b.rep.New += fr.New
	b.rep.Closed += fr.Closed
	b.rep.Reflagged += fr.Reflagged
	b.opts.Logf("%s: %d messages, +%d -%d ~%d", name, fr.Messages, fr.New, fr.Closed, fr.Reflagged)
	return nil
}
