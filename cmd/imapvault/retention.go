package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/michele/imapvault/internal/engine"
)

func newForgetCmd(opts *options) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "forget",
		Short: "Apply the retention policy: mark snapshots that are no longer kept as forgotten",
		Long: `Applies [retention] (keep_last, keep_hourly, keep_daily, keep_weekly, keep_monthly,
keep_yearly) and marks the snapshots it does not keep as forgotten. Nothing is deleted:
a forgotten snapshot stays addressable by number (ls, show, diff, restore) and is listed
by "snapshots --all" until prune removes what it showed. Pinned snapshots and the latest
one are always kept. A policy that keeps nothing is refused.

It needs no private key, and it fails at once while a backup is running. It works even
while guard alerts are unacknowledged; prune does not.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			rep, err := engine.Forget(cmd.Context(), cfg, engine.RetentionOptions{DryRun: dryRun})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				return writeJSON(out, rep)
			}
			printForget(out, rep, opts.account)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be forgotten and change nothing")
	return cmd
}

func printForget(out io.Writer, rep *engine.ForgetReport, account string) {
	if rep.DryRun {
		_, _ = fmt.Fprintln(out, "Dry run: nothing is changed.")
	}
	if len(rep.Verdicts) > 0 {
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "SNAPSHOT\tACTION\tWHY")
		for _, v := range rep.Verdicts {
			action := "forget"
			if v.Keep {
				action = "keep"
			}
			_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\n", v.Snapshot, action, strings.Join(v.Reasons, ", "))
		}
		_ = tw.Flush()
		_, _ = fmt.Fprintln(out)
	}
	if len(rep.Forgotten) == 0 {
		_, _ = fmt.Fprintf(out, "Nothing to forget: the policy keeps all %d snapshots.\n", len(rep.Kept))
		return
	}
	verb := "Forgot"
	if rep.DryRun {
		verb = "would forget"
	}
	ids := make([]string, len(rep.Forgotten))
	for i, id := range rep.Forgotten {
		ids[i] = strconv.FormatInt(id, 10)
	}
	line := fmt.Sprintf("%s %s (%s).", verb, plural(len(rep.Forgotten), "snapshot"), strings.Join(ids, ", "))
	if rep.DryRun {
		line = "Dry run: " + line
	} else {
		line += " Nothing was deleted: " + withAccount(account, "prune") + " frees the space."
	}
	_, _ = fmt.Fprintln(out, line)
}

func newPruneCmd(opts *options) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove what no kept snapshot needs and the grace period no longer holds",
		Long: `The only command that removes data from the repository. It removes
  - index entries that no kept snapshot shows and that were deleted from the server
    longer ago than [retention] deleted_grace,
  - the messages (object files) only those entries referenced,
  - forgotten snapshots that are no longer complete, and folders nothing shows any more,
  - object files no index row references and leftovers of interrupted writes (tmp/).
Pinned snapshots, the latest snapshot and everything deleted inside the grace period are
kept. Prune refuses to run while a guard alert is unacknowledged ("alerts", "ack").

The index is committed first and the files removed after, so an interruption leaves
orphan files that the next prune removes, never a dangling reference. It needs no private
key and fails at once while a backup is running. Use --dry-run to see the numbers first.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			rep, err := engine.Prune(cmd.Context(), cfg, engine.RetentionOptions{DryRun: dryRun})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				return writeJSON(out, rep)
			}
			printPrune(out, rep)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be removed and change nothing")
	return cmd
}

func printPrune(out io.Writer, rep *engine.PruneReport) {
	verb := "Removed"
	if rep.DryRun {
		verb = "Would remove"
		_, _ = fmt.Fprintln(out, "Dry run: nothing is changed.")
	}
	_, _ = fmt.Fprintf(out, "%s:\n", verb)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "  Index entries\t%d\n", rep.Entries)
	_, _ = fmt.Fprintf(tw, "  Messages\t%d\t(%s of mail, %s on disk)\n", rep.Blobs, humanBytes(rep.BlobBytes), humanBytes(rep.DiskBytes))
	_, _ = fmt.Fprintf(tw, "  Forgotten snapshots\t%d\t(no longer complete)\n", rep.Snapshots)
	_, _ = fmt.Fprintf(tw, "  Folders\t%d\n", rep.Folders)
	_, _ = fmt.Fprintf(tw, "  Orphan objects\t%d\t(%s)\n", rep.Orphans, humanBytes(rep.OrphanBytes))
	_, _ = fmt.Fprintf(tw, "  tmp leftovers\t%d\n", rep.TmpFiles)
	_ = tw.Flush()
	_, _ = fmt.Fprintf(out, "Mail deleted from the server after %s is kept (deleted_grace).\n", fmtTime(rep.Cutoff))
	if rep.IndexBackup != "" {
		_, _ = fmt.Fprintf(out, "Index backup: index-backups/%s\n", rep.IndexBackup)
	}
	for _, w := range rep.Warnings {
		_, _ = fmt.Fprintf(out, "warning: %s\n", clean(w))
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
