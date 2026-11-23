package index

import (
	"path/filepath"
	"testing"
	"time"
)

// runAt starts a run at a given time and returns its id.
func runAt(t *testing.T, db *DB, when time.Time) int64 {
	t.Helper()
	id, err := db.BeginRun(when, "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func finishAt(t *testing.T, db *DB, run int64, when time.Time) {
	t.Helper()
	if err := db.CompleteRun(run, RunComplete, when, nil); err != nil {
		t.Fatal(err)
	}
}

func day(n int) time.Time { return time.Date(2026, 1, n, 12, 0, 0, 0, time.UTC) }

// history builds this index (one folder, snapshots 1..4, one run per day):
//
//	run 1 (Jan 1): add a (uid 1), b (uid 2), shared (uid 3)
//	run 2 (Jan 2): expunge b
//	run 3 (Jan 3): move shared: expunge uid 3, add shared as uid 4 in a second folder
//	run 4 (Jan 4): expunge a
//
// and one blob row ("orphan") that no entry references.
type fixture struct {
	db           *DB
	path         string
	inbox, other int64
	a, b, shared NewBlob
	orphan       NewBlob
}

func newHistory(t *testing.T) *fixture {
	t.Helper()
	db, path := openTemp(t)
	f := &fixture{db: db, path: path, a: blob("a"), b: blob("b"), shared: blob("shared"), orphan: blob("orphan")}

	r1 := runAt(t, db, day(1))
	f.inbox = newFolder(t, db, r1, "INBOX")
	if err := db.ApplyFolder(r1, f.inbox, FolderChanges{UIDValidity: 7, LastUID: 3, Blobs: []NewBlob{f.a, f.b, f.shared, f.orphan},
		Open: []NewEntry{{1, f.a.SHA, ""}, {2, f.b.SHA, ""}, {3, f.shared.SHA, ""}}}); err != nil {
		t.Fatal(err)
	}
	finishAt(t, db, r1, day(1))

	r2 := runAt(t, db, day(2))
	open := entriesByUID(t, db, f.inbox)
	if err := db.ApplyFolder(r2, f.inbox, FolderChanges{UIDValidity: 7, LastUID: 3, Close: []int64{open[2]}}); err != nil {
		t.Fatal(err)
	}
	finishAt(t, db, r2, day(2))

	r3 := runAt(t, db, day(3))
	f.other = newFolder(t, db, r3, "Archive")
	open = entriesByUID(t, db, f.inbox)
	if err := db.ApplyFolder(r3, f.inbox, FolderChanges{UIDValidity: 7, LastUID: 3, Close: []int64{open[3]}}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyFolder(r3, f.other, FolderChanges{UIDValidity: 7, LastUID: 4, Open: []NewEntry{{4, f.shared.SHA, ""}}}); err != nil {
		t.Fatal(err)
	}
	finishAt(t, db, r3, day(3))

	r4 := runAt(t, db, day(4))
	open = entriesByUID(t, db, f.inbox)
	if err := db.ApplyFolder(r4, f.inbox, FolderChanges{UIDValidity: 7, LastUID: 3, Close: []int64{open[1]}}); err != nil {
		t.Fatal(err)
	}
	finishAt(t, db, r4, day(4))
	return f
}

// entriesByUID maps uid -> entry id for the open entries of a folder.
func entriesByUID(t *testing.T, db *DB, folder int64) map[uint32]int64 {
	t.Helper()
	open, err := db.OpenEntries(folder)
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint32]int64{}
	for _, e := range open {
		out[e.UID] = e.ID
	}
	return out
}

func TestClosedEntries(t *testing.T) {
	f := newHistory(t)
	got, err := f.db.ClosedEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("closed entries = %+v, want 3", got)
	}
	byBlob := map[[32]byte]ClosedEntry{}
	for _, e := range got {
		byBlob[e.Blob] = e
	}
	if e := byBlob[f.b.SHA]; e.From != 1 || e.To != 2 || !e.ClosedAt.Equal(day(2)) {
		t.Errorf("b = %+v, want [1,2) closed at Jan 2", e)
	}
	if e := byBlob[f.shared.SHA]; e.From != 1 || e.To != 3 || !e.ClosedAt.Equal(day(3)) {
		t.Errorf("shared = %+v, want [1,3) closed at Jan 3", e)
	}
	if e := byBlob[f.a.SHA]; e.From != 1 || e.To != 4 || !e.ClosedAt.Equal(day(4)) {
		t.Errorf("a = %+v, want [1,4) closed at Jan 4", e)
	}
}

