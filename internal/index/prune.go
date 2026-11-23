package index

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetForgotten marks snapshots forgotten, all or none. It refuses a snapshot that
// does not exist and a pinned one: pins always win over retention.
func (db *DB) SetForgotten(ids []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		var pinned bool
		if err := tx.QueryRow(`SELECT pinned FROM snapshots WHERE id = ?`, id).Scan(&pinned); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("there is no snapshot %d", id)
			}
			return err
		}
		if pinned {
			return fmt.Errorf("snapshot %d is pinned and cannot be forgotten", id)
		}
		if _, err := tx.Exec(`UPDATE snapshots SET forgotten = 1 WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClosedEntry is an entry that was present in snapshots From <= S < To. ClosedAt
// is when the run that saw it disappear started.
type ClosedEntry struct {
	ID       int64
	FolderID int64
	Blob     [32]byte
	From, To int64
	ClosedAt time.Time
}

// ClosedEntries returns every closed entry, by id. Open entries are not prune
// candidates and are left out.
func (db *DB) ClosedEntries() ([]ClosedEntry, error) {
	rows, err := db.Query(`SELECT e.id, e.folder_id, e.blob, e.valid_from_run, e.valid_to_run, r.started_at
		FROM entries e JOIN runs r ON r.id = e.valid_to_run
		WHERE e.valid_to_run IS NOT NULL ORDER BY e.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ClosedEntry
	for rows.Next() {
		var e ClosedEntry
		var b []byte
		var at string
		if err := rows.Scan(&e.ID, &e.FolderID, &b, &e.From, &e.To, &at); err != nil {
			return nil, err
		}
		if len(b) != len(e.Blob) {
			return nil, fmt.Errorf("entry %d has a %d-byte blob key", e.ID, len(b))
		}
		copy(e.Blob[:], b)
		e.ClosedAt = parseTS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// BlobRefs returns, for every blob that has at least one closed entry, how many
// entries (open and closed) reference it, and separately the blob rows that no
// entry references at all.
func (db *DB) BlobRefs() (refs map[[32]byte]int, unreferenced [][32]byte, err error) {
	rows, err := db.Query(`SELECT blob, count(*) FROM entries
		WHERE blob IN (SELECT blob FROM entries WHERE valid_to_run IS NOT NULL) GROUP BY blob`)
	if err != nil {
		return nil, nil, err
	}
	refs = map[[32]byte]int{}
	for rows.Next() {
		var b []byte
		var n int
		if err := rows.Scan(&b, &n); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		if len(b) != 32 {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("an entry has a %d-byte blob key", len(b))
		}
		refs[[32]byte(b)] = n
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, err
	}
	_ = rows.Close()

	rows, err = db.Query(`SELECT sha256 FROM blobs b WHERE NOT EXISTS (SELECT 1 FROM entries e WHERE e.blob = b.sha256) ORDER BY sha256`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, nil, err
		}
		if len(b) != 32 {
			return nil, nil, fmt.Errorf("a blob row has a %d-byte key", len(b))
		}
		unreferenced = append(unreferenced, [32]byte(b))
	}
	return refs, unreferenced, rows.Err()
}

// PruneApply is what ApplyPrune removes.
type PruneApply struct {
	Entries   []int64    // closed entry ids
	Blobs     [][32]byte // blob rows
	Snapshots []int64    // forgotten, unpinned snapshot rows
}

// PruneApplied counts what ApplyPrune removed.
type PruneApplied struct {
	Entries, Blobs, Snapshots, Folders int
}

// ApplyPrune removes the entries, blob rows and snapshot rows in one transaction,
// then folder rows that no entry and no remaining snapshot needs. It re-checks
// every claim against the index and rolls everything back on a surprise: an
// entry that is open, a blob something still references, a snapshot that is
// not forgotten or is pinned. Object files are the caller's business, and only
// after this commits (rule 2: a crash may leave orphan files, never dangling
// references).
func (db *DB) ApplyPrune(p PruneApply) (PruneApplied, error) {
	var res PruneApplied
	tx, err := db.Begin()
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	for _, id := range p.Entries {
		r, err := tx.Exec(`DELETE FROM entries WHERE id = ? AND valid_to_run IS NOT NULL`, id)
		if err != nil {
			return res, fmt.Errorf("prune entry %d: %w", id, err)
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return res, fmt.Errorf("prune entry %d: not a closed entry", id)
		}
		res.Entries++
	}
	for _, b := range p.Blobs {
		r, err := tx.Exec(`DELETE FROM blobs WHERE sha256 = ?1 AND NOT EXISTS (SELECT 1 FROM entries WHERE blob = ?1)`, b[:])
		if err != nil {
			return res, fmt.Errorf("prune blob %x: %w", b[:6], err)
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return res, fmt.Errorf("prune blob %x: an entry still references it, or it is not in the index", b[:6])
		}
		res.Blobs++
	}
	for _, id := range p.Snapshots {
		r, err := tx.Exec(`DELETE FROM snapshots WHERE id = ? AND forgotten = 1 AND pinned = 0`, id)
		if err != nil {
			return res, fmt.Errorf("prune snapshot %d: %w", id, err)
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return res, fmt.Errorf("prune snapshot %d: not a forgotten, unpinned snapshot", id)
		}
		res.Snapshots++
	}
	// A folder that is gone, holds no entry and that no remaining snapshot shows.
	r, err := tx.Exec(`DELETE FROM folders WHERE gone_run IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM entries e WHERE e.folder_id = folders.id)
		AND NOT EXISTS (SELECT 1 FROM snapshots s WHERE s.id >= folders.first_seen_run AND s.id < folders.gone_run)`)
	if err != nil {
		return res, fmt.Errorf("prune folders: %w", err)
	}
	n, _ := r.RowsAffected()
	res.Folders = int(n)
	return res, tx.Commit()
}

// VacuumInto writes a compacted, consistent copy of the index to path, which
// must not exist. The sealed columns are copied as they are, so it needs no key.
func (db *DB) VacuumInto(path string) error {
	_, err := db.Exec(`VACUUM INTO ?`, path)
	return err
}
