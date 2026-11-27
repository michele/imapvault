package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/objects"
)

// StatsOptions tune Stats.
type StatsOptions struct {
	// Names also shows folder names. On an encrypted repository that needs the
	// private key; without it they stay as #id.
	Names bool
	// NoDisk skips measuring the object files, the only slow part.
	NoDisk bool
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
}

type StatsSnapshots struct {
	Total     int       `json:"total"`
	Pinned    int       `json:"pinned"`
	Flagged   int       `json:"flagged"`
	Forgotten int       `json:"forgotten"`
	Oldest    time.Time `json:"oldest,omitzero"`
	Newest    time.Time `json:"newest,omitzero"`
}

type StatsMessages struct {
	Unique      int     `json:"unique"`    // distinct messages in the current mailbox
	Positions   int     `json:"positions"` // places they occupy (folder, UID)
	Dedupe      float64 `json:"dedupe"`    // positions per unique message
	RawBytes    int64   `json:"raw_bytes"` // of every message ever stored
	DiskBytes   int64   `json:"disk_bytes"`
	Compression float64 `json:"compression"` // raw per disk byte; 0 when the disk was not measured
	Unread      int     `json:"unread"`
	Starred     int     `json:"starred"`
}

type StatsWindow struct {
	Messages int   `json:"messages"`
	RawBytes int64 `json:"raw_bytes"`
}

type StatsMonth struct {
	Month    string `json:"month"` // YYYY-MM
	Messages int    `json:"messages"`
	RawBytes int64  `json:"raw_bytes"`
}

type StatsGrowth struct {
	Last7Days  StatsWindow  `json:"last_7_days"`
	Last30Days StatsWindow  `json:"last_30_days"`
	Months     []StatsMonth `json:"months"`
}

type StatsBackups struct {
	Runs              int       `json:"runs"`
	Complete          int       `json:"complete"`
	Partial           int       `json:"partial"`
	Failed            int       `json:"failed"`
	LastGood          time.Time `json:"last_good,omitzero"` // the last complete or partial run
	LongestGapSeconds int64     `json:"longest_gap_seconds"`
	TypicalRunSeconds int64     `json:"typical_run_seconds"` // the median complete run
}

type StatsHistory struct {
	Messages       int    `json:"messages"` // gone from the server, still kept
	RawBytes       int64  `json:"raw_bytes"`
	PruneMessages  int    `json:"prune_messages"` // what prune would remove now
	PruneRawBytes  int64  `json:"prune_raw_bytes"`
	PruneDiskBytes int64  `json:"prune_disk_bytes"`
	Grace          string `json:"grace,omitempty"`
	PruneBlocked   string `json:"prune_blocked,omitempty"` // why prune would refuse right now
}

type StatsAlerts struct {
	Total        int `json:"total"`
	Open         int `json:"open"`
	Acknowledged int `json:"acknowledged"`
}

// FolderStats is one folder of the current mailbox.
type FolderStats struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"` // "#id" when the name is sealed and no key was used
	SpecialUse string `json:"special_use,omitempty"`
	Messages   int    `json:"messages"`
	RawBytes   int64  `json:"raw_bytes"`
	Unread     int    `json:"unread"`
}

// StatsReport is what `imapvault stats` shows.
type StatsReport struct {
	Account    string         `json:"account"`
	Encrypted  bool           `json:"encrypted"`
	Recipients int            `json:"recipients"`
	Format     int            `json:"format_version"`
	NamesShown bool           `json:"names_shown"`
	Snapshots  StatsSnapshots `json:"snapshots"`
	Messages   StatsMessages  `json:"messages"`
	Growth     StatsGrowth    `json:"growth"`
	Backups    StatsBackups   `json:"backups"`
	History    StatsHistory   `json:"history"`
	Alerts     StatsAlerts    `json:"alerts"`
	Folders    []FolderStats  `json:"folders"`
	Note       string         `json:"note,omitempty"`
	Warnings   []string       `json:"warnings,omitempty"`
}

