package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/mail/mailtest"
)

var statsT0 = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

func statsBackup(t *testing.T, cfg *config.Config, f *mailtest.Fake, at time.Time) (*BackupReport, error) {
	t.Helper()
	return Backup(context.Background(), cfg, f, BackupOptions{Now: func() time.Time { return at }})
}

func statsAt(t *testing.T, cfg *config.Config, now time.Time, opts StatsOptions) *StatsReport {
	t.Helper()
	opts.Now = func() time.Time { return now }
	rep, err := Stats(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestStatsCountsDedupeUnreadAndFolders(t *testing.T) {
	cfg := initRepo(t)
	f := mailtest.New()
	a, b := msg("a", "alpha", "body a"), msg("b", "beta", "body b")
	f.AddFolder("INBOX").Add(a, `\Seen`)
	inbox := f.Folders[0]
	inbox.Add(b, `\Flagged`)
	f.AddFolder("Sent").Add(a, `\Seen`) // the same message in a second folder
	if _, err := statsBackup(t, cfg, f, statsT0); err != nil {
		t.Fatal(err)
	}
	r := statsAt(t, cfg, statsT0.Add(time.Hour), StatsOptions{})
	if r.Messages.Unique != 2 || r.Messages.Positions != 3 {
		t.Fatalf("unique %d positions %d, want 2 and 3", r.Messages.Unique, r.Messages.Positions)
	}
	if want := int64(len(a) + len(b)); r.Messages.RawBytes != want {
		t.Errorf("raw bytes %d, want %d", r.Messages.RawBytes, want)
	}
	if r.Messages.Dedupe < 1.49 || r.Messages.Dedupe > 1.51 {
		t.Errorf("dedupe %v, want 1.5", r.Messages.Dedupe)
	}
	if r.Messages.Unread != 1 || r.Messages.Starred != 1 {
		t.Errorf("unread %d starred %d, want 1 and 1", r.Messages.Unread, r.Messages.Starred)
	}
	if r.Messages.DiskBytes <= 0 {
		t.Errorf("disk bytes %d: the object files were not measured", r.Messages.DiskBytes)
	}
	names := map[string]FolderStats{}
	for _, fo := range r.Folders {
		names[fo.Name] = fo
	}
	if names["INBOX"].Messages != 2 || names["Sent"].Messages != 1 || names["INBOX"].Unread != 1 {
		t.Errorf("folders %+v", r.Folders)
	}
	if got := statsAt(t, cfg, statsT0, StatsOptions{NoDisk: true}); got.Messages.DiskBytes != 0 {
		t.Errorf("--no-disk still measured the disk: %d", got.Messages.DiskBytes)
	}
}

func TestStatsRunHealth(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	for _, h := range []int{0, 1, 7} {
		if _, err := statsBackup(t, cfg, f, statsT0.Add(time.Duration(h)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// A partial run: Sent fails after its first body, INBOX is committed.
	f.Folders[1].Add(msg("p1", "p1", "x"))
	f.Folders[1].Add(msg("p2", "p2", "y"))
	f.FailAfter = map[string]int{"Sent": 1}
	if rep, err := statsBackup(t, cfg, f, statsT0.Add(8*time.Hour)); err != nil || !rep.Partial() {
		t.Fatalf("want a partial run: %v %v", rep, err)
	}
	f.FailAfter = nil
	// A failed run: nothing can be read.
	f.FailAt = func(call string) error {
		if strings.HasPrefix(call, "examine") {
			return errors.New("server said no")
		}
		return nil
	}
	if _, err := statsBackup(t, cfg, f, statsT0.Add(9*time.Hour)); err == nil {
		t.Fatal("want the run to fail")
	}
	r := statsAt(t, cfg, statsT0.Add(10*time.Hour), StatsOptions{})
	b := r.Backups
	if b.Runs != 5 || b.Complete != 3 || b.Partial != 1 || b.Failed != 1 {
		t.Fatalf("runs %+v", b)
	}
	if got := time.Duration(b.LongestGapSeconds) * time.Second; got != 6*time.Hour {
		t.Errorf("longest gap %v, want 6h", got)
	}
	if !b.LastGood.Equal(statsT0.Add(8 * time.Hour)) {
		t.Errorf("last good %v, want the partial run at +8h", b.LastGood)
	}
}

func TestStatsHistoryAndWhatPruneWouldFree(t *testing.T) {
	cfg := initRepo(t)
	// Keep only the newest snapshot, so forget releases the older one.
	cfg.Retention = config.Retention{KeepLast: 1, DeletedGrace: "180d", DeletedGraceDur: 180 * 24 * time.Hour}
	f := fakeMailbox()
	if _, err := statsBackup(t, cfg, f, statsT0); err != nil {
		t.Fatal(err)
	}
	f.Folders[0].Delete(f.Folders[0].Messages[0].UID) // gone from the server
	if _, err := statsBackup(t, cfg, f, statsT0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := Forget(context.Background(), cfg, RetentionOptions{Now: func() time.Time { return statsT0.Add(2 * time.Hour) }}); err != nil {
		t.Fatal(err)
	}
	soon := statsAt(t, cfg, statsT0.Add(10*24*time.Hour), StatsOptions{})
	if soon.History.Messages != 1 {
		t.Fatalf("kept only by history: %d, want 1", soon.History.Messages)
	}
	if soon.History.PruneMessages != 0 {
		t.Errorf("inside the grace period prune frees %d, want 0", soon.History.PruneMessages)
	}
	late := statsAt(t, cfg, statsT0.Add(200*24*time.Hour), StatsOptions{})
	if late.History.PruneMessages != 1 || late.History.PruneRawBytes == 0 {
		t.Errorf("after the grace period prune frees %+v, want 1 message", late.History)
	}
	if _, err := Pin(context.Background(), cfg, "1", true); err != nil {
		t.Fatal(err)
	}
	if pinned := statsAt(t, cfg, statsT0.Add(200*24*time.Hour), StatsOptions{}); pinned.History.PruneMessages != 0 {
		t.Errorf("a pinned snapshot must stop prune: %+v", pinned.History)
	}
}

func TestStatsAlertsAndBlockedPrune(t *testing.T) {
	cfg := withRetention(guardConfig(t))
	f := bigMailbox(20)
	if _, err := statsBackup(t, cfg, f, statsT0); err != nil {
		t.Fatal(err)
	}
	deleteFirst(f.Folders[0], 15)
	if rep, err := statsBackup(t, cfg, f, statsT0.Add(time.Hour)); err != nil || !rep.Alerted() {
		t.Fatalf("want an alert: %v %v", rep, err)
	}
	r := statsAt(t, cfg, statsT0.Add(2*time.Hour), StatsOptions{})
	if r.Alerts.Open != 1 || r.Alerts.Total != 1 || r.History.PruneBlocked == "" {
		t.Fatalf("alerts %+v history %+v", r.Alerts, r.History)
	}
	if _, err := Ack(context.Background(), cfg, nil, true); err != nil {
		t.Fatal(err)
	}
	r = statsAt(t, cfg, statsT0.Add(2*time.Hour), StatsOptions{})
	if r.Alerts.Open != 0 || r.Alerts.Acknowledged != 1 || r.History.PruneBlocked != "" {
		t.Errorf("after ack: %+v %+v", r.Alerts, r.History)
	}
}

func TestStatsGrowthWindows(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	if _, err := statsBackup(t, cfg, f, statsT0); err != nil { // 3 messages, long ago
		t.Fatal(err)
	}
	later := statsT0.Add(40 * 24 * time.Hour)
	f.Folders[0].Add(msg("n1", "new one", "n1"))
	f.Folders[0].Add(msg("n2", "new two", "n2"))
	if _, err := statsBackup(t, cfg, f, later); err != nil {
		t.Fatal(err)
	}
	r := statsAt(t, cfg, later.Add(24*time.Hour), StatsOptions{})
	if r.Growth.Last30Days.Messages != 2 || r.Growth.Last7Days.Messages != 2 {
		t.Errorf("windows %+v / %+v, want the 2 new messages in both", r.Growth.Last30Days, r.Growth.Last7Days)
	}
	if len(r.Growth.Months) != 2 {
		t.Errorf("months %+v, want 2", r.Growth.Months)
	}
}

func TestStatsEncryptedFolderNames(t *testing.T) {
	cfg, keys := initEncrypted(t)
	if _, err := statsBackup(t, cfg, fakeMailbox(), statsT0); err != nil {
		t.Fatal(err)
	}
	r := statsAt(t, cfg, statsT0, StatsOptions{}) // no key, no --names
	if !r.Encrypted || r.NamesShown {
		t.Fatalf("encrypted %v names shown %v", r.Encrypted, r.NamesShown)
	}
	for _, fo := range r.Folders {
		if !strings.HasPrefix(fo.Name, "#") {
			t.Errorf("folder name %q leaked without a key", fo.Name)
		}
	}
	cfg.Encryption.Identity = keys.identity
	r = statsAt(t, cfg, statsT0, StatsOptions{Names: true})
	if !r.NamesShown || r.Folders[0].Name == "" || strings.HasPrefix(r.Folders[0].Name, "#") {
		t.Errorf("with the key and --names: %+v", r.Folders)
	}
}

func TestStatsOnAnEmptyRepository(t *testing.T) {
	r := statsAt(t, initRepo(t), statsT0, StatsOptions{})
	if r.Snapshots.Total != 0 || r.Note == "" {
		t.Errorf("empty repository: %+v", r)
	}
}
