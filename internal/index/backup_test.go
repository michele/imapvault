package index

import (
	"crypto/sha256"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

func blob(s string) NewBlob {
	h := sha256.Sum256([]byte(s))
	return NewBlob{SHA: h, StoredSHA: sha256.Sum256([]byte("stored" + s)), Size: int64(len(s)), MetaSealed: []byte("m-" + s)}
}

func newRun(t *testing.T, db *DB) int64 {
	t.Helper()
	id, err := db.BeginRun(t0, "CONDSTORE")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newFolder(t *testing.T, db *DB, run int64, name string) int64 {
	t.Helper()
	id, err := db.InsertFolder(run, NewFolder{NameHash: sha256.Sum256([]byte(name)), NameSealed: []byte("n-" + name), Delimiter: "/", UIDValidity: 7})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func finish(t *testing.T, db *DB, run int64) {
	t.Helper()
	if err := db.CompleteRun(run, RunComplete, t0); err != nil {
		t.Fatal(err)
	}
}

func uids(es []SnapEntry) (out []uint32) {
	for _, e := range es {
		out = append(out, e.UID)
	}
	return
}

// Three runs: add two messages, change flags of one and expunge the other, and
// each snapshot must show the state as of its own run.
func TestSnapshotIntervals(t *testing.T) {
	db, _ := openTemp(t)
	a, b := blob("a"), blob("b")

	r1 := newRun(t, db)
	f := newFolder(t, db, r1, "INBOX")
	if err := db.ApplyFolder(r1, f, FolderChanges{UIDValidity: 7, LastUID: 2, Blobs: []NewBlob{a, b},
		Open: []NewEntry{{1, a.SHA, ""}, {2, b.SHA, `\Seen`}}}); err != nil {
		t.Fatal(err)
	}
	finish(t, db, r1)

	r2 := newRun(t, db)
	open, err := db.OpenEntries(f)
	if err != nil || len(open) != 2 {
		t.Fatalf("open %v err %v", open, err)
	}
	if err := db.ApplyFolder(r2, f, FolderChanges{UIDValidity: 7, LastUID: 2,
		Reflag: []Reflag{{EntryID: open[0].ID, UID: 1, Blob: a.SHA, Flags: `\Seen`}}, Close: []int64{open[1].ID}}); err != nil {
		t.Fatal(err)
	}
	finish(t, db, r2)

	e1, _ := db.SnapshotEntries(r1, f)
	e2, _ := db.SnapshotEntries(r2, f)
	if got := uids(e1); len(got) != 2 || e1[0].Flags != "" || e1[1].Flags != `\Seen` {
		t.Fatalf("snapshot 1: %+v", e1)
	}
	if got := uids(e2); len(got) != 1 || got[0] != 1 || e2[0].Flags != `\Seen` || e2[0].Blob != a.SHA {
		t.Fatalf("snapshot 2: %+v", e2)
	}
	snaps, _ := db.Snapshots()
	if len(snaps) != 2 || snaps[0].Messages != 2 || snaps[1].Messages != 1 || snaps[0].Folders != 1 {
		t.Fatalf("snapshots %+v", snaps)
	}
	if latest, _ := db.LatestSnapshot(); latest != r2 {
		t.Fatalf("latest %d", latest)
	}
}

func TestFolderGoneKeepsOldSnapshotsIntact(t *testing.T) {
	db, _ := openTemp(t)
	a := blob("a")
	r1 := newRun(t, db)
	f := newFolder(t, db, r1, "Old")
	if err := db.ApplyFolder(r1, f, FolderChanges{UIDValidity: 7, Blobs: []NewBlob{a}, Open: []NewEntry{{1, a.SHA, ""}}}); err != nil {
		t.Fatal(err)
	}
	finish(t, db, r1)
	r2 := newRun(t, db)
	if err := db.MarkFolderGone(f, r2); err != nil {
		t.Fatal(err)
	}
	finish(t, db, r2)

	if live, _ := db.LiveFolders(); len(live) != 0 {
		t.Fatalf("live %+v", live)
	}
	if fs, _ := db.SnapshotFolders(r1); len(fs) != 1 {
		t.Fatalf("snapshot 1 folders %+v", fs)
	}
	if fs, _ := db.SnapshotFolders(r2); len(fs) != 0 {
		t.Fatalf("snapshot 2 folders %+v", fs)
	}
	if es, _ := db.SnapshotEntries(r1, f); len(es) != 1 {
		t.Fatalf("old snapshot lost its entry: %+v", es)
	}
	if ok, _ := db.HasBlob(a.SHA); !ok {
		t.Fatal("blob must survive a folder going away")
	}
}

func TestSameBlobInTwoFoldersIsOneRow(t *testing.T) {
	db, _ := openTemp(t)
	a := blob("a")
	r := newRun(t, db)
	f1, f2 := newFolder(t, db, r, "A"), newFolder(t, db, r, "B")
	for _, f := range []int64{f1, f2} {
		if err := db.ApplyFolder(r, f, FolderChanges{UIDValidity: 7, Blobs: []NewBlob{a}, Open: []NewEntry{{1, a.SHA, ""}}}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM blobs`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d blob rows", n)
	}
}

// A failure inside ApplyFolder commits nothing: no blob rows, no entries, folder state untouched.
func TestApplyFolderIsAtomic(t *testing.T) {
	db, _ := openTemp(t)
	a, ghost := blob("a"), blob("never inserted")
	r := newRun(t, db)
	f := newFolder(t, db, r, "INBOX")
	err := db.ApplyFolder(r, f, FolderChanges{UIDValidity: 7, LastUID: 9, Blobs: []NewBlob{a},
		Open: []NewEntry{{1, a.SHA, ""}, {2, ghost.SHA, ""}}}) // dangling blob reference
	if err == nil {
		t.Fatal("dangling reference must fail")
	}
	var blobs, entries int
	_ = db.QueryRow(`SELECT count(*) FROM blobs`).Scan(&blobs)
	_ = db.QueryRow(`SELECT count(*) FROM entries`).Scan(&entries)
	if blobs != 0 || entries != 0 {
		t.Fatalf("partial commit: %d blobs %d entries", blobs, entries)
	}
	if live, _ := db.LiveFolders(); live[0].LastUID != 0 {
		t.Fatal("folder state changed despite rollback")
	}
	if v, _ := db.ForeignKeyCheck(); len(v) != 0 {
		t.Fatalf("fk violations %v", v)
	}
}

func TestClosingAClosedEntryFails(t *testing.T) {
	db, _ := openTemp(t)
	a := blob("a")
	r := newRun(t, db)
	f := newFolder(t, db, r, "INBOX")
	_ = db.ApplyFolder(r, f, FolderChanges{UIDValidity: 7, Blobs: []NewBlob{a}, Open: []NewEntry{{1, a.SHA, ""}}})
	open, _ := db.OpenEntries(f)
	r2 := newRun(t, db)
	if err := db.ApplyFolder(r2, f, FolderChanges{UIDValidity: 7, Close: []int64{open[0].ID}}); err != nil {
		t.Fatal(err)
	}
	r3 := newRun(t, db)
	if err := db.ApplyFolder(r3, f, FolderChanges{UIDValidity: 7, Close: []int64{open[0].ID}}); err == nil {
		t.Fatal("second close must fail")
	}
}

func TestBeginRunFailsStaleRunning(t *testing.T) {
	db, _ := openTemp(t)
	r1 := newRun(t, db)
	_ = newRun(t, db)
	var st string
	_ = db.QueryRow(`SELECT status FROM runs WHERE id = ?`, r1).Scan(&st)
	if st != RunFailed {
		t.Fatalf("stale run is %q", st)
	}
}

func TestBlobsByPrefix(t *testing.T) {
	db, _ := openTemp(t)
	a := blob("a")
	r := newRun(t, db)
	f := newFolder(t, db, r, "X")
	_ = db.ApplyFolder(r, f, FolderChanges{UIDValidity: 7, Blobs: []NewBlob{a}, Open: []NewEntry{{1, a.SHA, ""}}})
	hex := ""
	for _, b := range a.SHA[:4] {
		hex += string("0123456789abcdef"[b>>4]) + string("0123456789abcdef"[b&15])
	}
	rows, err := db.BlobsByPrefix(hex, 5)
	if err != nil || len(rows) != 1 || rows[0].SHA != a.SHA || string(rows[0].MetaSealed) != "m-a" {
		t.Fatalf("rows %+v err %v", rows, err)
	}
	if _, err := db.BlobsByPrefix("zz%", 5); err == nil {
		t.Fatal("non-hex prefix must be refused")
	}
}

func TestApplyBatchLeavesFolderRowAndResumes(t *testing.T) {
	db, _ := openTemp(t)
	a, b := blob("a"), blob("b")
	r1 := newRun(t, db)
	f := newFolder(t, db, r1, "INBOX")

	// A killed run committed one batch: entries exist, last_uid did not move.
	if err := db.ApplyBatch(r1, f, 7, []NewBlob{a}, []NewEntry{{UID: 1, Blob: a.SHA}}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyBatch(r1, f, 7, nil, nil); err != nil { // empty batch is a no-op
		t.Fatal(err)
	}
	live, _ := db.LiveFolders()
	if live[0].LastUID != 0 || live[0].HighestModSeq != 0 {
		t.Fatalf("batch advanced the folder row: %+v", live[0])
	}
	open, err := db.OpenEntries(f)
	if err != nil || len(open) != 1 || open[0].UID != 1 || open[0].UIDValidity != 7 {
		t.Fatalf("open %+v err %v", open, err)
	}
	if err := db.FinishRun(r1, RunFailed, t0); err != nil {
		t.Fatal(err)
	}

	// The next run carries on from there and finishes the folder.
	r2 := newRun(t, db)
	if err := db.ApplyBatch(r2, f, 7, []NewBlob{b}, []NewEntry{{UID: 2, Blob: b.SHA}}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyFolder(r2, f, FolderChanges{UIDValidity: 7, LastUID: 2, HighestModSeq: 9}); err != nil {
		t.Fatal(err)
	}
	finish(t, db, r2)
	live, _ = db.LiveFolders()
	if live[0].LastUID != 2 || live[0].HighestModSeq != 9 {
		t.Fatalf("folder row %+v", live[0])
	}
	if es, _ := db.SnapshotEntries(r2, f); len(es) != 2 {
		t.Fatalf("snapshot entries %+v", es)
	}

	// An entry that names a blob row nobody supplied is refused and nothing commits.
	c := blob("c")
	r3 := newRun(t, db)
	if err := db.ApplyBatch(r3, f, 7, nil, []NewEntry{{UID: 3, Blob: c.SHA}}); err == nil {
		t.Fatal("dangling blob reference was accepted")
	}
	if open, _ := db.OpenEntries(f); len(open) != 2 {
		t.Fatalf("failed batch left entries behind: %+v", open)
	}
}

func TestOpenEntriesSpanGenerations(t *testing.T) {
	db, _ := openTemp(t)
	a := blob("a")
	r1 := newRun(t, db)
	f := newFolder(t, db, r1, "INBOX")
	if err := db.ApplyFolder(r1, f, FolderChanges{UIDValidity: 7, LastUID: 1, Blobs: []NewBlob{a}, Open: []NewEntry{{UID: 1, Blob: a.SHA}}}); err != nil {
		t.Fatal(err)
	}
	finish(t, db, r1)
	// a reset: same UID, new generation, opened before the old one was closed
	r2 := newRun(t, db)
	if err := db.ApplyBatch(r2, f, 8, nil, []NewEntry{{UID: 1, Blob: a.SHA}}); err != nil {
		t.Fatalf("same uid in a new generation must not collide: %v", err)
	}
	open, _ := db.OpenEntries(f)
	if len(open) != 2 || open[0].UIDValidity != 7 || open[1].UIDValidity != 8 {
		t.Fatalf("open %+v", open)
	}
}

func TestCompletePartialRun(t *testing.T) {
	db, _ := openTemp(t)
	r1 := newRun(t, db)
	if err := db.CompleteRun(r1, RunFailed, t0); err == nil {
		t.Fatal("a run was completed as failed")
	}
	if err := db.CompleteRun(r1, "bogus", t0); err == nil {
		t.Fatal("a run was completed with an unknown status")
	}
	if snaps, _ := db.Snapshots(); len(snaps) != 0 {
		t.Fatalf("a rejected completion wrote a snapshot: %+v", snaps)
	}
	if err := db.CompleteRun(r1, RunPartial, t0); err != nil {
		t.Fatal(err)
	}
	snaps, _ := db.Snapshots()
	if len(snaps) != 1 || snaps[0].Status != RunPartial {
		t.Fatalf("snapshots %+v", snaps)
	}
}

func TestSnapshotChanges(t *testing.T) {
	db, _ := openTemp(t)
	a, b, c, d := blob("a"), blob("b"), blob("c"), blob("d")

	r1 := newRun(t, db)
	f := newFolder(t, db, r1, "INBOX")
	if err := db.ApplyFolder(r1, f, FolderChanges{UIDValidity: 7, LastUID: 2, Blobs: []NewBlob{a, b},
		Open: []NewEntry{{1, a.SHA, ""}, {2, b.SHA, ""}}}); err != nil {
		t.Fatal(err)
	}
	finish(t, db, r1)

	// r2 crashes after committing: no snapshot, but its changes are real.
	r2 := newRun(t, db)
	open, _ := db.OpenEntries(f)
	if err := db.ApplyFolder(r2, f, FolderChanges{UIDValidity: 7, LastUID: 3, Blobs: []NewBlob{c},
		Open: []NewEntry{{3, c.SHA, ""}}, Reflag: []Reflag{{EntryID: open[0].ID, UID: 1, Blob: a.SHA, Flags: `\Seen`}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRun(r2, RunFailed, t0); err != nil {
		t.Fatal(err)
	}

	r3 := newRun(t, db)
	open, _ = db.OpenEntries(f)
	var bID int64
	for _, e := range open {
		if e.UID == 2 {
			bID = e.ID
		}
	}
	if err := db.ApplyFolder(r3, f, FolderChanges{UIDValidity: 7, LastUID: 4, Blobs: []NewBlob{d},
		Close: []int64{bID}, Open: []NewEntry{{4, d.SHA, ""}}}); err != nil {
		t.Fatal(err)
	}
	finish(t, db, r3)

	removed, added, err := db.SnapshotChanges(r1, r3)
	if err != nil {
		t.Fatal(err)
	}
	type k struct {
		uid   uint32
		flags string
	}
	keys := func(cs []SnapChange) (out []k) {
		for _, c := range cs {
			out = append(out, k{c.UID, c.Flags})
		}
		return
	}
	// removed: uid 1 (unflagged version) and uid 2. added: uid 1 flagged, 3, 4.
	if got := keys(removed); len(got) != 2 || got[0] != (k{1, ""}) || got[1] != (k{2, ""}) {
		t.Fatalf("removed %v", got)
	}
	if got := keys(added); len(got) != 3 || got[0] != (k{1, `\Seen`}) || got[1] != (k{3, ""}) || got[2] != (k{4, ""}) {
		t.Fatalf("added %v", got)
	}
	if removed[0].Blob != a.SHA || removed[0].FolderID != f || removed[0].UIDValidity != 7 {
		t.Fatalf("removed[0] = %+v", removed[0])
	}

	// the same snapshot against itself is an error, as is the wrong order.
	if _, _, err := db.SnapshotChanges(r3, r1); err == nil {
		t.Fatal("reversed range accepted")
	}
	if _, _, err := db.SnapshotChanges(r1, r1); err == nil {
		t.Fatal("empty range accepted")
	}
}

func TestBlobsByEmailID(t *testing.T) {
	db, _ := openTemp(t)
	r1 := newRun(t, db)
	f := newFolder(t, db, r1, "INBOX")
	var blobs []NewBlob
	var opens []NewEntry
	for i := 0; i < 3; i++ {
		x := blob(string(rune('a' + i)))
		x.EmailID = "M" + string(rune('a'+i))
		blobs = append(blobs, x)
		opens = append(opens, NewEntry{UID: uint32(i + 1), Blob: x.SHA})
	}
	if err := db.ApplyFolder(r1, f, FolderChanges{UIDValidity: 7, LastUID: 3, Blobs: blobs, Open: opens}); err != nil {
		t.Fatal(err)
	}
	got, err := db.BlobsByEmailID([]string{"Ma", "Mc", "Mnope"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["Ma"].SHA != blobs[0].SHA || got["Ma"].Size != 1 || got["Mc"].SHA != blobs[2].SHA {
		t.Fatalf("got %+v", got)
	}
	if got, err := db.BlobsByEmailID(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty lookup: %v %v", got, err)
	}
	// more ids than one query takes
	many := make([]string, 1200)
	for i := range many {
		many[i] = "x" + string(rune('A'+i%26)) + string(rune('a'+i/26%26)) + string(rune('0'+i/676))
	}
	many = append(many, "Mb")
	if got, err := db.BlobsByEmailID(many); err != nil || len(got) != 1 || got["Mb"].SHA != blobs[1].SHA {
		t.Fatalf("chunked lookup: %v %v", got, err)
	}
}

func TestFoldersInEither(t *testing.T) {
	db, _ := openTemp(t)
	r1 := newRun(t, db)
	old := newFolder(t, db, r1, "Old")
	finish(t, db, r1)
	r2 := newRun(t, db)
	renamed := newFolder(t, db, r2, "New")
	if err := db.MarkFolderGone(old, r2); err != nil {
		t.Fatal(err)
	}
	finish(t, db, r2)
	fs, err := db.FoldersInEither(r1, r2)
	if err != nil || len(fs) != 2 || fs[0].ID != old || fs[1].ID != renamed {
		t.Fatalf("folders %+v err %v", fs, err)
	}
}
