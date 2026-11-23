package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/mail/mailtest"
	"github.com/michele/imapvault/internal/seal"
)

func TestSnapshotsListsRunsWithoutAKey(t *testing.T) {
	cfg, _ := initEncrypted(t)
	runBackup(t, cfg, fakeMailbox())
	runBackup(t, cfg, fakeMailbox())
	// no identity configured at all: `snapshots` must not need one.
	snaps, err := Snapshots(context.Background(), cfg, false)
	if err != nil || len(snaps) != 2 || snaps[1].Messages != 3 || snaps[1].Folders != 2 || snaps[0].Pinned {
		t.Fatalf("%+v err %v", snaps, err)
	}
}

func TestLsPlainAndSnapshotSelection(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	first := runBackup(t, cfg, f)
	rawLen := int64(len(f.Folders[0].Messages[0].Raw))
	f.Folders[0].Delete(1)
	runBackup(t, cfg, f)

	res, err := Ls(context.Background(), cfg, "latest", "")
	if err != nil || len(res.Folders) != 2 || res.Folders[0].Name != "INBOX" || res.Folders[0].Messages != 1 || res.Folders[1].Messages != 1 {
		t.Fatalf("%+v err %v", res, err)
	}
	old, err := Ls(context.Background(), cfg, "1", "INBOX")
	if err != nil || old.Snapshot != first.Snapshot || len(old.Messages) != 2 {
		t.Fatalf("%+v err %v", old, err)
	}
	m := old.Messages[0]
	if m.UID != 1 || m.Subject != "Hello unique-subject-1" || m.MessageID != "<1@example.com>" || m.Size != rawLen {
		t.Fatalf("message %+v", m)
	}
	if m.Date.IsZero() || len(m.SHA256) != 64 {
		t.Fatalf("message %+v", m)
	}
	for _, bad := range []struct{ snap, folder string }{{"9", ""}, {"nope", ""}, {"1", "Nope"}, {"0", ""}} {
		if _, err := Ls(context.Background(), cfg, bad.snap, bad.folder); err == nil {
			t.Errorf("Ls(%q,%q) should fail", bad.snap, bad.folder)
		}
	}
}

func TestLsWithNoSnapshotsSaysSo(t *testing.T) {
	cfg := initRepo(t)
	if _, err := Ls(context.Background(), cfg, "latest", ""); err == nil || !strings.Contains(err.Error(), "no snapshots") {
		t.Fatalf("err %v", err)
	}
}

func TestEncryptedLsAndShowNeedTheRightKey(t *testing.T) {
	cfg, keys := initEncrypted(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)

	if _, err := Ls(context.Background(), cfg, "latest", ""); !errors.Is(err, config.ErrNoIdentity) {
		t.Fatalf("no key: %v", err)
	}
	cfg.Encryption.Identity = keys.identity // what the env var or identity file gives after parsing
	res, err := Ls(context.Background(), cfg, "latest", "INBOX")
	if err != nil || len(res.Messages) != 2 || res.Messages[1].Subject != "Meeting unique-subject-2" {
		t.Fatalf("%+v err %v", res, err)
	}
	sha := res.Messages[0].SHA256
	sh, err := Show(context.Background(), cfg, sha[:12], true)
	if err != nil || string(sh.Raw) != string(f.Folders[0].Messages[0].Raw) || sh.Meta.Subject != "Hello unique-subject-1" || sh.SHA256 != sha {
		t.Fatalf("%+v err %v", sh, err)
	}
	if hdr, err := Show(context.Background(), cfg, sha[:8], false); err != nil || hdr.Raw != nil || hdr.Size != sh.Size {
		t.Fatalf("%+v err %v", hdr, err)
	}

	// someone else's key is "wrong key", not corruption
	other := newEncKeys(t)
	cfg.Encryption.Identity = other.identity
	if _, err := Ls(context.Background(), cfg, "latest", ""); !errors.Is(err, seal.ErrWrongIdentity) {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestShowPrefixRules(t *testing.T) {
	cfg := initRepo(t)
	runBackup(t, cfg, fakeMailbox())
	if _, err := Show(context.Background(), cfg, "abc", false); err == nil || !strings.Contains(err.Error(), "at least 8") {
		t.Fatalf("short prefix: %v", err)
	}
	if _, err := Show(context.Background(), cfg, "00000000", false); err == nil || !strings.Contains(err.Error(), "no message") {
		t.Fatalf("unknown prefix: %v", err)
	}
	if _, err := Show(context.Background(), cfg, "zzzzzzzzzz", false); err == nil {
		t.Fatal("non-hex prefix must fail")
	}
}

func TestDoctorReportsCapabilitiesAndSelection(t *testing.T) {
	cfg := initRepo(t)
	cfg.Folders = config.Folders{Include: []string{"*"}, Exclude: []string{"Sent"}}
	f := fakeMailbox()
	f.Folders = append(f.Folders, &mailtest.Folder{Name: "[Gmail]", NoSelect: true})
	rep, err := Doctor(context.Background(), cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Features[0].Supported || rep.Features[1].Supported { // CONDSTORE yes, QRESYNC no on the fake
		t.Fatalf("features %+v", rep.Features)
	}
	sel := map[string]bool{}
	for _, fo := range rep.Folders {
		sel[fo.Name] = fo.Selected
	}
	if !sel["INBOX"] || sel["Sent"] || sel["[Gmail]"] || rep.Selected != 2 {
		t.Fatalf("%+v", rep)
	}
}
