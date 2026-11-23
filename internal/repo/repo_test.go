package repo

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var acct = Account{ID: "me@example.com@imap.example.com", Host: "imap.example.com", Username: "me@example.com"}

func TestInitCreatesLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	r, err := Init(dir, acct, Encryption{})
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
	if _, err := Init(dir, acct, Encryption{}); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("want not-empty refusal, got %v", err)
	}
}

func TestInitAllowsExistingEmptyDirAndRefusesSecondInit(t *testing.T) {
	dir := t.TempDir() // exists, empty
	if _, err := Init(dir, acct, Encryption{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir, acct, Encryption{}); !errors.Is(err, ErrExists) {
		t.Fatalf("second init: want ErrExists, got %v", err)
	}
}

func TestInitReusesLeftoverLayoutOfInterruptedInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	r, err := Init(dir, acct, Encryption{})
	if err != nil {
		t.Fatal(err)
	}
	// an interrupted init is one that never got repo.toml (or whose caller backed it out).
	if err := r.Uninit(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, nil); !errors.Is(err, ErrNotARepo) {
		t.Fatalf("after Uninit: want ErrNotARepo, got %v", err)
	}
	if _, err := Init(dir, acct, Encryption{}); err != nil {
		t.Fatalf("re-init over leftover layout: %v", err)
	}
}

func TestInitStillRefusesForeignFilesNextToLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	r, _ := Init(dir, acct, Encryption{})
	_ = r.Uninit()
	_ = os.WriteFile(filepath.Join(dir, "holiday-photos.zip"), []byte("x"), 0o600)
	if _, err := Init(dir, acct, Encryption{}); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("want not-empty refusal, got %v", err)
	}
}

func TestInitRequiresAbsolutePath(t *testing.T) {
	if _, err := Init("relative/repo", acct, Encryption{}); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestOpenRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	created, err := Init(dir, acct, Encryption{})
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
	if _, err := Init(dir, acct, Encryption{}); err != nil {
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
	for _, v := range []int{0, 1, 3, 99} {
		dir := filepath.Join(t.TempDir(), "repo")
		if _, err := Init(dir, acct, Encryption{}); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "repo.toml")
		b, _ := os.ReadFile(p)
		s := strings.Replace(string(b), "format_version = "+strconv.Itoa(FormatVersion), "format_version = "+strconv.Itoa(v), 1)
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
	r, _ := Init(dir, acct, Encryption{})
	if r.ObjectsDir() != filepath.Join(dir, "objects") || r.TmpDir() != filepath.Join(dir, "tmp") ||
		r.IndexPath() != filepath.Join(dir, "index.db") || r.IndexBackupsDir() != filepath.Join(dir, "index-backups") {
		t.Fatal("unexpected paths")
	}
}

func TestInitRecordsFolderKey(t *testing.T) {
	r1, _ := Init(filepath.Join(t.TempDir(), "a"), acct, Encryption{})
	r2, _ := Init(filepath.Join(t.TempDir(), "b"), acct, Encryption{})
	k1, err := r1.FolderKey()
	if err != nil || len(k1) != 32 {
		t.Fatalf("folder key: %d bytes, %v", len(k1), err)
	}
	k2, _ := r2.FolderKey()
	if string(k1) == string(k2) {
		t.Fatal("every repository needs its own folder key")
	}
	reopened, err := Open(r1.Path, &acct)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := reopened.FolderKey(); string(again) != string(k1) {
		t.Fatal("folder key must survive reopen")
	}
}

func TestEncryptionBindingRoundTrips(t *testing.T) {
	enc := Encryption{Enabled: true, Recipients: []string{"age1aaa", "age1bbb"}, Canary: "Y2FuYXJ5", CanarySHA256: strings.Repeat("ab", 32)}
	dir := filepath.Join(t.TempDir(), "repo")
	if _, err := Init(dir, acct, enc); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir, &acct)
	if err != nil {
		t.Fatal(err)
	}
	got := r.Meta.Encryption
	if !got.Enabled || len(got.Recipients) != 2 || got.Recipients[1] != "age1bbb" || got.Canary != enc.Canary || got.CanarySHA256 != enc.CanarySHA256 {
		t.Fatalf("encryption: %+v", got)
	}
	plain, _ := Init(filepath.Join(t.TempDir(), "p"), acct, Encryption{})
	if plain.Meta.Encryption.Enabled {
		t.Fatal("zero Encryption must mean plain")
	}
}

func TestInitRejectsHalfSpecifiedEncryption(t *testing.T) {
	for name, enc := range map[string]Encryption{
		"no recipients": {Enabled: true, Canary: "x", CanarySHA256: strings.Repeat("ab", 32)},
		"no canary":     {Enabled: true, Recipients: []string{"age1aaa"}, CanarySHA256: strings.Repeat("ab", 32)},
		"bad hash":      {Enabled: true, Recipients: []string{"age1aaa"}, Canary: "x", CanarySHA256: "zz"},
	} {
		if _, err := Init(filepath.Join(t.TempDir(), "r"), acct, enc); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestOpenRefusesDamagedMeta(t *testing.T) {
	cases := map[string]func(string) string{
		"folder key missing": func(s string) string { return regexpReplace(s, `(?m)^folder_key = .*\n`, "") },
		"folder key short":   func(s string) string { return regexpReplace(s, `(?m)^folder_key = .*$`, "folder_key = 'abcd'") },
		"folder key not hex": func(s string) string {
			return regexpReplace(s, `(?m)^folder_key = .*$`, "folder_key = '"+strings.Repeat("zz", 32)+"'")
		},
	}
	for name, mutate := range cases {
		dir := filepath.Join(t.TempDir(), "repo")
		if _, err := Init(dir, acct, Encryption{}); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "repo.toml")
		b, _ := os.ReadFile(p)
		mutated := mutate(string(b))
		if mutated == string(b) {
			t.Fatalf("%s: mutation did not change repo.toml", name)
		}
		_ = os.WriteFile(p, []byte(mutated), 0o600)
		if _, err := Open(dir, &acct); err == nil {
			t.Errorf("%s: damaged repo.toml opened", name)
		}
	}
}

func regexpReplace(s, pattern, repl string) string {
	return regexp.MustCompile(pattern).ReplaceAllString(s, repl)
}
