package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/mail/mailtest"
	"github.com/michele/imapvault/internal/repo"
)

type hookCall struct{ argv, env []string }

// stubHook replaces the on_alert exec for one test.
func stubHook(t *testing.T, result error) *[]hookCall {
	t.Helper()
	var calls []hookCall
	old := runHook
	runHook = func(_ context.Context, argv, env []string) error {
		calls = append(calls, hookCall{argv, env})
		return result
	}
	t.Cleanup(func() { runHook = old })
	return &calls
}

func guardPolicy(cfg *config.Config) *config.Config {
	cfg.Guard = config.Guard{MaxDeletedPercent: 10, MaxDeletedCount: 500}
	return cfg
}

func guardConfig(t *testing.T) *config.Config { return guardPolicy(initRepo(t)) }

// bigMailbox has n messages in INBOX and 5 in Trash.
func bigMailbox(n int) *mailtest.Fake {
	f := mailtest.New()
	inbox := f.AddFolder("INBOX")
	for i := range n {
		inbox.Add(msg(fmt.Sprintf("i%d", i), fmt.Sprintf("Inbox %d", i), fmt.Sprintf("body %d", i)))
	}
	trash := f.AddFolder("Trash")
	for i := range 5 {
		trash.Add(msg(fmt.Sprintf("t%d", i), fmt.Sprintf("Trash %d", i), fmt.Sprintf("trashed %d", i)))
	}
	return f
}

func deleteFirst(fo *mailtest.Folder, n int) {
	uids := []uint32{}
	for _, m := range fo.Messages[:n] {
		uids = append(uids, m.UID)
	}
	for _, u := range uids {
		fo.Delete(u)
	}
	fo.BumpOnExpunge()
}

func snapshotRows(t *testing.T, cfg *config.Config) map[int64]index.Snapshot {
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
	snaps, err := db.Snapshots()
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]index.Snapshot{}
	for _, s := range snaps {
		out[s.ID] = s
	}
	return out
}

func TestMassDeletionFlagsPinsAndAlerts(t *testing.T) {
	calls := stubHook(t, nil)
	cfg := guardConfig(t)
	cfg.Account = "personal"
	cfg.Guard.OnAlert = `notify 'imapvault alert: {account} {summary}'`
	f := bigMailbox(20)
	first := runBackup(t, cfg, f)
	if first.Alerted() || first.Flagged || len(*calls) != 0 {
		t.Fatalf("the first run has nothing to compare with: %+v", first.Alert)
	}

	deleteFirst(f.Folders[0], 15) // 15 of 25 messages
	second := runBackup(t, cfg, f)
	if second.Status != "complete" || !second.Flagged || !second.Alerted() {
		t.Fatalf("report %+v", second)
	}
	a := second.Alert
	if a.Kind != "mass-deletion" || a.PinnedSnapshot != first.Snapshot || a.ID == 0 {
		t.Fatalf("alert %+v", a)
	}
	for _, want := range []string{"15 of 25 messages deleted (60.0%)", "since snapshot 1", "INBOX 15"} {
		if !strings.Contains(a.Summary, want) {
			t.Errorf("summary %q lacks %q", a.Summary, want)
		}
	}

	rows := snapshotRows(t, cfg)
	if !rows[first.Snapshot].Pinned || rows[first.Snapshot].Flagged {
		t.Errorf("previous snapshot: %+v", rows[first.Snapshot])
	}
	if !rows[second.Snapshot].Flagged || rows[second.Snapshot].Pinned {
		t.Errorf("new snapshot: %+v", rows[second.Snapshot])
	}

	if len(*calls) != 1 {
		t.Fatalf("hook called %d times", len(*calls))
	}
	c := (*calls)[0]
	if len(c.argv) != 2 || c.argv[0] != "notify" || c.argv[1] != "imapvault alert: personal "+a.Summary {
		t.Errorf("argv = %q", c.argv)
	}
	env := strings.Join(c.env, "\n")
	for _, want := range []string{"IMAPVAULT_ACCOUNT=personal", "IMAPVAULT_SUMMARY=" + a.Summary, fmt.Sprintf("IMAPVAULT_ALERT_ID=%d", a.ID), fmt.Sprintf("IMAPVAULT_SNAPSHOT=%d", second.Snapshot)} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q", want)
		}
	}

	// The alert is stored and readable.
	res, err := Alerts(context.Background(), cfg, false)
	if err != nil || len(res.Alerts) != 1 {
		t.Fatalf("alerts %+v err %v", res, err)
	}
	if got := res.Alerts[0]; got.ID != a.ID || got.Snapshot != second.Snapshot || got.PinnedSnapshot != first.Snapshot || got.Summary != a.Summary || got.AcknowledgedAt != nil {
		t.Errorf("alert info %+v", got)
	}

	// The loss is already on record: the next run compares with the flagged
	// snapshot and says nothing more.
	third := runBackup(t, cfg, f)
	if third.Alerted() || third.Flagged || len(*calls) != 1 {
		t.Fatalf("the same loss alerted twice: %+v", third.Alert)
	}
}

