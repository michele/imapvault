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
	var full bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the mailbox into a new snapshot",
		Long: `Reads the mailbox and writes a new snapshot to the repository. It is read-only on
the server: folders are opened with EXAMINE and messages fetched with BODY.PEEK[], so
no flag changes and nothing is ever deleted or moved there.

After the first run only what changed is read. A folder the server reports as
unchanged costs one EXAMINE. With CONDSTORE (most servers) new mail, flag changes and
deletions are found without listing the folder; without it, or whenever the pieces do
not add up, the folder is listed in full and compared with the repository. --full
forces that full comparison everywhere. Messages that are already stored are not
downloaded again, even when they moved to another folder or UIDVALIDITY changed. An
encrypted repository needs only the public keys from the config: backup never reads
your private key.

If the run stops partway (a folder cannot be read, the connection drops, Ctrl-C) after
at least one folder was backed up, the snapshot is written and marked partial: it holds
what was verified, folders that were not reached keep their previous state, and nothing
is recorded as deleted. The command exits with code 2; run it again to finish. If
nothing could be backed up the run fails (exit 1) and no snapshot is written. What was
downloaded is kept either way, so the next run resumes where this one stopped.

Before the snapshot is written the run is compared with the previous snapshot. If more
was deleted than guard.max_deleted_percent or guard.max_deleted_count allow, or a folder
vanished with mail in it that was not just moved, the guard trips: the new snapshot is
flagged, the previous one is pinned, an alert is recorded, guard.on_alert runs, and the
command exits with code 3 (also when the run was partial). Acknowledge it with "ack".

Exit codes: 0 ok, 1 error, 2 partial, 3 guard alert.`,
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
			bo := engine.BackupOptions{Full: full}
			if opts.verbose {
				bo.Logf = func(format string, args ...any) { _, _ = fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...) }
			}
			rep, err := engine.Backup(cmd.Context(), cfg, src, bo)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				if err := writeJSON(out, rep); err != nil {
					return err
				}
			} else {
				printBackup(out, rep, opts.account)
			}
			return backupExitError(rep)
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "list every folder in full instead of trusting what the server says changed")
	return cmd
}

// backupExitError turns a finished run into the exit code: 3 when the guard
// tripped (it wins over partial, because it needs a human), 2 when the run
// stopped partway, 0 otherwise.
func backupExitError(rep *engine.BackupReport) error {
	switch {
	case rep.Alerted():
		msg := fmt.Sprintf("ALERT: the mass-deletion guard tripped (alert %d). Look at it with `imapvault alerts`, then `imapvault ack %d`.", rep.Alert.ID, rep.Alert.ID)
		if rep.Partial() {
			msg += " The backup is also partial: run it again to finish."
		}
		return &exitCodeError{code: exitAlert, msg: msg}
	case rep.Partial():
		return &exitCodeError{code: exitPartial, msg: "the backup is partial: not every folder was read. Run it again to finish."}
	}
	return nil
}

// withAccount adds --account to a command hint when the user selected one.
func withAccount(account, cmd string) string {
	if account == "" {
		return "imapvault " + cmd
	}
	return "imapvault --account " + account + " " + cmd
}

func printAlert(out io.Writer, rep *engine.BackupReport, account string) {
	a := rep.Alert
	_, _ = fmt.Fprintf(out, "\nALERT %d (%s): %s\n", a.ID, a.Kind, clean(a.Summary))
	_, _ = fmt.Fprintf(out, "  snapshot %d is flagged", rep.Snapshot)
	if a.PinnedSnapshot != 0 {
		_, _ = fmt.Fprintf(out, "; snapshot %d, the last one before it, is pinned and kept", a.PinnedSnapshot)
		_, _ = fmt.Fprintf(out, "\n  see what changed:  %s", withAccount(account, fmt.Sprintf("diff %d %d", a.PinnedSnapshot, rep.Snapshot)))
	}
	_, _ = fmt.Fprintf(out, "\n  acknowledge:       %s\n", withAccount(account, fmt.Sprintf("ack %d", a.ID)))
	if a.HookError != "" {
		_, _ = fmt.Fprintf(out, "  guard.on_alert failed: %s\n", clean(a.HookError))
	}
}

func printBackup(out io.Writer, rep *engine.BackupReport, account string) {
	state := ""
	if rep.Partial() {
		state = " (partial)"
	}
	_, _ = fmt.Fprintf(out, "Snapshot %d%s: %d messages in %d folders\n", rep.Snapshot, state, rep.Messages, len(rep.Folders))
	_, _ = fmt.Fprintf(out, "  messages  +%d new, -%d deleted, ~%d flag changes\n", rep.New, rep.Closed, rep.Reflagged)
	_, _ = fmt.Fprintf(out, "  storage   %d blob(s) written (%s), %d already stored\n", rep.BlobsAdded, humanBytes(rep.BytesRaw), rep.BlobsReused)
	modes := map[string]int{}
	for _, f := range rep.Folders {
		modes[f.Mode]++
	}
	read := fmt.Sprintf("%d body(ies) downloaded", rep.Fetched)
	if rep.SkippedByEmailID > 0 {
		read += fmt.Sprintf(", %d skipped (EMAILID)", rep.SkippedByEmailID)
	}
	_, _ = fmt.Fprintf(out, "  read      %s; folders: %d unchanged, %d incremental, %d full\n", read, modes["unchanged"], modes["incremental"], modes["full"])
	if rep.NewFolders > 0 || rep.GoneFolders > 0 {
		_, _ = fmt.Fprintf(out, "  folders   +%d new, -%d gone\n", rep.NewFolders, rep.GoneFolders)
	}
	for _, w := range rep.Warnings {
		_, _ = fmt.Fprintf(out, "warning: %s\n", clean(w))
	}
	if rep.Alerted() {
		printAlert(out, rep, account)
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
