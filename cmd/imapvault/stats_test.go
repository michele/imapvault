package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/michele/imapvault/internal/engine"
)

func TestPrintStatsFormatsTheNumbers(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	r := &engine.StatsReport{
		Account: "me@example.com@imap.example.com", Encrypted: true, Recipients: 2, Format: 2, NamesShown: false,
		Snapshots: engine.StatsSnapshots{Total: 212, Pinned: 3, Flagged: 1, Forgotten: 4, Newest: now.Add(-2 * time.Hour), Oldest: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		Messages:  engine.StatsMessages{Unique: 102838, Positions: 131402, Dedupe: 1.2778, RawBytes: 21 << 30, DiskBytes: 15 << 30, Compression: 1.4, Unread: 12000, Starred: 311},
		Growth: engine.StatsGrowth{
			Last30Days: engine.StatsWindow{Messages: 9341, RawBytes: 1288490188}, Last7Days: engine.StatsWindow{Messages: 2000, RawBytes: 210 << 20},
			Months: []engine.StatsMonth{{Month: "2026-10", Messages: 8000, RawBytes: 1 << 30}, {Month: "2026-11", Messages: 1341, RawBytes: 100 << 20}},
		},
		Backups: engine.StatsBackups{Runs: 212, Complete: 209, Partial: 2, Failed: 1, LastGood: now.Add(-2 * time.Hour), LongestGapSeconds: 6 * 3600, TypicalRunSeconds: 41},
		History: engine.StatsHistory{Messages: 1204, RawBytes: 310 << 20, PruneMessages: 880, PruneDiskBytes: 190 << 20, Grace: "180d"},
		Alerts:  engine.StatsAlerts{Total: 1, Acknowledged: 1},
		Folders: []engine.FolderStats{{ID: 1, Name: "#1", SpecialUse: `\Sent`, Messages: 4210, RawBytes: 1 << 30, Unread: 84}, {ID: 2, Name: "#2", Messages: 61982, RawBytes: 11 << 30, Unread: 6198}},
	}
	var out bytes.Buffer
	printStats(&out, r, now)
	got := out.String()
	for _, want := range []string{
		"encrypted to 2 keys, format v2",
		"212 (3 pinned, 1 flagged, 4 forgotten); newest 2h00m ago, oldest 2026-09-01",
		"102,838 unique, 131,402 positions (1.28x)", "on disk", "1.4x smaller",
		"9% of current mail; 311 starred",
		"+9,341 messages in the last 30 days", "last 7 days",
		"212 runs: 209 complete, 2 partial, 1 failed; last good 2h00m ago; longest gap 6h00m; typical run 41s",
		"1,204 messages", "prune would free 880 messages", "grace 180d",
		"1 acknowledged, 0 open",
		"#1 (\\Sent)", "4,210", "61,982",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestPrintStatsSaysWhyPruneIsBlockedAndHandlesAnEmptyRepository(t *testing.T) {
	now := time.Now()
	var out bytes.Buffer
	printStats(&out, &engine.StatsReport{Note: "No snapshots yet. Run: imapvault backup"}, now)
	if !strings.Contains(out.String(), "No snapshots yet") {
		t.Errorf("empty: %q", out.String())
	}
	out.Reset()
	printStats(&out, &engine.StatsReport{Snapshots: engine.StatsSnapshots{Total: 1, Newest: now}, History: engine.StatsHistory{Grace: "180d", PruneBlocked: "1 unacknowledged guard alert(s)"}}, now)
	if !strings.Contains(out.String(), "blocked") || !strings.Contains(out.String(), "unacknowledged") {
		t.Errorf("a blocked prune must say why:\n%s", out.String())
	}
}

func TestStatsCommandAgainstARealBackup(t *testing.T) {
	backedUp(t, "")
	out, err := execRoot(t, "--account", "test", "stats")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Snapshots", "Messages", "Backups", "Folders", "INBOX"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats output lacks %q:\n%s", want, out)
		}
	}
	js, err := execRoot(t, "--account", "test", "--json", "stats", "--no-disk")
	if err != nil {
		t.Fatal(err)
	}
	var rep engine.StatsReport
	if err := json.Unmarshal([]byte(js), &rep); err != nil || rep.Snapshots.Total == 0 || rep.Messages.DiskBytes != 0 {
		t.Fatalf("json %v %+v", err, rep)
	}
}

func TestStatsOnAnEmptyRepositoryIsFriendly(t *testing.T) {
	setupAccount(t)
	if _, err := execRoot(t, "--account", "test", "init"); err != nil {
		t.Fatal(err)
	}
	out, err := execRoot(t, "--account", "test", "stats")
	if err != nil || !strings.Contains(out, "No snapshots yet") {
		t.Fatalf("%v %q", err, out)
	}
}
