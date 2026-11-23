package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/mail/mailtest"
	"github.com/michele/imapvault/internal/objects"
	"github.com/michele/imapvault/internal/repo"
)

func jan(d int) time.Time { return time.Date(2026, 1, d, 12, 0, 0, 0, time.UTC) }

func backupAt(t *testing.T, cfg *config.Config, f *mailtest.Fake, when time.Time) *BackupReport {
	t.Helper()
	rep, err := Backup(context.Background(), cfg, f, BackupOptions{Now: func() time.Time { return when }})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func keyOf(raw string) objects.Key { return objects.Key(sha256.Sum256([]byte(raw))) }

// The messages of the fixture mailbox.
var (
	mInbox1 = msg("i1", "one", "first")
	mInbox2 = msg("i2", "two", "second")
	mInbox3 = msg("i3", "three", "third")
	mInbox4 = msg("i4", "four", "fourth")
	mTrash1 = msg("t1", "trash", "in the trash")
)

// retentionHistory is a plain repository with three snapshots, one per day:
//
//	snapshot 1 (Jan 1): INBOX i1 i2 i3, Trash t1
//	snapshot 2 (Jan 2): i2 deleted, i4 added
//	snapshot 3 (Jan 3): i1 deleted
//
// so i2 is a closed entry covering [1,2) and i1 one covering [1,3). The guard is off.
func retentionHistory(t *testing.T) (*config.Config, *mailtest.Fake) {
	t.Helper()
	cfg := initRepo(t)
	cfg.Guard = config.Guard{}
	cfg.Retention = config.Retention{KeepLast: 1, DeletedGraceDur: 10 * 24 * time.Hour}
	f := mailtest.New()
	inbox := f.AddFolder("INBOX")
	u1 := inbox.Add(mInbox1)
	u2 := inbox.Add(mInbox2)
	inbox.Add(mInbox3)
	f.AddFolder("Trash").Add(mTrash1)
	backupAt(t, cfg, f, jan(1))

	inbox.Delete(u2)
	inbox.BumpOnExpunge()
	inbox.Add(mInbox4)
	backupAt(t, cfg, f, jan(2))

	inbox.Delete(u1)
	inbox.BumpOnExpunge()
	backupAt(t, cfg, f, jan(3))
	return cfg, f
}

func objectKeys(t *testing.T, cfg *config.Config) []objects.Key {
	t.Helper()
	r, err := repo.Open(cfg.Repository.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStore(r, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var keys []objects.Key
	if err := st.Walk(func(k objects.Key) error { keys = append(keys, k); return nil }, nil); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(keys, func(a, b objects.Key) int { return strings.Compare(a.String(), b.String()) })
	return keys
}

func sortedKeys(raws ...string) []objects.Key {
	var keys []objects.Key
	for _, r := range raws {
		keys = append(keys, keyOf(r))
	}
	slices.SortFunc(keys, func(a, b objects.Key) int { return strings.Compare(a.String(), b.String()) })
	return keys
}

func snapshotIDs(t *testing.T, cfg *config.Config, all bool) []int64 {
	t.Helper()
	snaps, err := Snapshots(context.Background(), cfg, all)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	return ids
}

func at20() RetentionOptions {
	return RetentionOptions{Now: func() time.Time { return jan(20) }, Location: time.UTC}
}

func TestForgetMarksWhatThePolicyDoesNotKeep(t *testing.T) {
	cfg, _ := retentionHistory(t)
	rep, err := Forget(context.Background(), cfg, at20())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Forgotten, []int64{1, 2}) || !reflect.DeepEqual(rep.Kept, []int64{3}) {
		t.Errorf("forgotten %v kept %v, want [1 2] and [3]", rep.Forgotten, rep.Kept)
	}
	// Forgetting touches no data.
	if got := objectKeys(t, cfg); len(got) != 5 {
		t.Errorf("%d objects after forget, want all 5", len(got))
	}
	if got := snapshotIDs(t, cfg, false); !reflect.DeepEqual(got, []int64{3}) {
		t.Errorf("snapshots = %v, want only 3 without --all", got)
	}
	if got := snapshotIDs(t, cfg, true); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Errorf("snapshots --all = %v, want 1 2 3", got)
	}
}

func TestForgetDryRunChangesNothing(t *testing.T) {
	cfg, _ := retentionHistory(t)
	opts := at20()
	opts.DryRun = true
	rep, err := Forget(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || !reflect.DeepEqual(rep.Forgotten, []int64{1, 2}) {
		t.Errorf("report %+v", rep)
	}
	if got := snapshotIDs(t, cfg, false); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Errorf("a dry run forgot snapshots: %v", got)
	}
}

func TestForgetKeepsAPinnedSnapshot(t *testing.T) {
	cfg, _ := retentionHistory(t)
	if _, err := Pin(context.Background(), cfg, "1", true); err != nil {
		t.Fatal(err)
	}
	rep, err := Forget(context.Background(), cfg, at20())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Forgotten, []int64{2}) || !reflect.DeepEqual(rep.Kept, []int64{1, 3}) {
		t.Errorf("forgotten %v kept %v, want [2] and [1 3]", rep.Forgotten, rep.Kept)
	}
}

