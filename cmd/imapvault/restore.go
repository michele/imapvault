package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/engine"
)

// dialTarget and stdinIsTerminal are variables so tests can stand in a fake
// server and a terminal. Nothing else about the approval flow is injectable.
var (
	dialTarget      = engine.DialTarget
	stdinIsTerminal = isTerminal
)

// isTerminal reports whether r is an interactive terminal. /dev/null is a
// character device too but nobody is there to answer, so it does not count: a
// cron job started with </dev/null takes the non-interactive path.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}

type restoreFlags struct {
	snapshot     string
	deletedSince string
	folder       string
	message      string
	to           string
	into         string
	inPlace      bool
	dryRun       bool
	confirm      string
}

func newRestoreCmd(opts *options) *cobra.Command {
	f := &restoreFlags{}
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Put mail from a snapshot back on an IMAP server (previews first; writing needs your approval)",
		Long: `Appends messages from the repository to an IMAP server. It is the only command that
writes to a server, and the server is the source of truth, so nothing is written until
you have seen exactly what will happen and approved it:

  1. restore prints a preview: the target account, the folders it would create, the
     existing folders it would add to, how many messages and bytes, and how many are
     skipped because they are already there. It ends with a plan token.
  2. To write, type that token when asked (at a terminal), or pass --confirm TOKEN
     (scripts). There is no --yes. The plan is worked out again against the server
     before writing, so a token for a plan that no longer holds writes nothing.
  3. The target account's config must contain
         [restore]
         allow_append = true
     so a backup-only config (cron, a NAS) can never write.

Restore only ever creates folders and appends messages. It never deletes, moves,
flags or overwrites anything, never re-adds a message that is already in the target
folder (run it twice and the second run says "Nothing to restore"), and never
restores the \Deleted flag. Messages are appended with their original flags and dates.

What to restore: --snapshot (default latest; a number, latest~N or a time such as
2026-11-20T10:00), or --deleted-since 7d for the messages that were deleted from the
server in the last 7 days and are not on it now ("undo the hack"). --folder and
--message (a SHA-256 prefix, see "ls") narrow either.

Where to: by default into new folders under Restored/<snapshot>/, next to your own.
--into changes that prefix ({snapshot} is replaced). --in-place appends into the
original folders instead (creating any that are missing). --to ACCOUNT restores to
another configured account, which needs its own allow_append.

It needs the private key of an encrypted repository (it decrypts the messages).
--dry-run prints the preview and stops.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runRestore(cmd, opts, f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.snapshot, "snapshot", "", `snapshot to restore: a number, "latest", "latest~N" or a time (default latest)`)
	fl.StringVar(&f.deletedSince, "deleted-since", "", `restore what was deleted from the server within this long, e.g. 36h or 7d`)
	fl.StringVar(&f.folder, "folder", "", "only this original folder")
	fl.StringVar(&f.message, "message", "", "only the message with this SHA-256 prefix (8+ hex digits)")
	fl.StringVar(&f.to, "to", "", "restore to this other account (--account still selects the repository)")
	fl.StringVar(&f.into, "into", "", `folder prefix to restore under (default "`+engine.DefaultRestorePrefix+`")`)
	fl.BoolVar(&f.inPlace, "in-place", false, "append into the original folders instead of a Restored/ prefix")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the preview and stop: nothing is written")
	fl.StringVar(&f.confirm, "confirm", "", "approve writing: the plan token from the preview")
	return cmd
}

// loadRestoreConfigs loads the repository's config and the target's. They are
// one config unless --to names another account.
func loadRestoreConfigs(opts *options, to string) (repoCfg, tgtCfg *config.Config, err error) {
	if to == "" {
		cfg, err := loadOnlineConfig(opts)
		return cfg, cfg, err
	}
	if repoCfg, err = loadOfflineConfig(opts); err != nil {
		return nil, nil, err
	}
	o := *opts
	o.account = to
	if tgtCfg, err = loadOnlineConfig(&o); err != nil {
		return nil, nil, err
	}
	return repoCfg, tgtCfg, nil
}

// configPathOf is the config file of account (or the --config file for none).
func configPathOf(opts *options, account string) string {
	if account == "" {
		return opts.configPath
	}
	if p, err := config.AccountConfigPath(os.Getenv, account); err == nil {
		return p
	}
	return account + ".toml"
}

func runRestore(cmd *cobra.Command, opts *options, f *restoreFlags) error {
	ro := engine.RestoreOptions{
		Snapshot: f.snapshot, Folder: f.folder, Message: f.message, Into: f.into, InPlace: f.inPlace,
	}
	if f.deletedSince != "" {
		d, err := config.ParseDuration(f.deletedSince)
		if err != nil {
			return fmt.Errorf("--deleted-since: %w", err)
		}
		ro.DeletedSince = d
	}
	if opts.verbose {
		ro.Log = func(format string, args ...any) { _, _ = fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...) }
	}
	repoCfg, tgtCfg, err := loadRestoreConfigs(opts, f.to)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	tgt, err := dialTarget(ctx, tgtCfg)
	if err != nil {
		return err
	}
	defer func() { _ = tgt.Close() }()

	prev, err := engine.PlanRestore(ctx, repoCfg, tgtCfg, tgt, ro)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	// show prints the preview, once, however the run ends.
	showAfter := func(res *engine.RestoreResult, note string) error {
		if opts.json {
			return writeJSON(out, restoreJSONOf(prev, res, note))
		}
		printRestorePreview(out, prev)
		if res != nil {
			printRestoreResult(out, res)
		}
		if note != "" {
			_, _ = fmt.Fprintln(out, note)
		}
		return nil
	}
	show := func(note string) error { return showAfter(nil, note) }

	switch {
	case prev.Plan.Empty():
		return show("Nothing to restore: everything selected is already on the server.")
	case f.dryRun:
		note := "Dry run: nothing was written."
		if !prev.AllowAppend {
			note += "\n" + notAllowedHint(opts, f.to, prev)
		}
		return show(note)
	case !prev.AllowAppend:
		if err := show(""); err != nil {
			return err
		}
		return errors.New(notAllowedHint(opts, f.to, prev) + "\nNothing was written.")
	}

	token := strings.TrimSpace(f.confirm)
	interactive := false
	if token == "" {
		if !stdinIsTerminal(cmd.InOrStdin()) {
			if err := show(""); err != nil {
				return err
			}
			return fmt.Errorf("restore needs your approval: review the preview above, then run it again with --confirm %s. Nothing was written", prev.Token)
		}
		interactive = true
		if err := show(""); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Type the token to write these changes to %s, anything else aborts: ", clean(prev.TargetAccount))
		line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		token = strings.TrimSpace(line)
	}

	approved, err := prev.Approve(token)
	if err != nil {
		switch {
		case interactive:
			return errors.New("aborted: that is not the token of this plan. Nothing was written")
		default:
			if err := show(""); err != nil {
				return err
			}
			return fmt.Errorf("the token does not match the plan as it is now (the server or the repository changed, or the token is for another plan). Review the new preview above and approve it with --confirm %s. Nothing was written", prev.Token)
		}
	}
	if !interactive {
		if err := show(""); err != nil {
			return err
		}
	}
	res, err := engine.ApplyRestore(ctx, repoCfg, tgtCfg, tgt, approved, ro)
	if res != nil && (res.Appended > 0 || len(res.Created) > 0) && err != nil {
		_, _ = fmt.Fprintf(out, "Stopped after creating %s and appending %s.\n", plural(len(res.Created), "folder"), plural(res.Appended, "message"))
		_, _ = fmt.Fprintln(out, "Run restore again to plan the rest: what is already there is not added twice.")
	}
	if err != nil {
		return err
	}
	if opts.json {
		return writeJSON(out, restoreJSONOf(prev, res, ""))
	}
	printRestoreResult(out, res)
	return nil
}

func notAllowedHint(opts *options, to string, prev *engine.RestorePreview) string {
	account := opts.account
	if to != "" {
		account = to
	}
	return fmt.Sprintf("Writing to %s is not enabled. To enable it add this to %s:\n\n    [restore]\n    allow_append = true",
		clean(prev.TargetAccount), configPathOf(opts, account))
}

func printRestorePreview(out io.Writer, prev *engine.RestorePreview) {
	p := prev.Plan
	_, _ = fmt.Fprintln(out, "Restore preview. Nothing has been written yet.")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "  Source:  %s\n", clean(p.Source))
	_, _ = fmt.Fprintf(out, "  Target:  %s (%s:%d)\n", clean(prev.TargetAccount), clean(p.Target.Host), p.Target.Port)
	if prev.DifferentAccount {
		_, _ = fmt.Fprintf(out, "  ** DIFFERENT ACCOUNT: the repository was made from %s **\n", clean(prev.RepoAccount))
	}
	if p.InPlace {
		_, _ = fmt.Fprintln(out, "  Mode:    IN PLACE: messages are added to the original folders")
	} else {
		_, _ = fmt.Fprintf(out, "  Mode:    into new folders under %q, next to your own\n", clean(p.Prefix))
	}
	_, _ = fmt.Fprintln(out)
	if len(p.Folders) > 0 {
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  FOLDER\tSTATE\tADD\tSKIP (already there)")
		for _, f := range p.Folders {
			state := "new: will be created"
			if f.Exists {
				state = fmt.Sprintf("EXISTS: has %s", plural(f.ExistingMessages, "message"))
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\n", clean(f.Name), state, len(f.Append), f.Skipped)
		}
		_ = tw.Flush()
		_, _ = fmt.Fprintln(out)
	}
	_, _ = fmt.Fprintf(out, "  Will create %s and add %s (%s). %d already on the server.\n",
		plural(len(p.Create), "folder"), plural(p.Appends, "message"), humanBytes(p.Bytes), p.Skipped)
	if p.DeletedFlagDropped > 0 {
		_, _ = fmt.Fprintf(out, "  %s had the \\Deleted flag; it is not restored, so they will not be expunged later.\n", plural(p.DeletedFlagDropped, "message"))
	}
	_, _ = fmt.Fprintln(out, "  Nothing is deleted, moved, re-flagged or overwritten on the server.")
	for _, w := range prev.Warnings {
		_, _ = fmt.Fprintf(out, "  warning: %s\n", clean(w))
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "  Plan token: %s\n", prev.Token)
}

func printRestoreResult(out io.Writer, res *engine.RestoreResult) {
	_, _ = fmt.Fprintf(out, "Restored %s (%s) and created %s. %d were already there.\n",
		plural(res.Appended, "message"), humanBytes(res.Bytes), plural(len(res.Created), "folder"), res.Skipped)
}

type restoreFolderJSON struct {
	Name     string `json:"name"`
	Exists   bool   `json:"exists"`
	Existing int    `json:"existing_messages"`
	Add      int    `json:"add"`
	Skip     int    `json:"skip"`
	Bytes    int64  `json:"bytes"`
}

type restoreJSON struct {
	Source           string              `json:"source"`
	Target           string              `json:"target"`
	DifferentAccount bool                `json:"different_account"`
	InPlace          bool                `json:"in_place"`
	Prefix           string              `json:"prefix,omitempty"`
	Folders          []restoreFolderJSON `json:"folders"`
	Create           []string            `json:"create"`
	Appends          int                 `json:"appends"`
	Bytes            int64               `json:"bytes"`
	Skipped          int                 `json:"skipped"`
	AllowAppend      bool                `json:"allow_append"`
	Token            string              `json:"token"`
	Written          bool                `json:"written"`
	Appended         int                 `json:"appended,omitempty"`
	Note             string              `json:"note,omitempty"`
}

func restoreJSONOf(prev *engine.RestorePreview, res *engine.RestoreResult, note string) restoreJSON {
	p := prev.Plan
	j := restoreJSON{
		Source: p.Source, Target: prev.TargetAccount, DifferentAccount: prev.DifferentAccount, InPlace: p.InPlace,
		Prefix: p.Prefix, Folders: []restoreFolderJSON{}, Create: append([]string{}, p.Create...), Appends: p.Appends,
		Bytes: p.Bytes, Skipped: p.Skipped, AllowAppend: prev.AllowAppend, Token: prev.Token, Note: note,
	}
	for _, f := range p.Folders {
		j.Folders = append(j.Folders, restoreFolderJSON{Name: f.Name, Exists: f.Exists, Existing: f.ExistingMessages, Add: len(f.Append), Skip: f.Skipped, Bytes: f.Bytes})
	}
	if res != nil {
		j.Written, j.Appended = res.Appended > 0 || len(res.Created) > 0, res.Appended
	}
	return j
}
