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
	if err := db.CompleteRun(run, t0); err != nil {
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
