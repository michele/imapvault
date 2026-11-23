package index

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Run statuses.
const (
	RunRunning  = "running"
	RunComplete = "complete"
	RunPartial  = "partial"
	RunFailed   = "failed"
)

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// BeginRun starts a run. The caller holds the exclusive repository lock, so a
// run still marked running was left by a crash: it is marked failed first.
func (db *DB) BeginRun(now time.Time, capabilities string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE runs SET status = 'failed', finished_at = ? WHERE status = 'running'`, ts(now)); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO runs(started_at, status, capabilities) VALUES (?, 'running', ?)`, ts(now), capabilities)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// FinishRun records how a run ended.
func (db *DB) FinishRun(id int64, status string, now time.Time) error {
	_, err := db.Exec(`UPDATE runs SET status = ?, finished_at = ? WHERE id = ?`, status, ts(now), id)
	return err
}

// Folder is a row of folders.
type Folder struct {
	ID            int64
	NameHash      [32]byte
	NameSealed    []byte
	Delimiter     string
	SpecialUse    string
	UIDValidity   uint32
	LastUID       uint32
	HighestModSeq uint64
	FirstSeenRun  int64
	GoneRun       int64 // 0 while live
}

const folderCols = `id, name_hash, name_sealed, delimiter, special_use, uidvalidity, last_uid, highest_modseq, first_seen_run, coalesce(gone_run, 0)`

func scanFolders(rows *sql.Rows) ([]Folder, error) {
	defer func() { _ = rows.Close() }()
	var out []Folder
	for rows.Next() {
		var f Folder
		var h []byte
		if err := rows.Scan(&f.ID, &h, &f.NameSealed, &f.Delimiter, &f.SpecialUse, &f.UIDValidity, &f.LastUID, &f.HighestModSeq, &f.FirstSeenRun, &f.GoneRun); err != nil {
			return nil, err
		}
		if len(h) != len(f.NameHash) {
			return nil, fmt.Errorf("folder %d has a %d-byte name hash", f.ID, len(h))
		}
		copy(f.NameHash[:], h)
		out = append(out, f)
	}
	return out, rows.Err()
}

