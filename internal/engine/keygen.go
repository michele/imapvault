package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/michele/imapvault/internal/seal"
)

// GenerateKey creates a new age identity at path (0600, in a 0700 directory if it
// has to be created) and returns the matching public key, the recipient to put in
// encryption.recipients. It never overwrites an existing file: losing a key file
// by regenerating over it would lose every repository encrypted to it.
func GenerateKey(path string) (recipient string, err error) {
	if path == "" {
		return "", errors.New("a path for the identity file is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	id, err := seal.GenerateIdentity()
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("%s already exists; refusing to overwrite a key file", path)
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(seal.IdentityFileText(id)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return id.Recipient(), nil
}
