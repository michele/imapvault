package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// IdentityEnv is the environment variable that may hold the age identity (the
// AGE-SECRET-KEY-1... line, or a whole age-keygen file).
const IdentityEnv = "IMAPVAULT_IDENTITY"

// ErrNoIdentity means a command needs the private key but none is configured.
var ErrNoIdentity = errors.New("no age identity configured: set encryption.identity_file, encryption.identity_command or the " + IdentityEnv + " environment variable")

var identityKind = secretKind{"encryption.identity_file", "encryption.identity_command", "identity file"}

// Encryption configures encryption at rest. Recipients are public keys: they
// are read by init (which decides whether the repository is encrypted and
// records them in repo.toml) and by nothing else. The identity is the private
// key; only commands that read mail need it, so a cron job can run backups with
// no private key on the machine.
type Encryption struct {
	Recipients      []string `toml:"recipients"`
	IdentityFile    string   `toml:"identity_file"`
	IdentityCommand string   `toml:"identity_command"`

	// Identity is the resolved private key text. It is never read from the TOML
	// file; ResolveIdentity fills it.
	Identity string `toml:"-"`
}

// Enabled reports whether init should create an encrypted repository.
func (e Encryption) Enabled() bool { return len(e.Recipients) > 0 }

// IdentityEnvFor names the environment variable holding an account's identity:
// IMAPVAULT_IDENTITY_<NAME> (upper-case, '-' as '_'), or IMAPVAULT_IDENTITY when
// there is no account.
func IdentityEnvFor(account string) string {
	if account == "" {
		return IdentityEnv
	}
	return IdentityEnv + "_" + strings.ToUpper(strings.ReplaceAll(account, "-", "_"))
}

func (c *Config) validateEncryption(add func(string, ...any)) {
	e := c.Encryption
	if e.IdentityFile != "" && e.IdentityCommand != "" {
		add("encryption.identity_file and encryption.identity_command cannot both be set")
	}
	seen := map[string]bool{}
	for _, r := range e.Recipients {
		switch {
		case strings.TrimSpace(r) == "":
			add("encryption.recipients must not contain an empty entry")
		case seen[r]:
			add("encryption.recipients lists %s twice", r)
		}
		seen[r] = true
	}
}

// ResolveIdentity reads the private key from identity_file or identity_command
// into c.Encryption.Identity (either wins over the environment variable read at
// parse time) and returns warnings to show the user. With no source at all it
// returns ErrNoIdentity. Offline loads never call it, so commands that only
// need the public key never run identity_command.
func (c *Config) ResolveIdentity(ctx context.Context) ([]string, error) {
	return c.resolveIdentity(ctx, realPasswordDeps())
}

func (c *Config) resolveIdentity(ctx context.Context, d passwordDeps) ([]string, error) {
	var warnings []string
	switch {
	case c.Encryption.IdentityFile != "":
		data, w, err := readSecretFile(identityKind, c.Encryption.IdentityFile, c.baseDir, d)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(data)) == "" {
			return nil, fmt.Errorf("%s %s is empty", identityKind.fileKey, c.Encryption.IdentityFile)
		}
		c.Encryption.Identity, warnings = string(data), w
	case c.Encryption.IdentityCommand != "":
		out, err := runSecretCommand(ctx, identityKind, c.Encryption.IdentityCommand, d)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(out)) == "" {
			return nil, fmt.Errorf("the output of %s is empty", identityKind.cmdKey)
		}
		c.Encryption.Identity = string(out)
	}
	if strings.TrimSpace(c.Encryption.Identity) == "" {
		return nil, ErrNoIdentity
	}
	return warnings, nil
}
