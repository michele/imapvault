package main

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/engine"
	"github.com/michele/imapvault/internal/plan"
)

// minRunEvery is the shortest --every allowed. Each backup writes a copy of the
// index, and the mass-deletion guard compares each run with the one before, so
// very frequent runs are not free. A variable so the integration test can use a
// short one.
var minRunEvery = 5 * time.Minute

// parseInterval reads an interval: a Go duration (90s, 15m, 1h30m), days or weeks
// (7d, 2w), or 0 for "off".
func parseInterval(s string) (time.Duration, error) {
	if s == "0" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, nil
	}
	if d, err := config.ParseDuration(s); err == nil {
		if s[len(s)-1] == 'd' || s[len(s)-1] == 'w' { // the month and year units are for retention, not for a schedule
			return d, nil
		}
	}
	return 0, fmt.Errorf("invalid interval %q (want e.g. 15m, 1h, 24h, 7d, or 0 to turn it off)", s)
}

func newRunCmd(opts *options) *cobra.Command {
	var every, forgetEvery, pruneEvery, staleAfter, progress, minEvery string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Keep running: back up on an interval, forget old snapshots, optionally prune",
		Long: `Runs until it is stopped (Ctrl-C or SIGTERM), one job at a time:

  backup   at once, then every --every (counted from the start of the previous run, so a
           first backup that takes hours is followed straight away by an incremental one)
  forget   after the first good backup, then every --forget-every (it only marks snapshots)
  prune    only if --prune-every is set; it is the usual prune and still honours pins, the
           deleted-mail grace period and unacknowledged alerts

Each job holds the repository lock only while it runs, so a manual command works between
jobs and fails at once, as always, during one. The config and the IMAP connection are made
fresh for every backup, so a rotated password file is picked up without a restart.

A failed backup is retried after 30 s, 1 m, 2 m, then every 5 m (never later than --every).
After a guard alert it keeps going: the alert is logged, guard.on_alert and
guard.on_alert_url are notified, and prune stays refused until you acknowledge it. If no
backup has succeeded for --stale-after, one notification goes out the same way.

It writes one line per event to stderr, with no timestamps (use "docker logs -t"). It exits
0 when stopped, and 1 at once if the config or repository cannot be opened. Intervals are
like 15m, 1h, 24h, 7d; 0 turns forget, prune or the stale warning off.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sched := plan.Schedule{}
			for _, f := range []struct {
				flag string
				in   string
				dst  *time.Duration
			}{
				{"--every", every, &sched.Every}, {"--forget-every", forgetEvery, &sched.ForgetEvery},
				{"--prune-every", pruneEvery, &sched.PruneEvery}, {"--stale-after", staleAfter, &sched.StaleAfter},
			} {
				d, err := parseInterval(f.in)
				if err != nil {
					return fmt.Errorf("%s: %w", f.flag, err)
				}
				*f.dst = d
			}
			floor, err := parseInterval(minEvery)
			if err != nil {
				return fmt.Errorf("--min-every: %w", err)
			}
			if err := sched.Validate(floor); err != nil {
				return err
			}
			if progress != "log" && progress != "none" {
				return fmt.Errorf("--progress %q: run writes a log, so it is log or none", progress)
			}

			errw := cmd.ErrOrStderr()
			ro := engine.RunOptions{
				Schedule: sched,
				Logf:     func(format string, args ...any) { _, _ = fmt.Fprintf(errw, format+"\n", args...) },
			}
			// Password-file warnings are shown for the first load only; a run
			// that lasts weeks would repeat them every hour.
			loads := 0
			ro.LoadOnline = func() (*config.Config, error) {
				o := *opts
				if loads++; loads > 1 {
					o.stderr = io.Discard
				}
				return loadOnlineConfig(&o)
			}
			ro.LoadOffline = func() (*config.Config, error) { return loadOfflineConfig(opts) }
			if progress == "log" {
				ro.Backup.Progress = newProgress(errw, progressLog, time.Now).Event
			}
			if opts.verbose {
				ro.Backup.Logf = func(format string, args ...any) { _, _ = fmt.Fprintf(errw, format+"\n", args...) }
			}
			return engine.Run(cmd.Context(), ro)
		},
	}
	cmd.Flags().StringVar(&every, "every", "1h", "time between backups (at least "+minRunEvery.String()+")")
	cmd.Flags().StringVar(&forgetEvery, "forget-every", "24h", "apply the retention policy this often; 0 turns it off")
	cmd.Flags().StringVar(&pruneEvery, "prune-every", "0", "also prune this often (needs --forget-every); 0, the default, never prunes")
	cmd.Flags().StringVar(&staleAfter, "stale-after", "24h", "send a notification when no backup has succeeded for this long; 0 turns it off")
	cmd.Flags().StringVar(&progress, "progress", "log", "per-folder progress lines: log or none")
	// For tests and demos only: lowers the floor of --every. Hidden on purpose.
	cmd.Flags().StringVar(&minEvery, "min-every", minRunEvery.String(), "")
	_ = cmd.Flags().MarkHidden("min-every")
	return cmd
}
