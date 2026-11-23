package index

import (
	"fmt"
	"time"
)

// DeletedEntry is a message that was deleted from the server and is not on it
// now: the last row of its position's history, closed by a run in the window.
type DeletedEntry struct {
	FolderID    int64
	UIDValidity uint32
	UID         uint32
	Blob        [32]byte
	Flags       string // as of the last row, i.e. just before it was deleted
	Size        int64
	MetaSealed  []byte
	ClosedRun   int64
}

// DeletedSince returns the messages deleted by runs that started at or after
// cutoff. "Deleted" is strict, so that restoring them cannot duplicate mail:
//
//   - only the last row of a position's history counts: a flag change closes a
//     row and opens another at the same (folder, UIDVALIDITY, UID), and that is
//     not a deletion;
//   - the message must be gone everywhere: if any open entry holds the same blob
//     (it was moved, renumbered by a UIDVALIDITY reset, or a copy remains), it is
//     on the server and is not returned.
//
// A message moved into a folder the config excludes looks deleted to the backup,
// which can no longer see it, and is returned.
func (db *DB) DeletedSince(cutoff time.Time) ([]DeletedEntry, error) {
	rows, err := db.Query(`
		SELECT e.folder_id, e.uidvalidity, e.uid, e.blob, e.flags, b.size, b.meta_sealed, e.valid_to_run
		FROM entries e
		JOIN blobs b ON b.sha256 = e.blob
		JOIN runs r ON r.id = e.valid_to_run
		WHERE e.valid_to_run IS NOT NULL
		  AND r.started_at >= ?1
		  AND e.valid_from_run = (
		        SELECT max(h.valid_from_run) FROM entries h
		        WHERE h.folder_id = e.folder_id AND h.uidvalidity = e.uidvalidity AND h.uid = e.uid)
		  AND NOT EXISTS (SELECT 1 FROM entries o WHERE o.blob = e.blob AND o.valid_to_run IS NULL)
		ORDER BY e.folder_id, e.uidvalidity, e.uid`, ts(cutoff))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeletedEntry
	for rows.Next() {
		var e DeletedEntry
		var b []byte
		if err := rows.Scan(&e.FolderID, &e.UIDValidity, &e.UID, &b, &e.Flags, &e.Size, &e.MetaSealed, &e.ClosedRun); err != nil {
			return nil, err
		}
		if len(b) != len(e.Blob) {
			return nil, fmt.Errorf("entry uid %d has a %d-byte blob key", e.UID, len(b))
		}
		copy(e.Blob[:], b)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AllFolders returns every folder row, live or gone, by id. Restore needs the
// gone ones: a folder deleted from the server is still where its messages were.
func (db *DB) AllFolders() ([]Folder, error) {
	rows, err := db.Query(`SELECT ` + folderCols + ` FROM folders ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanFolders(rows)
}