// LiveFolders returns folders that have not gone, by id.
func (db *DB) LiveFolders() ([]Folder, error) {
	rows, err := db.Query(`SELECT ` + folderCols + ` FROM folders WHERE gone_run IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanFolders(rows)
}

// NewFolder is what InsertFolder needs.
type NewFolder struct {
	NameHash    [32]byte
	NameSealed  []byte
	Delimiter   string
	SpecialUse  string
	UIDValidity uint32
}

// InsertFolder records a folder first seen in run.
func (db *DB) InsertFolder(run int64, f NewFolder) (int64, error) {
	res, err := db.Exec(`INSERT INTO folders(name_hash, name_sealed, delimiter, special_use, uidvalidity, first_seen_run) VALUES (?, ?, ?, ?, ?, ?)`,
		f.NameHash[:], f.NameSealed, f.Delimiter, f.SpecialUse, f.UIDValidity, run)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// MarkFolderGone ends a folder in run and closes its open entries, in one
// transaction. Blobs stay: only prune removes data.
func (db *DB) MarkFolderGone(folderID, run int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE entries SET valid_to_run = ? WHERE folder_id = ? AND valid_to_run IS NULL`, run, folderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE folders SET gone_run = ? WHERE id = ? AND gone_run IS NULL`, run, folderID); err != nil {
		return err
	}
	return tx.Commit()
}

// OpenEntry is an entry that is still valid.
type OpenEntry struct {
	ID    int64
	UID   uint32
	Blob  [32]byte
	Flags string
}

// OpenEntries returns the open entries of a folder, by UID.
func (db *DB) OpenEntries(folderID int64) ([]OpenEntry, error) {
	rows, err := db.Query(`SELECT id, uid, blob, flags FROM entries WHERE folder_id = ? AND valid_to_run IS NULL ORDER BY uid`, folderID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OpenEntry
	for rows.Next() {
		var e OpenEntry
		var b []byte
		if err := rows.Scan(&e.ID, &e.UID, &b, &e.Flags); err != nil {
			return nil, err
		}
		if len(b) != len(e.Blob) {
			return nil, fmt.Errorf("entry %d has a %d-byte blob key", e.ID, len(b))
		}
		copy(e.Blob[:], b)
		out = append(out, e)
	}
	return out, rows.Err()
}

// HasBlob reports whether the index already has a row for sha.
func (db *DB) HasBlob(sha [32]byte) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM blobs WHERE sha256 = ?`, sha[:]).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// NewBlob is a blob row. The object file must already be durable (rule 2).
type NewBlob struct {
	SHA        [32]byte
	StoredSHA  [32]byte
	Size       int64
	MetaSealed []byte
	EmailID    string
}

// NewEntry opens an entry.
type NewEntry struct {
	UID   uint32
	Blob  [32]byte
	Flags string
}

// Reflag closes an entry and opens an identical one with new flags.
type Reflag struct {
	EntryID int64
	UID     uint32
	Blob    [32]byte
	Flags   string
}

// FolderChanges is everything one folder's backup changes. It commits atomically.
type FolderChanges struct {
	UIDValidity   uint32
	LastUID       uint32
	HighestModSeq uint64
	Blobs         []NewBlob
	Open          []NewEntry
	Close         []int64
	Reflag        []Reflag
}

// ApplyFolder commits one folder's changes in a single transaction. If anything
// fails (for instance an entry that references a blob row that was not
// supplied) nothing is committed.
func (db *DB) ApplyFolder(run, folderID int64, ch FolderChanges) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, b := range ch.Blobs {
		var emailid any
		if b.EmailID != "" {
			emailid = b.EmailID
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO blobs(sha256, size, stored_sha256, meta_sealed, emailid, first_seen_run) VALUES (?, ?, ?, ?, ?, ?)`,
			b.SHA[:], b.Size, b.StoredSHA[:], b.MetaSealed, emailid, run); err != nil {
			return fmt.Errorf("insert blob: %w", err)
		}
	}
	for _, id := range ch.Close {
		if err := closeEntry(tx, id, run); err != nil {
			return err
		}
	}
	for _, r := range ch.Reflag {
		if err := closeEntry(tx, r.EntryID, run); err != nil {
			return err
		}
		if err := openEntry(tx, folderID, ch.UIDValidity, run, NewEntry{UID: r.UID, Blob: r.Blob, Flags: r.Flags}); err != nil {
			return err
		}
	}
	for _, e := range ch.Open {
		if err := openEntry(tx, folderID, ch.UIDValidity, run, e); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE folders SET uidvalidity = ?, last_uid = ?, highest_modseq = ? WHERE id = ?`,
		ch.UIDValidity, ch.LastUID, ch.HighestModSeq, folderID); err != nil {
		return err
	}
	return tx.Commit()
}

func closeEntry(tx *sql.Tx, id, run int64) error {
	res, err := tx.Exec(`UPDATE entries SET valid_to_run = ? WHERE id = ? AND valid_to_run IS NULL`, run, id)
	if err != nil {
		return fmt.Errorf("close entry %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("close entry %d: not an open entry", id)
	}
	return nil
}

func openEntry(tx *sql.Tx, folderID int64, uidvalidity uint32, run int64, e NewEntry) error {
	if _, err := tx.Exec(`INSERT INTO entries(folder_id, uidvalidity, uid, blob, flags, valid_from_run) VALUES (?, ?, ?, ?, ?, ?)`,
		folderID, uidvalidity, e.UID, e.Blob[:], e.Flags, run); err != nil {
		return fmt.Errorf("open entry uid %d: %w", e.UID, err)
	}
	return nil
}

// Snapshot is a row of snapshots.
type Snapshot struct {
	ID        int64
	CreatedAt time.Time
	Pinned    bool
	Flagged   bool
	Forgotten bool
	Folders   int
	Messages  int
	Status    string // of the run that produced it
}

// The snapshot at run R: entries with valid_from_run <= R and (valid_to_run IS
// NULL or valid_to_run > R).
const entryInSnapshot = `e.valid_from_run <= ?1 AND (e.valid_to_run IS NULL OR e.valid_to_run > ?1)`
const folderInSnapshot = `f.first_seen_run <= ?1 AND (f.gone_run IS NULL OR f.gone_run > ?1)`

// InsertSnapshot writes the snapshot for run, computing its counts from the
// entries.
func (db *DB) InsertSnapshot(run int64, now time.Time) error {
	_, err := db.Exec(`INSERT INTO snapshots(id, created_at, folders, messages)
		SELECT ?1, ?2,
		  (SELECT count(*) FROM folders f WHERE `+folderInSnapshot+`),
		  (SELECT count(*) FROM entries e WHERE `+entryInSnapshot+`)`, run, ts(now))
	return err
}

// Snapshots lists snapshots, oldest first.
func (db *DB) Snapshots() ([]Snapshot, error) {
	rows, err := db.Query(`SELECT s.id, s.created_at, s.pinned, s.flagged, s.forgotten, s.folders, s.messages, r.status
		FROM snapshots s JOIN runs r ON r.id = s.id ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Snapshot
	for rows.Next() {
		var s Snapshot
		var created string
		if err := rows.Scan(&s.ID, &created, &s.Pinned, &s.Flagged, &s.Forgotten, &s.Folders, &s.Messages, &s.Status); err != nil {
			return nil, err
		}
		s.CreatedAt = parseTS(created)
		out = append(out, s)
	}
	return out, rows.Err()
}

// LatestSnapshot returns the id of the newest snapshot, or 0 when there is none.
func (db *DB) LatestSnapshot() (int64, error) {
	var id sql.NullInt64
	if err := db.QueryRow(`SELECT max(id) FROM snapshots`).Scan(&id); err != nil {
		return 0, err
	}
	return id.Int64, nil
}

// HasSnapshot reports whether snapshot id exists.
func (db *DB) HasSnapshot(id int64) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM snapshots WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// SnapshotFolders returns the folders that existed at snapshot run, by id.
func (db *DB) SnapshotFolders(run int64) ([]Folder, error) {
	rows, err := db.Query(`SELECT `+folderCols+` FROM folders f WHERE `+folderInSnapshot+` ORDER BY f.id`, run)
	if err != nil {
		return nil, err
	}
	return scanFolders(rows)
}

// SnapEntry is one message in a snapshot.
type SnapEntry struct {
	UID         uint32
	UIDValidity uint32
	Blob        [32]byte
	Flags       string
	Size        int64
	MetaSealed  []byte
}

// SnapshotEntries returns the messages of one folder at snapshot run, by UID.
func (db *DB) SnapshotEntries(run, folderID int64) ([]SnapEntry, error) {
	rows, err := db.Query(`SELECT e.uid, e.uidvalidity, e.blob, e.flags, b.size, b.meta_sealed
		FROM entries e JOIN blobs b ON b.sha256 = e.blob
		WHERE e.folder_id = ?2 AND `+entryInSnapshot+` ORDER BY e.uidvalidity, e.uid`, run, folderID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SnapEntry
	for rows.Next() {
		var e SnapEntry
		var b []byte
		if err := rows.Scan(&e.UID, &e.UIDValidity, &b, &e.Flags, &e.Size, &e.MetaSealed); err != nil {
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

// BlobRow is a blob with its sealed metadata.
type BlobRow struct {
	SHA        [32]byte
	Size       int64
	MetaSealed []byte
}

// BlobsByPrefix finds blobs whose hex key starts with prefix (hex digits only),
// returning at most limit rows.
func (db *DB) BlobsByPrefix(prefix string, limit int) ([]BlobRow, error) {
	prefix = strings.ToLower(prefix)
	for _, c := range prefix {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return nil, fmt.Errorf("%q is not a hex prefix", prefix)
		}
	}
	rows, err := db.Query(`SELECT sha256, size, meta_sealed FROM blobs WHERE lower(hex(sha256)) LIKE ? ORDER BY sha256 LIMIT ?`, prefix+"%", limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []BlobRow
	for rows.Next() {
		var r BlobRow
		var b []byte
		if err := rows.Scan(&b, &r.Size, &r.MetaSealed); err != nil {
			return nil, err
		}
		copy(r.SHA[:], b)
		out = append(out, r)
	}
	return out, rows.Err()
}
