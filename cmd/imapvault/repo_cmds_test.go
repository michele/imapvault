package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/repo"
)

// setupAccount writes <XDG_CONFIG_HOME>/imapvault/test.toml pointing at a fresh
// repository path and returns that path.
func setupAccount(t *testing.T) string {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	repoPath := filepath.Join(t.TempDir(), "repo")
	dir := filepath.Join(cfgHome, "imapvault")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[imap]\nhost = \"imap.example.com\"\nusername = \"me@example.com\"\n\n[repository]\npath = \"" + repoPath + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "test.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return repoPath
}

func TestInitThenCheck(t *testing.T) {
	repoPath := setupAccount(t)
	out, err := execRoot(t, "--account", "test", "init")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	for _, want := range []string{"Initialised repository", repoPath, "me@example.com@imap.example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("init output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(repoPath, "repo.toml")); err != nil {
		t.Fatal(err)
	}

	out, err = execRoot(t, "--account", "test", "check")
	if err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Check passed") {
		t.Errorf("check output:\n%s", out)
	}
	if _, err = execRoot(t, "--account", "test", "check", "--read-data"); err != nil {
		t.Fatalf("check --read-data: %v", err)
	}
}

func TestInitTwiceFails(t *testing.T) {
	setupAccount(t)
	if _, err := execRoot(t, "--account", "test", "init"); err != nil {
		t.Fatal(err)
	}
	_, err := execRoot(t, "--account", "test", "init")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want already-exists error, got %v", err)
	}
}

func TestInitNeedsNoPassword(t *testing.T) {
	setupAccount(t)
	t.Setenv("IMAPVAULT_IMAP_PASSWORD", "")
	t.Setenv("IMAPVAULT_IMAP_PASSWORD_TEST", "")
	if _, err := execRoot(t, "--account", "test", "init"); err != nil {
		t.Fatalf("init must not need a password: %v", err)
	}
}

func TestInitJSON(t *testing.T) {
	repoPath := setupAccount(t)
	out, err := execRoot(t, "--account", "test", "--json", "init")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not json: %v\n%s", err, out)
	}
	if got["path"] != repoPath || got["format_version"] != float64(repo.FormatVersion) {
		t.Fatalf("json: %v", got)
	}
}

func TestCheckReportsCorruptionAndFails(t *testing.T) {
	repoPath := setupAccount(t)
	if _, err := execRoot(t, "--account", "test", "init"); err != nil {
		t.Fatal(err)
	}
	// plant an orphan by hand-writing an object file: valid layout, garbage content.
	objDir := filepath.Join(repoPath, "objects", "ab")
	if err := os.MkdirAll(objDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := strings.Repeat("c", 62)
	if err := os.WriteFile(filepath.Join(objDir, name), []byte{0x01, 0xde, 0xad}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "tmp", "half"), []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}

	// without --read-data the garbage is only an orphan: warnings, exit ok.
	out, err := execRoot(t, "--account", "test", "check")
	if err != nil {
		t.Fatalf("warnings must not fail check: %v\n%s", err, out)
	}
	for _, want := range []string{"WARNING", "orphan-object", "tmp-leftover", "Check passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	out, err = execRoot(t, "--account", "test", "check", "--read-data")
	if err == nil {
		t.Fatalf("corrupt object must fail check:\n%s", out)
	}
	for _, want := range []string{"ERROR", "corrupt-object", "Check FAILED"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCheckJSONStillFailsOnErrors(t *testing.T) {
	repoPath := setupAccount(t)
	if _, err := execRoot(t, "--account", "test", "init"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repoPath, "index.db")); err != nil {
		t.Fatal(err)
	}
	out, err := execRoot(t, "--account", "test", "--json", "check")
	if err == nil {
		t.Fatal("missing index must fail check")
	}
	var rep struct {
		Errors []struct{ Kind string } `json:"errors"`
	}
	if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil || len(rep.Errors) == 0 || rep.Errors[0].Kind != "index-open" {
		t.Fatalf("json report: %v %s", jerr, out)
	}
}

func TestCheckOnUninitialisedRepositoryExplains(t *testing.T) {
	setupAccount(t)
	_, err := execRoot(t, "--account", "test", "check")
	if err == nil || !strings.Contains(err.Error(), "not an imapvault repository") {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownAccountIsAClearError(t *testing.T) {
	setupAccount(t)
	if _, err := execRoot(t, "--account", "nope", "check"); err == nil {
		t.Fatal("unknown account accepted")
	}
}

func TestConfigFlagWithoutAccount(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := filepath.Join(t.TempDir(), "c.toml")
	body := "[imap]\nhost = \"imap.example.com\"\nusername = \"me@example.com\"\n[repository]\npath = \"" + repoPath + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execRoot(t, "--config", cfg, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repoPath, "repo.toml")); err != nil {
		t.Fatal(err)
	}
}