// Stats summarises a repository. It reads plaintext index columns, the runs and
// the object files, and never writes. It needs no private key unless Names asks
// for folder names (rule 8: no identity is resolved otherwise), and it shows no
// subject or sender (rule 6).
func Stats(ctx context.Context, cfg *config.Config, opts StatsOptions) (*StatsReport, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	now := opts.Now()
	mode := keyNone
	if opts.Names {
		mode = keyIfAvailable
	}
	rd, err := openReaderMode(ctx, cfg, mode)
	if err != nil {
		return nil, err
	}
	defer rd.close()

	rep := &StatsReport{
		Account: rd.r.Meta.Account.ID, Encrypted: rd.r.Meta.Encryption.Enabled,
		Recipients: len(rd.r.Meta.Encryption.Recipients), Format: rd.r.Meta.FormatVersion,
		NamesShown: rd.names, Warnings: rd.warnings, Folders: []FolderStats{},
	}
	snaps, err := rd.db.Snapshots()
	if err != nil {
		return nil, err
	}
	rep.Snapshots.Total = len(snaps)
	for _, s := range snaps {
		if s.Pinned {
			rep.Snapshots.Pinned++
		}
		if s.Flagged {
			rep.Snapshots.Flagged++
		}
		if s.Forgotten {
			rep.Snapshots.Forgotten++
		}
	}
	if len(snaps) == 0 {
		rep.Note = "No snapshots yet. Run: imapvault backup"
		return rep, nil
	}
	rep.Snapshots.Oldest, rep.Snapshots.Newest = snaps[0].CreatedAt, snaps[len(snaps)-1].CreatedAt

	t, err := rd.db.StatsTotals()
	if err != nil {
		return nil, err
	}
	rep.Messages = StatsMessages{Unique: t.DistinctOpen, Positions: t.Positions, RawBytes: t.RawBytes, Unread: t.Unread, Starred: t.Starred}
	if t.DistinctOpen > 0 {
		rep.Messages.Dedupe = float64(t.Positions) / float64(t.DistinctOpen)
	}
	rep.History.Messages, rep.History.RawBytes = t.HistoryBlobs, t.HistoryBytes

	folders, err := rd.db.StatsFolders()
	if err != nil {
		return nil, err
	}
	for _, f := range folders {
		fs := FolderStats{ID: f.ID, Name: fmt.Sprintf("#%d", f.ID), SpecialUse: f.SpecialUse, Messages: f.Messages, RawBytes: f.RawBytes, Unread: f.Unread}
		if rd.names {
			if name, err := rd.opener.Open(f.NameSealed); err == nil {
				fs.Name = cleanText(string(name))
			}
		}
		rep.Folders = append(rep.Folders, fs)
	}

	if err := rd.growth(rep, now); err != nil {
		return nil, err
	}
	if err := rd.backups(rep); err != nil {
		return nil, err
	}
	if err := rd.alertsAndPrune(cfg, rep, now, opts.NoDisk); err != nil {
		return nil, err
	}
	if !opts.NoDisk {
		if err := rd.disk(rep); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

func (rd *reader) growth(rep *StatsReport, now time.Time) error {
	days, err := rd.db.StatsGrowth()
	if err != nil {
		return err
	}
	months := map[string]*StatsMonth{}
	cut7, cut30 := now.Add(-7*24*time.Hour).UTC().Format("2006-01-02"), now.Add(-30*24*time.Hour).UTC().Format("2006-01-02")
	for _, d := range days {
		m := d.Day[:7]
		if months[m] == nil {
			months[m] = &StatsMonth{Month: m}
		}
		months[m].Messages += d.Messages
		months[m].RawBytes += d.RawBytes
		if d.Day >= cut30 {
			rep.Growth.Last30Days.Messages += d.Messages
			rep.Growth.Last30Days.RawBytes += d.RawBytes
		}
		if d.Day >= cut7 {
			rep.Growth.Last7Days.Messages += d.Messages
			rep.Growth.Last7Days.RawBytes += d.RawBytes
		}
	}
	rep.Growth.Months = []StatsMonth{}
	for _, d := range days { // days are sorted, so months come out in order
		if m := months[d.Day[:7]]; m != nil {
			rep.Growth.Months = append(rep.Growth.Months, *m)
			delete(months, d.Day[:7])
		}
	}
	return nil
}

func (rd *reader) backups(rep *StatsReport) error {
	runs, err := rd.db.StatsRuns()
	if err != nil {
		return err
	}
	var ok []index.StatsRun
	var durations []int64
	for _, r := range runs {
		rep.Backups.Runs++
		switch r.Status {
		case "complete":
			rep.Backups.Complete++
			if !r.Finished.IsZero() {
				durations = append(durations, int64(r.Finished.Sub(r.Started).Seconds()))
			}
			ok = append(ok, r)
		case "partial":
			rep.Backups.Partial++
			ok = append(ok, r)
		case "failed":
			rep.Backups.Failed++
		}
	}
	for i, r := range ok {
		if i > 0 {
			rep.Backups.LongestGapSeconds = max(rep.Backups.LongestGapSeconds, int64(r.Started.Sub(ok[i-1].Started).Seconds()))
		}
	}
	if n := len(ok); n > 0 {
		last := ok[n-1]
		rep.Backups.LastGood = last.Finished
		if last.Finished.IsZero() {
			rep.Backups.LastGood = last.Started
		}
	}
	if len(durations) > 0 {
		slices.Sort(durations)
		rep.Backups.TypicalRunSeconds = durations[len(durations)/2]
	}
	return nil
}

// alertsAndPrune counts the alerts and works out what prune would remove now,
// with the same selection prune uses. It does not refuse while an alert is open
// (prune would): it says so in PruneBlocked.
func (rd *reader) alertsAndPrune(cfg *config.Config, rep *StatsReport, now time.Time, noDisk bool) error {
	alerts, err := rd.db.Alerts(true)
	if err != nil {
		return err
	}
	for _, a := range alerts {
		rep.Alerts.Total++
		if a.Acknowledged() {
			rep.Alerts.Acknowledged++
		} else {
			rep.Alerts.Open++
		}
	}
	if rep.Alerts.Open > 0 {
		rep.History.PruneBlocked = fmt.Sprintf("%d unacknowledged guard alert(s)", rep.Alerts.Open)
	}
	grace := cfg.Retention.DeletedGraceDur
	if grace <= 0 {
		return nil
	}
	rep.History.Grace = cfg.Retention.DeletedGrace
	pr, err := planPrune(rd.db, now.Add(-grace).UTC())
	if err != nil {
		return err
	}
	var store *objects.Store
	if !noDisk {
		if store, err = openStore(rd.r, ""); err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
	}
	for _, b := range pr.Blobs {
		n, err := rd.db.BlobSize(b)
		if err != nil {
			return err
		}
		rep.History.PruneMessages++
		rep.History.PruneRawBytes += n
		if store != nil {
			if sz, err := store.Size(objects.Key(b)); err == nil {
				rep.History.PruneDiskBytes += sz
			} else if !errors.Is(err, objects.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

// disk adds up the object files.
func (rd *reader) disk(rep *StatsReport) error {
	store, err := openStore(rd.r, "")
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	err = store.Walk(func(k objects.Key) error {
		sz, err := store.Size(k)
		if errors.Is(err, objects.ErrNotFound) {
			return nil
		}
		rep.Messages.DiskBytes += sz
		return err
	}, func(string) {})
	if err == nil && rep.Messages.DiskBytes > 0 {
		rep.Messages.Compression = float64(rep.Messages.RawBytes) / float64(rep.Messages.DiskBytes)
	}
	return err
}
