package main

import (
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/michele/imapvault/internal/engine"
)

func newAlertsCmd(opts *options) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "alerts",
		Short: "List guard alerts",
		Long: `Lists the alerts the mass-deletion guard raised, oldest first: only the ones nobody
acknowledged yet, or all of them with --all. Each alert names the snapshot the run wrote
(flagged) and the last good snapshot before it (pinned). prune refuses to run while an
alert is unacknowledged; "ack" acknowledges it.

The summary (which folders lost how many messages) is sealed in an encrypted
repository: it is shown when your private key is configured and left out otherwise.
Everything else needs no key.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			res, err := engine.Alerts(cmd.Context(), cfg, all)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				return writeJSON(out, res)
			}
			for _, w := range res.Warnings {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", clean(w))
			}
			if len(res.Alerts) == 0 {
				if all {
					_, _ = fmt.Fprintln(out, "No alerts.")
				} else {
					_, _ = fmt.Fprintln(out, "No unacknowledged alerts.")
				}
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tSNAPSHOT\tPINNED\tKIND\tCREATED\tSTATE\tSUMMARY")
			for _, a := range res.Alerts {
				state := "open"
				if a.AcknowledgedAt != nil {
					state = "acked " + fmtTime(*a.AcknowledgedAt)
				}
				pinned := "-"
				if a.PinnedSnapshot != 0 {
					pinned = strconv.FormatInt(a.PinnedSnapshot, 10)
				}
				_, _ = fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Snapshot, pinned, a.Kind, fmtTime(a.CreatedAt), state, clean(a.Summary))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include acknowledged alerts")
	return cmd
}

func newAckCmd(opts *options) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ack <id>... | --all",
		Short: "Acknowledge guard alerts",
		Long: `Marks alerts as dealt with. Until every alert is acknowledged prune refuses to run,
and the snapshot an alert pinned cannot be unpinned. Acknowledging an alert twice is
harmless. It needs no private key, and it fails at once while a backup is running.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ids := make([]int64, 0, len(args))
			for _, a := range args {
				id, err := strconv.ParseInt(a, 10, 64)
				if err != nil || id < 1 {
					return fmt.Errorf("%q is not an alert number (see `imapvault alerts`)", a)
				}
				ids = append(ids, id)
			}
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			done, err := engine.Ack(cmd.Context(), cfg, ids, all)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				if done == nil {
					done = []int64{}
				}
				return writeJSON(out, map[string]any{"acknowledged": done})
			}
			if len(done) == 0 {
				_, _ = fmt.Fprintln(out, "No unacknowledged alerts.")
				return nil
			}
			for _, id := range done {
				_, _ = fmt.Fprintf(out, "Acknowledged alert %d.\n", id)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "acknowledge every open alert")
	return cmd
}

func newPinCmd(opts *options, pin bool) *cobra.Command {
	use, short, long := "unpin <snapshot>", "Let retention treat a snapshot normally again", `Removes the pin. A snapshot that an unacknowledged guard alert pinned stays pinned
until you acknowledge the alert ("ack"). <snapshot> is a number, "latest" or
"latest~N". It needs no private key.`
	if pin {
		use, short, long = "pin <snapshot>", "Protect a snapshot from retention", `A pinned snapshot is always kept by forget and prune. The guard pins the last
good snapshot by itself when it raises an alert. <snapshot> is a number, "latest" or
"latest~N". It needs no private key.`
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Long:  long,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			res, err := engine.Pin(cmd.Context(), cfg, args[0], pin)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				return writeJSON(out, res)
			}
			if pin {
				_, _ = fmt.Fprintf(out, "Snapshot %d is pinned.\n", res.Snapshot)
			} else {
				_, _ = fmt.Fprintf(out, "Snapshot %d is no longer pinned.\n", res.Snapshot)
			}
			return nil
		},
	}
}
