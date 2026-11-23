package objects

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/seal"
)

type encFixture struct {
	root     string
	recip    seal.Recipients
	ids      seal.Identities
	identity string
}

func newEncFixture(t *testing.T) encFixture {
	t.Helper()
	id, err := seal.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	text := seal.IdentityFileText(id)
	recip, err := seal.ParseRecipients([]string{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := seal.ParseIdentities(text)
	if err != nil {
		t.Fatal(err)
	}
	return encFixture{root: t.TempDir(), recip: recip, ids: ids, identity: text}
}

func (f encFixture) open(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(filepath.Join(f.root, "objects"), filepath.Join(f.root, "tmp"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// both: a store that can write and read. writeOnly: the cron box, public key only.
func (f encFixture) both(t *testing.T) *Store {
	return f.open(t, Options{Sealer: seal.NewSealer(f.recip), Opener: seal.NewOpener(f.ids)})
}
func (f encFixture) writeOnly(t *testing.T) *Store {
	return f.open(t, Options{Sealer: seal.NewSealer(f.recip)})
}

func TestEncryptedRoundTripAndEnvelope(t *testing.T) {
	f := newEncFixture(t)
	s := f.both(t)
	raw := []byte("Subject: very private\r\n\r\n" + strings.Repeat("secret body ", 200))
	k, stored, added, err := s.Put(raw)
	if err != nil || !added {
		t.Fatalf("put: %v %v", added, err)
	}
	file, err := os.ReadFile(s.path(k))
	if err != nil {
		t.Fatal(err)
	}
	if file[0] != flagZstd|flagEncrypted {
		t.Fatalf("flags %#x", file[0])
	}
	for _, leak := range []string{"very private", "secret body"} {
		if bytes.Contains(file, []byte(leak)) {
			t.Fatalf("plaintext %q found in the object file", leak)
		}
	}
	if stored != sha256.Sum256(file) {
		t.Fatal("stored hash must be the SHA-256 of the exact file bytes")
	}
	got, err := s.Get(k)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("get: %v", err)
	}
	if enc, err := s.Encrypted(k); err != nil || !enc {
		t.Fatalf("Encrypted: %v %v", enc, err)
	}
}

func TestWriteOnlyStoreBacksUpButCannotRead(t *testing.T) {
	f := newEncFixture(t)
	cron := f.writeOnly(t)
	raw := []byte("written by a box that only has the public key")
	k, stored, _, err := cron.Put(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := cron.Has(k); err != nil || !ok {
		t.Fatalf("has: %v %v", ok, err)
	}
	if err := cron.VerifyStored(k, stored); err != nil {
		t.Fatalf("integrity check with no key: %v", err)
	}
	if _, err := cron.Get(k); !errors.Is(err, ErrNeedIdentity) {
		t.Fatalf("want ErrNeedIdentity, got %v", err)
	}
	if err := cron.Verify(k); !errors.Is(err, ErrNeedIdentity) {
		t.Fatalf("Verify needs the key: %v", err)
	}
	// the owner, later, with the private key, reads what the cron box wrote.
	got, err := f.both(t).Get(k)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("owner get: %v", err)
	}
}

func TestWrongIdentityIsDistinguishedFromCorruption(t *testing.T) {
	f := newEncFixture(t)
	k, _, _, _ := f.both(t).Put([]byte("x"))
	other := newEncFixture(t)
	s := f.open(t, Options{Sealer: seal.NewSealer(f.recip), Opener: seal.NewOpener(other.ids)})
	if _, err := s.Get(k); !errors.Is(err, seal.ErrWrongIdentity) {
		t.Fatalf("want ErrWrongIdentity, got %v", err)
	}
}

func TestPutDedupesAndReportsStoredHashOfExistingObject(t *testing.T) {
	f := newEncFixture(t)
	s := f.writeOnly(t)
	k1, st1, a1, _ := s.Put([]byte("same message"))
	k2, st2, a2, err := s.Put([]byte("same message"))
	if err != nil || k1 != k2 || !a1 || a2 || st1 != st2 {
		t.Fatalf("dedupe: added %v %v, stored equal %v, err %v", a1, a2, st1 == st2, err)
	}
}

func TestEveryByteFlipOfEncryptedObjectIsDetected(t *testing.T) {
	f := newEncFixture(t)
	s := f.both(t)
	k, stored, _, _ := s.Put(bytes.Repeat([]byte("flip "), 100))
	orig, _ := os.ReadFile(s.path(k))
	for i := range orig {
		bad := append([]byte(nil), orig...)
		bad[i] ^= 0x01
		if err := os.WriteFile(s.path(k), bad, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.VerifyStored(k, stored); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("flip %d: VerifyStored (no key) want ErrCorrupt, got %v", i, err)
		}
		if got, err := s.Get(k); err == nil {
			t.Fatalf("flip %d: Get returned data without error (%d bytes)", i, len(got))
		}
	}
}

func TestVerifyStoredMissingAndTruncated(t *testing.T) {
	f := newEncFixture(t)
	s := f.writeOnly(t)
	if err := s.VerifyStored(Key{9}, [32]byte{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	k, stored, _, _ := s.Put([]byte("trunc trunc trunc"))
	orig, _ := os.ReadFile(s.path(k))
	_ = os.WriteFile(s.path(k), orig[:len(orig)-1], 0o600)
	if err := s.VerifyStored(k, stored); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated: %v", err)
	}
}

func TestPlainStoreReportsNotEncrypted(t *testing.T) {
	s, _ := newStore(t)
	k, _, _, _ := s.Put([]byte("plain"))
	if enc, err := s.Encrypted(k); err != nil || enc {
		t.Fatalf("Encrypted: %v %v", enc, err)
	}
	if _, err := s.Encrypted(Key{1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestPlainStoreCannotReadEncryptedObject(t *testing.T) {
	f := newEncFixture(t)
	k, _, _, _ := f.both(t).Put([]byte("hidden"))
	plain := f.open(t, Options{})
	if _, err := plain.Get(k); !errors.Is(err, ErrNeedIdentity) {
		t.Fatalf("want ErrNeedIdentity, got %v", err)
	}
}

func TestEncryptedLargeObject(t *testing.T) {
	f := newEncFixture(t)
	s := f.both(t)
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<18) // 4 MiB, compresses well
	k, _, _, err := s.Put(big)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(k)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("round trip: %v", err)
	}
}

func TestEncryptedPutLeavesNoTmp(t *testing.T) {
	f := newEncFixture(t)
	s := f.writeOnly(t)
	for i := 0; i < 5; i++ {
		_, _, _, _ = s.Put([]byte{byte(i)})
	}
	ents, _ := os.ReadDir(filepath.Join(f.root, "tmp"))
	if len(ents) != 0 {
		t.Fatalf("tmp not empty: %d", len(ents))
	}
}

func TestCorruptEncryptedObjectErrorNeverEchoesContent(t *testing.T) {
	f := newEncFixture(t)
	s := f.both(t)
	k, _, _, _ := s.Put([]byte("x"))
	// plaintext mail where ciphertext should be, behind an "encrypted" envelope byte.
	body := append([]byte{flagZstd | flagEncrypted}, []byte("Subject: the merger is confidential\r\n\r\nbody")...)
	if err := os.WriteFile(s.path(k), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(k)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
	if strings.Contains(err.Error(), "merger") || strings.Contains(err.Error(), "Subject") {
		t.Fatalf("error echoes object content: %v", err)
	}
}
