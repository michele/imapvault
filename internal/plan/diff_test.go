package plan

import (
	"math/rand"
	"reflect"
	"testing"
)

func de(folder int64, uv, uid uint32, blob byte, flags string) DiffEntry {
	return DiffEntry{FolderID: folder, UIDValidity: uv, UID: uid, Blob: h(blob), Flags: flags}
}

func TestDiffSnapshots(t *testing.T) {
	tests := []struct {
		name    string
		removed []DiffEntry
		added   []DiffEntry
		want    map[int64]FolderDiff
	}{
		{name: "nothing", want: map[int64]FolderDiff{}},
		{name: "added", added: []DiffEntry{de(1, 1, 5, 1, ""), de(1, 1, 6, 2, "")}, want: map[int64]FolderDiff{1: {Added: 2}}},
		{name: "deleted", removed: []DiffEntry{de(1, 1, 5, 1, "")}, want: map[int64]FolderDiff{1: {Deleted: 1}}},
		{name: "flag change", removed: []DiffEntry{de(1, 1, 5, 1, "")}, added: []DiffEntry{de(1, 1, 5, 1, `\Seen`)},
			want: map[int64]FolderDiff{1: {Reflagged: 1}}},
		{name: "identical entry reopened is not a change", removed: []DiffEntry{de(1, 1, 5, 1, `\Seen`)}, added: []DiffEntry{de(1, 1, 5, 1, `\Seen`)},
			want: map[int64]FolderDiff{}},
		{name: "moved between folders",
			removed: []DiffEntry{de(1, 1, 5, 1, "")}, added: []DiffEntry{de(2, 1, 9, 1, "")},
			want: map[int64]FolderDiff{1: {MovedOut: 1}, 2: {MovedIn: 1}}},
		{name: "renumbered after a UIDVALIDITY reset",
			removed: []DiffEntry{de(1, 1, 5, 1, ""), de(1, 1, 6, 2, "")}, added: []DiffEntry{de(1, 2, 1, 1, ""), de(1, 2, 2, 2, "")},
			want: map[int64]FolderDiff{1: {Renumbered: 2}}},
		{name: "folder rename: old folder moves out, new folder moves in",
			removed: []DiffEntry{de(1, 1, 1, 1, ""), de(1, 1, 2, 2, "")}, added: []DiffEntry{de(7, 1, 1, 1, ""), de(7, 1, 2, 2, "")},
			want: map[int64]FolderDiff{1: {MovedOut: 2}, 7: {MovedIn: 2}}},
		{name: "same blob twice in one folder, one copy deleted",
			removed: []DiffEntry{de(1, 1, 1, 1, ""), de(1, 1, 2, 1, "")}, added: []DiffEntry{de(1, 1, 1, 1, "")},
			want: map[int64]FolderDiff{1: {Deleted: 1}}},
		{name: "copy to another folder is an addition, not a move",
			added: []DiffEntry{de(2, 1, 1, 1, "")},
			want:  map[int64]FolderDiff{2: {Added: 1}}},
		{name: "one blob leaves two folders, arrives in one: one move, one delete (folder-id order)",
			removed: []DiffEntry{de(2, 1, 1, 1, ""), de(1, 1, 1, 1, "")}, added: []DiffEntry{de(3, 1, 1, 1, "")},
			want: map[int64]FolderDiff{1: {MovedOut: 1}, 2: {Deleted: 1}, 3: {MovedIn: 1}}},
		{name: "everything at once",
			removed: []DiffEntry{de(1, 1, 1, 1, ""), de(1, 1, 2, 2, ""), de(1, 1, 3, 3, ""), de(1, 1, 4, 4, "")},
			added:   []DiffEntry{de(1, 1, 1, 1, `\Seen`), de(2, 1, 1, 2, ""), de(1, 1, 9, 9, "")},
			want:    map[int64]FolderDiff{1: {Reflagged: 1, MovedOut: 1, Deleted: 2, Added: 1}, 2: {MovedIn: 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DiffSnapshots(tc.removed, tc.added)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

// The result must not depend on input order, and each removed or added entry
// must be counted exactly once.
func TestDiffSnapshotsInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 300; i++ {
		var removed, added []DiffEntry
		for n := rng.Intn(12); n > 0; n-- {
			removed = append(removed, de(int64(rng.Intn(3)+1), uint32(rng.Intn(2)+1), uint32(rng.Intn(5)+1), byte(rng.Intn(4)+1), []string{"", `\Seen`}[rng.Intn(2)]))
		}
		for n := rng.Intn(12); n > 0; n-- {
			added = append(added, de(int64(rng.Intn(3)+1), uint32(rng.Intn(2)+1), uint32(rng.Intn(5)+1), byte(rng.Intn(4)+1), []string{"", `\Seen`}[rng.Intn(2)]))
		}
		want := DiffSnapshots(removed, added)
		sr, sa := append([]DiffEntry(nil), removed...), append([]DiffEntry(nil), added...)
		rng.Shuffle(len(sr), func(a, b int) { sr[a], sr[b] = sr[b], sr[a] })
		rng.Shuffle(len(sa), func(a, b int) { sa[a], sa[b] = sa[b], sa[a] })
		if got := DiffSnapshots(sr, sa); !reflect.DeepEqual(got, want) {
			t.Fatalf("order changed the result:\nremoved=%v\nadded=%v\n%v vs %v", removed, added, want, got)
		}
		var outs, ins int
		for _, d := range want {
			outs += d.Deleted + d.MovedOut + d.Renumbered + d.Reflagged
			ins += d.Added + d.MovedIn + d.Renumbered + d.Reflagged
		}
		// Reflagged pairs with equal flags are dropped, so counts can only be <=.
		if outs > len(removed) || ins > len(added) {
			t.Fatalf("counted more than given: outs=%d/%d ins=%d/%d", outs, len(removed), ins, len(added))
		}
	}
}
