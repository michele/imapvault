package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/objects"
	"github.com/michele/imapvault/internal/repo"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		IMAP:       config.IMAP{Host: "imap.example.com", Username: "me@example.com"},
		Repository: config.Repository{Path: filepath.Join(t.TempDir(), "repo")},
	}
}

func initRepo(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	if _, err := Init(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// addBlob stores raw in the object store and records it in the index, in the
// durable order the real backup uses (blob first, then the index row).
func addBlob(t *testing.T, cfg *config.Config, raw string) objects.Key {
	t.Helper()
	r, err := repo.Open(cfg.Repository.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := objects.Open(r.ObjectsDir(), r.TmpDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	k, _, err := st.Put([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(r.IndexPath(), index.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT OR IGNORE INTO runs(id, started_at, status) VALUES (1,'2026-11-23T10:00:00Z','complete')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO blobs(sha256, size, first_seen_run) VALUES (?, ?, 1)`, k[:], len(raw)); err != nil {
		t.Fatal(err)
	}
	return k
}

func objPath(cfg *config.Config, k objects.Key) string {
	h := k.String()
	return filepath.Join(cfg.Repository.Path, "objects", h[:2], h[2:])
}

func kinds(fs []Finding) map[string]int {
	m := map[string]int{}
	for _, f := range fs {
		m[f.Kind]++
	}
	return m
}

func TestInitCreatesRepositoryAndIndex(t *testing.T) {
	cfg := testConfig(t)
	res, err := Init(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != cfg.Repository.Path || res.RepoID == "" || res.FormatVersion != repo.FormatVersion || res.SchemaVersion != index.LatestVersion {
		t.Fatalf("result: %+v", res)
	}
	r, err := repo.Open(cfg.Repository.Path, &repo.Account{ID: config.AccountID(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Meta.Account.Host != "imap.example.com" || r.Meta.Account.Username != "me@example.com" {
		t.Fatalf("binding: %+v", r.Meta.Account)
	}
	if _, err := os.Stat(r.IndexPath()); err != nil {
		t.Fatal(err)
	}
}

func TestInitTwiceRefused(t *testing.T) {
	cfg := initRepo(t)
	if _, err := Init(cfg); !errors.Is(err, repo.ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
}

func TestCheckCleanRepository(t *testing.T) {
	cfg := initRepo(t)
	rep, err := Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || len(rep.Warnings) != 0 || rep.Blobs != 0 || rep.Objects != 0 {
		t.Fatalf("fresh repo: %+v", rep)
	}
	if rep.SchemaVersion != index.LatestVersion || rep.FormatVersion != repo.FormatVersion {
		t.Fatalf("versions: %+v", rep)
	}
}

func TestCheckHealthyWithBlobs(t *testing.T) {
	cfg := initRepo(t)
	addBlob(t, cfg, "one")
	addBlob(t, cfg, "two")
	for _, readData := range []bool{false, true} {
		rep, err := Check(context.Background(), cfg, CheckOptions{ReadData: readData})
		if err != nil || !rep.OK() || len(rep.Warnings) != 0 {
			t.Fatalf("readData=%v: err=%v rep=%+v", readData, err, rep)
		}
		if rep.Blobs != 2 || rep.Objects != 2 {
			t.Fatalf("counts: %+v", rep)
		}
		want := 0
		if readData {
			want = 2
		}
		if rep.ObjectsVerified != want {
			t.Fatalf("readData=%v verified %d, want %d", readData, rep.ObjectsVerified, want)
		}
	}
}

func TestCheckMissingObjectIsError(t *testing.T) {
	cfg := initRepo(t)
	k := addBlob(t, cfg, "will vanish")
	addBlob(t, cfg, "stays")
	if err := os.Remove(objPath(cfg, k)); err != nil {
		t.Fatal(err)
	}
	rep, err := Check(context.Background(), cfg, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || kinds(rep.Errors)["missing-object"] != 1 {
		t.Fatalf("want one missing-object error: %+v", rep.Errors)
	}
	if !strings.Contains(rep.Errors[0].Message, k.String()) {
		t.Fatalf("message should name the hash: %q", rep.Errors[0].Message)
	}
}

func TestCheckCorruptObjectOnlyWithReadData(t *testing.T) {
	cfg := initRepo(t)
	k := addBlob(t, cfg, strings.Repeat("payload ", 100))
	p := objPath(cfg, k)
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := Check(context.Background(), cfg, CheckOptions{})
	if err != nil || !rep.OK() {
		t.Fatalf("without --read-data the object is not read: err=%v rep=%+v", err, rep)
	}
	rep, err = Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || kinds(rep.Errors)["corrupt-object"] != 1 || rep.ObjectsVerified != 1 {
		t.Fatalf("want a corrupt-object error: %+v", rep)
	}
}

func TestCheckOrphanAndTmpAndJunkAreWarnings(t *testing.T) {
	cfg := initRepo(t)
	addBlob(t, cfg, "indexed")
	// an orphan: stored but never indexed (crash between blob write and index commit).
	r, _ := repo.Open(cfg.Repository.Path, nil)
	st, _ := objects.Open(r.ObjectsDir(), r.TmpDir())
	if _, _, err := st.Put([]byte("orphan")); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	_ = os.WriteFile(filepath.Join(r.TmpDir(), "half-written"), []byte{1}, 0o600)
	_ = os.WriteFile(filepath.Join(r.ObjectsDir(), "stray.txt"), []byte("?"), 0o600)

	rep, err := Check(context.Background(), cfg, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("orphans, tmp leftovers and stray files are allowed by the durability rule: %+v", rep.Errors)
	}
	got := kinds(rep.Warnings)
	if got["orphan-object"] != 1 || got["tmp-leftover"] != 1 || got["unexpected-file"] != 1 {
		t.Fatalf("warnings: %+v", rep.Warnings)
	}
	if rep.Objects != 2 || rep.Blobs != 1 {
		t.Fatalf("counts: %+v", rep)
	}
}

func TestCheckDanglingIndexReference(t *testing.T) {
	cfg := initRepo(t)
	addBlob(t, cfg, "x")
	r, _ := repo.Open(cfg.Repository.Path, nil)
	db, err := index.Open(r.IndexPath(), index.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, _ := db.Conn(ctx)
	_, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	_, _ = conn.ExecContext(ctx, `INSERT INTO folders(id, name, first_seen_run) VALUES (1,'INBOX',1)`)
	_, err = conn.ExecContext(ctx, `INSERT INTO entries(folder_id, uidvalidity, uid, blob, valid_from_run) VALUES (1,1,1,x'ff',1)`)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	_ = db.Close()

	rep, err := Check(ctx, cfg, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || kinds(rep.Errors)["dangling-reference"] == 0 {
		t.Fatalf("want a dangling-reference error: %+v", rep.Errors)
	}
}

func TestCheckMissingIndexIsReportedNotFatal(t *testing.T) {
	cfg := initRepo(t)
	if err := os.Remove(filepath.Join(cfg.Repository.Path, "index.db")); err != nil {
		t.Fatal(err)
	}
	rep, err := Check(context.Background(), cfg, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || kinds(rep.Errors)["index-open"] != 1 {
		t.Fatalf("want an index-open error: %+v", rep.Errors)
	}
}

func TestCheckMissingDirectoriesAreErrors(t *testing.T) {
	cfg := initRepo(t)
	if err := os.RemoveAll(filepath.Join(cfg.Repository.Path, "objects")); err != nil {
		t.Fatal(err)
	}
	rep, err := Check(context.Background(), cfg, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || kinds(rep.Errors)["missing-dir"] != 1 {
		t.Fatalf("want a missing-dir error: %+v", rep.Errors)
	}
	if _, err := os.Stat(filepath.Join(cfg.Repository.Path, "objects")); err == nil {
		t.Fatal("check must not recreate directories")
	}
}

func TestCheckRefusesWrongAccountAndNonRepository(t *testing.T) {
	cfg := initRepo(t)
	other := *cfg
	other.IMAP.Username = "someone-else@example.com"
	if _, err := Check(context.Background(), &other, CheckOptions{}); !errors.Is(err, repo.ErrWrongAccount) {
		t.Fatalf("want ErrWrongAccount, got %v", err)
	}
	empty := testConfig(t)
	if _, err := Check(context.Background(), empty, CheckOptions{}); !errors.Is(err, repo.ErrNotARepo) {
		t.Fatalf("want ErrNotARepo, got %v", err)
	}
}

func TestCheckRespectsLock(t *testing.T) {
	cfg := initRepo(t)
	r, _ := repo.Open(cfg.Repository.Path, nil)
	unlock, err := r.Lock(repo.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Check(context.Background(), cfg, CheckOptions{}); !errors.Is(err, repo.ErrLocked) {
		t.Fatalf("want ErrLocked while a writer runs, got %v", err)
	}
}

func TestCheckFindingsAreCapped(t *testing.T) {
	cfg := initRepo(t)
	r, _ := repo.Open(cfg.Repository.Path, nil)
	st, _ := objects.Open(r.ObjectsDir(), r.TmpDir())
	for i := 0; i < maxFindingsPerKind+15; i++ {
		_, _, _ = st.Put([]byte{byte(i), byte(i >> 8), 'o'})
	}
	_ = st.Close()
	rep, err := Check(context.Background(), cfg, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Objects != maxFindingsPerKind+15 {
		t.Fatalf("objects counted: %d", rep.Objects)
	}
	if n := kinds(rep.Warnings)["orphan-object"]; n > maxFindingsPerKind+1 {
		t.Fatalf("orphan findings not capped: %d", n)
	}
	last := rep.Warnings[len(rep.Warnings)-1]
	if !strings.Contains(last.Message, "15 more") {
		t.Fatalf("cap should say how many were left out: %q", last.Message)
	}
}
