package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestFreshOpenMigratesAndSetsPragmas(t *testing.T) {
	db, path := openTemp(t)
	v, err := db.SchemaVersion()
	if err != nil || v != LatestVersion {
		t.Fatalf("version %d err %v, want %d", v, err, LatestVersion)
	}
	for _, table := range []string{"runs", "snapshots", "folders", "blobs", "entries", "alerts"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d err=%v)", table, n, err)
		}
	}
	var mode string
	_ = db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode %q", mode)
	}
	var fk int
	_ = db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Fatal("foreign_keys off")
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("index.db mode: %v %v", err, fi)
	}
}

func TestReopenIsIdempotentAndKeepsData(t *testing.T) {
	db, path := openTemp(t)
	if _, err := db.Exec(`INSERT INTO runs(started_at, status) VALUES ('2026-11-23T10:00:00Z','running')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	var n int
	_ = db2.QueryRow(`SELECT count(*) FROM runs`).Scan(&n)
	if n != 1 {
		t.Fatalf("runs after reopen: %d", n)
	}
}

func TestOpenRefusesNewerIndex(t *testing.T) {
	db, path := openTemp(t)
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	_, err := Open(path, Options{})
	if !errors.Is(err, ErrNewerIndex) {
		t.Fatalf("want ErrNewerIndex, got %v", err)
	}
}

func TestReadOnlyDoesNotMigrateOrWrite(t *testing.T) {
	// missing file
	if _, err := Open(filepath.Join(t.TempDir(), "nope.db"), Options{ReadOnly: true}); err == nil {
		t.Fatal("read-only open of a missing index succeeded")
	}
	// existing, current
	_, path := openTemp(t)
	ro, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if _, err := ro.Exec(`INSERT INTO runs(started_at, status) VALUES ('x','running')`); err == nil {
		t.Fatal("write succeeded on a read-only handle")
	}
}

func TestReadOnlyRefusesUnmigratedIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// pretend this index predates the current schema
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(path, Options{ReadOnly: true}); !errors.Is(err, ErrNeedsMigration) {
		t.Fatalf("want ErrNeedsMigration, got %v", err)
	}
}

func TestOpenRejectsNonDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("this is not sqlite ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("garbage file opened as an index")
	}
}

func seedRun(t *testing.T, db *DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO runs(id, started_at, status) VALUES (1,'2026-11-23T10:00:00Z','complete')`,
		`INSERT INTO folders(id, name_hash, name_sealed, first_seen_run) VALUES (1, zeroblob(32), x'00', 1)`,
		`INSERT INTO blobs(sha256, size, stored_sha256, first_seen_run) VALUES (zeroblob(32), 10, zeroblob(32), 1)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	db, _ := openTemp(t)
	seedRun(t, db)
	_, err := db.Exec(`INSERT INTO entries(folder_id, uidvalidity, uid, blob, valid_from_run) VALUES (1,1,1,x'ff',1)`)
	if err == nil {
		t.Fatal("entry with a dangling blob reference accepted")
	}
}

func TestConstraints(t *testing.T) {
	db, _ := openTemp(t)
	seedRun(t, db)
	ins := `INSERT INTO entries(folder_id, uidvalidity, uid, blob, valid_from_run, valid_to_run) VALUES (1,1,1,zeroblob(32),1,%s)`
	if _, err := db.Exec(strings.Replace(ins, "%s", "NULL", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(strings.Replace(ins, "%s", "NULL", 1)); err == nil {
		t.Fatal("two open entries for the same (folder, uidvalidity, uid) accepted")
	}
	// closed entries for the same position are the history and must be allowed.
	if _, err := db.Exec(`INSERT INTO runs(id, started_at, status) VALUES (2,'2026-11-24T10:00:00Z','complete')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(strings.Replace(ins, "%s", "2", 1)); err != nil {
		t.Fatalf("closed entry rejected: %v", err)
	}
	if _, err := db.Exec(strings.Replace(ins, "%s", "1", 1)); err == nil {
		t.Fatal("entry closed in the run it was opened accepted")
	}
	if _, err := db.Exec(`INSERT INTO runs(started_at, status) VALUES ('x','bogus')`); err == nil {
		t.Fatal("bogus run status accepted")
	}
	if _, err := db.Exec(`INSERT INTO blobs(sha256, size, stored_sha256, first_seen_run) VALUES (x'00', 1, zeroblob(32), 1)`); err == nil {
		t.Fatal("short blob key accepted")
	}
	// two live folders cannot share a name; a gone folder can be followed by a new one.
	if _, err := db.Exec(`INSERT INTO folders(name_hash, name_sealed, first_seen_run) VALUES (zeroblob(32), x'00', 1)`); err == nil {
		t.Fatal("duplicate live folder name accepted")
	}
	if _, err := db.Exec(`UPDATE folders SET gone_run = 2 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO folders(name_hash, name_sealed, first_seen_run) VALUES (zeroblob(32), x'00', 2)`); err != nil {
		t.Fatalf("re-created folder rejected: %v", err)
	}
}

func TestIntegrityAndForeignKeyChecks(t *testing.T) {
	db, _ := openTemp(t)
	seedRun(t, db)
	if problems, err := db.IntegrityCheck(); err != nil || len(problems) != 0 {
		t.Fatalf("healthy index: %v %v", problems, err)
	}
	if v, err := db.ForeignKeyCheck(); err != nil || len(v) != 0 {
		t.Fatalf("healthy index fk: %v %v", v, err)
	}
	// plant a dangling reference with enforcement off on one dedicated connection.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO entries(folder_id, uidvalidity, uid, blob, valid_from_run) VALUES (1,1,7,x'ff',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	v, err := db.ForeignKeyCheck()
	if err != nil || len(v) != 1 || v[0].Table != "entries" || v[0].Parent != "blobs" {
		t.Fatalf("fk violations: %+v err=%v", v, err)
	}
}

func TestBlobKeys(t *testing.T) {
	db, _ := openTemp(t)
	seedRun(t, db)
	if _, err := db.Exec(`INSERT INTO blobs(sha256, size, stored_sha256, first_seen_run) VALUES (randomblob(32), 5, randomblob(32), 1)`); err != nil {
		t.Fatal(err)
	}
	var n int
	err := db.BlobKeys(func(k [32]byte) error { n++; return nil })
	if err != nil || n != 2 {
		t.Fatalf("BlobKeys: n=%d err=%v", n, err)
	}
	stop := errors.New("stop")
	if err := db.BlobKeys(func([32]byte) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error not propagated: %v", err)
	}
}

func TestSealedColumnsAndNoPlaintextMetadata(t *testing.T) {
	db, _ := openTemp(t)
	columns := func(table string) map[string]bool {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		out := map[string]bool{}
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			out[n] = true
		}
		return out
	}
	// what a backup-only machine may see in the clear is deliberate; what must be sealed must not creep back.
	for table, forbidden := range map[string][]string{
		"folders":   {"name"},
		"blobs":     {"message_id", "internal_date", "subject", "from_addr"},
		"snapshots": {"note"},
		"alerts":    {"summary"},
	} {
		cols := columns(table)
		for _, c := range forbidden {
			if cols[c] {
				t.Errorf("%s.%s is a plaintext column; it must be sealed", table, c)
			}
		}
	}
	for table, want := range map[string][]string{
		"folders":   {"name_hash", "name_sealed"},
		"blobs":     {"stored_sha256", "meta_sealed"},
		"snapshots": {"note_sealed"},
		"alerts":    {"summary_sealed"},
	} {
		cols := columns(table)
		for _, c := range want {
			if !cols[c] {
				t.Errorf("%s.%s is missing", table, c)
			}
		}
	}
}

func TestSealedColumnConstraints(t *testing.T) {
	db, _ := openTemp(t)
	seedRun(t, db)
	if _, err := db.Exec(`INSERT INTO folders(name_hash, name_sealed, first_seen_run) VALUES (x'00', x'00', 1)`); err == nil {
		t.Fatal("short name_hash accepted")
	}
	if _, err := db.Exec(`INSERT INTO blobs(sha256, size, stored_sha256, first_seen_run) VALUES (randomblob(32), 1, x'00', 1)`); err == nil {
		t.Fatal("short stored_sha256 accepted")
	}
	if _, err := db.Exec(`INSERT INTO blobs(sha256, size, first_seen_run) VALUES (randomblob(32), 1, 1)`); err == nil {
		t.Fatal("blob without stored_sha256 accepted")
	}
	if _, err := db.Exec(`INSERT INTO alerts(run_id, kind, created_at) VALUES (1, 'mass-deletion', 'x')`); err == nil {
		t.Fatal("alert without a sealed summary accepted")
	}
	if _, err := db.Exec(`INSERT INTO alerts(run_id, kind, summary_sealed, created_at) VALUES (1, 'mass-deletion', x'00', 'x')`); err != nil {
		t.Fatalf("valid alert rejected: %v", err)
	}
}
