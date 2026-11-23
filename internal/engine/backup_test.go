package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/mail/mailtest"
	"github.com/michele/imapvault/internal/repo"
	"github.com/michele/imapvault/internal/seal"
)

func msg(id, subject, body string) string {
	return fmt.Sprintf("Message-ID: <%s@example.com>\r\nFrom: A <a@example.com>\r\nTo: b@example.com\r\nSubject: %s\r\nDate: Mon, 02 Jan 2025 03:04:05 +0000\r\n\r\n%s\r\n", id, subject, body)
}

func fakeMailbox() *mailtest.Fake {
	f := mailtest.New()
	inbox := f.AddFolder("INBOX")
	inbox.Add(msg("1", "Hello unique-subject-1", "body one"))
	inbox.Add(msg("2", "Meeting unique-subject-2", "body two"), `\Seen`)
	sent := f.AddFolder("Sent")
	sent.Add(msg("3", "Reply unique-subject-3", "body three"), `\Seen`)
	return f
}

func runBackup(t *testing.T, cfg *config.Config, f *mailtest.Fake) *BackupReport {
	t.Helper()
	rep, err := Backup(context.Background(), cfg, f, BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// snapshotOf reads a snapshot back through the index: folder name -> "uid flags sha8".
func snapshotOf(t *testing.T, cfg *config.Config, snap int64, identity string) map[string][]string {
	t.Helper()
	acct := accountOf(cfg)
	r, err := repo.Open(cfg.Repository.Path, &acct)
	if err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(r.IndexPath(), index.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	op, err := openerOf(identity)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := db.SnapshotFolders(snap)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, f := range fs {
		name, err := op.Open(f.NameSealed)
		if err != nil {
			t.Fatal(err)
		}
		es, err := db.SnapshotEntries(snap, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		list := []string{}
		for _, e := range es {
			list = append(list, fmt.Sprintf("%d [%s] %x", e.UID, e.Flags, e.Blob[:4]))
		}
		out[string(name)] = list
	}
	return out
}

func sum4(s string) string { h := sha256.Sum256([]byte(s)); return fmt.Sprintf("%x", h[:4]) }

func TestFirstBackupWritesSnapshotMatchingServer(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	rep := runBackup(t, cfg, f)
	if rep.Snapshot == 0 || rep.Messages != 3 || rep.New != 3 || rep.NewFolders != 2 || rep.BlobsAdded != 3 {
		t.Fatalf("report %+v", rep)
	}
	inbox := f.Folders[0].Messages
	want := map[string][]string{
		"INBOX": {fmt.Sprintf("1 [] %s", sum4(string(inbox[0].Raw))), fmt.Sprintf(`2 [\Seen] %s`, sum4(string(inbox[1].Raw)))},
		"Sent":  {fmt.Sprintf(`1 [\Seen] %s`, sum4(string(f.Folders[1].Messages[0].Raw)))},
	}
	if got := snapshotOf(t, cfg, rep.Snapshot, ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot %v\nwant %v", got, want)
	}
	chk, err := Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if err != nil || !chk.OK() || len(chk.Warnings) != 0 {
		t.Fatalf("check after backup: %+v err %v", chk, err)
	}
}

func TestBackupOnlyUsesTheReadOnlySurface(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	for _, c := range f.Calls {
		name, _, _ := strings.Cut(c, " ")
		switch name {
		case "capabilities", "list", "examine", "list-messages", "changed-flags", "uids", "fetch-bodies":
		default:
			t.Errorf("unexpected source call %q", c)
		}
	}
}

func TestSecondRunWithNoChangesDownloadsNothing(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	first := runBackup(t, cfg, f)
	f.Calls = nil
	second := runBackup(t, cfg, f)
	if second.Snapshot == first.Snapshot || second.New+second.Closed+second.Reflagged != 0 || second.Messages != 3 {
		t.Fatalf("second %+v", second)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "fetch-bodies") {
			t.Fatalf("a no-change run downloaded bodies: %v", f.Calls)
		}
	}
	if !reflect.DeepEqual(snapshotOf(t, cfg, first.Snapshot, ""), snapshotOf(t, cfg, second.Snapshot, "")) {
		t.Fatal("snapshots differ without server changes")
	}
}

func TestRescanSeesFlagsDeletesAndNewMail(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	first := runBackup(t, cfg, f)
	before := snapshotOf(t, cfg, first.Snapshot, "")

	inbox := f.Folders[0]
	inbox.SetFlags(1, `\Seen`, `\Flagged`, `\Recent`)
	inbox.Delete(2)
	newUID := inbox.Add(msg("4", "Fresh unique-subject-4", "body four"))
	second := runBackup(t, cfg, f)
	if second.New != 1 || second.Closed != 1 || second.Reflagged != 1 || second.Messages != 3 {
		t.Fatalf("second %+v", second)
	}
	got := snapshotOf(t, cfg, second.Snapshot, "")["INBOX"]
	want := []string{
		fmt.Sprintf(`1 [\Flagged \Seen] %s`, sum4(string(inbox.Messages[0].Raw))),
		fmt.Sprintf("%d [] %s", newUID, sum4(string(inbox.Messages[1].Raw))),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot 2 INBOX %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(snapshotOf(t, cfg, first.Snapshot, ""), before) {
		t.Fatal("snapshot 1 changed after a later backup")
	}
	// the deleted message's blob is still in the repository: only prune removes data.
	chk, err := Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if err != nil || !chk.OK() || chk.Blobs != 4 {
		t.Fatalf("check: %+v err %v", chk, err)
	}
}

func TestSameMessageInTwoFoldersIsStoredOnce(t *testing.T) {
	cfg := initRepo(t)
	f := mailtest.New()
	same := msg("9", "Shared unique-subject-9", "same bytes")
	f.AddFolder("INBOX").Add(same)
	f.AddFolder("Archive").Add(same)
	rep := runBackup(t, cfg, f)
	if rep.BlobsAdded != 1 || rep.BlobsReused != 1 || rep.Messages != 2 {
		t.Fatalf("report %+v", rep)
	}
	chk, _ := Check(context.Background(), cfg, CheckOptions{})
	if chk.Blobs != 1 || chk.Objects != 1 {
		t.Fatalf("blobs %d objects %d", chk.Blobs, chk.Objects)
	}
}

func TestFolderGoneOrExcludedClosesEntriesButKeepsBlobs(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	first := runBackup(t, cfg, f)

	cfg.Folders = config.Folders{Include: []string{"*"}, Exclude: []string{"Sent"}}
	second := runBackup(t, cfg, f)
	if second.GoneFolders != 1 || second.Messages != 2 {
		t.Fatalf("second %+v", second)
	}
	if _, ok := snapshotOf(t, cfg, second.Snapshot, "")["Sent"]; ok {
		t.Fatal("excluded folder still in the new snapshot")
	}
	if len(snapshotOf(t, cfg, first.Snapshot, "")["Sent"]) != 1 {
		t.Fatal("old snapshot lost the folder")
	}
	chk, _ := Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if !chk.OK() || chk.Blobs != 3 {
		t.Fatalf("check %+v", chk)
	}
}

func TestNoSelectFoldersAreSkipped(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	f.Folders = append(f.Folders, &mailtest.Folder{Name: "[Gmail]", NoSelect: true})
	rep := runBackup(t, cfg, f)
	if rep.NewFolders != 2 {
		t.Fatalf("%+v", rep)
	}
	for _, c := range f.Calls {
		if strings.Contains(c, "[Gmail]") && strings.HasPrefix(c, "examine") {
			t.Fatal("examined a \\Noselect folder")
		}
	}
}

// A run that fails mid-folder keeps the bodies it already fetched: they are
// committed before the error is returned, so nothing is left dangling or
// orphaned and the next run only fetches what is missing.
func TestFailedRunKeepsFetchedBodiesAndResumes(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	f.FailAfter = map[string]int{"INBOX": 1}
	if _, err := Backup(context.Background(), cfg, f, BackupOptions{}); err == nil {
		t.Fatal("expected the injected failure")
	}
	chk, err := Check(context.Background(), cfg, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !chk.OK() || len(chk.Warnings) != 0 {
		t.Fatalf("a failed run must leave a consistent repository: %+v %+v", chk.Errors, chk.Warnings)
	}
	if got := snapshotsCount(t, cfg); got != 0 {
		t.Fatalf("%d snapshots after a failed run", got)
	}

	f.FailAfter = nil
	f.Reset()
	rep := runBackup(t, cfg, f)
	if rep.Messages != 3 || rep.BlobsAdded != 2 || rep.Fetched != 2 || rep.New != 2 {
		t.Fatalf("resumed run %+v (the one body fetched before the failure must not be fetched again)", rep)
	}
	if chk, _ := Check(context.Background(), cfg, CheckOptions{ReadData: true}); !chk.OK() || len(chk.Warnings) != 0 {
		t.Fatalf("after recovery: %+v", chk)
	}
}

// A blob object whose index row never made it (the process was killed between the
// two) is an orphan. The next run reuses the file instead of writing it again.
func TestOrphanFromAKilledRunIsReusedNotRewritten(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	putOrphan(t, cfg, string(f.Folders[0].Messages[0].Raw))
	chk, _ := Check(context.Background(), cfg, CheckOptions{})
	if kinds(chk.Warnings)["orphan-object"] != 1 || !chk.OK() {
		t.Fatalf("setup: %+v", chk)
	}
	rep := runBackup(t, cfg, f)
	if rep.Messages != 3 || rep.BlobsAdded != 2 || rep.BlobsReused != 1 {
		t.Fatalf("report %+v", rep)
	}
	if chk, _ := Check(context.Background(), cfg, CheckOptions{ReadData: true}); !chk.OK() || len(chk.Warnings) != 0 {
		t.Fatalf("after the run: %+v", chk)
	}
}

func snapshotsCount(t *testing.T, cfg *config.Config) int {
	t.Helper()
	acct := accountOf(cfg)
	r, _ := repo.Open(cfg.Repository.Path, &acct)
	db, err := index.Open(r.IndexPath(), index.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	s, _ := db.Snapshots()
	return len(s)
}

func TestUIDValidityChangeStartsANewGenerationAndKeepsHistory(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	first := runBackup(t, cfg, f)
	f.Folders[0].Recreate()
	f.Folders[0].Add(msg("7", "After reset unique-subject-7", "body seven"))
	second := runBackup(t, cfg, f)
	if second.Status != "complete" || second.Closed != 2 || second.New != 1 {
		t.Fatalf("second %+v", second)
	}
	if got := snapshotOf(t, cfg, second.Snapshot, "")["INBOX"]; len(got) != 1 {
		t.Fatalf("new generation: %v", got)
	}
	if got := snapshotOf(t, cfg, first.Snapshot, "")["INBOX"]; len(got) != 2 {
		t.Fatalf("history lost: %v", got)
	}
}

func TestCancelledRunWritesNoSnapshot(t *testing.T) {
	cfg := initRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Backup(ctx, cfg, fakeMailbox(), BackupOptions{}); err == nil {
		t.Fatal("expected an error")
	}
	if n := snapshotsCount(t, cfg); n != 0 {
		t.Fatalf("%d snapshots", n)
	}
}

func TestBackupRefusesWhileLocked(t *testing.T) {
	cfg := initRepo(t)
	acct := accountOf(cfg)
	r, _ := repo.Open(cfg.Repository.Path, &acct)
	unlock, err := r.Lock(repo.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Backup(context.Background(), cfg, fakeMailbox(), BackupOptions{}); !errors.Is(err, repo.ErrLocked) {
		t.Fatalf("err %v", err)
	}
}

func TestBackupRefusesWrongAccount(t *testing.T) {
	cfg := initRepo(t)
	cfg.IMAP.Username = "someone-else@example.com"
	if _, err := Backup(context.Background(), cfg, fakeMailbox(), BackupOptions{}); !errors.Is(err, repo.ErrWrongAccount) {
		t.Fatalf("err %v", err)
	}
}

// ---- encrypted repositories -------------------------------------------------

func TestEncryptedBackupNeedsNoPrivateKeyAndStoresNothingReadable(t *testing.T) {
	cfg, keys := initEncrypted(t)
	// An identity source that would fail loudly if backup ever tried to use it.
	marker := filepath.Join(t.TempDir(), "identity-was-read")
	cfg.Encryption.IdentityCommand = "touch " + marker + "; exit 1"
	f := fakeMailbox()
	rep := runBackup(t, cfg, f)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("backup ran the identity command: it must never resolve the private key")
	}

	// the index holds no folder name or subject in the clear.
	acct := accountOf(cfg)
	r, _ := repo.Open(cfg.Repository.Path, &acct)
	db, err := index.Open(r.IndexPath(), index.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT name_sealed FROM folders UNION ALL SELECT meta_sealed FROM blobs`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var b []byte
		_ = rows.Scan(&b)
		n++
		for _, secret := range []string{"INBOX", "Sent", "unique-subject", "example.com"} {
			if strings.Contains(string(b), secret) {
				t.Fatalf("sealed column contains %q in the clear", secret)
			}
		}
	}
	_ = rows.Close()
	if n != 5 {
		t.Fatalf("%d sealed values", n)
	}

	// with the key, the snapshot reads back, and --decrypt verifies every object.
	want := map[string]bool{"INBOX": true, "Sent": true}
	for name := range snapshotOf(t, cfg, rep.Snapshot, keys.identity) {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("missing folders %v", want)
	}
	cfg.Encryption.IdentityCommand = ""
	cfg.Encryption.Identity = keys.identity
	if chk, err := Check(context.Background(), cfg, CheckOptions{Decrypt: true}); err != nil || !chk.OK() || chk.ObjectsDecrypted != 3 {
		t.Fatalf("decrypt check: %+v err %v", chk, err)
	}
}

func TestMetaRoundTripsThroughSeal(t *testing.T) {
	keys := newEncKeys(t)
	rs, _ := seal.ParseRecipients([]string{keys.recipient})
	ids, _ := seal.ParseIdentities(keys.identity)
	raw := "Message-ID: <x@y>\r\nFrom: =?UTF-8?Q?J=C3=BCrgen?= <j@example.com>\r\nSubject: =?UTF-8?B?w6l0w6k=?=\r\n\r\nbody"
	sealed, err := sealMeta(seal.NewSealer(rs), parseMeta([]byte(raw), mailtestTime()))
	if err != nil {
		t.Fatal(err)
	}
	m, err := openMeta(seal.NewOpener(ids), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if m.MessageID != "<x@y>" || m.Subject != "été" || !strings.HasPrefix(m.From, "Jürgen") {
		t.Fatalf("meta %+v", m)
	}
	// garbage headers still give a usable (empty) value, never a failure.
	if m := parseMeta([]byte("not a header block at all"), mailtestTime()); m.Subject != "" {
		t.Fatalf("%+v", m)
	}
}

func TestBackupRefusesDriftedRecipients(t *testing.T) {
	cfg, _ := initEncrypted(t)
	other := newEncKeys(t)
	cases := map[string][]string{
		"different key": {other.recipient},
		"extra key":     append(append([]string{}, cfg.Encryption.Recipients...), other.recipient),
		"none":          nil,
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			c := *cfg
			c.Encryption.Recipients = rec
			_, err := Backup(context.Background(), &c, fakeMailbox(), BackupOptions{})
			if err == nil || !strings.Contains(err.Error(), "recipient") {
				t.Fatalf("err %v", err)
			}
		})
	}
	// order alone is not drift
	c := *cfg
	c.Encryption.Recipients = append([]string{" "}, cfg.Encryption.Recipients...)
	if _, err := Backup(context.Background(), &c, fakeMailbox(), BackupOptions{}); err != nil {
		t.Fatalf("whitespace-only difference refused: %v", err)
	}
}

func TestBackupRefusesRecipientsOnPlainRepository(t *testing.T) {
	cfg := initRepo(t)
	cfg.Encryption.Recipients = []string{newEncKeys(t).recipient}
	_, err := Backup(context.Background(), cfg, fakeMailbox(), BackupOptions{})
	if err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("err %v", err)
	}
}
