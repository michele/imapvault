package plan

import (
	"bytes"
	"slices"
	"sort"
	"time"
)

// PruneEntry is a closed index entry: the message was present in snapshots
// From <= S < To. ClosedAt is when the run that observed the deletion started.
type PruneEntry struct {
	ID       int64
	Blob     [32]byte
	From, To int64
	ClosedAt time.Time
}

// PruneSnapshot is a snapshot as prune sees it. A forgotten snapshot that is
// pinned counts as kept: pins always win.
type PruneSnapshot struct {
	ID        int64
	Forgotten bool
	Pinned    bool
}

// PruneInput is everything prune decides from. It performs no I/O.
type PruneInput struct {
	// Entries are the closed entries only; open entries are never candidates.
	Entries   []PruneEntry
	Snapshots []PruneSnapshot
	// Cutoff is now minus deleted_grace: an entry closed at or after it is kept.
	Cutoff time.Time
	// Refs is the number of entries (open and closed) that reference each blob.
	Refs map[[32]byte]int
	// Unreferenced lists blob rows that no entry references at all.
	Unreferenced [][32]byte
}

// PruneResult is what to remove, each list sorted.
type PruneResult struct {
	Entries   []int64    // entry ids
	Blobs     [][32]byte // blob keys: rows and files
	Snapshots []int64    // forgotten snapshots that are no longer complete
}

// Prune decides what to remove. An entry goes when it was closed before the
// cutoff and no kept snapshot (not forgotten, or pinned) lies in From <= S < To.
// A blob goes when every entry referencing it goes, or nothing references it. A
// forgotten snapshot goes when one of its entries does: it would no longer be
// complete. Anything inconsistent (a blob with no reference count) is kept.
func Prune(in PruneInput) PruneResult {
	var kept, forgotten []int64
	for _, s := range in.Snapshots {
		if s.Forgotten && !s.Pinned {
			forgotten = append(forgotten, s.ID)
		} else {
			kept = append(kept, s.ID)
		}
	}
	slices.Sort(kept)
	slices.Sort(forgotten)

	var res PruneResult
	dropped := map[[32]byte]int{}
	gone := map[int64]bool{}
	for _, e := range in.Entries {
		if !e.ClosedAt.Before(in.Cutoff) || covers(kept, e.From, e.To) {
			continue
		}
		res.Entries = append(res.Entries, e.ID)
		dropped[e.Blob]++
		i := sort.Search(len(forgotten), func(i int) bool { return forgotten[i] >= e.From })
		for ; i < len(forgotten) && forgotten[i] < e.To; i++ {
			gone[forgotten[i]] = true
		}
	}
	for b, n := range dropped {
		if refs, ok := in.Refs[b]; ok && refs == n {
			res.Blobs = append(res.Blobs, b)
		}
	}
	res.Blobs = append(res.Blobs, in.Unreferenced...)
	for id := range gone {
		res.Snapshots = append(res.Snapshots, id)
	}
	slices.Sort(res.Entries)
	slices.Sort(res.Snapshots)
	slices.SortFunc(res.Blobs, func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) })
	res.Blobs = slices.Compact(res.Blobs)
	return res
}

// covers reports whether the sorted ids hold one with from <= id < to.
func covers(sorted []int64, from, to int64) bool {
	i := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= from })
	return i < len(sorted) && sorted[i] < to
}
