//go:build integration

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/michele/imapvault/internal/engine"
)

// These tests run against the docker dovecot server. Every test works in folders
// of its own (named with the clock) and restricts the account to them, so tests
// and other packages never see each other's mail. `make itest` runs packages one
// at a time (-p 1) because TestBackupMatchesServer backs up every folder.

func createFolder(t *testing.T, c *imapclient.Client, name string) {
	t.Helper()
	if err := c.Create(name, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(name).Wait() })
}

func appendN(t *testing.T, c *imapclient.Client, folder, prefix string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		appendMsg(t, c, folder, fmt.Sprintf("%s-%d", prefix, i))
	}
}

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()) }

func include(names ...string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = `"` + n + `"`
	}
	return "\n[folders]\ninclude = [" + strings.Join(q, ", ") + "]\n"
}

func backupJSON(t *testing.T, args ...string) engine.BackupReport {
	t.Helper()
	var rep engine.BackupReport
	if err := json.Unmarshal([]byte(run(t, append([]string{"--json", "backup"}, args...)...)), &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// backupJSONAlerted runs a backup that must end with a guard alert (exit 3) and
// returns its report.
func backupJSONAlerted(t *testing.T) engine.BackupReport {
	t.Helper()
	out, err := execRoot(t, "--account", "it", "--json", "backup")
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != exitAlert {
		t.Fatalf("expected exit code %d, got %v\n%s", exitAlert, err, out)
	}
	var rep engine.BackupReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func modeIn(t *testing.T, rep engine.BackupReport, folder string) string {
	t.Helper()
	for _, f := range rep.Folders {
		if f.Name == folder {
			return f.Mode
		}
	}
	t.Fatalf("folder %q is not in the report: %+v", folder, rep.Folders)
	return ""
}

func diffJSON(t *testing.T, from, to int64) map[string]engine.DiffCounts {
	t.Helper()
	var res engine.DiffResult
	if err := json.Unmarshal([]byte(run(t, "--json", "diff", fmt.Sprint(from), fmt.Sprint(to))), &res); err != nil {
		t.Fatal(err)
	}
	out := map[string]engine.DiffCounts{}
	for _, f := range res.Folders {
		out[f.Name] = f.DiffCounts
	}
	return out
}

func blobCount(t *testing.T) int {
	t.Helper()
	var rep engine.CheckReport
	if err := json.Unmarshal([]byte(run(t, "--json", "check")), &rep); err != nil {
		t.Fatal(err)
	}
	return rep.Blobs
}

func selectRW(t *testing.T, c *imapclient.Client, folder string) {
	t.Helper()
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
}

func uidSet(uids ...imap.UID) imap.UIDSet {
	var s imap.UIDSet
	s.AddNum(uids...)
	return s
}

func expunge(t *testing.T, c *imapclient.Client, folder string, uids ...imap.UID) {
	t.Helper()
	selectRW(t, c, folder)
	if err := c.Store(uidSet(uids...), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge().Close(); err != nil {
		t.Fatal(err)
	}
	_ = c.Unselect().Wait()
}

// Every kind of change a folder can see is found by the mode that should find
// it, and each snapshot equals the server's state at its time.
func TestIncrementalModesAgainstServer(t *testing.T) {
	c := dialTestServer(t)
	folder := uniq("itest-modes")
	createFolder(t, c, folder)
	appendN(t, c, folder, "base", 3)
	itestAccount(t, include(folder)+guardThresholdsOff)
	run(t, "init")

	r1 := backupJSON(t)
	if modeIn(t, r1, folder) != "full" || r1.Fetched != 3 {
		t.Fatalf("first run %+v", r1)
	}
	requireLsMatches(t, fmt.Sprint(r1.Snapshot), serverState(t, c, folder))

	// nothing changed: only EXAMINE.
	r2 := backupJSON(t)
	if modeIn(t, r2, folder) != "unchanged" || r2.Fetched != 0 || r2.New+r2.Closed+r2.Reflagged != 0 {
		t.Fatalf("second run %+v", r2)
	}
	requireLsMatches(t, fmt.Sprint(r2.Snapshot), serverState(t, c, folder))

	// new mail
	appendN(t, c, folder, "fresh", 2)
	r3 := backupJSON(t)
	if modeIn(t, r3, folder) != "incremental" || r3.Fetched != 2 || r3.New != 2 || r3.Closed != 0 {
		t.Fatalf("new mail %+v", r3)
	}
	requireLsMatches(t, fmt.Sprint(r3.Snapshot), serverState(t, c, folder))
	if d := diffJSON(t, r2.Snapshot, r3.Snapshot)[folder]; d != (engine.DiffCounts{Added: 2}) {
		t.Fatalf("diff after new mail: %+v", d)
	}

	// flag change
	selectRW(t, c, folder)
	if err := c.Store(uidSet(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	_ = c.Unselect().Wait()
	r4 := backupJSON(t)
	if modeIn(t, r4, folder) != "incremental" || r4.Reflagged != 1 || r4.Fetched != 0 || r4.New != 0 {
		t.Fatalf("flag change %+v", r4)
	}
	requireLsMatches(t, fmt.Sprint(r4.Snapshot), serverState(t, c, folder))
	if d := diffJSON(t, r3.Snapshot, r4.Snapshot)[folder]; d != (engine.DiffCounts{Reflagged: 1}) {
		t.Fatalf("diff after flag change: %+v", d)
	}

	// expunge
	expunge(t, c, folder, 2)
	r5 := backupJSON(t)
	if modeIn(t, r5, folder) != "incremental" || r5.Closed != 1 || r5.Fetched != 0 || r5.Messages != 4 {
		t.Fatalf("expunge %+v", r5)
	}
	requireLsMatches(t, fmt.Sprint(r5.Snapshot), serverState(t, c, folder))
	if d := diffJSON(t, r4.Snapshot, r5.Snapshot)[folder]; d != (engine.DiffCounts{Deleted: 1}) {
		t.Fatalf("diff after expunge: %+v", d)
	}

	// all three at once, then --full must agree with what the fast path built
	appendMsg(t, c, folder, "late")
	expunge(t, c, folder, 3)
	r6 := backupJSON(t)
	if modeIn(t, r6, folder) != "incremental" || r6.New != 1 || r6.Closed != 1 {
		t.Fatalf("mixed %+v", r6)
	}
	want := serverState(t, c, folder)
	requireLsMatches(t, fmt.Sprint(r6.Snapshot), want)
	r7 := backupJSON(t, "--full")
	if modeIn(t, r7, folder) != "full" || r7.New+r7.Closed+r7.Reflagged != 0 || r7.Fetched != 0 {
		t.Fatalf("--full found a difference the fast path missed: %+v", r7)
	}
	requireLsMatches(t, fmt.Sprint(r7.Snapshot), want)
	run(t, "check", "--read-data")
}

// A message moved to another folder is stored once; a renamed folder costs no new
// blobs. (dovecot has no OBJECTID, so the body is fetched again to be hashed.)
func TestMoveAndRenameAgainstServer(t *testing.T) {
	c := dialTestServer(t)
	a, b, renamed := uniq("itest-mv-a"), uniq("itest-mv-b"), uniq("itest-mv-r")
	createFolder(t, c, a)
	createFolder(t, c, b)
	t.Cleanup(func() { _ = c.Delete(renamed).Wait() })
	appendN(t, c, a, "a", 3)
	appendN(t, c, b, "b", 1)
	itestAccount(t, include(a, b, renamed))
	run(t, "init")

	r1 := backupJSON(t)
	if got := blobCount(t); got != 4 {
		t.Fatalf("blobs after the first run: %d", got)
	}

	// move uid 1 of a into b, the way a client without MOVE does: copy, delete, expunge.
	selectRW(t, c, a)
	if _, err := c.Copy(uidSet(1), b).Wait(); err != nil {
		t.Fatal(err)
	}
	_ = c.Unselect().Wait()
	expunge(t, c, a, 1)
	r2 := backupJSON(t)
	if r2.New != 1 || r2.Closed != 1 || r2.BlobsAdded != 0 || r2.BlobsReused != 1 {
		t.Fatalf("move %+v", r2)
	}
	if got := blobCount(t); got != 4 {
		t.Fatalf("a moved message must not add a blob: %d", got)
	}
	state2 := serverState(t, c, a, b)
	requireLsMatches(t, fmt.Sprint(r2.Snapshot), state2)
	d := diffJSON(t, r1.Snapshot, r2.Snapshot)
	if d[a] != (engine.DiffCounts{MovedOut: 1}) || d[b] != (engine.DiffCounts{MovedIn: 1}) {
		t.Fatalf("diff after the move: %+v", d)
	}

	// rename b: the old name goes, the new one appears, the blobs stay.
	if err := c.Rename(b, renamed, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	r3 := backupJSON(t)
	if r3.GoneFolders != 1 || r3.NewFolders != 1 || r3.Status != "complete" {
		t.Fatalf("rename %+v", r3)
	}
	if got := blobCount(t); got != 4 {
		t.Fatalf("a renamed folder must not add a blob: %d", got)
	}
	requireLsMatches(t, fmt.Sprint(r3.Snapshot), serverState(t, c, a, renamed))
	requireLsMatches(t, fmt.Sprint(r2.Snapshot), state2) // history is intact
	d = diffJSON(t, r2.Snapshot, r3.Snapshot)
	if d[b] != (engine.DiffCounts{MovedOut: 2}) || d[renamed] != (engine.DiffCounts{MovedIn: 2}) {
		t.Fatalf("diff after the rename: %+v", d)
	}
	run(t, "check", "--read-data")
}

// Delete and recreate a folder: new UIDVALIDITY, old generation closed, history
// kept. Then delete it for good.
func TestUIDValidityResetAndFolderDeleteAgainstServer(t *testing.T) {
	c := dialTestServer(t)
	folder := uniq("itest-reset")
	createFolder(t, c, folder)
	appendN(t, c, folder, "old", 3)
	itestAccount(t, include(folder)+guardThresholdsOff)
	run(t, "init")

	r1 := backupJSON(t)
	state1 := serverState(t, c, folder)
	requireLsMatches(t, fmt.Sprint(r1.Snapshot), state1)
	uv1 := examineUIDValidity(t, c, folder)

	if err := c.Delete(folder).Wait(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // dovecot derives UIDVALIDITY from the creation second
	if err := c.Create(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	appendN(t, c, folder, "new", 2)
	if uv2 := examineUIDValidity(t, c, folder); uv2 == uv1 {
		t.Skipf("the server reused UIDVALIDITY %d; cannot test a reset", uv1)
	}

	r2 := backupJSON(t)
	if modeIn(t, r2, folder) != "full" || r2.Closed != 3 || r2.New != 2 || r2.Status != "complete" {
		t.Fatalf("reset %+v", r2)
	}
	state2 := serverState(t, c, folder)
	requireLsMatches(t, fmt.Sprint(r2.Snapshot), state2)
	requireLsMatches(t, fmt.Sprint(r1.Snapshot), state1) // history is intact
	if d := diffJSON(t, r1.Snapshot, r2.Snapshot)[folder]; d != (engine.DiffCounts{Added: 2, Deleted: 3}) {
		t.Fatalf("diff across the reset: %+v", d)
	}
	if again := backupJSON(t); modeIn(t, again, folder) != "unchanged" || again.Closed != 0 {
		t.Fatalf("the reset must not repeat: %+v", again)
	}

	if err := c.Delete(folder).Wait(); err != nil {
		t.Fatal(err)
	}
	// A folder that vanished with mail in it trips the guard even though the
	// thresholds are off: that rule has no switch.
	r4 := backupJSONAlerted(t)
	if r4.GoneFolders != 1 || r4.Messages != 0 || r4.Alert == nil || r4.Alert.Kind != "folder-vanished" {
		t.Fatalf("folder delete %+v alert %+v", r4, r4.Alert)
	}
	requireLsMatches(t, fmt.Sprint(r4.Snapshot), map[string][]serverMsg{})
	requireLsMatches(t, fmt.Sprint(r2.Snapshot), state2)
}

func examineUIDValidity(t *testing.T, c *imapclient.Client, folder string) uint32 {
	t.Helper()
	sel, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Unselect().Wait()
	return sel.UIDValidity
}

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "imapvault")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// startBackup runs the built binary and streams its stderr (--verbose progress
// lines: folder names, UIDs and hashes only) on a channel.
func startBackup(t *testing.T, bin string) (*exec.Cmd, *bytes.Buffer, <-chan string) {
	t.Helper()
	cmd := exec.Command(bin, "--account", "it", "--verbose", "backup")
	cmd.Env = os.Environ()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 4096)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	return cmd, &stdout, lines
}

func isBodyLine(line, folder string) bool {
	return strings.HasPrefix(line, folder+" uid ") && strings.Contains(line, "sha256")
}

// kill -9 in the middle of downloading a folder: the next run resumes from what
// was committed instead of starting over, and the repository stays consistent.
func TestKilledRunResumes(t *testing.T) {
	c := dialTestServer(t)
	folder := uniq("itest-kill")
	createFolder(t, c, folder)
	const total = 600
	appendN(t, c, folder, "m", total)
	itestAccount(t, include(folder))
	run(t, "init")
	bin := buildBinary(t)

	cmd, _, lines := startBackup(t, bin)
	seen := 0
	for line := range lines {
		if isBodyLine(line, folder) {
			seen++
			if seen == 250 {
				// bodies are committed every 100: at least 200 are in the index now.
				_ = cmd.Process.Kill()
			}
		}
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("the backup finished before it could be killed (seen %d lines, err %v): raise the folder size", seen, err)
	}

	rep := backupJSON(t)
	if rep.Status != "complete" || rep.Snapshot != 2 {
		t.Fatalf("resumed run %+v (the killed run must not have left a snapshot)", rep)
	}
	if rep.Fetched == 0 || rep.Fetched > total-200 {
		t.Fatalf("resumed run fetched %d of %d bodies: it did not resume from the committed batches", rep.Fetched, total)
	}
	requireLsMatches(t, fmt.Sprint(rep.Snapshot), serverState(t, c, folder))
	var snaps []engine.SnapshotInfo
	if err := json.Unmarshal([]byte(run(t, "--json", "snapshots")), &snaps); err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots %v err %v", snaps, err)
	}
	// a killed run may leave orphan blobs (warnings), never errors.
	run(t, "check", "--read-data")
}

// Ctrl-C after one folder is done and another is half-way: a partial snapshot,
// exit code 2, and the next run finishes the job.
func TestInterruptedRunIsPartialWithExitCode2(t *testing.T) {
	c := dialTestServer(t)
	first, second := uniq("itest-int-a"), uniq("itest-int-b")
	createFolder(t, c, first)
	createFolder(t, c, second)
	appendN(t, c, first, "a", 3)
	const total = 600
	appendN(t, c, second, "b", total)
	itestAccount(t, include(first, second))
	run(t, "init")
	bin := buildBinary(t)

	cmd, stdout, lines := startBackup(t, bin)
	seen := 0
	for line := range lines {
		if isBodyLine(line, second) {
			seen++
			if seen == 50 {
				_ = cmd.Process.Signal(os.Interrupt)
			}
		}
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("exit status: %v (want 2)\nstdout:\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "(partial)") {
		t.Fatalf("the report does not say the run is partial:\n%s", stdout.String())
	}
	var snaps []engine.SnapshotInfo
	if err := json.Unmarshal([]byte(run(t, "--json", "snapshots")), &snaps); err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots %v err %v", snaps, err)
	}
	want := serverState(t, c, first, second)
	if got := lsJSON(t, "1", first); len(got.Messages) != 3 {
		t.Fatalf("the finished folder must be in the partial snapshot in full: %+v", got.Messages)
	}
	if got := lsJSON(t, "1", second); len(got.Messages) >= total {
		t.Fatalf("the interrupted folder cannot be complete: %d messages", len(got.Messages))
	}

	rep := backupJSON(t)
	if rep.Status != "complete" || rep.Fetched >= total || rep.Fetched == 0 {
		t.Fatalf("finishing run %+v", rep)
	}
	requireLsMatches(t, fmt.Sprint(rep.Snapshot), want)
	run(t, "check", "--read-data")
}
