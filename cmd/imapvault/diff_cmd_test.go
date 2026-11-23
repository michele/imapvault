package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/engine"
	"github.com/michele/imapvault/internal/mail/mailtest"
)

// twoSnapshots initialises the "test" account and backs up a fake mailbox, then
// changes it and backs up again: snapshot 1 -> 2 adds one message to INBOX, flags
// another and deletes a third.
func twoSnapshots(t *testing.T, extra string) {
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
	inbox.Add("Subject: one\r\n\r\nbody one\r\n")
	inbox.Add("Subject: two\r\n\r\nbody two\r\n")
	inbox.Add("Subject: three\r\n\r\nbody three\r\n")
	if _, err := engine.Backup(context.Background(), cfg, f, engine.BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	inbox.Add("Subject: four\r\n\r\nbody four\r\n")
	inbox.SetFlags(1, `\Seen`)
	inbox.Delete(3)
	if _, err := engine.Backup(context.Background(), cfg, f, engine.BackupOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestDiffCommand(t *testing.T) {
	twoSnapshots(t, "")

	out, err := execRoot(t, "--account", "test", "diff", "1", "latest")
	if err != nil {
		t.Fatalf("diff: %v\n%s", err, out)
	}
	for _, want := range []string{"Snapshot 1", "-> snapshot 2", "FOLDER", "INBOX", "TOTAL"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// INBOX: +1 added, -1 deleted, 0 moved in/out, 1 reflagged, 0 renumbered
	var inboxLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "INBOX") {
			inboxLine = strings.Join(strings.Fields(l), " ")
		}
	}
	if inboxLine != "INBOX 1 1 0 0 1 0" {
		t.Fatalf("INBOX line %q\n%s", inboxLine, out)
	}

	js, err := execRoot(t, "--account", "test", "--json", "diff", "latest~1", "latest")
	if err != nil {
		t.Fatal(err)
	}
	var res engine.DiffResult
	if err := json.Unmarshal([]byte(js), &res); err != nil {
		t.Fatalf("%v\n%s", err, js)
	}
	if res.From != 1 || res.To != 2 || len(res.Folders) != 1 || res.Folders[0].Name != "INBOX" ||
		res.Totals != (engine.DiffCounts{Added: 1, Deleted: 1, Reflagged: 1}) {
		t.Fatalf("json result %+v", res)
	}
	if !strings.Contains(js, `"moved_in": 0`) {
		t.Fatalf("zero counts must be present in JSON:\n%s", js)
	}
}

func TestDiffCommandNoDifferencesAndBadOrder(t *testing.T) {
	twoSnapshots(t, "")
	if out, err := execRoot(t, "--account", "test", "diff", "2", "2"); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("same snapshot: %v\n%s", err, out)
	}
	if _, err := execRoot(t, "--account", "test", "diff", "2", "1"); err == nil {
		t.Fatal("reversed order accepted")
	}
	if _, err := execRoot(t, "--account", "test", "diff", "1"); err == nil {
		t.Fatal("one argument accepted")
	}

	// a third snapshot of an unchanged mailbox: 2 -> 3 is empty
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
	inbox.Add("Subject: one\r\n\r\nbody one\r\n")
	inbox.Add("Subject: two\r\n\r\nbody two\r\n")
	inbox.Add("Subject: three\r\n\r\nbody three\r\n")
	inbox.Add("Subject: four\r\n\r\nbody four\r\n")
	inbox.SetFlags(1, `\Seen`)
	inbox.Delete(3)
	// the fake above is a new server for the same repository: only the final
	// state matters, and it equals snapshot 2.
	if _, err := engine.Backup(context.Background(), cfg, f, engine.BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err := execRoot(t, "--account", "test", "diff", "2", "3")
	if err != nil || !strings.Contains(out, "No differences.") {
		t.Fatalf("2 -> 3: %v\n%s", err, out)
	}
}

func TestDiffCommandEncryptedWithAndWithoutKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "k.key")
	out, err := execRoot(t, "keygen", "--out", keyFile)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	recipient := strings.Fields(out[strings.Index(out, "age1"):])[0]
	twoSnapshots(t, "\n[encryption]\nrecipients = [\""+recipient+"\"]\n")

	out, err = execRoot(t, "--account", "test", "diff", "1", "2")
	if err != nil {
		t.Fatalf("without a key: %v\n%s", err, out)
	}
	if strings.Contains(out, "INBOX") || !strings.Contains(out, "#1") || !strings.Contains(out, "no private key") {
		t.Fatalf("without a key the folder must show by number, with a warning:\n%s", out)
	}

	t.Setenv("IMAPVAULT_IDENTITY", mustRead(t, keyFile))
	out, err = execRoot(t, "--account", "test", "diff", "1", "2")
	if err != nil || !strings.Contains(out, "INBOX") || strings.Contains(out, "no private key") {
		t.Fatalf("with the key: %v\n%s", err, out)
	}
}

func TestPrintBackup(t *testing.T) {
	rep := &engine.BackupReport{
		Snapshot: 7, Status: "partial", Messages: 10, New: 2, Closed: 1, Reflagged: 3, Fetched: 2, SkippedByEmailID: 1, BlobsAdded: 2, BytesRaw: 2048,
		Folders:  []engine.FolderReport{{Name: "A", Mode: "unchanged"}, {Name: "B", Mode: "incremental"}, {Name: "C", Mode: "full"}, {Name: "D", Mode: "full"}},
		Warnings: []string{"run stopped: boom \x1b[31m"},
	}
	var b bytes.Buffer
	printBackup(&b, rep, "")
	got := b.String()
	for _, want := range []string{"Snapshot 7 (partial): 10 messages in 4 folders", "+2 new, -1 deleted, ~3 flag changes",
		"2 body(ies) downloaded, 1 skipped (EMAILID); folders: 1 unchanged, 1 incremental, 2 full", "warning: run stopped: boom ?[31m"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Fatal("a control character reached the terminal")
	}
	rep.Status = "complete"
	b.Reset()
	printBackup(&b, rep, "")
	if strings.Contains(b.String(), "partial") {
		t.Fatalf("a complete run says partial:\n%s", b.String())
	}
}
