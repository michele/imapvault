package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/michele/imapvault/internal/engine"
)

func newDiffCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "diff <older> <newer>",
		Short: "Count what changed between two snapshots, per folder",
		Long: `Each snapshot is a number from "snapshots", "latest", or "latest~N" (N snapshots
before the latest). Give the older one first.

For every folder with changes it counts messages added, deleted, moved (the same
message left one folder and arrived in another), re-flagged (flags changed) and
renumbered (the same message in the same folder under a new UID, as after a
UIDVALIDITY reset). A renamed folder shows as moves out of the old name and into the
new one. Messages are never printed, only counts.

It reads only positions, hashes and flags, so it works without the private key; folder
names are shown when one is configured and as #<number> otherwise.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			res, err := engine.Diff(cmd.Context(), cfg, args[0], args[1])
			if err != nil {
				return err
			}
			for _, w := range res.Warnings {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			out := cmd.OutOrStdout()
			if opts.json {
				return writeJSON(out, res)
			}
			_, _ = fmt.Fprintf(out, "Snapshot %d (%s) -> snapshot %d (%s)\n", res.From, fmtTime(res.FromTime), res.To, fmtTime(res.ToTime))
			if res.Empty() {
				_, _ = fmt.Fprintln(out, "No differences.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "FOLDER\tADDED\tDELETED\tMOVED IN\tMOVED OUT\tREFLAGGED\tRENUMBERED")
			for _, f := range res.Folders {
				_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\n", clean(f.Name), f.Added, f.Deleted, f.MovedIn, f.MovedOut, f.Reflagged, f.Renumbered)
			}
			t := res.Totals
			_, _ = fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%d\t%d\t%d\t%d\n", t.Added, t.Deleted, t.MovedIn, t.MovedOut, t.Reflagged, t.Renumbered)
			return tw.Flush()
		},
	}
}