func TestBlobRefs(t *testing.T) {
	f := newHistory(t)
	refs, unref, err := f.db.BlobRefs()
	if err != nil {
		t.Fatal(err)
	}
	// shared has two entries: one closed (Inbox uid 3) and one open (Archive uid 4).
	if refs[f.shared.SHA] != 2 || refs[f.a.SHA] != 1 || refs[f.b.SHA] != 1 {
		t.Errorf("refs = %v", refs)
	}
	if len(unref) != 1 || unref[0] != f.orphan.SHA {
		t.Errorf("unreferenced = %v, want only the orphan row", unref)
	}
}

func TestSetForgotten(t *testing.T) {
	f := newHistory(t)
	if err := f.db.SetForgotten([]int64{1, 2}); err != nil {
		t.Fatal(err)
	}
	snaps, _ := f.db.Snapshots()
	for _, s := range snaps {
		if want := s.ID <= 2; s.Forgotten != want {
			t.Errorf("snapshot %d forgotten = %v, want %v", s.ID, s.Forgotten, want)
		}
	}
}

func TestSetForgottenIsAllOrNothing(t *testing.T) {
	f := newHistory(t)
	if err := f.db.SetForgotten([]int64{1, 99}); err == nil {
		t.Fatal("forgetting a snapshot that does not exist must fail")
	}
	snaps, _ := f.db.Snapshots()
	for _, s := range snaps {
		if s.Forgotten {
			t.Errorf("snapshot %d was forgotten although the call failed", s.ID)
		}
	}
}

func TestSetForgottenRefusesAPinnedSnapshot(t *testing.T) {
	f := newHistory(t)
	if err := f.db.SetPinned(2, true); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetForgotten([]int64{1, 2}); err == nil {
		t.Fatal("a pinned snapshot must not be forgotten")
	}
	snaps, _ := f.db.Snapshots()
	for _, s := range snaps {
		if s.Forgotten {
			t.Errorf("snapshot %d was forgotten although the call failed", s.ID)
		}
	}
}