func TestForgetWithAPolicyThatKeepsNothingIsRefused(t *testing.T) {
	cfg, _ := retentionHistory(t)
	cfg.Retention = config.Retention{DeletedGraceDur: time.Hour}
	if _, err := Forget(context.Background(), cfg, at20()); err == nil {
		t.Fatal("an all-zero policy must be refused")
	}
	if got := snapshotIDs(t, cfg, false); len(got) != 3 {
		t.Errorf("snapshots = %v, none may be forgotten", got)
	}
}

// The done criterion of M5: prune frees exactly the blobs nothing needs.
func TestPruneFreesExactlyTheExpectedBlobs(t *testing.T) {
	cfg, _ := retentionHistory(t)
	before3 := snapshotOf(t, cfg, 3, "")
	if _, err := Forget(context.Background(), cfg, at20()); err != nil {
		t.Fatal(err)
	}

	dryOpts := at20()
	dryOpts.DryRun = true
	dry, err := Prune(context.Background(), cfg, dryOpts)
	if err != nil {
		t.Fatal(err)
	}
	if got := objectKeys(t, cfg); len(got) != 5 {
		t.Fatalf("a dry run removed objects: %d left", len(got))
	}

	rep, err := Prune(context.Background(), cfg, at20())
	if err != nil {
		t.Fatal(err)
	}
	// i1 and i2 were deleted on Jan 3 and Jan 2, long before the cutoff (Jan 10), and
	// only forgotten snapshots could still show them.
	want := sortedKeys(mInbox3, mInbox4, mTrash1)
	if got := objectKeys(t, cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("objects left = %v, want %v", got, want)
	}
	if rep.Entries != 2 || rep.Blobs != 2 || rep.Snapshots != 2 || rep.Orphans != 0 {
		t.Errorf("report %+v, want 2 entries, 2 blobs, 2 snapshots", rep)
	}
	if rep.BlobBytes != int64(len(mInbox1)+len(mInbox2)) {
		t.Errorf("BlobBytes = %d, want %d", rep.BlobBytes, len(mInbox1)+len(mInbox2))
	}
	if rep.DiskBytes <= 0 {
		t.Errorf("DiskBytes = %d, want the size of the two object files", rep.DiskBytes)
	}
	// The dry run said the same, apart from itself being a dry run.
	dry.DryRun, dry.IndexBackup, rep.IndexBackup = false, "", ""
	if !reflect.DeepEqual(dry, rep) {
		t.Errorf("dry run %+v differs from the real run %+v", dry, rep)
	}
	if got := snapshotIDs(t, cfg, true); !reflect.DeepEqual(got, []int64{3}) {
		t.Errorf("snapshots --all = %v, want only 3", got)
	}
	// What is kept is untouched, and the repository is consistent.
	if after3 := snapshotOf(t, cfg, 3, ""); !reflect.DeepEqual(before3, after3) {
		t.Errorf("snapshot 3 changed: %v -> %v", before3, after3)
	}
	chk, err := Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if err != nil {
		t.Fatal(err)
	}
	if !chk.OK() || len(chk.Warnings) != 0 {
		t.Errorf("check after prune: errors %v warnings %v", chk.Errors, chk.Warnings)
	}
}

