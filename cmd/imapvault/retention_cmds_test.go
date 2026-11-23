package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/engine"
	"github.com/michele/imapvault/internal/mail/mailtest"
)

// threeSnapshots sets up an account (keep_last = 1, a one hour grace period) with
// three backups, the second after one message was expunged. extra is more config.
func threeSnapshots(t *testing.T, extra string) {
	t.Helper()
	setupAccountWith(t, "\n[retention]\nkeep_last = 1\ndeleted_grace = \"1h\"\n"+extra)
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
	for i := range 3 {
		inbox.Add(fmt.Sprintf("Message-ID: <m%d@example.com>\r\nSubject: s%d\r\n\r\nbody %d\r\n", i, i, i))
	}
	run := func() {
		t.Helper()
		if _, err := engine.Backup(context.Background(), cfg, f, engine.BackupOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	inbox.Delete(inbox.Messages[0].UID)
	inbox.BumpOnExpunge()
	run()
	run()
}

func TestForgetDryRunThenForget(t *testing.T) {
	threeSnapshots(t, "[guard]\nmax_deleted_percent = 0\nmax_deleted_count = 0\n")

	out, err := execRoot(t, "--account", "test", "forget", "--dry-run")
	if err != nil {
		t.Fatalf("forget --dry-run: %v\n%s", err, out)
	}
	for _, want := range []string{"Dry run", "would forget 2 snapshots", "1, 2", "latest"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out)
		}
	}
	if out, _ = execRoot(t, "--account", "test", "snapshots"); !strings.Contains(out, "\n1 ") || strings.Contains(out, "forgotten") {
		t.Errorf("a dry run changed the list:\n%s", out)
	}

	out, err = execRoot(t, "--account", "test", "forget")
	if err != nil || !strings.Contains(out, "Forgot 2 snapshots") || !strings.Contains(out, "prune") {
		t.Fatalf("forget: %v\n%s", err, out)
	}
	out, err = execRoot(t, "--account", "test", "snapshots")
	if err != nil || strings.Contains(out, "\n1 ") || strings.Contains(out, "\n2 ") || !strings.Contains(out, "\n3 ") {
		t.Fatalf("snapshots after forget: %v\n%s", err, out)
	}
	out, err = execRoot(t, "--account", "test", "snapshots", "--all")
	if err != nil || !strings.Contains(out, "forgotten") || !strings.Contains(out, "\n1 ") {
		t.Fatalf("snapshots --all: %v\n%s", err, out)
	}
	js, err := execRoot(t, "--account", "test", "--json", "snapshots", "--all")
	if err != nil {
		t.Fatal(err)
	}
	var snaps []engine.SnapshotInfo
	if err := json.Unmarshal([]byte(js), &snaps); err != nil || len(snaps) != 3 || !snaps[0].Forgotten || snaps[2].Forgotten {
		t.Fatalf("snapshots --all --json: %v\n%s", err, js)
	}

	out, err = execRoot(t, "--account", "test", "forget")
	if err != nil || !strings.Contains(out, "Nothing to forget") {
		t.Fatalf("a second forget: %v\n%s", err, out)
	}
}

func TestForgetJSON(t *testing.T) {
	threeSnapshots(t, "[guard]\nmax_deleted_percent = 0\nmax_deleted_count = 0\n")
	js, err := execRoot(t, "--account", "test", "--json", "forget", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	var rep engine.ForgetReport
	if err := json.Unmarshal([]byte(js), &rep); err != nil || !rep.DryRun || len(rep.Forgotten) != 2 || len(rep.Kept) != 1 || len(rep.Verdicts) != 3 {
		t.Fatalf("forget --json: %v\n%s", err, js)
	}
}

func TestPruneDryRunAndRealRun(t *testing.T) {
	threeSnapshots(t, "[guard]\nmax_deleted_percent = 0\nmax_deleted_count = 0\n")
	if out, err := execRoot(t, "--account", "test", "forget"); err != nil {
		t.Fatalf("forget: %v\n%s", err, out)
	}

	out, err := execRoot(t, "--account", "test", "prune", "--dry-run")
	if err != nil {
		t.Fatalf("prune --dry-run: %v\n%s", err, out)
	}
	for _, want := range []string{"Dry run", "Would remove", "Index entries", "Messages", "Forgotten snapshots", "grace"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out)
		}
	}
	// The deletion happened a moment ago, well inside the one hour grace period.
	out, err = execRoot(t, "--account", "test", "prune")
	if err != nil || !strings.Contains(out, "Removed") || strings.Contains(out, "Dry run") {
		t.Fatalf("prune: %v\n%s", err, out)
	}
	js, err := execRoot(t, "--account", "test", "--json", "prune", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	var rep engine.PruneReport
	if err := json.Unmarshal([]byte(js), &rep); err != nil || !rep.DryRun || rep.Cutoff.IsZero() {
		t.Fatalf("prune --json: %v\n%s", err, js)
	}
	if out, err := execRoot(t, "--account", "test", "check", "--read-data"); err != nil {
		t.Fatalf("check after prune: %v\n%s", err, out)
	}
}

func TestPruneIsRefusedWhileAnAlertIsOpen(t *testing.T) {
	alerted(t, "\n[retention]\nkeep_last = 1\ndeleted_grace = \"1h\"\n")
	for _, args := range [][]string{{"prune"}, {"prune", "--dry-run"}} {
		_, err := execRoot(t, append([]string{"--account", "test"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "unacknowledged") {
			t.Errorf("%v: err = %v, want a refusal about the unacknowledged alert", args, err)
		}
	}
	// forget only marks, and the pinned snapshot is protected.
	if out, err := execRoot(t, "--account", "test", "forget"); err != nil {
		t.Fatalf("forget with an open alert: %v\n%s", err, out)
	}
	out, err := execRoot(t, "--account", "test", "snapshots")
	if err != nil || !strings.Contains(out, "pinned") {
		t.Fatalf("the pinned snapshot must stay listed: %v\n%s", err, out)
	}
}

func TestForgetWithAnEmptyPolicyIsRefused(t *testing.T) {
	setupAccountWith(t, "\n[retention]\nkeep_last = 0\nkeep_hourly = 0\nkeep_daily = 0\nkeep_weekly = 0\nkeep_monthly = 0\nkeep_yearly = 0\n")
	if out, err := execRoot(t, "--account", "test", "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	_, err := execRoot(t, "--account", "test", "forget")
	if err == nil || !strings.Contains(err.Error(), "keeps nothing") {
		t.Fatalf("err = %v, want the policy refused", err)
	}
}