func TestApplyPrune(t *testing.T) {
	f := newHistory(t)
	if err := f.db.SetForgotten([]int64{1, 2}); err != nil {
		t.Fatal(err)
	}
	closed, _ := f.db.ClosedEntries()
	var ids []int64
	for _, e := range closed {
		if e.Blob == f.b.SHA {
			ids = append(ids, e.ID)
		}
	}
	res, err := f.db.ApplyPrune(PruneApply{
		Entries:   ids,
		Blobs:     [][32]byte{f.b.SHA, f.orphan.SHA},
		Snapshots: []int64{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries != 1 || res.Blobs != 2 || res.Snapshots != 1 {
		t.Errorf("applied = %+v, want 1 entry, 2 blobs, 1 snapshot", res)
	}
	if ok, _ := f.db.HasBlob(f.b.SHA); ok {
		t.Error("blob b is still in the index")
	}
	if ok, _ := f.db.HasBlob(f.a.SHA); !ok {
		t.Error("blob a was removed")
	}
	if ok, _ := f.db.HasSnapshot(1); ok {
		t.Error("snapshot 1 is still there")
	}
	if ok, _ := f.db.HasSnapshot(2); !ok {
		t.Error("snapshot 2 was removed")
	}
	if v, err := f.db.ForeignKeyCheck(); err != nil || len(v) != 0 {
		t.Errorf("foreign key violations after prune: %v (err %v)", v, err)
	}
	if probs, err := f.db.IntegrityCheck(); err != nil || len(probs) != 0 {
		t.Errorf("integrity: %v (err %v)", probs, err)
	}
}

func TestApplyPruneRefusesToDropAReferencedBlobAndRollsBack(t *testing.T) {
	f := newHistory(t)
	closed, _ := f.db.ClosedEntries()
	var bEntry int64
	for _, e := range closed {
		if e.Blob == f.b.SHA {
			bEntry = e.ID
		}
	}
	// "a" is still referenced by an open entry, so dropping its blob row must fail,
	// and the entry deletion in the same call must not stick.
	_, err := f.db.ApplyPrune(PruneApply{Entries: []int64{bEntry}, Blobs: [][32]byte{f.b.SHA, f.a.SHA}})
	if err == nil {
		t.Fatal("dropping a referenced blob must fail")
	}
	after, _ := f.db.ClosedEntries()
	if len(after) != len(closed) {
		t.Errorf("closed entries = %d after the failed prune, want %d", len(after), len(closed))
	}
	if ok, _ := f.db.HasBlob(f.b.SHA); !ok {
		t.Error("blob b was removed although the prune failed")
	}
}

func TestApplyPruneRefusesToDropAnOpenEntry(t *testing.T) {
	f := newHistory(t)
	open := entriesByUID(t, f.db, f.other)
	if _, err := f.db.ApplyPrune(PruneApply{Entries: []int64{open[4]}}); err == nil {
		t.Fatal("an open entry must never be pruned")
	}
}

func TestApplyPruneRefusesToDropAPinnedOrKeptSnapshot(t *testing.T) {
	f := newHistory(t)
	if _, err := f.db.ApplyPrune(PruneApply{Snapshots: []int64{1}}); err == nil {
		t.Fatal("a snapshot that is not forgotten must not be removed")
	}
	if err := f.db.SetForgotten([]int64{1}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetPinned(1, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ApplyPrune(PruneApply{Snapshots: []int64{1}}); err == nil {
		t.Fatal("a pinned snapshot must not be removed")
	}
}

func TestApplyPruneRemovesFoldersNothingNeedsAnymore(t *testing.T) {
	f := newHistory(t)
	// Close the Archive folder at run 5, then remove every entry and every
	// snapshot that could still show it.
	r5 := runAt(t, f.db, day(5))
	open := entriesByUID(t, f.db, f.other)
	if err := f.db.ApplyFolder(r5, f.other, FolderChanges{UIDValidity: 7, LastUID: 4, Close: []int64{open[4]}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkFolderGone(f.other, r5); err != nil {
		t.Fatal(err)
	}
	finishAt(t, f.db, r5, day(5))

	count := func() int {
		var n int
		if err := f.db.QueryRow(`SELECT count(*) FROM folders`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 2 {
		t.Fatalf("folders = %d, want 2", count())
	}
	// Archive existed for snapshots 3 and 4. Snapshot 3 and 4 still exist, so it stays,
	// even with its entries gone.
	closed, _ := f.db.ClosedEntries()
	var ids []int64
	for _, e := range closed {
		if e.FolderID == f.other {
			ids = append(ids, e.ID)
		}
	}
	if _, err := f.db.ApplyPrune(PruneApply{Entries: ids}); err != nil {
		t.Fatal(err)
	}
	if count() != 2 {
		t.Errorf("a folder that snapshots 3 and 4 still show was removed")
	}
	// Forget and remove 3 and 4: now nothing needs the Archive folder row.
	if err := f.db.SetForgotten([]int64{3, 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ApplyPrune(PruneApply{Snapshots: []int64{3, 4}}); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Errorf("folders = %d, want the Archive row gone", count())
	}
	if v, err := f.db.ForeignKeyCheck(); err != nil || len(v) != 0 {
		t.Errorf("foreign key violations: %v (err %v)", v, err)
	}
}

func TestVacuumInto(t *testing.T) {
	f := newHistory(t)
	dst := filepath.Join(t.TempDir(), "copy.db")
	if err := f.db.VacuumInto(dst); err != nil {
		t.Fatal(err)
	}
	cp, err := Open(dst, Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("the copy does not open: %v", err)
	}
	defer func() { _ = cp.Close() }()
	a, _ := f.db.Snapshots()
	b, err := cp.Snapshots()
	if err != nil || len(a) != len(b) {
		t.Errorf("copy has %d snapshots (err %v), original %d", len(b), err, len(a))
	}
	if probs, err := cp.IntegrityCheck(); err != nil || len(probs) != 0 {
		t.Errorf("copy integrity: %v (err %v)", probs, err)
	}
	if err := f.db.VacuumInto(dst); err == nil {
		t.Error("VacuumInto must not overwrite an existing file")
	}
}

func TestApplyPruneDryRunCountsLikeARealRunAndChangesNothing(t *testing.T) {
	f := newHistory(t)
	if err := f.db.SetForgotten([]int64{1}); err != nil {
		t.Fatal(err)
	}
	closed, _ := f.db.ClosedEntries()
	var ids []int64
	for _, e := range closed {
		if e.Blob == f.b.SHA {
			ids = append(ids, e.ID)
		}
	}
	p := PruneApply{Entries: ids, Blobs: [][32]byte{f.b.SHA, f.orphan.SHA}, Snapshots: []int64{1}}

	p.DryRun = true
	dry, err := f.db.ApplyPrune(p)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.db.HasBlob(f.b.SHA); !ok {
		t.Fatal("a dry run removed a blob")
	}
	if ok, _ := f.db.HasSnapshot(1); !ok {
		t.Fatal("a dry run removed a snapshot")
	}
	after, _ := f.db.ClosedEntries()
	if len(after) != len(closed) {
		t.Fatalf("a dry run removed entries: %d left of %d", len(after), len(closed))
	}

	p.DryRun = false
	real, err := f.db.ApplyPrune(p)
	if err != nil {
		t.Fatal(err)
	}
	if dry != real {
		t.Errorf("dry run counted %+v, the real run %+v", dry, real)
	}
}

func TestApplyPruneDryRunStillRefusesAWrongPlan(t *testing.T) {
	f := newHistory(t)
	if _, err := f.db.ApplyPrune(PruneApply{Blobs: [][32]byte{f.a.SHA}, DryRun: true}); err == nil {
		t.Fatal("a dry run must report a plan that would drop a referenced blob")
	}
}