func TestSmallChangesDoNotAlert(t *testing.T) {
	calls := stubHook(t, nil)
	cfg := guardConfig(t)
	cfg.Guard.OnAlert = "notify"
	f := bigMailbox(20)
	runBackup(t, cfg, f)
	deleteFirst(f.Folders[0], 2) // 2 of 25: 8%
	f.Folders[0].SetFlags(f.Folders[0].Messages[0].UID, `\Seen`)
	rep := runBackup(t, cfg, f)
	if rep.Alerted() || rep.Flagged || len(*calls) != 0 {
		t.Fatalf("alert %+v", rep.Alert)
	}
}

func TestEmptyingTrashAloneTripsOnPercent(t *testing.T) {
	cfg := guardConfig(t)
	cfg.Guard.MaxDeletedPercent = 15
	f := bigMailbox(20)
	runBackup(t, cfg, f)
	deleteFirst(f.Folders[1], 5) // Trash: 5 of 25 = 20%
	rep := runBackup(t, cfg, f)
	if !rep.Alerted() || !strings.Contains(rep.Alert.Summary, "Trash 5") {
		t.Fatalf("alert %+v", rep.Alert)
	}
}

func TestRenameAndMoveDoNotAlert(t *testing.T) {
	cfg := guardConfig(t)
	f := bigMailbox(20)
	runBackup(t, cfg, f)
	f.RenameFolder("Trash", "Bin")
	rep := runBackup(t, cfg, f)
	if rep.Alerted() || rep.GoneFolders != 1 || rep.NewFolders != 1 {
		t.Fatalf("a rename must be quiet: gone %d new %d alert %+v", rep.GoneFolders, rep.NewFolders, rep.Alert)
	}
}

func TestVanishedFolderWithLossAlertsBelowTheThresholds(t *testing.T) {
	calls := stubHook(t, nil)
	cfg := guardConfig(t)
	cfg.Guard.OnAlert = "notify {summary}"
	f := bigMailbox(30)
	runBackup(t, cfg, f)
	f.RemoveFolder("Trash") // 5 of 35 = 14%: lower the percent so only the rule trips
	cfg.Guard.MaxDeletedPercent = 50
	rep := runBackup(t, cfg, f)
	if !rep.Alerted() || rep.Alert.Kind != "folder-vanished" || rep.GoneFolders != 1 {
		t.Fatalf("report %+v alert %+v", rep, rep.Alert)
	}
	if !strings.Contains(rep.Alert.Summary, "folder #2 vanished (5 messages deleted)") {
		t.Errorf("summary %q", rep.Alert.Summary)
	}
	if len(*calls) != 1 {
		t.Errorf("hook calls %d", len(*calls))
	}
}

func TestVanishedFolderRuleStaysOnWithThresholdsOff(t *testing.T) {
	cfg := initRepo(t) // zero guard: both thresholds off
	f := bigMailbox(10)
	runBackup(t, cfg, f)
	f.RemoveFolder("Trash")
	if rep := runBackup(t, cfg, f); !rep.Alerted() || rep.Alert.Kind != "folder-vanished" {
		t.Fatalf("alert %+v", rep.Alert)
	}

	cfg2 := initRepo(t)
	f2 := bigMailbox(10)
	runBackup(t, cfg2, f2)
	deleteFirst(f2.Folders[0], 10)
	if rep := runBackup(t, cfg2, f2); rep.Alerted() {
		t.Fatalf("with both thresholds off a plain deletion must not alert: %+v", rep.Alert)
	}
}

