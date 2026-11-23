package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/michele/imapvault/internal/engine"
)

func newKeygenCmd(opts *options) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate an age key pair for an encrypted repository",
		Long: `Creates a new age identity (private key) in the file given with --out, mode 0600,
and prints the matching public key. Put the public key in encryption.recipients
before running init; the machine that runs backups needs only that. Keep the
identity file where you read mail (restore, show, check --decrypt) and back it up
somewhere safe: without it an encrypted repository cannot be read, by anyone.

keygen never overwrites an existing file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			recipient, err := engine.GenerateKey(out)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if opts.json {
				return writeJSON(w, map[string]string{"identity_file": out, "recipient": recipient})
			}
			_, _ = fmt.Fprintf(w, `Wrote the private key to %[1]s (mode 0600).
Back it up somewhere safe: without it the repository cannot be read.

Public key (recipient): %[2]s

Add to your account config:

  [encryption]
  recipients = ["%[2]s"]
  identity_file = "%[1]s"   # only on machines that read mail; a backup-only machine omits it

Tip: add a second recipient, a recovery key kept offline, so losing this file does
not lose the mailbox. Encryption is fixed when the repository is created with init.
`, out, recipient)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "file to write the private key to (required)")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}
