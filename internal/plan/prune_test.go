package plan

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

func blob(b byte) [32]byte { return [32]byte{b} }

// pruneNow is "now" for the prune tests; the grace period is 10 days, so the
// cutoff is 2026-03-01 00:00.
var (
	pruneNow    = time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)
	pruneCutoff = pruneNow.Add(-10 * 24 * time.Hour)
)

func old() time.Time    { return pruneCutoff.Add(-time.Hour) }
func recent() time.Time { return pruneCutoff.Add(time.Hour) }

// ent is a closed entry covering snapshots from <= S < to.
func ent(id int64, b byte, from, to int64, closed time.Time) PruneEntry {
	return PruneEntry{ID: id, Blob: blob(b), From: from, To: to, ClosedAt: closed}
}

func snapOK(ids ...int64) []PruneSnapshot {
	var out []PruneSnapshot
	for _, id := range ids {
		out = append(out, PruneSnapshot{ID: id})
	}
	return out
}

func TestPrune(t *testing.T) {
	tests := []struct {
		name      string
		in        PruneInput
		entries   []int64
		blobs     [][32]byte
		snapshots []int64
	}{
		{
			name: "nothing to do",
			in:   PruneInput{Cutoff: pruneCutoff},
		},
		{
			name: "closed long ago and covering no kept snapshot: dropped with its blob",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 1, 3, old())},
				Snapshots: snapOK(5),
				Refs:      map[[32]byte]int{blob(1): 1},
				Cutoff:    pruneCutoff,
			},
			entries: []int64{1},
			blobs:   [][32]byte{blob(1)},
		},
		{
			name: "inside the grace period: kept",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 1, 3, recent())},
				Snapshots: snapOK(5),
				Refs:      map[[32]byte]int{blob(1): 1},
				Cutoff:    pruneCutoff,
			},
		},
		{
			name: "closed exactly at the cutoff: kept",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 1, 3, pruneCutoff)},
				Snapshots: snapOK(5),
				Refs:      map[[32]byte]int{blob(1): 1},
				Cutoff:    pruneCutoff,
			},
		},
		{
			name: "covered by a kept snapshot: kept however old",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 1, 5, old())},
				Snapshots: snapOK(2),
				Refs:      map[[32]byte]int{blob(1): 1},
				Cutoff:    pruneCutoff,
			},
		},
		{
			name: "the interval is from <= S < to: S == from covers",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 2, 5, old())},
				Snapshots: snapOK(2),
				Refs:      map[[32]byte]int{blob(1): 1},
				Cutoff:    pruneCutoff,
			},
		},
		{
			name: "the interval is from <= S < to: S == to does not cover",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 2, 5, old())},
				Snapshots: snapOK(5),
				Refs:      map[[32]byte]int{blob(1): 1},
				Cutoff:    pruneCutoff,
			},
			entries: []int64{1},
			blobs:   [][32]byte{blob(1)},
		},
		{
			name: "the interval is from <= S < to: S just before from does not cover",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 2, 5, old())},
				Snapshots: snapOK(1),
				Refs:      map[[32]byte]int{blob(1): 1},
				Cutoff:    pruneCutoff,
			},
			entries: []int64{1},
			blobs:   [][32]byte{blob(1)},
		},
		{
			name: "covered only by a forgotten snapshot: dropped, and that snapshot goes with it",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 1, 3, old())},
				Snapshots: []PruneSnapshot{{ID: 2, Forgotten: true}, {ID: 5}},
				Refs:      map[[32]byte]int{blob(1): 1},
				Cutoff:    pruneCutoff,
			},
			entries:   []int64{1},
			blobs:     [][32]byte{blob(1)},
			snapshots: []int64{2},
		},
		{
			name: "a forgotten snapshot whose mail is all still held stays",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 1, 3, recent())},
				Snapshots: []PruneSnapshot{{ID: 2, Forgotten: true}, {ID: 5}},
				Refs:      map[[32]byte]int{blob(1): 1},
				Cutoff:    pruneCutoff,
			},
		},
		{
			name: "a forgotten snapshot loses its row as soon as one of its entries goes",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 1, 3, old()), ent(2, 2, 1, 3, recent())},
				Snapshots: []PruneSnapshot{{ID: 2, Forgotten: true}, {ID: 5}},
				Refs:      map[[32]byte]int{blob(1): 1, blob(2): 1},
				Cutoff:    pruneCutoff,
			},
			entries:   []int64{1},
			blobs:     [][32]byte{blob(1)},
			snapshots: []int64{2},
		},
		{
			name: "a pinned forgotten snapshot counts as kept",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 1, 3, old())},
				Snapshots: []PruneSnapshot{{ID: 2, Forgotten: true, Pinned: true}, {ID: 5}},
				Refs:      map[[32]byte]int{blob(1): 1},
				Cutoff:    pruneCutoff,
			},
		},
		{
			name: "entry covering no snapshot at all (a failed run closed it): dropped after grace",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 3, 4, old())},
				Snapshots: snapOK(1, 2, 5),
				Refs:      map[[32]byte]int{blob(1): 1},
				Cutoff:    pruneCutoff,
			},
			entries: []int64{1},
			blobs:   [][32]byte{blob(1)},
		},
		{
			name: "blob also held by an open entry: entry dropped, blob kept",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 1, 3, old())},
				Snapshots: snapOK(5),
				Refs:      map[[32]byte]int{blob(1): 2}, // one closed, one open
				Cutoff:    pruneCutoff,
			},
			entries: []int64{1},
		},
		{
			name: "blob in two folders (a move): kept until both entries go",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 1, 3, old()), ent(2, 1, 3, 4, recent())},
				Snapshots: snapOK(5),
				Refs:      map[[32]byte]int{blob(1): 2},
				Cutoff:    pruneCutoff,
			},
			entries: []int64{1},
		},
		{
			name: "blob in two entries that both go: dropped once",
			in: PruneInput{
				Entries:   []PruneEntry{ent(1, 1, 1, 3, old()), ent(2, 1, 3, 4, old())},
				Snapshots: snapOK(5),
				Refs:      map[[32]byte]int{blob(1): 2},
				Cutoff:    pruneCutoff,
			},
			entries: []int64{1, 2},
			blobs:   [][32]byte{blob(1)},
		},
		{
			name: "blob rows nothing references are dropped",
			in: PruneInput{
				Unreferenced: [][32]byte{blob(9), blob(3)},
				Cutoff:       pruneCutoff,
			},
			blobs: [][32]byte{blob(3), blob(9)},
		},
		{
			name: "results are sorted",
			in: PruneInput{
				Entries:   []PruneEntry{ent(7, 5, 1, 2, old()), ent(3, 4, 1, 2, old()), ent(5, 2, 1, 2, old())},
				Snapshots: snapOK(9),
				Refs:      map[[32]byte]int{blob(5): 1, blob(4): 1, blob(2): 1},
				Cutoff:    pruneCutoff,
			},
			entries: []int64{3, 5, 7},
			blobs:   [][32]byte{blob(2), blob(4), blob(5)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := Prune(tc.in)
			if !sameList(res.Entries, tc.entries) {
				t.Errorf("entries = %v, want %v", res.Entries, tc.entries)
			}
			if !sameList(res.Blobs, tc.blobs) {
				t.Errorf("blobs = %v, want %v", res.Blobs, tc.blobs)
			}
			if !sameList(res.Snapshots, tc.snapshots) {
				t.Errorf("snapshots = %v, want %v", res.Snapshots, tc.snapshots)
			}
		})
	}
}