func TestPartialRunStillGuardsWhatItCommitted(t *testing.T) {
	cfg := guardConfig(t)
	f := fakeMailbox()
	arch := f.AddFolder("Archive")
	for i := range 20 {
		arch.Add(msg(fmt.Sprintf("a%d", i), fmt.Sprintf("Archived %d", i), fmt.Sprintf("old %d", i)))
	}
	first := runBackup(t, cfg, f)

	deleteFirst(f.Folders[2], 20)                                   // Archive, backed up first: all gone
	f.Folders[0].Add(msg("n1", "Inbox new unique-subject-n1", "x")) // INBOX: fails
	f.FailAt = func(call string) error {
		if strings.HasPrefix(call, "fetch-bodies INBOX") {
			return errors.New("connection reset")
		}
		return nil
	}
	rep, err := Backup(context.Background(), cfg, f, BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Partial() || !rep.Alerted() || rep.Alert.PinnedSnapshot != first.Snapshot {
		t.Fatalf("a partial run that lost mail must alert: %+v / %+v", rep, rep.Alert)
	}
	if !strings.Contains(rep.Alert.Summary, "Archive 20") {
		t.Errorf("summary %q", rep.Alert.Summary)
	}
}

// A run killed after it committed a folder's deletions leaves them in the index
// without a snapshot. The next run still has to count them.
func TestDeletionsCommittedByADeadRunAreStillCounted(t *testing.T) {
	cfg := guardConfig(t)
	f := bigMailbox(20)
	first := runBackup(t, cfg, f)

	acct := accountOf(cfg)
	r, err := repo.Open(cfg.Repository.Path, &acct)
	if err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(r.IndexPath(), index.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dead, err := db.BeginRun(mailtestTime(), "")
	if err != nil {
		t.Fatal(err)
	}
	live, _ := db.LiveFolders()
	var inbox index.Folder
	for _, lf := range live {
		if lf.ID == 1 {
			inbox = lf
		}
	}
	open, _ := db.OpenEntries(inbox.ID)
	var closeIDs []int64
	for _, e := range open[:15] {
		closeIDs = append(closeIDs, e.ID)
	}
	if err := db.ApplyFolder(dead, inbox.ID, index.FolderChanges{UIDValidity: inbox.UIDValidity, LastUID: inbox.LastUID, HighestModSeq: inbox.HighestModSeq, Close: closeIDs}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close() // ... and the process dies here: the run stays "running"

	deleteFirst(f.Folders[0], 15)
	rep := runBackup(t, cfg, f)
	if !rep.Alerted() || rep.Alert.PinnedSnapshot != first.Snapshot || !strings.Contains(rep.Alert.Summary, "15 of 25") {
		t.Fatalf("alert %+v", rep.Alert)
	}
}

func TestEncryptedBackupAlertsWithoutAnyKeyAndSealsTheSummary(t *testing.T) {
	cfg, keys := initEncrypted(t)
	guardPolicy(cfg)
	f := bigMailbox(20)
	runBackup(t, cfg, f)
	deleteFirst(f.Folders[0], 15)
	rep := runBackup(t, cfg, f) // no identity configured
	if !rep.Alerted() {
		t.Fatalf("no alert: %+v", rep)
	}

	// The index holds the summary sealed.
	acct := accountOf(cfg)
	r, _ := repo.Open(cfg.Repository.Path, &acct)
	db, err := index.Open(r.IndexPath(), index.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := db.Alerts(true)
	_ = db.Close()
	if len(rows) != 1 || strings.Contains(string(rows[0].SummarySealed), "INBOX") || strings.Contains(string(rows[0].SummarySealed), "deleted") {
		t.Fatalf("the summary is stored readable: %q", rows[0].SummarySealed)
	}

	// Without the key: the alert is listed, its summary is not.
	res, err := Alerts(context.Background(), cfg, false)
	if err != nil || len(res.Alerts) != 1 || res.Alerts[0].Summary != "" || len(res.Warnings) == 0 {
		t.Fatalf("no key: %+v err %v", res, err)
	}
	// With it, the summary comes back.
	cfg.Encryption.Identity = keys.identity
	res, err = Alerts(context.Background(), cfg, false)
	if err != nil || res.Alerts[0].Summary != rep.Alert.Summary {
		t.Fatalf("with key: %+v err %v", res, err)
	}
}

func TestFailingHookIsAWarningNotAFailure(t *testing.T) {
	stubHook(t, errors.New("notify: exit status 7"))
	cfg := guardConfig(t)
	cfg.Guard.OnAlert = "notify"
	f := bigMailbox(20)
	runBackup(t, cfg, f)
	deleteFirst(f.Folders[0], 15)
	rep, err := Backup(context.Background(), cfg, f, BackupOptions{})
	if err != nil {
		t.Fatalf("a broken hook must not fail the run: %v", err)
	}
	if !rep.Alerted() || rep.Alert.HookError == "" || !strings.Contains(strings.Join(rep.Warnings, "\n"), "guard.on_alert failed") {
		t.Fatalf("report %+v warnings %v", rep.Alert, rep.Warnings)
	}
	if rows := snapshotRows(t, cfg); !rows[rep.Snapshot].Flagged {
		t.Error("the alert must be recorded even when the hook fails")
	}
}

func TestBrokenOnAlertRefusesToStartTheRun(t *testing.T) {
	cfg := guardConfig(t)
	cfg.Guard.OnAlert = `notify 'oops`
	if _, err := Backup(context.Background(), cfg, bigMailbox(3), BackupOptions{}); err == nil || !strings.Contains(err.Error(), "guard.on_alert") {
		t.Fatalf("err = %v", err)
	}
	if rows := snapshotRows(t, cfg); len(rows) != 0 {
		t.Fatalf("a refused run wrote %v", rows)
	}
}

// Folder names come from the server. A name built to look like shell syntax must
// reach the hook as plain data: nothing is executed.
func TestHostileFolderNameIsNeverExecuted(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	out := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+out+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := guardConfig(t)
	cfg.Guard.OnAlert = "'" + script + "' 'alert {account}: {summary}'"
	cfg.Account = "personal"
	f := mailtest.New()
	evil := f.AddFolder("$(touch PWNED); `touch PWNED2`\nline2")
	for i := range 10 {
		evil.Add(msg(fmt.Sprintf("e%d", i), "s", "b"+fmt.Sprint(i)))
	}
	runBackup(t, cfg, f)
	deleteFirst(evil, 10)
	rep := runBackup(t, cfg, f)
	if !rep.Alerted() || rep.Alert.HookError != "" {
		t.Fatalf("alert %+v", rep.Alert)
	}
	for _, n := range []string{"PWNED", "PWNED2"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Fatalf("%s was created: the folder name was executed", n)
		}
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "$(touch PWNED); `touch PWNED2` line2 10") || strings.Count(string(got), "\n") != 1 {
		t.Fatalf("the hook got %q", got)
	}
}

func TestAckAndPin(t *testing.T) {
	stubHook(t, nil)
	cfg := guardConfig(t)
	f := bigMailbox(20)
	first := runBackup(t, cfg, f)
	deleteFirst(f.Folders[0], 15)
	second := runBackup(t, cfg, f)
	ctx := context.Background()

	if _, err := Pin(ctx, cfg, "1", false); err == nil || !strings.Contains(err.Error(), "ack") {
		t.Fatalf("unpinning the alert's snapshot must be refused until it is acknowledged: %v", err)
	}
	if _, err := Ack(ctx, cfg, nil, false); err == nil {
		t.Error("ack with nothing to acknowledge must be an error")
	}
	if _, err := Ack(ctx, cfg, []int64{999}, false); err == nil {
		t.Error("an unknown alert id must be an error")
	}
	if got, err := Ack(ctx, cfg, []int64{second.Alert.ID}, false); err != nil || len(got) != 1 {
		t.Fatalf("ack = %v, %v", got, err)
	}
	if got, err := Ack(ctx, cfg, nil, true); err != nil || len(got) != 0 {
		t.Fatalf("ack --all with nothing open = %v, %v", got, err)
	}
	if res, _ := Alerts(ctx, cfg, false); len(res.Alerts) != 0 {
		t.Fatalf("still open: %+v", res.Alerts)
	}
	if res, _ := Alerts(ctx, cfg, true); len(res.Alerts) != 1 || res.Alerts[0].AcknowledgedAt == nil {
		t.Fatalf("--all: %+v", res.Alerts)
	}

	if _, err := Pin(ctx, cfg, "1", false); err != nil {
		t.Fatalf("unpin after ack: %v", err)
	}
	if rows := snapshotRows(t, cfg); rows[first.Snapshot].Pinned {
		t.Error("still pinned")
	}
	if res, err := Pin(ctx, cfg, "latest", true); err != nil || res.Snapshot != second.Snapshot || !res.Pinned {
		t.Fatalf("pin latest = %+v, %v", res, err)
	}
	if res, err := Pin(ctx, cfg, "latest~1", true); err != nil || res.Snapshot != first.Snapshot {
		t.Fatalf("pin latest~1 = %+v, %v", res, err)
	}
	if _, err := Pin(ctx, cfg, "42", true); err == nil {
		t.Error("pinning a missing snapshot must fail")
	}
}

func TestAckAllAcknowledgesEverythingOpen(t *testing.T) {
	stubHook(t, nil)
	cfg := guardConfig(t)
	f := bigMailbox(40)
	runBackup(t, cfg, f)
	deleteFirst(f.Folders[0], 20)
	runBackup(t, cfg, f)
	deleteFirst(f.Folders[0], 10)
	runBackup(t, cfg, f)
	if res, _ := Alerts(context.Background(), cfg, false); len(res.Alerts) != 2 {
		t.Fatalf("alerts %+v", res.Alerts)
	}
	got, err := Ack(context.Background(), cfg, nil, true)
	if err != nil || len(got) != 2 {
		t.Fatalf("ack --all = %v, %v", got, err)
	}
}

func TestAckAndPinRefuseWhileABackupRuns(t *testing.T) {
	cfg := guardConfig(t)
	runBackup(t, cfg, bigMailbox(3))
	acct := accountOf(cfg)
	r, _ := repo.Open(cfg.Repository.Path, &acct)
	unlock, err := r.Lock(repo.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Pin(context.Background(), cfg, "latest", true); !errors.Is(err, repo.ErrLocked) {
		t.Fatalf("err = %v", err)
	}
}
