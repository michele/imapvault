package engine

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/michele/imapvault/internal/objects"
	"github.com/michele/imapvault/internal/repo"
	"github.com/michele/imapvault/internal/seal"
)

// sealerOf builds the Sealer a repository writes with. repo.toml is the source of
// truth for the recipients, not the current config, so a config that drifted
// cannot silently encrypt new data to different keys.
func sealerOf(r *repo.Repo) (*seal.Sealer, error) {
	enc := r.Meta.Encryption
	if !enc.Enabled {
		return seal.NewSealer(nil), nil
	}
	rs, err := seal.ParseRecipients(enc.Recipients)
	if err != nil {
		return nil, fmt.Errorf("repo.toml: %w", err)
	}
	return seal.NewSealer(rs), nil
}

// openerOf builds an Opener from identity text; empty text means no private key.
func openerOf(identity string) (*seal.Opener, error) {
	if strings.TrimSpace(identity) == "" {
		return seal.NewOpener(nil), nil
	}
	ids, err := seal.ParseIdentities(identity)
	if err != nil {
		return nil, err
	}
	return seal.NewOpener(ids), nil
}

// openStore opens the repository's blob store: it writes with the repository's
// recipients and reads with identity (empty on a machine that only backs up).
func openStore(r *repo.Repo, identity string) (*objects.Store, error) {
	sealer, err := sealerOf(r)
	if err != nil {
		return nil, err
	}
	opener, err := openerOf(identity)
	if err != nil {
		return nil, err
	}
	return objects.Open(r.ObjectsDir(), r.TmpDir(), objects.Options{Sealer: sealer, Opener: opener})
}

// newEncryption turns the config's recipients into the binding stored in
// repo.toml, including a canary sealed to them. It returns the zero value (a plain
// repository) when the config has no recipients.
func newEncryption(recipients []string) (repo.Encryption, error) {
	if len(recipients) == 0 {
		return repo.Encryption{}, nil
	}
	trimmed := make([]string, len(recipients))
	for i, r := range recipients {
		trimmed[i] = strings.TrimSpace(r)
	}
	rs, err := seal.ParseRecipients(trimmed)
	if err != nil {
		return repo.Encryption{}, fmt.Errorf("encryption.recipients: %w", err)
	}
	sealed, hash, err := seal.NewCanary(seal.NewSealer(rs))
	if err != nil {
		return repo.Encryption{}, err
	}
	return repo.Encryption{
		Enabled:      true,
		Recipients:   trimmed,
		Canary:       base64.StdEncoding.EncodeToString(sealed),
		CanarySHA256: hex.EncodeToString(hash[:]),
	}, nil
}

// checkCanary proves the opener holds a key for this repository.
func checkCanary(r *repo.Repo, o *seal.Opener) error {
	sealed, err := base64.StdEncoding.DecodeString(r.Meta.Encryption.Canary)
	if err != nil {
		return fmt.Errorf("repo.toml: encryption.canary is not valid base64")
	}
	hash, err := hex.DecodeString(r.Meta.Encryption.CanarySHA256)
	if err != nil || len(hash) != 32 {
		return fmt.Errorf("repo.toml: encryption.canary_sha256 is not valid")
	}
	var h [32]byte
	copy(h[:], hash)
	return seal.CheckCanary(o, sealed, h)
}
