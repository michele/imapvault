package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/objects"
	"github.com/michele/imapvault/internal/plan"
)

// RetentionOptions are the knobs of forget and prune.
type RetentionOptions struct {
	// DryRun reports what would happen and changes nothing.
	DryRun bool
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
	// Location decides where an hour, day, week, month or year starts when
	// forget buckets snapshots; nil means the local time zone.
	Location *time.Location
}

func (o *RetentionOptions) defaults() {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.Local
	}
}

// Verdict is forget's decision about one snapshot.
type Verdict struct {
	Snapshot int64    `json:"snapshot"`
	Keep     bool     `json:"keep"`
	Reasons  []string `json:"reasons,omitempty"`
}

// ForgetReport is what forget did (or, with DryRun, would do).
type ForgetReport struct {
	DryRun    bool      `json:"dry_run"`
	Forgotten []int64   `json:"forgotten"`
	Kept      []int64   `json:"kept"`
	Verdicts  []Verdict `json:"verdicts"` // newest first
}

// Forget applies the retention policy: it marks the snapshots the policy does not
// keep as forgotten. Nothing is deleted (that is prune), and a forgotten snapshot
// stays addressable until prune removes what it showed. Pinned snapshots and the
// latest one are always kept. It reads only plaintext columns, so it needs no
// private key, and it runs even while guard alerts are unacknowledged (it only
// marks; prune is the gate).
func Forget(_ context.Context, cfg *config.Config, opts RetentionOptions) (*ForgetReport, error) {
	opts.defaults()
	pol := cfg.Retention
	w, err := openWriter(cfg)
	if err != nil {
		return nil, err
	}
	defer w.close()
	snaps, err := w.db.Snapshots()
	if err != nil {
		return nil, err
	}
	in := make([]plan.RetainSnapshot, len(snaps))
	for i, s := range snaps {
		in[i] = plan.RetainSnapshot{ID: s.ID, CreatedAt: s.CreatedAt, Pinned: s.Pinned, Forgotten: s.Forgotten}
	}
	res, err := plan.Retain(in, plan.RetainPolicy{
		Last: pol.KeepLast, Hourly: pol.KeepHourly, Daily: pol.KeepDaily,
		Weekly: pol.KeepWeekly, Monthly: pol.KeepMonthly, Yearly: pol.KeepYearly,
	}, opts.Location)
	if err != nil {
		return nil, fmt.Errorf("[retention]: %w", err)
	}
	rep := &ForgetReport{DryRun: opts.DryRun, Forgotten: res.Forget, Kept: res.Keep, Verdicts: []Verdict{}}
	for _, d := range res.Decisions {
		rep.Verdicts = append(rep.Verdicts, Verdict{Snapshot: d.ID, Keep: d.Keep, Reasons: d.Reasons})
	}
	if !opts.DryRun && len(res.Forget) > 0 {
		if err := w.db.SetForgotten(res.Forget); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

// ErrUnackedAlerts is what errors.Is matches when prune refuses to run because a
// guard alert has not been acknowledged.
var ErrUnackedAlerts = errors.New("unacknowledged guard alerts")

type unackedAlertsError struct{ n int }

func (e *unackedAlertsError) Error() string {
	return fmt.Sprintf("%d unacknowledged guard alert(s): look at `imapvault alerts`, then `imapvault ack`; prune stays off until then", e.n)
}

func (e *unackedAlertsError) Is(target error) bool { return target == ErrUnackedAlerts }

// planPrune works out what prune would remove at the given cutoff: the pure
// plan.Prune over the index's snapshots, closed entries and blob references. It
// reads only; Prune and Stats both use it so they can never disagree.
func planPrune(db *index.DB, cutoff time.Time) (plan.PruneResult, error) {
	snaps, err := db.Snapshots()
	if err != nil {
		return plan.PruneResult{}, err
	}
	closed, err := db.ClosedEntries()
	if err != nil {
		return plan.PruneResult{}, err
	}
	refs, unreferenced, err := db.BlobRefs()
	if err != nil {
		return plan.PruneResult{}, err
	}
	in := plan.PruneInput{Cutoff: cutoff, Refs: refs, Unreferenced: unreferenced}
	for _, sn := range snaps {
		in.Snapshots = append(in.Snapshots, plan.PruneSnapshot{ID: sn.ID, Forgotten: sn.Forgotten, Pinned: sn.Pinned})
	}
	for _, e := range closed {
		in.Entries = append(in.Entries, plan.PruneEntry{ID: e.ID, Blob: e.Blob, From: e.From, To: e.To, ClosedAt: e.ClosedAt})
	}
	return plan.Prune(in), nil
}

// PruneReport is what prune removed (or, with DryRun, would remove).
type PruneReport struct {
	DryRun bool `json:"dry_run"`
	// Cutoff is now minus deleted_grace: mail deleted from the server after it is kept.
	Cutoff time.Time `json:"cutoff"`

	Entries   int   `json:"entries"`    // index entries removed
	Blobs     int   `json:"blobs"`      // messages whose last reference went
	BlobBytes int64 `json:"blob_bytes"` // size of those original messages
	DiskBytes int64 `json:"disk_bytes"` // object files freed (compressed, and sealed when encrypted)
	Snapshots int   `json:"snapshots"`  // forgotten snapshot rows removed: they were no longer complete
	Folders   int   `json:"folders"`    // folder rows nothing needs any more

	Orphans     int   `json:"orphans"` // object files no index row references
	OrphanBytes int64 `json:"orphan_bytes"`
	TmpFiles    int   `json:"tmp_files"` // leftovers of interrupted writes

	IndexBackup string   `json:"index_backup,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

// Prune removes what no kept snapshot needs and the grace period no longer holds:
// closed index entries, the blobs only they referenced (rows and files), forgotten
// snapshot rows that are no longer complete, gone folders nothing shows, orphan
// object files and tmp leftovers. It refuses to run while a guard alert is
// unacknowledged. It never resolves a private key (rule 8) and commits the index
// before it deletes a file (rule 2): a crash leaves orphan files, never dangling
// references. A dry run takes the same lock and runs the same checks, then rolls
// back.
func Prune(_ context.Context, cfg *config.Config, opts RetentionOptions) (*PruneReport, error) {
	opts.defaults()
	if cfg.Retention.DeletedGraceDur <= 0 {
		return nil, errors.New("retention.deleted_grace is not set: refusing to prune without a grace period")
	}
	w, err := openWriter(cfg)
	if err != nil {
		return nil, err
	}
	defer w.close()
	if n, err := w.db.UnackedAlertCount(); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, &unackedAlertsError{n}
	}

	now := opts.Now()
	rep := &PruneReport{DryRun: opts.DryRun, Cutoff: now.Add(-cfg.Retention.DeletedGraceDur).UTC()}

	pr, err := planPrune(w.db, rep.Cutoff)
	if err != nil {
		return nil, err
	}

	store, err := openStore(w.r, "") // no identity: prune never reads a message
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()

	// Sizes and orphans are worked out before anything changes.
	for _, b := range pr.Blobs {
		n, err := w.db.BlobSize(b)
		if err != nil {
			return nil, err
		}
		rep.BlobBytes += n
		if sz, err := store.Size(objects.Key(b)); err == nil {
			rep.DiskBytes += sz
		} else if !errors.Is(err, objects.ErrNotFound) {
			return nil, err
		}
	}
	orphans, err := orphanObjects(w.db, store)
	if err != nil {
		return nil, err
	}
	for _, o := range orphans {
		sz, err := store.Size(o)
		if err != nil {
			return nil, err
		}
		rep.OrphanBytes += sz
	}
	rep.Orphans = len(orphans)
	tmpFiles, err := os.ReadDir(w.r.TmpDir())
	if err != nil {
		return nil, err
	}
	rep.TmpFiles = len(tmpFiles)

	applied, err := w.db.ApplyPrune(index.PruneApply{Entries: pr.Entries, Blobs: pr.Blobs, Snapshots: pr.Snapshots, DryRun: opts.DryRun})
	if err != nil {
		return nil, err
	}
	rep.Entries, rep.Blobs, rep.Snapshots, rep.Folders = applied.Entries, applied.Blobs, applied.Snapshots, applied.Folders
	if opts.DryRun {
		return rep, nil
	}

	// The index no longer references any of these: now the files can go. A failure
	// leaves an orphan the next prune removes.
	warn := func(format string, args ...any) { rep.Warnings = append(rep.Warnings, fmt.Sprintf(format, args...)) }
	for _, b := range pr.Blobs {
		if _, err := store.Delete(objects.Key(b)); err != nil {
			warn("could not remove object %x: %v (the next prune retries)", b[:6], err)
		}
	}
	for _, o := range orphans {
		if _, err := store.Delete(o); err != nil {
			warn("could not remove orphan object %s: %v (the next prune retries)", o, err)
		}
	}
	for _, e := range tmpFiles {
		if err := os.RemoveAll(filepath.Join(w.r.TmpDir(), e.Name())); err != nil {
			warn("could not remove tmp/%s: %v", e.Name(), err)
		}
	}
	// A copy of the index is worth writing only if prune changed the index: an
	// idle prune would otherwise push older, useful copies out of the newest seven.
	if rep.Entries+rep.Blobs+rep.Snapshots+rep.Folders > 0 {
		if p, err := writeIndexBackup(w.r, w.db, "prune", now); err != nil {
			warn("index backup: %v", err)
		} else {
			rep.IndexBackup = filepath.Base(p)
		}
	}
	return rep, nil
}

// orphanObjects lists the object files that no blob row references. A crash
// between writing a blob and committing its row leaves such a file; so does a
// prune interrupted after its index commit.
func orphanObjects(db *index.DB, store *objects.Store) ([]objects.Key, error) {
	known := map[objects.Key]bool{}
	if err := db.BlobHashes(func(sha, _ [32]byte) error { known[objects.Key(sha)] = true; return nil }); err != nil {
		return nil, err
	}
	var out []objects.Key
	err := store.Walk(func(k objects.Key) error {
		if !known[k] {
			out = append(out, k)
		}
		return nil
	}, nil)
	return out, err
}
