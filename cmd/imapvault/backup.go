package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/engine"
)

// loadOnlineConfig is loadOfflineConfig plus the IMAP password: for commands that
// connect to the server. Password warnings (a password file others can read) go
// to stderr.
func loadOnlineConfig(opts *options) (*config.Config, error) {
	path, account := opts.configPath, opts.account
	if account != "" {
		p, err := config.AccountConfigPath(os.Getenv, account)
		if err != nil {
			return nil, err
		}
		path = p
	}
	cfg, err := config.LoadAccount(path, account)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, w := range cfg.PasswordWarnings {
		_, _ = fmt.Fprintln(opts.stderr, "warning:", w)
	}
	return cfg, nil
}

// clean makes a string safe to print to a terminal: control characters (escape
// sequences in a hostile subject line) become '?'. JSON output is not cleaned.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

func newBackupCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "backup",
		Short: "Back up the mailbox into a new snapshot",
		Long: `Reads the mailbox and writes a new snapshot to the repository. It is read-only on
the server: folders are opened with EXAMINE and messages fetched with BODY.PEEK[], so
no flag changes and nothing is ever deleted or moved there.

Every run lists each selected folder in full and compares it with the repository, so
new mail, deleted mail and flag changes are all recorded. Messages that are already
stored are not downloaded again. An encrypted repository needs only the public keys
from the config: backup never reads your private key.

If the run fails, folders already finished stay in the repository but no snapshot is
written; the next run carries on and reuses what was stored.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadOnlineConfig(opts)
			if err != nil {
				return err
			}
			src, err := engine.Dial(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			bo := engine.BackupOptions{}
			if opts.verbose {
				bo.Logf = func(format string, args ...any) { _, _ = fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...) }
			}
			rep, err := engine.Backup(cmd.Context(), cfg, src, bo)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				return writeJSON(out, rep)
			}
			printBackup(out, rep)
			return nil
		},
	}
}

func printBackup(out io.Writer, rep *engine.BackupReport) {
	_, _ = fmt.Fprintf(out, "Snapshot %d: %d messages in %d folders\n", rep.Snapshot, rep.Messages, len(rep.Folders))
	_, _ = fmt.Fprintf(out, "  messages  +%d new, -%d deleted, ~%d flag changes\n", rep.New, rep.Closed, rep.Reflagged)
	_, _ = fmt.Fprintf(out, "  storage   %d blob(s) written (%s), %d already stored\n", rep.BlobsAdded, humanBytes(rep.BytesRaw), rep.BlobsReused)
	if rep.NewFolders > 0 || rep.GoneFolders > 0 {
		_, _ = fmt.Fprintf(out, "  folders   +%d new, -%d gone\n", rep.NewFolders, rep.GoneFolders)
	}
	for _, w := range rep.Warnings {
		_, _ = fmt.Fprintf(out, "warning: %s\n", w)
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

func newDoctorCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Connect and show what the server supports and what would be backed up",
		Long: `Logs in (read-only) and prints the server's capabilities that matter to
imapvault and every folder with its message count, marking which ones the current
folders.include / folders.exclude would back up. It needs no repository.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadOnlineConfig(opts)
			if err != nil {
				return err
			}
			src, err := engine.Dial(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			rep, err := engine.Doctor(cmd.Context(), cfg, src)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				return writeJSON(out, rep)
			}
			_, _ = fmt.Fprintln(out, "Server features:")
			for _, f := range rep.Features {
				mark := "no "
				if f.Supported {
					mark = "yes"
				}
				_, _ = fmt.Fprintf(out, "  %-12s %s  %s\n", f.Name, mark, f.Use)
			}
			_, _ = fmt.Fprintln(out, "\nFolders:")
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "  FOLDER\tMESSAGES\tROLE\tBACKED UP")
			for _, f := range rep.Folders {
				sel := "yes"
				switch {
				case f.NoSelect:
					sel = "- (container, holds no mail)"
				case !f.Selected:
					sel = "no (excluded)"
				}
				_, _ = fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\n", clean(f.Name), f.Messages, f.SpecialUse, sel)
			}
			_ = tw.Flush()
			_, _ = fmt.Fprintf(out, "\n%d message(s) in the folders that would be backed up.\n", rep.Selected)
			return nil
		},
	}
}

func fmtTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") }
