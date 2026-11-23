package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func execRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestAccountsEmptyAndListed(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	out, err := execRoot(t, "accounts")
	if err != nil || !strings.Contains(out, "No accounts found") {
		t.Fatalf("empty: out=%q err=%v", out, err)
	}

	dir := filepath.Join(cfg, "imapvault")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	good := "[imap]\nhost = \"imap.example.com\"\nusername = \"me@example.com\"\n[repository]\npath = \"/backup/me\"\n"
	if err := os.WriteFile(filepath.Join(dir, "personal.toml"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.toml"), []byte("nope ["), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = execRoot(t, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"REPOSITORY", "personal", "imap.example.com", "/backup/me", "broken", "error"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
