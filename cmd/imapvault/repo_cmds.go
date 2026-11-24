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
		return nil, configError(path, err)
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
The path must be missing or an empty directory. Nothing connects to a mailbox.

With encryption.recipients configured (see keygen) everything is encrypted to
those public keys: message bodies, folder names and message details. That is
decided here and cannot be changed afterwards. Without recipients the repository
is plaintext.`,
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
			if res.Encrypted {
				_, _ = fmt.Fprintf(out, "  mail     encrypted to %d recipient(s); backups need no private key\n", res.Recipients)
			} else {
				_, _ = fmt.Fprintln(out, "  mail     NOT encrypted (no encryption.recipients configured; this cannot be changed later)")
			}
			return nil
		},
	}
}

func newCheckCmd(opts *options) *cobra.Command {
	var readData, decrypt bool
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Verify the repository (index consistency, objects present)",
		Long: `Checks the index with SQLite's integrity and foreign key checks and confirms
that every blob the index references has an object file. Orphan objects and
leftover temporary files are warnings (a crash can legitimately leave them);
missing or corrupt data is an error and makes the command fail.

With --read-data every object is also read back. In an encrypted repository this
needs no private key: each object is compared with the hash recorded when it was
written, which catches bit rot and truncation. It cannot catch someone who
rewrites an object and its recorded hash together.

With --decrypt (implies --read-data) every object is decrypted with your private
key (encryption.identity_file, identity_command or IMAPVAULT_IDENTITY) and
re-hashed against its key. That is the full check, and the only one that
authenticates the data. Both read the whole repository, so they can take a long
time on a large one.

check never modifies the repository. It waits for nothing: if a backup or prune
is running it fails immediately.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadOfflineConfig(opts)
			if err != nil {
				return err
			}
			rep, err := engine.Check(cmd.Context(), cfg, engine.CheckOptions{ReadData: readData, Decrypt: decrypt})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json {
				if err := writeJSON(out, rep); err != nil {
					return err
				}
			} else {
				printCheck(out, rep)
			}
			if !rep.OK() {
				return fmt.Errorf("check found %d error(s)", len(rep.Errors))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&readData, "read-data", false, "read every object and verify it (slow; needs no private key)")
	cmd.Flags().BoolVar(&decrypt, "decrypt", false, "also decrypt every object with your private key and re-hash it (implies --read-data)")
	return cmd
}

func printCheck(out io.Writer, rep *engine.CheckReport) {
	_, _ = fmt.Fprintf(out, "Repository: %s\n", rep.Repository)
	schema := "unknown"
	if rep.SchemaVersion > 0 {
		schema = fmt.Sprintf("v%d", rep.SchemaVersion)
	}
	mode := "not encrypted"
	if rep.Encrypted {
		mode = "encrypted"
	}
	_, _ = fmt.Fprintf(out, "Format v%d, index schema %s, %s\n", rep.FormatVersion, schema, mode)
	_, _ = fmt.Fprintf(out, "Blobs in index: %d, objects on disk: %d", rep.Blobs, rep.Objects)
	if rep.ObjectsVerified > 0 {
		_, _ = fmt.Fprintf(out, ", verified: %d", rep.ObjectsVerified)
	}
	if rep.ObjectsDecrypted > 0 {
		_, _ = fmt.Fprintf(out, ", decrypted: %d", rep.ObjectsDecrypted)
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
