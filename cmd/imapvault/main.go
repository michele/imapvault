// Command imapvault keeps point-in-time snapshots of an IMAP mailbox in a local
// repository. This package only parses flags and calls the engine; it never
// touches the repository or the server directly.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	root.AddCommand(newAccountsCmd(opts), newInitCmd(opts), newCheckCmd(opts), newVersionCmd())
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
