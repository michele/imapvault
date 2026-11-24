package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/mail"
	"github.com/michele/imapvault/internal/plan"
	"github.com/michele/imapvault/internal/repo"
)

// RunOptions drive Run. The loaders and the dialler are functions so that every
// job starts from a fresh config (a rotated password file is picked up without a
// restart) and a fresh connection (a dead one cannot wedge the loop).
type RunOptions struct {
	Schedule plan.Schedule
	// LoadOnline returns the config with the IMAP password, for a backup.
	LoadOnline func() (*config.Config, error)
	// LoadOffline returns the config without needing the password, for forget,
	// prune and the alert count.
	LoadOffline func() (*config.Config, error)
	// Dial connects to the server; nil means Dial.
	Dial func(ctx context.Context, cfg *config.Config) (mail.Source, error)
	// Backup is the template for each backup (progress, log, clock).
	Backup BackupOptions
	// Logf receives one line per event. Folder names, counts and times only.
	Logf func(format string, args ...any)
	// Now and Sleep are the clock; tests replace them. Sleep returns early with
	// ctx.Err() when ctx ends.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

func sleepFor(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run is the long-running scheduler behind `imapvault run`: it backs up on an
// interval, applies the retention policy ("forget") now and then, and prunes only
// if asked to, one job at a time, so the repository lock is held only while a job
// runs and a manual command can slip in between. It returns nil when ctx ends and
// an error only when it cannot start (a config or repository that does not open);
// after that a failed job is logged and retried, never fatal.
//
// It is read-only on the server (it only calls Backup), never resolves a private
// key (rule 8), and prune is the existing Prune, so it still honours pins, the
// grace period and unacknowledged alerts (rule 3).
func Run(ctx context.Context, o RunOptions) error {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepFor
	}
	if o.Dial == nil {
		o.Dial = Dial
	}
	if o.Backup.Now == nil {
		o.Backup.Now = o.Now
	}

	// Find a bad deployment now, so it shows up as a failed start and not as a
	// daemon that quietly does nothing.
	cfg, err := o.LoadOffline()
	if err != nil {
		return err
	}
	acct := accountOf(cfg)
	if _, err := repo.Open(cfg.Repository.Path, &acct); err != nil {
		return err
	}
	if _, err := o.LoadOnline(); err != nil {
		return err
	}

	r := &runner{o: o, st: plan.RunState{Started: o.Now()}}
	o.Logf("run: started: backup every %s, forget %s, prune %s", o.Schedule.Every, every(o.Schedule.ForgetEvery), every(o.Schedule.PruneEvery))
	r.remindAboutAlerts()
	for {
		if ctx.Err() != nil {
			o.Logf("run: stopping")
			return nil
		}
		job, wait := plan.NextJob(o.Schedule, r.st, o.Now())
		if wait > 0 {
			if o.Sleep(ctx, wait) != nil {
				o.Logf("run: stopping")
				return nil
			}
			continue
		}
		switch job {
		case plan.JobBackup:
			r.backup(ctx)
		case plan.JobForget:
			r.forget(ctx)
		case plan.JobPrune:
			r.prune(ctx)
		}
	}
}

func every(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	return "every " + d.String()
}

type runner struct {
	o       RunOptions
	st      plan.RunState
	lastErr string
}

func (r *runner) logNext() {
	job, wait := plan.NextJob(r.o.Schedule, r.st, r.o.Now())
	if wait > 0 {
		r.o.Logf("run: next %s in %s", job, wait.Round(time.Second))
	}
}

func (r *runner) backup(ctx context.Context) {
	o := r.o
	start := o.Now()
	r.st.LastBackupStart = start
	o.Logf("run: backup started")
	rep, err := r.doBackup(ctx)
	end := o.Now()
	r.st.LastBackupEnd = end
	if ctx.Err() != nil {
		return // stopped from outside; Run says so on the next turn
	}
	switch {
	case errors.Is(err, repo.ErrLocked):
		r.st.Busy = true
		o.Logf("run: backup busy: another imapvault command holds the repository; retrying in %s", time.Minute)
		return
	case err != nil:
		r.st.Busy = false
		r.st.Failures++
		r.lastErr = cleanText(err.Error())
		_, wait := plan.NextJob(o.Schedule, r.st, end)
		o.Logf("run: backup failed: %s; retrying in %s", r.lastErr, wait.Round(time.Second))
		r.tellIfStale(ctx)
		return
	}
	r.st.Busy = false
	r.st.Failures = 0
	r.st.StaleNotified = false
	r.st.LastBackupOK = end
	took := end.Sub(start).Round(time.Second)
	state := "done"
	if rep.Partial() {
		state = "partial (the next run continues)"
	}
	o.Logf("run: backup %s: snapshot %d, %d messages, +%d new, -%d deleted, ~%d flag changes (%s)",
		state, rep.Snapshot, rep.Messages, rep.New, rep.Closed, rep.Reflagged, took)
	for _, w := range rep.Warnings {
		o.Logf("run: warning: %s", cleanText(w))
	}
	if rep.Alerted() {
		o.Logf("run: GUARD ALERT %d (%s): %s. Snapshot %d is flagged and %d is pinned; read it and acknowledge it from a machine that holds the key",
			rep.Alert.ID, rep.Alert.Kind, cleanText(rep.Alert.Summary), rep.Snapshot, rep.Alert.PinnedSnapshot)
		if rep.Alert.HookError != "" {
			o.Logf("run: notification failed: %s", cleanText(rep.Alert.HookError))
		}
	}
	r.logNext()
}

func (r *runner) doBackup(ctx context.Context) (*BackupReport, error) {
	cfg, err := r.o.LoadOnline()
	if err != nil {
		return nil, err
	}
	src, err := r.o.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	return Backup(ctx, cfg, src, r.o.Backup)
}

// tellIfStale sends the "no good backup for a while" notice, once per streak.
func (r *runner) tellIfStale(ctx context.Context) {
	if !plan.Stale(r.o.Schedule, r.st, r.o.Now()) {
		return
	}
	r.st.StaleNotified = true
	since := r.st.LastBackupOK
	if since.IsZero() {
		since = r.st.Started
	}
	summary := fmt.Sprintf("no successful backup for %s; last error: %s", r.o.Now().Sub(since).Round(time.Minute), r.lastErr)
	r.o.Logf("run: WARNING: %s", summary)
	cfg, err := r.o.LoadOffline()
	if err != nil {
		r.o.Logf("run: notification failed: %s", cleanText(err.Error()))
		return
	}
	cmdErr, urlErr := Notify(context.WithoutCancel(ctx), cfg, Notification{Event: EventStale, Summary: summary})
	for _, e := range []error{cmdErr, urlErr} {
		if e != nil {
			r.o.Logf("run: notification failed: %s", cleanText(e.Error()))
		}
	}
}

func (r *runner) forget(ctx context.Context) {
	o := r.o
	r.st.LastForget = o.Now() // whatever happens, so a failing forget cannot spin
	cfg, err := o.LoadOffline()
	if err == nil {
		var rep *ForgetReport
		if rep, err = Forget(ctx, cfg, RetentionOptions{}); err == nil {
			o.Logf("run: forget: kept %d snapshot(s), forgot %d", len(rep.Kept), len(rep.Forgotten))
			r.remindAboutAlerts()
			r.logNext()
			return
		}
	}
	r.jobFailed("forget", err)
}

func (r *runner) prune(ctx context.Context) {
	o := r.o
	r.st.LastPrune = o.Now()
	cfg, err := o.LoadOffline()
	if err == nil {
		var rep *PruneReport
		if rep, err = Prune(ctx, cfg, RetentionOptions{}); err == nil {
			o.Logf("run: prune: removed %d entries and %d message(s) (%s), %d snapshot(s)", rep.Entries, rep.Blobs, humanBytes(rep.BlobBytes), rep.Snapshots)
			r.logNext()
			return
		}
	}
	r.jobFailed("prune", err)
}

func (r *runner) jobFailed(job string, err error) {
	switch {
	case errors.Is(err, repo.ErrLocked):
		r.o.Logf("run: %s skipped: another imapvault command holds the repository; it is tried again at the next interval", job)
	case errors.Is(err, ErrUnackedAlerts):
		r.o.Logf("run: %s refused: %s", job, cleanText(err.Error()))
	default:
		r.o.Logf("run: %s failed: %s", job, cleanText(err.Error()))
	}
}

func (r *runner) remindAboutAlerts() {
	cfg, err := r.o.LoadOffline()
	if err != nil {
		return
	}
	n, err := UnackedAlerts(cfg)
	if err != nil {
		if !errors.Is(err, repo.ErrLocked) {
			r.o.Logf("run: could not count unacknowledged alerts: %s", cleanText(err.Error()))
		}
		return
	}
	if n > 0 {
		r.o.Logf("run: %d unacknowledged guard alert(s): run `imapvault alerts` on a machine that holds the key, then `imapvault ack`; prune stays off until then", n)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
