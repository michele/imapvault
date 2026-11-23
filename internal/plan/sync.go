package plan

import (
	"fmt"
	"slices"
)

// KnownFolder is a live folder row in the index.
type KnownFolder struct {
	ID          int64
	NameHash    [32]byte
	UIDValidity uint32
}

// ListedFolder is a selected, selectable folder on the server.
type ListedFolder struct {
	NameHash    [32]byte
	Name        string
	UIDValidity uint32 // 0 when not yet known (only learned by EXAMINE)
}

// FolderPlan says what to do with folders.
type FolderPlan struct {
	New  []ListedFolder // not in the index yet
	Same []KnownFolder  // known and listed
	Gone []KnownFolder  // known, no longer listed
}

// DiffFolders compares the live folders in the index with the folders the
// server lists after config filtering. A folder that was excluded by config
// since the last run counts as gone: its entries close, its blobs stay until
// prune. The result is deterministic: New sorted by name, others by ID.
func DiffFolders(known []KnownFolder, listed []ListedFolder) FolderPlan {
	byHash := map[[32]byte]KnownFolder{}
	for _, k := range known {
		byHash[k.NameHash] = k
	}
	seen := map[[32]byte]bool{}
	var p FolderPlan
	for _, l := range listed {
		seen[l.NameHash] = true
		if k, ok := byHash[l.NameHash]; ok {
			p.Same = append(p.Same, k)
		} else {
			p.New = append(p.New, l)
		}
	}
	for _, k := range known {
		if !seen[k.NameHash] {
			p.Gone = append(p.Gone, k)
		}
	}
	slices.SortFunc(p.New, func(a, b ListedFolder) int { return compareStrings(a.Name, b.Name) })
	byID := func(a, b KnownFolder) int { return compareInt64(a.ID, b.ID) }
	slices.SortFunc(p.Same, byID)
	slices.SortFunc(p.Gone, byID)
	return p
}

// OpenEntry is an entry still valid in the index.
type OpenEntry struct {
	EntryID int64
	UID     uint32
	Flags   string // normalised
}

// ServerMsg is a message the server lists.
type ServerMsg struct {
	UID   uint32
	Flags []string
}

// Reflag is a flag change on an open entry.
type Reflag struct {
	EntryID int64
	UID     uint32
	Flags   string // normalised, new
}

// MessagePlan says what to do with one folder's messages.
type MessagePlan struct {
	Fetch  []uint32 // UIDs on the server with no open entry: download
	Close  []int64  // entries whose UID is gone from the server
	Reflag []Reflag // entries whose flags changed
}

// Empty reports whether there is nothing to do.
func (p MessagePlan) Empty() bool {
	return len(p.Fetch) == 0 && len(p.Close) == 0 && len(p.Reflag) == 0
}

// DiffMessages is the full-rescan diff for one folder in one UIDVALIDITY
// generation. It errors on a duplicate UID in either input, which would mean a
// broken server or a broken index; the caller must not guess.
func DiffMessages(open []OpenEntry, server []ServerMsg) (MessagePlan, error) {
	have := make(map[uint32]OpenEntry, len(open))
	for _, e := range open {
		if _, dup := have[e.UID]; dup {
			return MessagePlan{}, fmt.Errorf("index has two open entries for uid %d", e.UID)
		}
		have[e.UID] = e
	}
	onServer := make(map[uint32]bool, len(server))
	var p MessagePlan
	for _, m := range server {
		if onServer[m.UID] {
			return MessagePlan{}, fmt.Errorf("server listed uid %d twice", m.UID)
		}
		onServer[m.UID] = true
		e, ok := have[m.UID]
		if !ok {
			p.Fetch = append(p.Fetch, m.UID)
			continue
		}
		if nf := NormalizeFlags(m.Flags); nf != e.Flags {
			p.Reflag = append(p.Reflag, Reflag{EntryID: e.EntryID, UID: m.UID, Flags: nf})
		}
	}
	for _, e := range open {
		if !onServer[e.UID] {
			p.Close = append(p.Close, e.EntryID)
		}
	}
	slices.Sort(p.Fetch)
	slices.Sort(p.Close)
	slices.SortFunc(p.Reflag, func(a, b Reflag) int { return compareInt64(a.EntryID, b.EntryID) })
	return p, nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
