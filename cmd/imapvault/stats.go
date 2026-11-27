package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/michele/imapvault/internal/engine"
)

func newStatsCmd(opts *options) *cobra.Command {
	var names, noDisk bool
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Insights into the repository: size, growth, backup health, what prune would free",
		Long: `Summarises the repository: how many messages it holds (and how many places they
occupy, which shows how much de-duplication saves), raw and on-disk size, growth over the
last 7 and 30 days, how the backup runs are doing (complete, partial, failed, the last good
one, the longest gap), what is kept only because it is history, what prune would remove
now, guard alerts, and per-folder numbers.

It reads plaintext columns and the object files only, so an encrypted repository needs no
private key. Folder names are sealed: they show as #id unless you pass --names and the key
is configured. It never shows a subject or a sender. Like the other read commands it fails
at once while a backup holds the repository lock. --no-disk skips measuring the object
files, the only slow part on a large repository. --json prints the same data.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			rep, err := engine.Stats(cmd.Context(), cfg, engine.StatsOptions{Names: names, NoDisk: noDisk})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, w := range rep.Warnings {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", clean(w))
			}
			if opts.json {
				return writeJSON(out, rep)
			}
			printStats(out, rep, time.Now())
			return nil
		},
	}
	cmd.Flags().BoolVar(&names, "names", false, "show folder names (needs the private key on an encrypted repository)")
	cmd.Flags().BoolVar(&noDisk, "no-disk", false, "do not measure the object files on disk")
	return cmd
}

// msgs writes "1 message" or "1,204 messages".
func msgs(n int) string {
	if n == 1 {
		return "1 message"
	}
	return commas(n) + " messages"
}

// ago writes "2h00m ago" for a recent time and the date for an older one.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	if d := now.Sub(t); d >= 0 && d < 48*time.Hour {
		return formatDuration(d) + " ago"
	}
	return t.Format("2006-01-02")
}

func printStats(out io.Writer, r *engine.StatsReport, now time.Time) {
	if r.Note != "" {
		_, _ = fmt.Fprintln(out, r.Note)
		return
	}
	row := func(label, format string, args ...any) {
		_, _ = fmt.Fprintf(out, "%-12s %s\n", label, fmt.Sprintf(format, args...))
	}
	enc := "not encrypted"
	if r.Encrypted {
		enc = "encrypted to " + plural(r.Recipients, "key")
	}
	row("Repository", "%s   %s, format v%d", r.Account, enc, r.Format)
	s := r.Snapshots
	row("Snapshots", "%d (%d pinned, %d flagged, %d forgotten); newest %s, oldest %s", s.Total, s.Pinned, s.Flagged, s.Forgotten, ago(now, s.Newest), s.Oldest.Format("2006-01-02"))
	m := r.Messages
	line := fmt.Sprintf("%s unique, %s positions (%.2fx), %s raw", commas(m.Unique), commas(m.Positions), m.Dedupe, humanBytes(m.RawBytes))
	if m.DiskBytes > 0 {
		line += fmt.Sprintf(", %s on disk (%.1fx smaller)", humanBytes(m.DiskBytes), m.Compression)
	}
	row("Messages", "%s", line)
	if m.Positions > 0 {
		row("Unread", "%d%% of current mail; %s starred", m.Unread*100/m.Positions, commas(m.Starred))
	}
	g := r.Growth
	row("Growth", "+%s and +%s messages in the last 30 days (+%d/day); last 7 days +%s",
		humanBytes(g.Last30Days.RawBytes), commas(g.Last30Days.Messages), g.Last30Days.Messages/30, humanBytes(g.Last7Days.RawBytes))
	b := r.Backups
	row("Backups", "%d runs: %d complete, %d partial, %d failed; last good %s; longest gap %s; typical run %s",
		b.Runs, b.Complete, b.Partial, b.Failed, ago(now, b.LastGood), formatDuration(time.Duration(b.LongestGapSeconds)*time.Second), formatDuration(time.Duration(b.TypicalRunSeconds)*time.Second))
	h := r.History
	row("History", "%s kept only by history (%s): gone from the server, still restorable", msgs(h.Messages), humanBytes(h.RawBytes))
	switch {
	case h.Grace == "":
		row("", "prune: no retention.deleted_grace is set, so nothing is worked out")
	case h.PruneBlocked != "":
		row("", "prune is blocked: %s; it would otherwise free %s (%s on disk)", h.PruneBlocked, msgs(h.PruneMessages), humanBytes(h.PruneDiskBytes))
	default:
		row("", "prune would free %s, %s on disk (grace %s, pins and open alerts are honoured)", msgs(h.PruneMessages), humanBytes(h.PruneDiskBytes), h.Grace)
	}
	a := r.Alerts
	row("Alerts", "%d acknowledged, %d open", a.Acknowledged, a.Open)
	if len(r.Folders) > 0 {
		_, _ = fmt.Fprintln(out, "\nFolders")
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  FOLDER\tMESSAGES\tRAW\tUNREAD")
		for _, f := range r.Folders {
			name := clean(f.Name)
			if strings.HasPrefix(name, "#") && f.SpecialUse != "" {
				name += " (" + f.SpecialUse + ")"
			}
			unread := "-"
			if f.Messages > 0 {
				unread = fmt.Sprintf("%d%%", f.Unread*100/f.Messages)
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", name, commas(f.Messages), humanBytes(f.RawBytes), unread)
		}
		_ = tw.Flush()
	}
}
