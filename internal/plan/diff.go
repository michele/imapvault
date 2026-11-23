package plan

import "slices"

// DiffEntry is one index entry that differs between two snapshots.
type DiffEntry struct {
	FolderID    int64
	UIDValidity uint32
	UID         uint32
	Blob        [32]byte
	Flags       string
}

// FolderDiff counts what changed in one folder between two snapshots.
type FolderDiff struct {
	Added      int // new messages (a blob this folder did not have)
	Deleted    int // messages gone, and not found anywhere else
	MovedIn    int // a blob that left another folder arrived here
	MovedOut   int // a blob that left here arrived in another folder
	Reflagged  int // same message, different flags
	Renumbered int // same blob, same folder, new position (UIDVALIDITY reset, re-append)
}

// Empty reports whether nothing changed.
func (d FolderDiff) Empty() bool { return d == FolderDiff{} }

type posKey struct {
	folder int64
	uv     uint32
	uid    uint32
	blob   [32]byte
}

type folderBlob struct {
	folder int64
	blob   [32]byte
}

// DiffSnapshots classifies the entries valid in the older snapshot but not the
// newer one (removed) and the other way round (added). It is a pure function of
// its inputs and deterministic: a blob that left one folder and appeared in
// another counts as a move, matched in folder-id order, so a rename (old folder
// gone, new folder new) shows up as moves out of the old and into the new.
// Folders with no change are not in the result.
func DiffSnapshots(removed, added []DiffEntry) map[int64]FolderDiff {
	out := map[int64]FolderDiff{}
	bump := func(folder int64, f func(*FolderDiff)) {
		d := out[folder]
		f(&d)
		out[folder] = d
	}

	// 1. Same position, same blob: the flags changed.
	rem := map[posKey][]DiffEntry{}
	for _, e := range removed {
		k := posKey{e.FolderID, e.UIDValidity, e.UID, e.Blob}
		rem[k] = append(rem[k], e)
	}
	var addedLeft []DiffEntry
	for _, e := range added {
		k := posKey{e.FolderID, e.UIDValidity, e.UID, e.Blob}
		if old := rem[k]; len(old) > 0 {
			rem[k] = old[1:]
			if old[0].Flags != e.Flags {
				bump(e.FolderID, func(d *FolderDiff) { d.Reflagged++ })
			}
			continue
		}
		addedLeft = append(addedLeft, e)
	}
	var removedLeft []DiffEntry
	for _, e := range removed {
		k := posKey{e.FolderID, e.UIDValidity, e.UID, e.Blob}
		if len(rem[k]) > 0 {
			removedLeft = append(removedLeft, e)
			rem[k] = rem[k][1:]
		}
	}

	// 2. Same blob in the same folder, new position.
	remByFB := map[folderBlob]int{}
	for _, e := range removedLeft {
		remByFB[folderBlob{e.FolderID, e.Blob}]++
	}
	var added2 []DiffEntry
	for _, e := range addedLeft {
		k := folderBlob{e.FolderID, e.Blob}
		if remByFB[k] > 0 {
			remByFB[k]--
			bump(e.FolderID, func(d *FolderDiff) { d.Renumbered++ })
			continue
		}
		added2 = append(added2, e)
	}
	// remByFB now holds, per (folder, blob), how many removed entries found no
	// partner; keep exactly that many.
	var removed2 []DiffEntry
	for _, e := range removedLeft {
		k := folderBlob{e.FolderID, e.Blob}
		if remByFB[k] > 0 {
			remByFB[k]--
			removed2 = append(removed2, e)
		}
	}

	// 3. Same blob, different folder: a move. Pair in folder-id order.
	outBy := map[[32]byte][]int64{}
	for _, e := range removed2 {
		outBy[e.Blob] = append(outBy[e.Blob], e.FolderID)
	}
	inBy := map[[32]byte][]int64{}
	for _, e := range added2 {
		inBy[e.Blob] = append(inBy[e.Blob], e.FolderID)
	}
	for blob, outs := range outBy {
		ins := inBy[blob]
		slices.Sort(outs)
		slices.Sort(ins)
		n := min(len(outs), len(ins))
		for i := 0; i < n; i++ {
			bump(outs[i], func(d *FolderDiff) { d.MovedOut++ })
			bump(ins[i], func(d *FolderDiff) { d.MovedIn++ })
		}
		outBy[blob], inBy[blob] = outs[n:], ins[n:]
	}

	// 4. What is left is plainly deleted or added.
	for _, outs := range outBy {
		for _, f := range outs {
			bump(f, func(d *FolderDiff) { d.Deleted++ })
		}
	}
	for _, ins := range inBy {
		for _, f := range ins {
			bump(f, func(d *FolderDiff) { d.Added++ })
		}
	}
	for f, d := range out {
		if d.Empty() {
			delete(out, f)
		}
	}
	return out
}
