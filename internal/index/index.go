// Package index is the SQLite index of a repository: folders, blobs, entries,
// snapshots and alerts. The schema version lives in PRAGMA user_version and
// migrations are embedded .sql files applied in order, each in one transaction.
package index

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure Go driver, registers "sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	// ErrNewerIndex means the index was written by a newer imapvault.
	ErrNewerIndex = errors.New("index schema is newer than this imapvault understands")
	// ErrNeedsMigration means a read-only open found an older schema; a command
	// that writes (backup) migrates it.
	ErrNeedsMigration = errors.New("index schema needs migration")
)

// Options control Open.
type Options struct {
	// ReadOnly opens an existing index without creating it, migrating it or
	// allowing writes. Used by check and the read commands.
	ReadOnly bool
}

// DB is an open index.
type DB struct {
	*sql.DB
}

type migration struct {
	version int
	name    string
}

func migrations() ([]migration, error) {
	ents, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range ents {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return nil, fmt.Errorf("bad migration name %q", e.Name())
		}
		ms = append(ms, migration{v, e.Name()})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %q: versions must be 1..N without gaps", m.name)
		}
	}
	return ms, nil
}

// LatestVersion is the schema version this binary writes.
var LatestVersion = func() int {
	ms, err := migrations()
	if err != nil {
		panic(err)
	}
	return len(ms)
}()

// Open opens (and, unless ReadOnly, creates and migrates) the index at path.
func Open(path string, opts Options) (*DB, error) {
	if opts.ReadOnly {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("open index: %w", err)
		}
	} else if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		// create it ourselves so the file is 0600 regardless of umask.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}

	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "synchronous(FULL)")
	if opts.ReadOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Set("_txlock", "immediate")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db := &DB{sdb}
	// a garbage file only fails on first use, so force a real read now.
	v, err := db.SchemaVersion()
	if err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("open index %s: %w", path, err)
	}
	switch {
	case v > LatestVersion:
		_ = sdb.Close()
		return nil, fmt.Errorf("%w: index is v%d, this imapvault reads up to v%d", ErrNewerIndex, v, LatestVersion)
	case v < LatestVersion && opts.ReadOnly:
		_ = sdb.Close()
		return nil, fmt.Errorf("%w: index is v%d, current is v%d", ErrNeedsMigration, v, LatestVersion)
	case v < LatestVersion:
		if err := db.migrate(v); err != nil {
			_ = sdb.Close()
			return nil, err
		}
	}
	return db, nil
}

// SchemaVersion reads PRAGMA user_version.
func (db *DB) SchemaVersion() (int, error) {
	var v int
	err := db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

func (db *DB) migrate(from int) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	for _, m := range ms[from:] {
		body, err := migrationFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		// user_version is transactional: the schema change and the version bump
		// commit together or not at all.
		if _, err := tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(`PRAGMA user_version = ` + strconv.Itoa(m.version)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: set version: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

// FKViolation is one row reported by PRAGMA foreign_key_check.
type FKViolation struct {
	Table  string
	RowID  int64
	Parent string
}

// IntegrityCheck runs PRAGMA integrity_check and returns the problems it found
// (empty when the index is healthy).
func (db *DB) IntegrityCheck() ([]string, error) {
	rows, err := db.Query(`PRAGMA integrity_check`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var problems []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if s != "ok" {
			problems = append(problems, s)
		}
	}
	return problems, rows.Err()
}

// ForeignKeyCheck returns every row whose foreign key points at nothing.
func (db *DB) ForeignKeyCheck() ([]FKViolation, error) {
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []FKViolation
	for rows.Next() {
		var v FKViolation
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&v.Table, &rowid, &v.Parent, &fkid); err != nil {
			return nil, err
		}
		v.RowID = rowid.Int64
		out = append(out, v)
	}
	return out, rows.Err()
}

// BlobKeys calls fn with the key of every row in blobs.
func (db *DB) BlobKeys(fn func(sha256 [32]byte) error) error {
	rows, err := db.Query(`SELECT sha256 FROM blobs`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return err
		}
		var k [32]byte
		if len(b) != len(k) {
			return fmt.Errorf("blobs row has a %d-byte key", len(b))
		}
		copy(k[:], b)
		if err := fn(k); err != nil {
			return err
		}
	}
	return rows.Err()
}
