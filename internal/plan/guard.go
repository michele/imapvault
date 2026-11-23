package plan

import "slices"

// Guard alert kinds.
const (
	GuardMassDeletion   = "mass-deletion"
	GuardFolderVanished = "folder-vanished"
)

// GuardPolicy holds the thresholds. A threshold of 0 switches that threshold
// off. The vanished-folder rule has no switch: it is always on.
type GuardPolicy struct {
	MaxDeletedPercent float64 // of the messages in the previous snapshot
	MaxDeletedCount   int
}

// GuardInput is what a run changed compared with the previous snapshot.
type GuardInput struct {
	PrevSnapshot int64 // 0 when there is no previous snapshot: nothing to compare with
	PrevMessages int   // messages in the previous snapshot
	// Folders is DiffSnapshots(previous snapshot, this run). Only Deleted counts:
	// moves, re-flags and renumbering do not.
	Folders map[int64]FolderDiff
	// Vanished lists folders that went away since the previous snapshot.
	Vanished []int64
}

// FolderDeleted is the number of deleted messages in one folder.
type FolderDeleted struct {
	FolderID int64
	Deleted  int
}

// GuardResult says whether the run looks like a mass deletion.
type GuardResult struct {
	Tripped bool
	Kind    string // GuardMassDeletion or GuardFolderVanished; "" when not tripped
	Deleted int    // messages deleted since the previous snapshot
	Percent float64
	// VanishedWithLoss lists vanished folders that lost messages for good. A
	// folder that vanished with every message moved elsewhere (a rename) is not here.
	VanishedWithLoss []int64
	ByFolder         []FolderDeleted // folders with deletions, most first, then by id
}

// CheckGuard is the mass-deletion guard. It trips when deletions strictly exceed
// MaxDeletedCount or MaxDeletedPercent (of the previous snapshot), or when a
// folder vanished and some of its messages are gone rather than moved. It is a
// pure function of its inputs.
func CheckGuard(in GuardInput, pol GuardPolicy) GuardResult {
	var res GuardResult
	if in.PrevSnapshot == 0 {
		return res
	}
	for id, d := range in.Folders {
		if d.Deleted > 0 {
			res.Deleted += d.Deleted
			res.ByFolder = append(res.ByFolder, FolderDeleted{FolderID: id, Deleted: d.Deleted})
		}
	}
	slices.SortFunc(res.ByFolder, func(a, b FolderDeleted) int {
		if a.Deleted != b.Deleted {
			return b.Deleted - a.Deleted
		}
		return compareInt64(a.FolderID, b.FolderID)
	})
	if in.PrevMessages > 0 {
		res.Percent = float64(res.Deleted) * 100 / float64(in.PrevMessages)
	}
	for _, id := range in.Vanished {
		if in.Folders[id].Deleted > 0 {
			res.VanishedWithLoss = append(res.VanishedWithLoss, id)
		}
	}
	slices.Sort(res.VanishedWithLoss)
	res.VanishedWithLoss = slices.Compact(res.VanishedWithLoss)

	switch {
	case pol.MaxDeletedCount > 0 && res.Deleted > pol.MaxDeletedCount,
		pol.MaxDeletedPercent > 0 && in.PrevMessages > 0 && res.Percent > pol.MaxDeletedPercent:
		res.Tripped, res.Kind = true, GuardMassDeletion
	case len(res.VanishedWithLoss) > 0:
		res.Tripped, res.Kind = true, GuardFolderVanished
	}
	return res
}
