// Command imapvault keeps point-in-time snapshots of an IMAP mailbox in a local
// repository. This package only parses flags and calls the engine; it never
// touches the repository or the server directly.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
)

type options struct {
	configPath string
	configSet  bool // --config was given explicitly
	account    string
	verbose    bool
	json       bool
	stderr     io.Writer // where config warnings go; set from the command
}

// Exit codes: 0 ok, 1 error, 2 partial backup, 3 guard alert.
const (
	exitError   = 1
	exitPartial = 2
	exitAlert   = 3
)

// exitCodeError makes main exit with a specific status after the command has
// already printed its own output. The message goes to stderr as is.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

func defaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "imapvault", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(home, ".config", "imapvault", "config.toml")
}

func newRootCmd() *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:           "imapvault",
		Short:         "Point-in-time snapshots of an IMAP mailbox, with restore",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       versionString(),
	}
	root.SetVersionTemplate("imapvault {{.Version}}\n")
	root.PersistentFlags().StringVar(&opts.configPath, "config", defaultConfigPath(), "path to the TOML config file")
	root.PersistentFlags().StringVar(&opts.account, "account", "", "use <config folder>/NAME.toml (see the accounts command)")
	root.PersistentFlags().BoolVarP(&opts.verbose, "verbose", "v", false, "print progress to stderr (never message content or credentials)")
	root.PersistentFlags().BoolVar(&opts.json, "json", false, "machine-readable output on list commands")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		opts.configSet = cmd.Flags().Changed("config")
		opts.stderr = cmd.ErrOrStderr()
		return nil
	}
	root.AddCommand(newAccountsCmd(opts), newInitCmd(opts), newCheckCmd(opts), newKeygenCmd(opts),
		newBackupCmd(opts), newAlertsCmd(opts), newAckCmd(opts), newPinCmd(opts, true), newPinCmd(opts, false), newForgetCmd(opts), newPruneCmd(opts), newSnapshotsCmd(opts), newLsCmd(opts), newShowCmd(opts), newDiffCmd(opts), newDoctorCmd(opts), newVersionCmd())
	return root
}

func main() {
	// Ctrl-C and SIGTERM cancel the context, so a running backup stops cleanly: it
	// keeps what it already committed and, if at least one folder was done, ends
	// as a partial run (exit 2) instead of being killed mid-folder.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		var ec *exitCodeError
		if errors.As(err, &ec) {
			fmt.Fprintln(os.Stderr, ec.msg)
			os.Exit(ec.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitError)
	}
}
