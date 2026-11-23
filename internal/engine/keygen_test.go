package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/seal"
)

func TestGenerateKeyWritesPrivateIdentityFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "personal.key") // parent does not exist yet
	recipient, err := GenerateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity file: %v %v", err, fi)
	}
	if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o700 {
		t.Fatalf("parent directory mode %v", di.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# public key: "+recipient) {
		t.Fatalf("file does not name its public key:\n%s", data)
	}
	if _, err := seal.ParseIdentities(string(data)); err != nil {
		t.Fatalf("file does not parse as an identity: %v", err)
	}
	if _, err := seal.ParseRecipients([]string{recipient}); err != nil {
		t.Fatalf("returned recipient does not parse: %v", err)
	}
}

func TestGenerateKeyNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(path, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateKey(path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("got %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "precious" {
		t.Fatal("existing key file was modified")
	}
}

func TestGenerateKeyRequiresAPath(t *testing.T) {
	if _, err := GenerateKey(""); err == nil {
		t.Fatal("empty path accepted")
	}
}
