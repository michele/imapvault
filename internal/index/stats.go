package index

import (
	"database/sql"
	"time"
)

// The queries behind `imapvault stats`. They read plaintext columns only, so an
// encrypted repository needs no key for them, and they never write.

// StatsTotals is the repository in numbers. Positions are (folder, UID) places
// in the current mailbox; a message in two folders is two positions and one blob.
type StatsTotals struct {
	Blobs        int   // unique messages ever stored
	RawBytes     int64 // their size before compression
	Positions    int   // places in the current mailbox (open entries)
	DistinctOpen int   // unique messages among them
	OpenRawBytes int64 // raw size of those unique messages
	Unread       int   // open entries without \Seen
	Starred      int   // open entries with \Flagged
	HistoryBlobs int   // messages no open entry references: gone from the server, still kept
	HistoryBytes int64
}

// StatsTotals counts what the index holds.
func (db *DB) StatsTotals() (StatsTotals, error) {
	var t StatsTotals
	if err := db.QueryRow(`SELECT count(*), coalesce(sum(size), 0) FROM blobs`).Scan(&t.Blobs, &t.RawBytes); err != nil {
		return t, err
	}
	if err := db.QueryRow(`SELECT count(*), count(DISTINCT blob) FROM entries WHERE valid_to_run IS NULL`).Scan(&t.Positions, &t.DistinctOpen); err != nil {
		return t, err
	}
	if err := db.QueryRow(`SELECT coalesce(sum(size), 0) FROM blobs WHERE sha256 IN (SELECT blob FROM entries WHERE valid_to_run IS NULL)`).Scan(&t.OpenRawBytes); err != nil {
		return t, err
	}
	if err := db.QueryRow(`SELECT
			coalesce(sum(CASE WHEN instr(flags, '\Seen') = 0 THEN 1 ELSE 0 END), 0),
			coalesce(sum(CASE WHEN instr(flags, '\Flagged') > 0 THEN 1 ELSE 0 END), 0)
		FROM entries WHERE valid_to_run IS NULL`).Scan(&t.Unread, &t.Starred); err != nil {
		return t, err
	}
	err := db.QueryRow(`SELECT count(*), coalesce(sum(size), 0) FROM blobs b
		WHERE NOT EXISTS (SELECT 1 FROM entries e WHERE e.blob = b.sha256 AND e.valid_to_run IS NULL)`).Scan(&t.HistoryBlobs, &t.HistoryBytes)
	return t, err
}

// StatsFolder is one live folder of the current mailbox. NameSealed is sealed:
// the caller opens it only when it may.
type StatsFolder struct {
	ID         int64
	SpecialUse string
	NameSealed []byte
	Messages   int
	RawBytes   int64
	Unread     int
}

// StatsFolders returns the live folders that hold mail, by id.
func (db *DB) StatsFolders() ([]StatsFolder, error) {
	rows, err := db.Query(`SELECT f.id, f.special_use, f.name_sealed, count(*), coalesce(sum(b.size), 0),
			coalesce(sum(CASE WHEN instr(e.flags, '\Seen') = 0 THEN 1 ELSE 0 END), 0)
		FROM entries e JOIN folders f ON f.id = e.folder_id JOIN blobs b ON b.sha256 = e.blob
		WHERE e.valid_to_run IS NULL AND f.gone_run IS NULL GROUP BY f.id ORDER BY f.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StatsFolder
	for rows.Next() {
		var f StatsFolder
		if err := rows.Scan(&f.ID, &f.SpecialUse, &f.NameSealed, &f.Messages, &f.RawBytes, &f.Unread); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// StatsDay is what the runs of one UTC day first stored.
type StatsDay struct {
	Day      string // YYYY-MM-DD
	Messages int
	RawBytes int64
}

// StatsGrowth groups the blobs by the day of the run that first saw them.
func (db *DB) StatsGrowth() ([]StatsDay, error) {
	rows, err := db.Query(`SELECT substr(r.started_at, 1, 10), count(*), coalesce(sum(b.size), 0)
		FROM blobs b JOIN runs r ON r.id = b.first_seen_run GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StatsDay
	for rows.Next() {
		var d StatsDay
		if err := rows.Scan(&d.Day, &d.Messages, &d.RawBytes); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// StatsRun is one row of the runs table.
type StatsRun struct {
	ID       int64
	Started  time.Time
	Finished time.Time // zero while running or after a crash
	Status   string
}

// StatsRuns returns every run, oldest first.
func (db *DB) StatsRuns() ([]StatsRun, error) {
	rows, err := db.Query(`SELECT id, started_at, finished_at, status FROM runs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StatsRun
	for rows.Next() {
		var r StatsRun
		var started string
		var finished sql.NullString
		if err := rows.Scan(&r.ID, &started, &finished, &r.Status); err != nil {
			return nil, err
		}
		r.Started = parseTS(started)
		if finished.Valid {
			r.Finished = parseTS(finished.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