// sameList compares two lists, treating nil and empty as equal.
func sameList[T comparable](a, b []T) bool { return slices.Equal(a, b) }

func TestPruneDoesNotMutateItsInput(t *testing.T) {
	in := PruneInput{
		Entries:      []PruneEntry{ent(2, 1, 1, 3, old()), ent(1, 2, 1, 3, old())},
		Snapshots:    []PruneSnapshot{{ID: 5}, {ID: 4}},
		Refs:         map[[32]byte]int{blob(1): 1, blob(2): 1},
		Unreferenced: [][32]byte{blob(9), blob(8)},
		Cutoff:       pruneCutoff,
	}
	want := PruneInput{
		Entries:      append([]PruneEntry(nil), in.Entries...),
		Snapshots:    append([]PruneSnapshot(nil), in.Snapshots...),
		Refs:         map[[32]byte]int{blob(1): 1, blob(2): 1},
		Unreferenced: append([][32]byte(nil), in.Unreferenced...),
		Cutoff:       pruneCutoff,
	}
	_ = Prune(in)
	if !reflect.DeepEqual(in, want) {
		t.Errorf("Prune changed its input: %+v", in)
	}
}

// A dropped blob must never be one that a kept entry still references: the
// refcount comparison is the only thing standing between prune and data loss.
func TestPruneNeverDropsABlobWithAnUncountedReference(t *testing.T) {
	in := PruneInput{
		Entries:   []PruneEntry{ent(1, 1, 1, 3, old())},
		Snapshots: snapOK(5),
		// Refs says there are two references but only one is a prune candidate.
		Refs:   map[[32]byte]int{blob(1): 2},
		Cutoff: pruneCutoff,
	}
	if res := Prune(in); len(res.Blobs) != 0 {
		t.Errorf("blobs = %v, want none", res.Blobs)
	}
}

// A blob that has candidate entries but no refcount at all is an inconsistent
// input: do not drop it.
func TestPruneKeepsABlobWithoutAReferenceCount(t *testing.T) {
	in := PruneInput{
		Entries:   []PruneEntry{ent(1, 1, 1, 3, old())},
		Snapshots: snapOK(5),
		Cutoff:    pruneCutoff,
	}
	if res := Prune(in); len(res.Blobs) != 0 {
		t.Errorf("blobs = %v, want none", res.Blobs)
	}
}
