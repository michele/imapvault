package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/plan"
)

// DiffCounts is what changed in a folder (or in total) between two snapshots.
type DiffCounts struct {
	Added      int `json:"added"`
	Deleted    int `json:"deleted"`
	MovedIn    int `json:"moved_in"`
	MovedOut   int `json:"moved_out"`
	Reflagged  int `json:"reflagged"`
	Renumbered int `json:"renumbered"`
}

// DiffFolder is one folder with changes. Name is "#<id>" when the folder names
// are sealed and no private key is configured.
type DiffFolder struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	DiffCounts
}

// DiffResult compares two snapshots, older first.
type DiffResult struct {
	From     int64        `json:"from"`
	To       int64        `json:"to"`
	FromTime time.Time    `json:"from_time"`
	ToTime   time.Time    `json:"to_time"`
	Folders  []DiffFolder `json:"folders"` // only folders with changes
	Totals   DiffCounts   `json:"totals"`  // a message that moved counts once, as moved_in
	Warnings []string     `json:"warnings,omitempty"`
}

// Empty reports whether the snapshots are identical.
func (r *DiffResult) Empty() bool { return len(r.Folders) == 0 }

// Diff counts, per folder, the messages added, deleted, moved, re-flagged and
// renumbered between two snapshots (snapshot numbers, "latest" or "latest~N";
// older first). It reads positions, blob keys and flags, which are plaintext, so
// it works on a backup machine without the private key; folder names are shown
// when a key is configured and as "#<id>" otherwise.
func Diff(ctx context.Context, cfg *config.Config, from, to string) (*DiffResult, error) {
	rd, err := openReaderMode(ctx, cfg, keyIfAvailable)
	if err != nil {
		return nil, err
	}
	defer rd.close()
	a, err := rd.snapshotID(from)
	if err != nil {
		return nil, err
	}
	b, err := rd.snapshotID(to)
	if err != nil {
		return nil, err
	}
	if a >= b {
		return nil, fmt.Errorf("snapshot %d is not older than snapshot %d: give the older one first", a, b)
	}
	snaps, err := rd.db.Snapshots()
	if err != nil {
		return nil, err
	}
	res := &DiffResult{From: a, To: b, Warnings: rd.warnings, Folders: []DiffFolder{}}
	for _, s := range snaps {
		switch s.ID {
		case a:
			res.FromTime = s.CreatedAt
		case b:
			res.ToTime = s.CreatedAt
		}
	}

	removed, added, err := rd.db.SnapshotChanges(a, b)
	if err != nil {
		return nil, err
	}
	conv := func(cs []index.SnapChange) []plan.DiffEntry {
		out := make([]plan.DiffEntry, len(cs))
		for i, c := range cs {
			out[i] = plan.DiffEntry{FolderID: c.FolderID, UIDValidity: c.UIDValidity, UID: c.UID, Blob: c.Blob, Flags: c.Flags}
		}
		return out
	}
	per := plan.DiffSnapshots(conv(removed), conv(added))

	folders, err := rd.db.FoldersInEither(a, b)
	if err != nil {
		return nil, err
	}
	sealed := map[int64][]byte{}
	for _, f := range folders {
		sealed[f.ID] = f.NameSealed
	}
	for id, d := range per {
		name := fmt.Sprintf("#%d", id)
		if sn, ok := sealed[id]; ok && rd.names {
			n, err := rd.opener.Open(sn)
			if err != nil {
				return nil, fmt.Errorf("folder %d: %w", id, err)
			}
			name = string(n)
		}
		c := DiffCounts{Added: d.Added, Deleted: d.Deleted, MovedIn: d.MovedIn, MovedOut: d.MovedOut, Reflagged: d.Reflagged, Renumbered: d.Renumbered}
		res.Folders = append(res.Folders, DiffFolder{ID: id, Name: name, DiffCounts: c})
		res.Totals.Added += c.Added
		res.Totals.Deleted += c.Deleted
		res.Totals.MovedIn += c.MovedIn
		res.Totals.MovedOut += c.MovedOut
		res.Totals.Reflagged += c.Reflagged
		res.Totals.Renumbered += c.Renumbered
	}
	sort.Slice(res.Folders, func(i, j int) bool {
		if res.Folders[i].Name != res.Folders[j].Name {
			return res.Folders[i].Name < res.Folders[j].Name
		}
		return res.Folders[i].ID < res.Folders[j].ID
	})
	return res, nil
}
