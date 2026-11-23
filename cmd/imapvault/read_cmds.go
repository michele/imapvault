package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/michele/imapvault/internal/engine"
)

func newSnapshotsCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "snapshots",
		Short: "List snapshots",
		Long: `Lists the snapshots in the repository, oldest first. It reads only plaintext
columns, so an encrypted repository needs no private key here.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			snaps, err := engine.Snapshots(cmd.Context(), cfg, false)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				if snaps == nil {
					snaps = []engine.SnapshotInfo{}
				}
				return writeJSON(out, snaps)
			}
			if len(snaps) == 0 {
				_, _ = fmt.Fprintln(out, "No snapshots yet. Run: imapvault backup")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tCREATED\tFOLDERS\tMESSAGES\tNOTES")
			for _, s := range snaps {
				var notes []string
				if s.Pinned {
					notes = append(notes, "pinned")
				}
				if s.Flagged {
					notes = append(notes, "flagged")
				}
				if s.Forgotten {
					notes = append(notes, "forgotten")
				}
				_, _ = fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%s\n", s.ID, fmtTime(s.CreatedAt), s.Folders, s.Messages, strings.Join(notes, ","))
			}
			return tw.Flush()
		},
	}
}

func newLsCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "ls <snapshot> [folder]",
		Short: "List the folders of a snapshot, or the messages in one folder",
		Long: `<snapshot> is a number from "snapshots" or "latest". Without a folder it lists the
folders and their message counts; with one it lists that folder's messages (UID,
flags, size, date, sender, subject and the SHA-256 that "show" takes). It never prints
message bodies.

Folder names and message details are sealed in an encrypted repository, so this
needs your private key (encryption.identity_file, identity_command or
IMAPVAULT_IDENTITY).`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			folder := ""
			if len(args) == 2 {
				folder = args[1]
			}
			res, err := engine.Ls(cmd.Context(), cfg, args[0], folder)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, w := range res.Warnings {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			if opts.json {
				return writeJSON(out, res)
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			if folder == "" {
				_, _ = fmt.Fprintf(out, "Snapshot %d (%s)\n", res.Snapshot, fmtTime(res.CreatedAt))
				_, _ = fmt.Fprintln(tw, "FOLDER\tMESSAGES\tROLE")
				for _, f := range res.Folders {
					_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\n", clean(f.Name), f.Messages, f.SpecialUse)
				}
				return tw.Flush()
			}
			_, _ = fmt.Fprintf(out, "Snapshot %d (%s), folder %s\n", res.Snapshot, fmtTime(res.CreatedAt), clean(res.Folder))
			_, _ = fmt.Fprintln(tw, "UID\tFLAGS\tSIZE\tDATE\tFROM\tSUBJECT\tSHA256")
			for _, m := range res.Messages {
				_, _ = fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\t%s\t%s\n", m.UID, clean(m.Flags), m.Size,
					m.Date.Local().Format("2006-01-02 15:04"), clean(m.From), clean(m.Subject), m.SHA256[:12])
			}
			return tw.Flush()
		},
	}
}

func newShowCmd(opts *options) *cobra.Command {
	var raw bool
	cmd := &cobra.Command{
		Use:   "show <hash>",
		Short: "Show one message's details, or write its raw bytes with --raw",
		Long: `<hash> is the SHA-256 shown by "ls", or any unambiguous prefix of at least 8
characters. By default it prints the message's sender, recipients, subject, date and
Message-ID. With --raw it writes the exact original message (.eml) to standard output
and nothing else, so it can be redirected to a file. Needs your private key when the
repository is encrypted.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			res, err := engine.Show(cmd.Context(), cfg, args[0], raw)
			if err != nil {
				return err
			}
			for _, w := range res.Warnings {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			out := cmd.OutOrStdout()
			if raw {
				_, err := out.Write(res.Raw)
				return err
			}
			if opts.json {
				return writeJSON(out, res)
			}
			m := res.Meta
			_, _ = fmt.Fprintf(out, "SHA-256     %s\nSize        %d bytes\nMessage-ID  %s\nDate        %s\nFrom        %s\nTo          %s\nSubject     %s\nReceived    %s\n",
				res.SHA256, res.Size, clean(m.MessageID), clean(m.Date), clean(m.From), clean(m.To), clean(m.Subject), fmtTime(m.InternalDate))
			return nil
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "write the original message bytes to stdout")
	return cmd
}
