package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/engine"
	"github.com/michele/imapvault/internal/mail/mailtest"
)

// backedUp creates an account, initialises it and runs one backup from a fake
// mailbox, returning the raw bytes of the first INBOX message.
func backedUp(t *testing.T, extra string) (raw string) {
	t.Helper()
	setupAccountWith(t, extra)
	if out, err := execRoot(t, "--account", "test", "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	path, err := config.AccountConfigPath(os.Getenv, "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAccountOffline(path, "test")
	if err != nil {
		t.Fatal(err)
	}
	f := mailtest.New()
	inbox := f.AddFolder("INBOX")
	raw = "Message-ID: <a@example.com>\r\nFrom: Alice <alice@example.com>\r\nSubject: Hi \x1b[31mred\r\nDate: Mon, 02 Jan 2025 03:04:05 +0000\r\n\r\nhello\r\n"
	inbox.Add(raw)
	inbox.Add("Subject: second\r\n\r\nbody\r\n", `\Seen`)
	if _, err := engine.Backup(context.Background(), cfg, f, engine.BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSnapshotsLsShow(t *testing.T) {
	raw := backedUp(t, "")

	out, err := execRoot(t, "--account", "test", "snapshots")
	if err != nil || !strings.Contains(out, "ID") || !strings.Contains(out, "2") {
		t.Fatalf("snapshots: %v\n%s", err, out)
	}
	out, err = execRoot(t, "--account", "test", "ls", "latest")
	if err != nil || !strings.Contains(out, "INBOX") {
		t.Fatalf("ls: %v\n%s", err, out)
	}
	out, err = execRoot(t, "--account", "test", "ls", "latest", "INBOX")
	if err != nil {
		t.Fatalf("ls INBOX: %v\n%s", err, out)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("a terminal escape from a subject reached the output: %q", out)
	}
	if !strings.Contains(out, "Hi ?[31mred") || !strings.Contains(out, `\Seen`) {
		t.Fatalf("ls INBOX:\n%s", out)
	}

	js, err := execRoot(t, "--account", "test", "--json", "ls", "1", "INBOX")
	if err != nil || !strings.Contains(js, `"uid": 1`) {
		t.Fatalf("ls --json: %v\n%s", err, js)
	}
	// the sha column is the first 12 hex characters of the full hash
	i := strings.Index(js, `"sha256": "`)
	sha := js[i+len(`"sha256": "`) : i+len(`"sha256": "`)+16]
	got, err := execRoot(t, "--account", "test", "show", "--raw", sha)
	if err != nil || got != raw {
		t.Fatalf("show --raw: %v\n%q", err, got)
	}
	out, err = execRoot(t, "--account", "test", "show", sha)
	if err != nil || !strings.Contains(out, "alice@example.com") || strings.Contains(out, "\x1b") {
		t.Fatalf("show: %v\n%s", err, out)
	}
}

func TestSnapshotsEmpty(t *testing.T) {
	setupAccount(t)
	if _, err := execRoot(t, "--account", "test", "init"); err != nil {
		t.Fatal(err)
	}
	out, err := execRoot(t, "--account", "test", "snapshots")
	if err != nil || !strings.Contains(out, "No snapshots yet") {
		t.Fatalf("%v %q", err, out)
	}
	js, err := execRoot(t, "--account", "test", "--json", "snapshots")
	if err != nil || strings.TrimSpace(js) != "[]" {
		t.Fatalf("%v %q", err, js)
	}
}

func TestEncryptedLsNeedsKeyFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "k.key")
	out, err := execRoot(t, "keygen", "--out", keyFile)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	i := strings.Index(out, "age1")
	recipient := strings.Fields(out[i:])[0]
	backedUp(t, "\n[encryption]\nrecipients = [\""+recipient+"\"]\n")

	if _, err := execRoot(t, "--account", "test", "ls", "latest"); err == nil || !strings.Contains(err.Error(), "no age identity") {
		t.Fatalf("without a key: %v", err)
	}
	if out, err := execRoot(t, "--account", "test", "snapshots"); err != nil {
		t.Fatalf("snapshots must not need the key: %v\n%s", err, out)
	}
	t.Setenv("IMAPVAULT_IDENTITY", mustRead(t, keyFile))
	out, err = execRoot(t, "--account", "test", "ls", "latest", "INBOX")
	if err != nil || !strings.Contains(out, "alice@example.com") {
		t.Fatalf("with the key: %v\n%s", err, out)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCleanStripsControlCharacters(t *testing.T) {
	if got := clean("a\x1b[2Jb\tc\n"); got != "a?[2Jb?c?" {
		t.Fatalf("%q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 5 << 20: "5.0 MiB"} {
		if got := humanBytes(in); got != want {
			t.Errorf("%d: %q want %q", in, got, want)
		}
	}
}
