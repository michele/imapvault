package plan

import (
	"reflect"
	"testing"
)

func fd(deleted int) FolderDiff { return FolderDiff{Deleted: deleted} }

func TestCheckGuard(t *testing.T) {
	pol := GuardPolicy{MaxDeletedPercent: 10, MaxDeletedCount: 500}
	tests := []struct {
		name     string
		in       GuardInput
		pol      GuardPolicy
		tripped  bool
		kind     string
		deleted  int
		vanished []int64
	}{
		{"no previous snapshot", GuardInput{PrevSnapshot: 0, PrevMessages: 0, Folders: map[int64]FolderDiff{1: fd(900)}}, pol, false, "", 0, nil},
		{"nothing changed", GuardInput{PrevSnapshot: 1, PrevMessages: 1000}, pol, false, "", 0, nil},
		{"percent at the limit", GuardInput{PrevSnapshot: 1, PrevMessages: 1000, Folders: map[int64]FolderDiff{1: fd(100)}}, pol, false, "", 100, nil},
		{"percent just over", GuardInput{PrevSnapshot: 1, PrevMessages: 1000, Folders: map[int64]FolderDiff{1: fd(101)}}, pol, true, GuardMassDeletion, 101, nil},
		{"count at the limit", GuardInput{PrevSnapshot: 1, PrevMessages: 100000, Folders: map[int64]FolderDiff{1: fd(500)}}, pol, false, "", 500, nil},
		{"count just over", GuardInput{PrevSnapshot: 1, PrevMessages: 100000, Folders: map[int64]FolderDiff{1: fd(501)}}, pol, true, GuardMassDeletion, 501, nil},
		{"sums folders", GuardInput{PrevSnapshot: 1, PrevMessages: 100, Folders: map[int64]FolderDiff{1: fd(6), 2: fd(6)}}, pol, true, GuardMassDeletion, 12, nil},
		{"percent off", GuardInput{PrevSnapshot: 1, PrevMessages: 100, Folders: map[int64]FolderDiff{1: fd(99)}}, GuardPolicy{MaxDeletedCount: 500}, false, "", 99, nil},
		{"count off", GuardInput{PrevSnapshot: 1, PrevMessages: 100000, Folders: map[int64]FolderDiff{1: fd(900)}}, GuardPolicy{MaxDeletedPercent: 10}, false, "", 900, nil},
		{"both off", GuardInput{PrevSnapshot: 1, PrevMessages: 100, Folders: map[int64]FolderDiff{1: fd(100)}}, GuardPolicy{}, false, "", 100, nil},
		{"previous snapshot was empty", GuardInput{PrevSnapshot: 1, PrevMessages: 0, Folders: map[int64]FolderDiff{1: fd(5)}}, pol, false, "", 5, nil},
		{"moves, reflags and renumbering are not deletions",
			GuardInput{PrevSnapshot: 1, PrevMessages: 100, Folders: map[int64]FolderDiff{
				1: {MovedOut: 90, Reflagged: 90, Renumbered: 90}, 2: {MovedIn: 90, Added: 50}}},
			pol, false, "", 0, nil},
		{"rename: folder vanished, nothing deleted",
			GuardInput{PrevSnapshot: 1, PrevMessages: 100, Folders: map[int64]FolderDiff{1: {MovedOut: 40}, 2: {MovedIn: 40}}, Vanished: []int64{1}},
			pol, false, "", 0, nil},
		{"vanished folder that lost mail trips even below the thresholds",
			GuardInput{PrevSnapshot: 1, PrevMessages: 100000, Folders: map[int64]FolderDiff{3: fd(2)}, Vanished: []int64{3}},
			pol, true, GuardFolderVanished, 2, []int64{3}},
		{"vanished folder trips even with thresholds off",
			GuardInput{PrevSnapshot: 1, PrevMessages: 100, Folders: map[int64]FolderDiff{3: fd(2)}, Vanished: []int64{3}},
			GuardPolicy{}, true, GuardFolderVanished, 2, []int64{3}},
		{"vanished folder, partly moved, partly deleted",
			GuardInput{PrevSnapshot: 1, PrevMessages: 100000, Folders: map[int64]FolderDiff{3: {Deleted: 1, MovedOut: 9}}, Vanished: []int64{3}},
			pol, true, GuardFolderVanished, 1, []int64{3}},
		{"emptying Trash",
			GuardInput{PrevSnapshot: 1, PrevMessages: 200, Folders: map[int64]FolderDiff{7: fd(60)}},
			pol, true, GuardMassDeletion, 60, nil},
		{"mass deletion wins over vanished",
			GuardInput{PrevSnapshot: 1, PrevMessages: 100, Folders: map[int64]FolderDiff{3: fd(50)}, Vanished: []int64{3}},
			pol, true, GuardMassDeletion, 50, []int64{3}},
		{"a vanished folder with no changes is quiet",
			GuardInput{PrevSnapshot: 1, PrevMessages: 100, Vanished: []int64{9}},
			pol, false, "", 0, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckGuard(tc.in, tc.pol)
			if got.Tripped != tc.tripped || got.Kind != tc.kind || got.Deleted != tc.deleted {
				t.Fatalf("got tripped=%v kind=%q deleted=%d; want tripped=%v kind=%q deleted=%d", got.Tripped, got.Kind, got.Deleted, tc.tripped, tc.kind, tc.deleted)
			}
			if !reflect.DeepEqual(got.VanishedWithLoss, tc.vanished) {
				t.Fatalf("vanished with loss = %v, want %v", got.VanishedWithLoss, tc.vanished)
			}
		})
	}
}

func TestCheckGuardPercentAndOrdering(t *testing.T) {
	res := CheckGuard(GuardInput{
		PrevSnapshot: 4, PrevMessages: 1300,
		Folders:  map[int64]FolderDiff{5: fd(10), 1: fd(880), 2: fd(400), 9: fd(10)},
		Vanished: []int64{9, 9},
	}, GuardPolicy{MaxDeletedPercent: 10, MaxDeletedCount: 500})
	if !res.Tripped || res.Deleted != 1300 {
		t.Fatalf("got %+v", res)
	}
	if res.Percent != 100 {
		t.Errorf("percent = %v, want 100", res.Percent)
	}
	want := []FolderDeleted{{1, 880}, {2, 400}, {5, 10}, {9, 10}}
	if !reflect.DeepEqual(res.ByFolder, want) {
		t.Errorf("by folder = %v, want %v", res.ByFolder, want)
	}
	if !reflect.DeepEqual(res.VanishedWithLoss, []int64{9}) {
		t.Errorf("vanished = %v, want [9] (deduplicated)", res.VanishedWithLoss)
	}
}
