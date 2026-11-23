package index

import (
	"testing"
)

func deletedUIDs(t *testing.T, f *fixture, cutoff int) map[[32]byte]DeletedEntry {
	t.Helper()
	got, err := f.db.DeletedSince(day(cutoff))
	if err != nil {
		t.Fatal(err)
	}
	out := map[[32]byte]DeletedEntry{}
	for _, e := range got {
		out[e.Blob] = e
	}
	if len(out) != len(got) {
		t.Fatalf("duplicate blobs in %+v", got)
	}
	return out
}

// newHistory: a expunged Jan 4, b expunged Jan 2, shared moved Jan 3 (still on the server).
func TestDeletedSinceWindow(t *testing.T) {
	f := newHistory(t)

	all := deletedUIDs(t, f, 1)
	if len(all) != 2 || all[f.a.SHA].ClosedRun != 4 || all[f.b.SHA].ClosedRun != 2 {
		t.Fatalf("since Jan 1 = %+v, want a (run 4) and b (run 2)", all)
	}
	if _, moved := all[f.shared.SHA]; moved {
		t.Fatal("a moved message is not deleted: it is open in the other folder")
	}
	if e := all[f.b.SHA]; e.FolderID != f.inbox || e.UID != 2 || e.UIDValidity != 7 || e.Size != f.b.Size {
		t.Fatalf("b = %+v", e)
	}

	// The window starts at the run's start time, inclusive.
	if got := deletedUIDs(t, f, 2); len(got) != 2 {
		t.Fatalf("since Jan 2 (b closed by the run that started then) = %d entries, want 2", len(got))
	}
	if got := deletedUIDs(t, f, 3); len(got) != 1 || got[f.a.SHA].ClosedRun != 4 {
		t.Fatalf("since Jan 3 = %+v, want only a", got)
	}
	if got := deletedUIDs(t, f, 5); len(got) != 0 {
		t.Fatalf("since Jan 5 = %+v, want none", got)
	}
}

// A flag change closes a row and opens another at the same position. Only the
// last row is the deleted message, and a message that is still open is not deleted.
func TestDeletedSinceUsesTheLastRowOfAPosition(t *testing.T) {
	db, _ := openTemp(t)
	x, y := blob("x"), blob("y")

	r1 := runAt(t, db, day(1))
	folder := newFolder(t, db, r1, "INBOX")
	if err := db.ApplyFolder(r1, folder, FolderChanges{UIDValidity: 7, LastUID: 2, Blobs: []NewBlob{x, y},
		Open: []NewEntry{{1, x.SHA, ""}, {2, y.SHA, ""}}}); err != nil {
		t.Fatal(err)
	}
	finishAt(t, db, r1, day(1))

	// Run 2: both get \Seen (close + reopen at the same position).
	r2 := runAt(t, db, day(2))
	open := entriesByUID(t, db, folder)
	if err := db.ApplyFolder(r2, folder, FolderChanges{UIDValidity: 7, LastUID: 2,
		Close: []int64{open[1], open[2]}, Open: []NewEntry{{1, x.SHA, `\Seen`}, {2, y.SHA, `\Seen`}}}); err != nil {
		t.Fatal(err)
	}
	finishAt(t, db, r2, day(2))

	// Run 3: x is expunged, y stays.
	r3 := runAt(t, db, day(3))
	open = entriesByUID(t, db, folder)
	if err := db.ApplyFolder(r3, folder, FolderChanges{UIDValidity: 7, LastUID: 2, Close: []int64{open[1]}}); err != nil {
		t.Fatal(err)
	}
	finishAt(t, db, r3, day(3))

	got, err := db.DeletedSince(day(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v, want only x (y is still on the server; x's flag-change row is not a second deletion)", got)
	}
	if got[0].Blob != x.SHA || got[0].Flags != `\Seen` || got[0].ClosedRun != 3 {
		t.Fatalf("x = %+v, want the \\Seen row closed by run 3", got[0])
	}
}

// Two copies of one blob, one deleted: the other copy is on the server, so the
// blob is not deleted. Both deleted: both rows come back.
func TestDeletedSinceDuplicateCopies(t *testing.T) {
	db, _ := openTemp(t)
	d := blob("dup")

	r1 := runAt(t, db, day(1))
	folder := newFolder(t, db, r1, "INBOX")
	if err := db.ApplyFolder(r1, folder, FolderChanges{UIDValidity: 7, LastUID: 2, Blobs: []NewBlob{d},
		Open: []NewEntry{{1, d.SHA, ""}, {2, d.SHA, ""}}}); err != nil {
		t.Fatal(err)
	}
	finishAt(t, db, r1, day(1))

	r2 := runAt(t, db, day(2))
	open := entriesByUID(t, db, folder)
	if err := db.ApplyFolder(r2, folder, FolderChanges{UIDValidity: 7, LastUID: 2, Close: []int64{open[1]}}); err != nil {
		t.Fatal(err)
	}
	finishAt(t, db, r2, day(2))
	if got, _ := db.DeletedSince(day(1)); len(got) != 0 {
		t.Fatalf("one copy is still open: %+v", got)
	}

	r3 := runAt(t, db, day(3))
	open = entriesByUID(t, db, folder)
	if err := db.ApplyFolder(r3, folder, FolderChanges{UIDValidity: 7, LastUID: 2, Close: []int64{open[2]}}); err != nil {
		t.Fatal(err)
	}
	finishAt(t, db, r3, day(3))
	got, err := db.DeletedSince(day(1))
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, err %v; want both copies", got, err)
	}
}
