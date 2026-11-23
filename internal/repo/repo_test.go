package repo

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var acct = Account{ID: "me@example.com@imap.example.com", Host: "imap.example.com", Username: "me@example.com"}

func TestInitCreatesLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	r, err := Init(dir, acct)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"objects", "tmp", "index-backups"} {
		fi, err := os.Stat(filepath.Join(dir, sub))
		if err != nil || !fi.IsDir() {
			t.Fatalf("%s missing: %v", sub, err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v", sub, fi.Mode().Perm())
		}
	}
	fi, err := os.Stat(filepath.Join(dir, "repo.toml"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("repo.toml: %v %v", err, fi)
	}
	if r.Meta.FormatVersion != FormatVersion || len(r.Meta.ID) != 32 || r.Meta.Account != acct {
		t.Fatalf("meta: %+v", r.Meta)
	}
	if time.Since(r.Meta.CreatedAt) > time.Minute {
		t.Fatalf("created_at %v", r.Meta.CreatedAt)
	}
}

func TestInitRefusesNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir, acct); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("want not-empty refusal, got %v", err)
	}
}

func TestInitAllowsExistingEmptyDirAndRefusesSecondInit(t *testing.T) {
	dir := t.TempDir() // exists, empty
	if _, err := Init(dir, acct); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir, acct); !errors.Is(err, ErrExists) {
		t.Fatalf("second init: want ErrExists, got %v", err)
	}
}

func TestInitRequiresAbsolutePath(t *testing.T) {
	if _, err := Init("relative/repo", acct); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestOpenRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	created, err := Init(dir, acct)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Open(dir, &acct)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Meta.ID != created.Meta.ID || !opened.Meta.CreatedAt.Equal(created.Meta.CreatedAt) {
		t.Fatalf("meta differs: %+v vs %+v", opened.Meta, created.Meta)
	}
	if _, err := Open(dir, nil); err != nil {
		t.Fatalf("open without binding check: %v", err)
	}
}

func TestOpenRefusesOtherAccount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	if _, err := Init(dir, acct); err != nil {
		t.Fatal(err)
	}
	other := Account{ID: "evil@example.com@imap.example.com", Host: "imap.example.com", Username: "evil@example.com"}
	_, err := Open(dir, &other)
	if !errors.Is(err, ErrWrongAccount) {
		t.Fatalf("want ErrWrongAccount, got %v", err)
	}
	if !strings.Contains(err.Error(), acct.ID) || !strings.Contains(err.Error(), other.ID) {
		t.Fatalf("message should name both accounts: %v", err)
	}
}

func TestOpenRefusesOtherFormatVersions(t *testing.T) {
	for _, v := range []int{0, 2, 99} {
		dir := filepath.Join(t.TempDir(), "repo")
		if _, err := Init(dir, acct); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "repo.toml")
		b, _ := os.ReadFile(p)
		s := strings.Replace(string(b), "format_version = 1", "format_version = "+strconv.Itoa(v), 1)
		if s == string(b) {
			t.Fatal("format_version line not found in repo.toml")
		}
		_ = os.WriteFile(p, []byte(s), 0o600)
		if _, err := Open(dir, &acct); !errors.Is(err, ErrFormatVersion) {
			t.Fatalf("version %d: want ErrFormatVersion, got %v", v, err)
		}
	}
}

func TestOpenMissingAndBrokenMeta(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, nil); !errors.Is(err, ErrNotARepo) {
		t.Fatalf("empty dir: want ErrNotARepo, got %v", err)
	}
	if _, err := Open(filepath.Join(dir, "nope"), nil); !errors.Is(err, ErrNotARepo) {
		t.Fatalf("missing dir: want ErrNotARepo, got %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "repo.toml"), []byte("not [ toml"), 0o600)
	if _, err := Open(dir, nil); err == nil || errors.Is(err, ErrNotARepo) {
		t.Fatalf("broken toml: want a parse error, got %v", err)
	}
}

func TestPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	r, _ := Init(dir, acct)
	if r.ObjectsDir() != filepath.Join(dir, "objects") || r.TmpDir() != filepath.Join(dir, "tmp") ||
		r.IndexPath() != filepath.Join(dir, "index.db") || r.IndexBackupsDir() != filepath.Join(dir, "index-backups") {
		t.Fatal("unexpected paths")
	}
}
