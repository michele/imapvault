//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/michele/imapvault/internal/engine"
)

// runBinary runs the built binary against the test account and returns what it
// printed (stdout and stderr together) and its exit code.
func runBinary(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"--account", "it"}, args...)...)
	cmd.Env = os.Environ()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &ee):
		return out.String(), ee.ExitCode()
	}
	t.Fatalf("run %v: %v", args, err)
	return "", -1
}

// expungeAll deletes and expunges every message of folder.
func expungeAll(t *testing.T, c *imapclient.Client, folder string) {
	t.Helper()
	selectRW(t, c, folder)
	var all imap.UIDSet
	all.AddRange(1, 0) // 1:*
	if err := c.Store(all, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge().Close(); err != nil {
		t.Fatal(err)
	}
	_ = c.Unselect().Wait()
}

// hookScript writes a notifier that records its arguments and environment, and
// returns the guard config that runs it and the file it writes.
func hookScript(t *testing.T) (guardCfg, outFile string) {
	t.Helper()
	dir := t.TempDir()
	outFile = filepath.Join(dir, "hook.out")
	script := filepath.Join(dir, "hook.sh")
	body := "#!/bin/sh\n{ printf 'arg=%s\\n' \"$@\"; printf 'alert_id=%s\\nsnapshot=%s\\n' \"$IMAPVAULT_ALERT_ID\" \"$IMAPVAULT_SNAPSHOT\"; } > '" + outFile + "'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	guardCfg = "\n[guard]\nmax_deleted_percent = 10\nmax_deleted_count = 500\non_alert = \"'" + script + "' 'alert {account}: {summary}'\"\n"
	return guardCfg, outFile
}

// The scenario the tool exists for: someone deletes everything, empties the
// trash, and the next backup raises the alarm.
func TestGuardMassDeletionAgainstServer(t *testing.T) {
	c := dialTestServer(t)
	inbox, trash := uniq("itest-guard-inbox"), uniq("itest-guard-trash")
	createFolder(t, c, inbox)
	createFolder(t, c, trash)
	appendN(t, c, inbox, "m", 12)
	appendN(t, c, trash, "t", 3)
	guardCfg, hookOut := hookScript(t)
	itestAccount(t, include(inbox, trash)+guardCfg)
	run(t, "init")
	bin := buildBinary(t)

	out, code := runBinary(t, bin, "backup")
	if code != 0 {
		t.Fatalf("the first backup exited %d:\n%s", code, out)
	}
	if _, err := os.Stat(hookOut); err == nil {
		t.Fatal("the hook ran without an alert")
	}

	// "Delete everything": move the inbox into Trash, then empty Trash.
	selectRW(t, c, inbox)
	var all imap.UIDSet
	all.AddRange(1, 0)
	if _, err := c.Copy(all, trash).Wait(); err != nil {
		t.Fatal(err)
	}
	_ = c.Unselect().Wait()
	expungeAll(t, c, inbox)
	expungeAll(t, c, trash)

	out, code = runBinary(t, bin, "backup")
	t.Logf("backup after the deletion exited %d:\n%s", code, out)
	if code != 3 {
		t.Fatalf("exit code %d, want 3:\n%s", code, out)
	}
	for _, want := range []string{"ALERT 1 (mass-deletion)", "15 of 15 messages deleted (100.0%)", inbox + " 12", trash + " 3", "snapshot 2 is flagged", "snapshot 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// the server really is empty, and the snapshot says so
	requireLsMatches(t, "2", serverState(t, c, inbox, trash))

	var snaps []engine.SnapshotInfo
	if err := json.Unmarshal([]byte(run(t, "--json", "snapshots")), &snaps); err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots %v err %v", snaps, err)
	}
	if !snaps[0].Pinned || snaps[0].Flagged || snaps[1].Pinned || !snaps[1].Flagged || snaps[0].Messages != 15 || snaps[1].Messages != 0 {
		t.Fatalf("snapshots: %+v", snaps)
	}
	// ... and the last good state is still all there.
	if n := len(lsJSON(t, "1", inbox).Messages); n != 12 {
		t.Fatalf("snapshot 1 inbox has %d messages, want 12", n)
	}

	t.Logf("alerts:\n%s", run(t, "alerts"))
	var al engine.AlertsResult
	if err := json.Unmarshal([]byte(run(t, "--json", "alerts")), &al); err != nil || len(al.Alerts) != 1 {
		t.Fatalf("alerts %+v err %v", al, err)
	}
	if a := al.Alerts[0]; a.ID != 1 || a.Snapshot != 2 || a.PinnedSnapshot != 1 || a.Kind != "mass-deletion" || a.AcknowledgedAt != nil {
		t.Fatalf("alert %+v", a)
	}

	hook, err := os.ReadFile(hookOut)
	if err != nil {
		t.Fatalf("the on_alert hook did not run: %v", err)
	}
	for _, want := range []string{"arg=alert it: 15 of 15 messages deleted", inbox + " 12", "alert_id=1", "snapshot=2"} {
		if !strings.Contains(string(hook), want) {
			t.Errorf("hook output lacks %q:\n%s", want, hook)
		}
	}

	// The pin cannot be dropped while the alert is open; after ack it can.
	if out, err := execRoot(t, "--account", "it", "unpin", "1"); err == nil {
		t.Fatalf("unpin of the alert's snapshot succeeded:\n%s", out)
	}
	if out := run(t, "ack", "1"); !strings.Contains(out, "Acknowledged alert 1") {
		t.Fatalf("ack:\n%s", out)
	}
	t.Logf("snapshots:\n%s", run(t, "snapshots"))
	if out := run(t, "alerts"); !strings.Contains(out, "No unacknowledged alerts.") {
		t.Fatalf("alerts after ack:\n%s", out)
	}

	// The loss is on record: backing up again says nothing more.
	if err := os.Remove(hookOut); err != nil {
		t.Fatal(err)
	}
	out, code = runBinary(t, bin, "backup")
	if code != 0 || strings.Contains(out, "ALERT") {
		t.Fatalf("a repeat run exited %d:\n%s", code, out)
	}
	if _, err := os.Stat(hookOut); err == nil {
		t.Error("the hook ran again for a loss that was already reported")
	}
	run(t, "check", "--read-data")
}

// A rename moves every message and a UIDVALIDITY reset renumbers them: neither
// loses mail, so neither may raise the alarm.
func TestGuardIgnoresRenameAndRenumberingAgainstServer(t *testing.T) {
	c := dialTestServer(t)
	folder, renamed := uniq("itest-guard-keep"), uniq("itest-guard-renamed")
	createFolder(t, c, folder)
	t.Cleanup(func() { _ = c.Delete(renamed).Wait() })
	appendN(t, c, folder, "k", 12)
	guardCfg, hookOut := hookScript(t)
	itestAccount(t, include(folder, renamed)+guardCfg)
	run(t, "init")
	bin := buildBinary(t)
	if out, code := runBinary(t, bin, "backup"); code != 0 {
		t.Fatalf("first backup exited %d:\n%s", code, out)
	}

	if err := c.Rename(folder, renamed, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	out, code := runBinary(t, bin, "backup")
	if code != 0 || strings.Contains(out, "ALERT") {
		t.Fatalf("a rename exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "+1 new, -1 gone") {
		t.Fatalf("the rename was not seen as gone + new:\n%s", out)
	}
	if _, err := os.Stat(hookOut); err == nil {
		t.Fatal("the hook ran for a rename")
	}
	if out := run(t, "alerts", "--all"); !strings.Contains(out, "No alerts.") {
		t.Fatalf("alerts:\n%s", out)
	}
	requireLsMatches(t, "2", serverState(t, c, renamed))
	run(t, "check", "--read-data")
}
