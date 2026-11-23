// Package seal is the only place that imports age. It encrypts blob payloads and
// the "sealed" index columns to a set of public-key recipients, and decrypts them
// with private-key identities.
//
// The split is the point of the design: a Sealer needs only public keys, so a
// cron job can back up with no private key on the machine, while an Opener (used
// by restore, show, ls and the TUI) needs the identity. A repository created
// without recipients runs the same code in plain mode, so there is one code path.
package seal

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
)

var (
	// ErrNeedIdentity means the data is encrypted and no identity was supplied.
	ErrNeedIdentity = errors.New("data is encrypted and no age identity was provided")
	// ErrWrongIdentity means the supplied identities do not match the data's
	// recipients, which is what using the wrong key looks like.
	ErrWrongIdentity = errors.New("the age identity does not match this repository")
	// ErrCorrupt means the data is truncated, altered or not sealed data at all.
	ErrCorrupt = errors.New("sealed data is corrupt")
)

// Prefix byte of a sealed index value.
const (
	prefixPlain byte = 0x00
	prefixAge   byte = 0x01
)

// Recipients are age public keys.
type Recipients []age.Recipient

// Identities are age private keys.
type Identities []age.Identity

// Identity is a freshly generated private key.
type Identity struct{ id *age.X25519Identity }

// Recipient is the public key (age1...) to put in the config.
func (i *Identity) Recipient() string { return i.id.Recipient().String() }

// GenerateIdentity creates a new X25519 identity.
func GenerateIdentity() (*Identity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	return &Identity{id}, nil
}

// IdentityFileText is the age-keygen file format for an identity.
func IdentityFileText(i *Identity) string {
	return fmt.Sprintf("# created: %s\n# public key: %s\n%s\n",
		time.Now().UTC().Format(time.RFC3339), i.Recipient(), i.id.String())
}

// ParseRecipients parses age public keys (age1...). Each entry must be exactly one key.
func ParseRecipients(keys []string) (Recipients, error) {
	var out Recipients
	for i, k := range keys {
		k = strings.TrimSpace(k)
		switch {
		case k == "":
			return nil, fmt.Errorf("recipient %d is empty", i+1)
		case strings.HasPrefix(k, "AGE-SECRET-KEY"):
			return nil, fmt.Errorf("recipient %d is a private key; recipients are public keys that start with age1", i+1)
		case strings.ContainsAny(k, "\r\n"):
			return nil, fmt.Errorf("recipient %d must be a single key", i+1)
		}
		rs, err := age.ParseRecipients(strings.NewReader(k))
		if err != nil || len(rs) != 1 {
			return nil, fmt.Errorf("recipient %d is not a valid age public key", i+1)
		}
		out = append(out, rs[0])
	}
	return out, nil
}

// ParseIdentities parses an age-keygen file or a bare AGE-SECRET-KEY line.
// Errors never include the key text.
func ParseIdentities(text string) (Identities, error) {
	ids, err := age.ParseIdentities(strings.NewReader(text))
	if err != nil {
		return nil, errors.New("the age identity is not valid (expected an age-keygen file or an AGE-SECRET-KEY-1... line)")
	}
	return ids, nil
}

// Sealer encrypts to a fixed set of recipients. With none it is in plain mode.
type Sealer struct{ recipients Recipients }

// NewSealer returns a Sealer; no recipients means plain (unencrypted) mode.
func NewSealer(r Recipients) *Sealer { return &Sealer{recipients: r} }

// Encrypting reports whether this Sealer encrypts.
func (s *Sealer) Encrypting() bool { return len(s.recipients) > 0 }

// Encrypt returns the raw age encryption of plain. It is for payloads that carry
// their own envelope (blobs). It refuses in plain mode rather than silently
// storing plaintext.
func (s *Sealer) Encrypt(plain []byte) ([]byte, error) {
	if !s.Encrypting() {
		return nil, errors.New("seal: no recipients, refusing to encrypt")
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, s.recipients...)
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	if _, err := w.Write(plain); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	return buf.Bytes(), nil
}

// Seal wraps an index value: one prefix byte, then the value either plain or
// encrypted. The prefix lets one reader handle both kinds of repository.
func (s *Sealer) Seal(plain []byte) ([]byte, error) {
	if !s.Encrypting() {
		return append([]byte{prefixPlain}, plain...), nil
	}
	ct, err := s.Encrypt(plain)
	if err != nil {
		return nil, err
	}
	return append([]byte{prefixAge}, ct...), nil
}

// Opener decrypts with a fixed set of identities. With none it can still read
// plain values.
type Opener struct{ identities Identities }

// NewOpener returns an Opener.
func NewOpener(ids Identities) *Opener { return &Opener{identities: ids} }

// Decrypt reverses Sealer.Encrypt.
func (o *Opener) Decrypt(ct []byte) ([]byte, error) {
	if len(o.identities) == 0 {
		return nil, ErrNeedIdentity
	}
	r, err := age.Decrypt(bytes.NewReader(ct), o.identities...)
	if err != nil {
		var nm *age.NoIdentityMatchError
		if errors.As(err, &nm) {
			return nil, ErrWrongIdentity
		}
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return plain, nil
}

// Open reverses Sealer.Seal.
func (o *Opener) Open(sealed []byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, fmt.Errorf("%w: empty value", ErrCorrupt)
	}
	switch sealed[0] {
	case prefixPlain:
		return sealed[1:], nil
	case prefixAge:
		return o.Decrypt(sealed[1:])
	default:
		return nil, fmt.Errorf("%w: unknown prefix %#02x", ErrCorrupt, sealed[0])
	}
}

// NewFolderKey returns a random key for FolderHash.
func NewFolderKey() ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// FolderHash is the plaintext lookup key for a folder: HMAC-SHA256 of its name.
// The index keeps the name itself sealed. The key lives in repo.toml, so this
// hides unique folder names from someone who only has the index, but a person
// holding the whole repository can still test guessable names ("INBOX").
func FolderHash(key []byte, name string) [32]byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(name))
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

// NewCanary creates a small sealed value and the hash of its plaintext. Stored in
// repo.toml, it lets a command say "this identity does not match this
// repository" instead of reporting corruption when the wrong key is used.
func NewCanary(s *Sealer) (sealed []byte, hash [32]byte, err error) {
	var plain [32]byte
	if _, err = rand.Read(plain[:]); err != nil {
		return nil, hash, err
	}
	sealed, err = s.Encrypt(plain[:])
	if err != nil {
		return nil, hash, err
	}
	return sealed, sha256.Sum256(plain[:]), nil
}

// CheckCanary decrypts the canary and compares it with its recorded hash.
func CheckCanary(o *Opener, sealed []byte, hash [32]byte) error {
	plain, err := o.Decrypt(sealed)
	if err != nil {
		return err
	}
	if sha256.Sum256(plain) != hash {
		return fmt.Errorf("%w: canary does not match its hash", ErrCorrupt)
	}
	return nil
}
