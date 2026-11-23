package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/engine"
)

// loadOfflineConfig resolves --account / --config and loads the config without
// requiring a password: init and check never connect to the server.
func loadOfflineConfig(opts *options) (*config.Config, error) {
	path, account := opts.configPath, opts.account
	if account != "" {
		p, err := config.AccountConfigPath(os.Getenv, account)
		if err != nil {
			return nil, err
		}
		path = p
	}
	cfg, err := config.LoadAccountOffline(path, account)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newInitCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create the repository for an account",
		Long: `Creates the repository at the configured repository.path and binds it to the
account's host and username, so it cannot later be used for a different mailbox.
The path must be missing or an empty directory. Nothing connects to a mailbox.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			res, err := engine.Init(cfg)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				return writeJSON(out, res)
			}
			_, _ = fmt.Fprintf(out, "Initialised repository at %s\n  account  %s\n  id       %s\n  format   v%d (index schema v%d)\n",
				res.Path, res.Account, res.RepoID, res.FormatVersion, res.SchemaVersion)
			return nil
		},
	}
}

func newCheckCmd(opts *options) *cobra.Command {
	var readData bool
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Verify the repository (index consistency, objects present)",
		Long: `Checks the index with SQLite's integrity and foreign key checks and confirms
that every blob the index references has an object file. Orphan objects and
leftover temporary files are warnings (a crash can legitimately leave them);
missing or corrupt data is an error and makes the command fail.

With --read-data every object is also read back and re-hashed. That reads the
whole repository, so it can take a long time on a large one.

check never modifies the repository. It waits for nothing: if a backup or prune
is running it fails immediately.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			rep, err := engine.Check(cmd.Context(), cfg, engine.CheckOptions{ReadData: readData})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				if err := writeJSON(out, rep); err != nil {
					return err
				}
			} else {
				printCheck(out, rep, readData)
			}
			if !rep.OK() {
				return fmt.Errorf("check found %d error(s)", len(rep.Errors))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&readData, "read-data", false, "re-read and re-hash every object (slow)")
	return cmd
}

func printCheck(out io.Writer, rep *engine.CheckReport, readData bool) {
	_, _ = fmt.Fprintf(out, "Repository: %s\n", rep.Repository)
	schema := "unknown"
	if rep.SchemaVersion > 0 {
		schema = fmt.Sprintf("v%d", rep.SchemaVersion)
	}
	_, _ = fmt.Fprintf(out, "Format v%d, index schema %s\n", rep.FormatVersion, schema)
	_, _ = fmt.Fprintf(out, "Blobs in index: %d, objects on disk: %d", rep.Blobs, rep.Objects)
	if readData {
		_, _ = fmt.Fprintf(out, ", read and verified: %d", rep.ObjectsVerified)
	}
	_, _ = fmt.Fprintln(out)
	for _, f := range rep.Errors {
		_, _ = fmt.Fprintf(out, "ERROR   %s: %s\n", f.Kind, f.Message)
	}
	for _, f := range rep.Warnings {
		_, _ = fmt.Fprintf(out, "WARNING %s: %s\n", f.Kind, f.Message)
	}
	if rep.OK() {
		_, _ = fmt.Fprintf(out, "Check passed (%d warning(s))\n", len(rep.Warnings))
		return
	}
	_, _ = fmt.Fprintf(out, "Check FAILED: %d error(s), %d warning(s)\n", len(rep.Errors), len(rep.Warnings))
}
