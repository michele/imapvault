//go:build integration

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/engine"
	"github.com/michele/imapvault/internal/index"
)

const retentionCfg = "\n[retention]\nkeep_last = 1\ndeleted_grace = \"1h\"\n"

// backdateRuns makes every run look like it happened long ago, as if the repository
// were months old. It edits the index directly: the product has no test hook for it.
func backdateRuns(t *testing.T, repoPath string) {
	t.Helper()
	db, err := index.Open(filepath.Join(repoPath, "index.db"), index.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE runs SET started_at = '2020-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
}

// objectFiles lists the object files of the repository as hex hashes.
func objectFiles(t *testing.T, repoPath string) []string {
	t.Helper()
	var out []string
	shards, err := os.ReadDir(filepath.Join(repoPath, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range shards {
		files, err := os.ReadDir(filepath.Join(repoPath, "objects", s.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			out = append(out, s.Name()+f.Name())
		}
	}
	sort.Strings(out)
	return out
}

// snapshotHashes is the sorted SHA-256 of every message in a snapshot's folders.
func snapshotHashes(t *testing.T, snap string, folders ...string) []string {
	t.Helper()
	var out []string
	for _, f := range folders {
		for _, m := range lsJSON(t, snap, f).Messages {
			out = append(out, m.SHA256)
		}
	}
	sort.Strings(out)
	return out
}

func pruneJSON(t *testing.T, args ...string) engine.PruneReport {
	t.Helper()
	var rep engine.PruneReport
	if err := json.Unmarshal([]byte(run(t, append([]string{"--json", "prune"}, args...)...)), &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// Forget and prune against the real server: mail deleted from the server stays
// restorable through the grace period, and goes once both the grace period and
// the last snapshot that showed it are gone.
func TestForgetAndPruneAgainstServer(t *testing.T) {
	c := dialTestServer(t)
	box := uniq("itest-retention")
	createFolder(t, c, box)
	appendN(t, c, box, "m", 10)
	repoPath := itestAccount(t, include(box)+guardThresholdsOff+retentionCfg)
	run(t, "init")

	run(t, "backup") // snapshot 1: 10 messages
	expunge(t, c, box, 1, 2, 3, 4)
	run(t, "backup") // snapshot 2: 6 messages
	appendN(t, c, box, "n", 2)
	run(t, "backup") // snapshot 3: 8 messages
	requireLsMatches(t, "3", serverState(t, c, box))
	if got := len(objectFiles(t, repoPath)); got != 12 {
		t.Fatalf("%d object files after three backups, want 12 (10 + 2 new)", got)
	}
	t.Logf("snapshots:\n%s", run(t, "snapshots"))

	// forget marks, deletes nothing, and hides the old snapshots from the list.
	t.Logf("forget --dry-run:\n%s", run(t, "forget", "--dry-run"))
	out := run(t, "forget")
	t.Logf("forget:\n%s", out)
	if !strings.Contains(out, "Forgot 2 snapshots (1, 2)") {
		t.Fatalf("forget:\n%s", out)
	}
	if got := len(objectFiles(t, repoPath)); got != 12 {
		t.Fatalf("forget removed objects: %d left", got)
	}
	var visible, all []engine.SnapshotInfo
	if err := json.Unmarshal([]byte(run(t, "--json", "snapshots")), &visible); err != nil || len(visible) != 1 {
		t.Fatalf("snapshots: %v %+v", err, visible)
	}
	if err := json.Unmarshal([]byte(run(t, "--json", "snapshots", "--all")), &all); err != nil || len(all) != 3 || !all[0].Forgotten || !all[1].Forgotten || all[2].Forgotten {
		t.Fatalf("snapshots --all: %v %+v", err, all)
	}
	// A forgotten snapshot is still addressable while prune has not removed it.
	if n := len(lsJSON(t, "1", box).Messages); n != 10 {
		t.Fatalf("forgotten snapshot 1 lists %d messages, want 10", n)
	}

	// Inside the grace period nothing goes, dry run or not.
	dry := pruneJSON(t, "--dry-run")
	if dry.Entries != 0 || dry.Blobs != 0 || dry.Snapshots != 0 {
		t.Fatalf("a dry run inside the grace period: %+v", dry)
	}
	t.Logf("prune inside the grace period:\n%s", run(t, "prune"))
	if got := len(objectFiles(t, repoPath)); got != 12 {
		t.Fatalf("prune inside the grace period removed objects: %d left", got)
	}
	run(t, "check", "--read-data")

	// Months later: the deletions are older than the grace period.
	backdateRuns(t, repoPath)
	dry = pruneJSON(t, "--dry-run")
	t.Logf("prune --dry-run after the grace period:\n%s", run(t, "prune", "--dry-run"))
	// Snapshot 2 is forgotten too, but all 6 of its messages are still on the server
	// (open entries), so it is still complete and stays until one of them goes.
	if dry.Entries != 4 || dry.Blobs != 4 || dry.Snapshots != 1 {
		t.Fatalf("dry run: %+v, want the 4 expunged messages and forgotten snapshot 1", dry)
	}
	if got := len(objectFiles(t, repoPath)); got != 12 {
		t.Fatalf("a dry run removed objects: %d left", got)
	}
	out = run(t, "prune")
	t.Logf("prune:\n%s", out)
	for _, want := range []string{"Removed:", "Index entries", "index-backups/index-"} {
		if !strings.Contains(out, want) {
			t.Errorf("prune output lacks %q", want)
		}
	}

	// Exactly the messages snapshot 3 shows are left, and it still matches the server.
	want := snapshotHashes(t, "3", box)
	if got := objectFiles(t, repoPath); strings.Join(got, ",") != strings.Join(want, ",") || len(got) != 8 {
		t.Fatalf("objects left %v, want exactly snapshot 3's messages %v", got, want)
	}
	requireLsMatches(t, "3", serverState(t, c, box))
	if err := json.Unmarshal([]byte(run(t, "--json", "snapshots", "--all")), &all); err != nil || len(all) != 2 || all[0].ID != 2 || !all[0].Forgotten || all[1].ID != 3 {
		t.Fatalf("snapshots --all after prune: %v %+v", err, all)
	}
	if out, err := execRoot(t, "--account", "it", "ls", "1"); err == nil {
		t.Fatalf("snapshot 1 should be gone:\n%s", out)
	}
	if n := len(lsJSON(t, "2", box).Messages); n != 6 {
		t.Fatalf("forgotten snapshot 2 lists %d messages, want 6: it is still complete", n)
	}
	run(t, "check", "--read-data")

	// A second prune has nothing left to do.
	if again := pruneJSON(t); again.Entries != 0 || again.Blobs != 0 || again.Snapshots != 0 || again.Orphans != 0 {
		t.Fatalf("a second prune removed more: %+v", again)
	}
	ents, err := os.ReadDir(filepath.Join(repoPath, "index-backups"))
	if err != nil || len(ents) < 4 {
		t.Fatalf("index-backups holds %d files (err %v), want a copy per backup and per prune", len(ents), err)
	}
	t.Logf("index-backups: %d copies", len(ents))
}

// The scenario the tool exists for, end to end: someone deletes everything, the
// guard pins the last good snapshot, and neither forget nor prune can touch it.
func TestPruneRespectsTheGuardAgainstServer(t *testing.T) {
	c := dialTestServer(t)
	inbox := uniq("itest-retention-guard")
	createFolder(t, c, inbox)
	appendN(t, c, inbox, "m", 12)
	repoPath := itestAccount(t, include(inbox)+"\n[guard]\nmax_deleted_percent = 10\nmax_deleted_count = 500\n"+retentionCfg)
	run(t, "init")
	bin := buildBinary(t)

	if out, code := runBinary(t, bin, "backup"); code != 0 {
		t.Fatalf("first backup exited %d:\n%s", code, out)
	}
	expungeAll(t, c, inbox)
	if out, code := runBinary(t, bin, "backup"); code != 3 {
		t.Fatalf("backup after the deletion exited %d, want 3:\n%s", code, out)
	}

	// Prune is off while the alert is open, with or without --dry-run.
	for _, args := range [][]string{{"prune"}, {"prune", "--dry-run"}} {
		out, code := runBinary(t, bin, args...)
		if code == 0 || !strings.Contains(out, "unacknowledged") {
			t.Fatalf("%v exited %d, want a refusal:\n%s", args, code, out)
		}
	}
	// Forget works, and cannot touch the pinned snapshot 1.
	t.Logf("forget with an open alert:\n%s", run(t, "forget"))
	var all []engine.SnapshotInfo
	if err := json.Unmarshal([]byte(run(t, "--json", "snapshots", "--all")), &all); err != nil || len(all) != 2 || all[0].Forgotten || !all[0].Pinned {
		t.Fatalf("snapshots: %v %+v", err, all)
	}

	// Even after the alert is acknowledged and the grace period is long over,
	// the pinned snapshot keeps the mail.
	run(t, "ack", "1")
	backdateRuns(t, repoPath)
	out := run(t, "prune")
	t.Logf("prune after ack, months later:\n%s", out)
	if got := len(lsJSON(t, "1", inbox).Messages); got != 12 {
		t.Fatalf("pinned snapshot 1 lists %d messages after prune, want 12", got)
	}
	if got := len(objectFiles(t, repoPath)); got != 12 {
		t.Fatalf("%d object files after prune, want all 12 kept for the pinned snapshot", got)
	}

	// Only when the pin goes is the mail released.
	run(t, "unpin", "1")
	run(t, "forget")
	run(t, "prune")
	if got := len(objectFiles(t, repoPath)); got != 0 {
		t.Fatalf("%d object files left after unpin, forget and prune, want 0", got)
	}
	run(t, "check", "--read-data")
}