func TestPruneHonoursTheGracePeriod(t *testing.T) {
	cfg, _ := retentionHistory(t)
	if _, err := Forget(context.Background(), cfg, at20()); err != nil {
		t.Fatal(err)
	}
	// Jan 5: the cutoff is Dec 26, so everything that was deleted is still held.
	opts := RetentionOptions{Now: func() time.Time { return jan(5) }, Location: time.UTC}
	rep, err := Prune(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries != 0 || rep.Blobs != 0 || rep.Snapshots != 0 {
		t.Errorf("report %+v, want nothing removed inside the grace period", rep)
	}
	if got := objectKeys(t, cfg); len(got) != 5 {
		t.Errorf("%d objects, want all 5", len(got))
	}
	// The forgotten snapshots are still complete, so they are still there.
	if got := snapshotIDs(t, cfg, true); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Errorf("snapshots --all = %v, want 1 2 3", got)
	}
	// And on Jan 12 (cutoff Jan 2 12:00): i2 was closed on Jan 2 12:00, exactly the
	// cutoff, so it is kept; i1 (Jan 3) too.
	opts.Now = func() time.Time { return jan(12) }
	rep, err = Prune(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries != 0 {
		t.Errorf("report %+v: an entry closed exactly at the cutoff must be kept", rep)
	}
	// Jan 13 (cutoff Jan 3 12:00): i2 goes (closed Jan 2), i1 (closed Jan 3 12:00) is exactly at the cutoff.
	opts.Now = func() time.Time { return jan(13) }
	rep, err = Prune(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries != 1 || rep.Blobs != 1 {
		t.Errorf("report %+v, want just i2's entry and blob", rep)
	}
	if got := objectKeys(t, cfg); !reflect.DeepEqual(got, sortedKeys(mInbox1, mInbox3, mInbox4, mTrash1)) {
		t.Errorf("objects left = %v", got)
	}
}

func TestPruneKeepsWhatAPinnedSnapshotShows(t *testing.T) {
	cfg, _ := retentionHistory(t)
	if _, err := Pin(context.Background(), cfg, "1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := Forget(context.Background(), cfg, at20()); err != nil {
		t.Fatal(err)
	}
	rep, err := Prune(context.Background(), cfg, at20())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries != 0 || rep.Blobs != 0 {
		t.Errorf("report %+v: snapshot 1 is pinned and shows i1 and i2", rep)
	}
	if got := objectKeys(t, cfg); len(got) != 5 {
		t.Errorf("%d objects, want all 5", len(got))
	}
}

func TestPruneIsRefusedWhileAnAlertIsUnacknowledged(t *testing.T) {
	cfg := guardConfig(t)
	cfg.Retention = config.Retention{KeepLast: 1, DeletedGraceDur: time.Hour}
	f := bigMailbox(20)
	backupAt(t, cfg, f, jan(1))
	deleteFirst(f.Folders[0], 15)
	rep := backupAt(t, cfg, f, jan(2))
	if !rep.Alerted() {
		t.Fatal("setup: the guard should have tripped")
	}
	opts := at20()
	for _, dry := range []bool{false, true} {
		opts.DryRun = dry
		_, err := Prune(context.Background(), cfg, opts)
		if err == nil || !strings.Contains(err.Error(), "unacknowledged") {
			t.Errorf("dry=%v: err = %v, want a refusal that names the unacknowledged alert", dry, err)
		}
	}
	before := len(objectKeys(t, cfg))
	// forget only marks, so it still works; the pinned snapshot is protected.
	if _, err := Forget(context.Background(), cfg, at20()); err != nil {
		t.Fatalf("forget with an alert: %v", err)
	}
	if got := snapshotIDs(t, cfg, false); !slices.Contains(got, rep.Alert.PinnedSnapshot) {
		t.Errorf("the pinned snapshot %d left the visible list: %v", rep.Alert.PinnedSnapshot, got)
	}
	if _, err := Ack(context.Background(), cfg, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(context.Background(), cfg, at20()); err != nil {
		t.Fatalf("prune after ack: %v", err)
	}
	if got := len(objectKeys(t, cfg)); got != before {
		t.Errorf("%d objects after prune, want %d: the pinned snapshot still shows everything", got, before)
	}
}

func TestPruneRemovesOrphanObjectsAndTmpLeftovers(t *testing.T) {
	cfg, _ := retentionHistory(t)
	r, err := repo.Open(cfg.Repository.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStore(r, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.Put([]byte("a blob a crashed run left behind")); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	leftover := filepath.Join(r.TmpDir(), "obj-123")
	if err := os.WriteFile(leftover, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := Prune(context.Background(), cfg, at20()) // nothing is forgotten yet
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orphans != 1 || rep.TmpFiles != 1 || rep.Entries != 0 {
		t.Errorf("report %+v, want 1 orphan and 1 tmp file", rep)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp leftover still there: %v", err)
	}
	chk, err := Check(context.Background(), cfg, CheckOptions{})
	if err != nil || !chk.OK() || len(chk.Warnings) != 0 {
		t.Errorf("check after prune: %+v err %v", chk, err)
	}
}

func TestPruneDryRunTouchesNeitherFilesNorIndex(t *testing.T) {
	cfg, _ := retentionHistory(t)
	if _, err := Forget(context.Background(), cfg, at20()); err != nil {
		t.Fatal(err)
	}
	r, _ := repo.Open(cfg.Repository.Path, nil)
	leftover := filepath.Join(r.TmpDir(), "obj-1")
	if err := os.WriteFile(leftover, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	backups := func() []string {
		ents, _ := os.ReadDir(r.IndexBackupsDir())
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}
	beforeBackups := backups()
	opts := at20()
	opts.DryRun = true
	rep, err := Prune(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || rep.Entries != 2 || rep.TmpFiles != 1 {
		t.Errorf("report %+v", rep)
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Errorf("a dry run removed a tmp file: %v", err)
	}
	if !reflect.DeepEqual(backups(), beforeBackups) {
		t.Errorf("a dry run wrote an index backup: %v -> %v", beforeBackups, backups())
	}
	if got := snapshotIDs(t, cfg, true); len(got) != 3 {
		t.Errorf("a dry run removed snapshots: %v", got)
	}
}

// An encrypted repository is pruned by a machine that holds only the public key.
func TestPruneAndForgetNeverResolveAnIdentity(t *testing.T) {
	cfg, _ := initEncrypted(t)
	cfg.Guard = config.Guard{}
	cfg.Retention = config.Retention{KeepLast: 1, DeletedGraceDur: 10 * 24 * time.Hour}
	// If forget or prune tried to resolve the identity this would fail.
	cfg.Encryption.IdentityFile = filepath.Join(t.TempDir(), "no-such-key")

	f := mailtest.New()
	inbox := f.AddFolder("INBOX")
	u := inbox.Add(mInbox1)
	inbox.Add(mInbox2)
	backupAt(t, cfg, f, jan(1))
	inbox.Delete(u)
	inbox.BumpOnExpunge()
	backupAt(t, cfg, f, jan(2))

	if _, err := Forget(context.Background(), cfg, at20()); err != nil {
		t.Fatalf("forget: %v", err)
	}
	rep, err := Prune(context.Background(), cfg, at20())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rep.Entries != 1 || rep.Blobs != 1 {
		t.Errorf("report %+v", rep)
	}
	if got := objectKeys(t, cfg); len(got) != 1 {
		t.Errorf("%d objects left, want 1", len(got))
	}
}

func TestForgottenSnapshotsStayAddressableUntilPruned(t *testing.T) {
	cfg, _ := retentionHistory(t)
	if _, err := Forget(context.Background(), cfg, at20()); err != nil {
		t.Fatal(err)
	}
	// 2 is forgotten but nothing was pruned: ls by number still works.
	res, err := Ls(context.Background(), cfg, "2", "")
	if err != nil {
		t.Fatalf("ls of a forgotten snapshot: %v", err)
	}
	if len(res.Folders) != 2 {
		t.Errorf("folders = %+v", res.Folders)
	}
	// latest~1 counts visible snapshots only: with 1 and 2 forgotten there is no latest~1.
	if _, err := Ls(context.Background(), cfg, "latest~1", ""); err == nil {
		t.Error("latest~1 must not resolve to a forgotten snapshot")
	}
	if _, err := Ls(context.Background(), cfg, "latest", ""); err != nil {
		t.Errorf("latest: %v", err)
	}
}

func TestIndexBackupAfterEveryRunKeepsTheNewestSeven(t *testing.T) {
	cfg := initRepo(t)
	cfg.Guard = config.Guard{}
	f := mailtest.New()
	inbox := f.AddFolder("INBOX")
	inbox.Add(mInbox1)
	r, _ := repo.Open(cfg.Repository.Path, nil)
	for d := 1; d <= 9; d++ {
		inbox.Add(msg("n"+string(rune('a'+d)), "s", "b"))
		rep := backupAt(t, cfg, f, jan(d))
		if len(rep.Warnings) != 0 {
			t.Fatalf("run %d warnings: %v", d, rep.Warnings)
		}
	}
	ents, err := os.ReadDir(r.IndexBackupsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 7 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("%d index backups %v, want 7", len(ents), names)
	}
	// The oldest kept is the one from run 3; each is a readable index.
	newest := ents[len(ents)-1].Name()
	if !strings.Contains(ents[0].Name(), "20260103") || !strings.Contains(newest, "20260109") {
		t.Errorf("kept %s … %s, want Jan 3 … Jan 9", ents[0].Name(), newest)
	}
	db, err := index.Open(filepath.Join(r.IndexBackupsDir(), newest), index.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("the newest index backup does not open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if snaps, err := db.Snapshots(); err != nil || len(snaps) != 9 {
		t.Errorf("the newest index backup has %d snapshots (err %v), want 9", len(snaps), err)
	}
	// Nothing is left in tmp/.
	if left, _ := os.ReadDir(r.TmpDir()); len(left) != 0 {
		t.Errorf("tmp/ holds %d leftovers", len(left))
	}
}

func TestIndexBackupFailureIsAWarningNotAFailedRun(t *testing.T) {
	cfg := initRepo(t)
	cfg.Guard = config.Guard{}
	f := mailtest.New()
	f.AddFolder("INBOX").Add(mInbox1)
	r, _ := repo.Open(cfg.Repository.Path, nil)
	// A file where the directory should be: the backup cannot be written.
	if err := os.RemoveAll(r.IndexBackupsDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.IndexBackupsDir(), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := Backup(context.Background(), cfg, f, BackupOptions{Now: func() time.Time { return jan(1) }})
	if err != nil {
		t.Fatalf("the run failed: %v", err)
	}
	if rep.Status != index.RunComplete || rep.Snapshot != 1 {
		t.Errorf("report %+v", rep)
	}
	found := false
	for _, w := range rep.Warnings {
		if strings.Contains(w, "index backup") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings %v, want one about the index backup", rep.Warnings)
	}
}

// A prune that removed nothing leaves the index as the last backup copy already
// has it: writing another copy would only push an older, useful one out of the
// newest seven.
func TestPruneThatRemovesNothingWritesNoIndexBackup(t *testing.T) {
	cfg, _ := retentionHistory(t)
	r, _ := repo.Open(cfg.Repository.Path, nil)
	count := func() int {
		ents, _ := os.ReadDir(r.IndexBackupsDir())
		return len(ents)
	}
	before := count()
	rep, err := Prune(context.Background(), cfg, at20()) // nothing forgotten: nothing to remove
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries != 0 || rep.IndexBackup != "" || count() != before {
		t.Errorf("report %+v, %d index backups (was %d): a prune that removed nothing must not write one", rep, count(), before)
	}

	// One that removed something does.
	if _, err := Forget(context.Background(), cfg, at20()); err != nil {
		t.Fatal(err)
	}
	rep, err = Prune(context.Background(), cfg, at20())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries == 0 || rep.IndexBackup == "" {
		t.Errorf("report %+v: a prune that removed entries must write an index backup", rep)
	}
}
