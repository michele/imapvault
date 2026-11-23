package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/objects"
	"github.com/michele/imapvault/internal/repo"
	"github.com/michele/imapvault/internal/seal"
)

// encKeys is a throwaway age key pair as the config would carry it.
type encKeys struct{ recipient, identity string }

func newEncKeys(t *testing.T) encKeys {
	t.Helper()
	id, err := seal.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return encKeys{recipient: id.Recipient(), identity: seal.IdentityFileText(id)}
}

// encConfig is testConfig with encryption to keys.recipient. The identity is NOT
// set: a backup machine does not have it.
func encConfig(t *testing.T, keys encKeys) *config.Config {
	cfg := testConfig(t)
	cfg.Encryption.Recipients = []string{keys.recipient}
	return cfg
}

func initEncrypted(t *testing.T) (*config.Config, encKeys) {
	t.Helper()
	keys := newEncKeys(t)
	cfg := encConfig(t, keys)
	if _, err := Init(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, keys
}

// addEncBlob stores raw in an encrypted repository and indexes it, the way backup
// will: blob first, then the index row carrying the stored hash.
func addEncBlob(t *testing.T, cfg *config.Config, keys encKeys, raw string) objects.Key {
	t.Helper()
	r, err := repo.Open(cfg.Repository.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStore(r, keys.identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	k, stored, _, err := st.Put([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	insertBlobRow(t, r, k, stored, len(raw))
	return k
}

func insertBlobRow(t *testing.T, r *repo.Repo, k objects.Key, stored [32]byte, size int) {
	t.Helper()
	db, err := index.Open(r.IndexPath(), index.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT OR IGNORE INTO runs(id, started_at, status) VALUES (1,'2026-11-23T10:00:00Z','complete')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO blobs(sha256, size, stored_sha256, first_seen_run) VALUES (?, ?, ?, 1)`, k[:], size, stored[:]); err != nil {
		t.Fatal(err)
	}
}

func TestInitEncryptedRecordsBinding(t *testing.T) {
	cfg, keys := initEncrypted(t)
	res, err := Init(testConfig(t)) // a plain one for contrast
	if err != nil || res.Encrypted {
		t.Fatalf("plain init: %+v %v", res, err)
	}
	r, err := repo.Open(cfg.Repository.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	enc := r.Meta.Encryption
	if !enc.Enabled || len(enc.Recipients) != 1 || enc.Recipients[0] != keys.recipient {
		t.Fatalf("binding: %+v", enc)
	}
	canary, err := base64.StdEncoding.DecodeString(enc.Canary)
	if err != nil || len(canary) == 0 || len(enc.CanarySHA256) != 64 {
		t.Fatalf("canary: %v %q", err, enc.CanarySHA256)
	}
	// the canary opens with the matching identity only
	ids, _ := seal.ParseIdentities(keys.identity)
	if _, err := seal.NewOpener(ids).Decrypt(canary); err != nil {
		t.Fatalf("canary does not decrypt with the identity: %v", err)
	}
}

func TestInitEncryptedResult(t *testing.T) {
	keys := newEncKeys(t)
	cfg := encConfig(t, keys)
	second := newEncKeys(t)
	cfg.Encryption.Recipients = append(cfg.Encryption.Recipients, second.recipient)
	res, err := Init(cfg)
	if err != nil || !res.Encrypted || res.Recipients != 2 {
		t.Fatalf("result: %+v %v", res, err)
	}
}

func TestInitRejectsBadRecipientWithoutLeavingARepository(t *testing.T) {
	cfg := testConfig(t)
	cfg.Encryption.Recipients = []string{"age1notakey"}
	if _, err := Init(cfg); err == nil {
		t.Fatal("bad recipient accepted")
	}
	if _, err := repo.Open(cfg.Repository.Path, nil); !errors.Is(err, repo.ErrNotARepo) {
		t.Fatalf("a failed init must leave no repository, got %v", err)
	}
	// and the directory can be initialised again with a good key
	cfg.Encryption.Recipients = []string{newEncKeys(t).recipient}
	if _, err := Init(cfg); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestInitRejectsPrivateKeyAsRecipient(t *testing.T) {
	keys := newEncKeys(t)
	cfg := testConfig(t)
	cfg.Encryption.Recipients = []string{strings.TrimSpace(keys.identity[strings.Index(keys.identity, "AGE-SECRET-KEY-1"):])}
	_, err := Init(cfg)
	if err == nil || !strings.Contains(err.Error(), "private key") || strings.Contains(err.Error(), "AGE-SECRET-KEY-1") {
		t.Fatalf("got %v", err)
	}
}

func TestCheckEncryptedWithoutAnyKey(t *testing.T) {
	cfg, keys := initEncrypted(t)
	addEncBlob(t, cfg, keys, "first secret message")
	addEncBlob(t, cfg, keys, "second secret message")
	// the cron box: no identity anywhere in the config.
	rep, err := Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if err != nil || !rep.OK() || len(rep.Warnings) != 0 {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	if !rep.Encrypted || rep.Blobs != 2 || rep.Objects != 2 || rep.ObjectsVerified != 2 || rep.ObjectsDecrypted != 0 {
		t.Fatalf("report: %+v", rep)
	}
}

func TestCheckEncryptedDetectsBitRotWithoutKey(t *testing.T) {
	cfg, keys := initEncrypted(t)
	k := addEncBlob(t, cfg, keys, strings.Repeat("rot ", 200))
	p := objPath(cfg, k)
	b, _ := os.ReadFile(p)
	b[len(b)-3] ^= 0xff
	_ = os.WriteFile(p, b, 0o600)

	rep, err := Check(context.Background(), cfg, CheckOptions{})
	if err != nil || !rep.OK() {
		t.Fatalf("without --read-data nothing is read: %v %+v", err, rep.Errors)
	}
	rep, err = Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || kinds(rep.Errors)["corrupt-object"] != 1 {
		t.Fatalf("want corrupt-object found with no key: %+v", rep.Errors)
	}
}

func TestCheckEncryptedIgnoresUnverifiableOrphans(t *testing.T) {
	cfg, keys := initEncrypted(t)
	addEncBlob(t, cfg, keys, "indexed")
	r, _ := repo.Open(cfg.Repository.Path, nil)
	st, _ := openStore(r, "")
	if _, _, _, err := st.Put([]byte("orphan from a crashed run")); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	rep, err := Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if err != nil || !rep.OK() || kinds(rep.Warnings)["orphan-object"] != 1 {
		t.Fatalf("err=%v errors=%+v warnings=%+v", err, rep.Errors, rep.Warnings)
	}
}

func TestCheckEncryptedWrongRecordedStoredHash(t *testing.T) {
	cfg, keys := initEncrypted(t)
	addEncBlob(t, cfg, keys, "hash recorded wrongly")
	r, _ := repo.Open(cfg.Repository.Path, nil)
	db, _ := index.Open(r.IndexPath(), index.Options{})
	if _, err := db.Exec(`UPDATE blobs SET stored_sha256 = zeroblob(32)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	rep, _ := Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if rep.OK() || kinds(rep.Errors)["corrupt-object"] != 1 {
		t.Fatalf("want corrupt-object: %+v", rep.Errors)
	}
}

func TestCheckFlagsPlaintextObjectInEncryptedRepository(t *testing.T) {
	cfg, _ := initEncrypted(t)
	r, _ := repo.Open(cfg.Repository.Path, nil)
	plain, err := objects.Open(r.ObjectsDir(), r.TmpDir(), objects.Options{})
	if err != nil {
		t.Fatal(err)
	}
	k, stored, _, err := plain.Put([]byte("this should have been encrypted"))
	_ = plain.Close()
	if err != nil {
		t.Fatal(err)
	}
	insertBlobRow(t, r, k, stored, 31)
	rep, err := Check(context.Background(), cfg, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || kinds(rep.Errors)["plaintext-object"] != 1 {
		t.Fatalf("want plaintext-object: %+v", rep.Errors)
	}
}

func TestCheckFlagsEncryptedObjectInPlainRepository(t *testing.T) {
	cfg := initRepo(t)
	keys := newEncKeys(t)
	r, _ := repo.Open(cfg.Repository.Path, nil)
	ids, _ := seal.ParseIdentities(keys.identity)
	rs, _ := seal.ParseRecipients([]string{keys.recipient})
	st, err := objects.Open(r.ObjectsDir(), r.TmpDir(), objects.Options{Sealer: seal.NewSealer(rs), Opener: seal.NewOpener(ids)})
	if err != nil {
		t.Fatal(err)
	}
	k, stored, _, _ := st.Put([]byte("unexpectedly encrypted"))
	_ = st.Close()
	insertBlobRow(t, r, k, stored, 22)
	rep, _ := Check(context.Background(), cfg, CheckOptions{})
	if rep.OK() || kinds(rep.Errors)["unexpected-encrypted-object"] != 1 {
		t.Fatalf("want unexpected-encrypted-object: %+v", rep.Errors)
	}
}

func TestCheckDecryptWithRightKey(t *testing.T) {
	cfg, keys := initEncrypted(t)
	addEncBlob(t, cfg, keys, "one")
	addEncBlob(t, cfg, keys, "two")
	cfg.Encryption.Identity = keys.identity
	rep, err := Check(context.Background(), cfg, CheckOptions{Decrypt: true})
	if err != nil || !rep.OK() {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	if rep.ObjectsDecrypted != 2 || rep.ObjectsVerified != 2 {
		t.Fatalf("Decrypt implies ReadData: %+v", rep)
	}
}

func TestCheckDecryptNeedsTheRightIdentity(t *testing.T) {
	cfg, keys := initEncrypted(t)
	addEncBlob(t, cfg, keys, "x")

	cfg.Encryption.Identity = ""
	if _, err := Check(context.Background(), cfg, CheckOptions{Decrypt: true}); !errors.Is(err, config.ErrNoIdentity) {
		t.Fatalf("no identity: want ErrNoIdentity, got %v", err)
	}
	cfg.Encryption.Identity = newEncKeys(t).identity
	_, err := Check(context.Background(), cfg, CheckOptions{Decrypt: true})
	if !errors.Is(err, seal.ErrWrongIdentity) {
		t.Fatalf("wrong identity must be reported as such, not as corruption: %v", err)
	}
	cfg.Encryption.Identity = "not an identity"
	if _, err := Check(context.Background(), cfg, CheckOptions{Decrypt: true}); err == nil || strings.Contains(err.Error(), "not an identity") {
		t.Fatalf("garbage identity: %v", err)
	}
}

func TestCheckDecryptCatchesWhatTheStoredHashCannot(t *testing.T) {
	cfg, keys := initEncrypted(t)
	k := addEncBlob(t, cfg, keys, "attacker rewrites file and index together")
	// an attacker with disk access replaces the object with garbage and fixes the recorded hash.
	garbage := append([]byte{0x03}, []byte("not really age data at all, but self-consistent")...)
	_ = os.WriteFile(objPath(cfg, k), garbage, 0o600)
	r, _ := repo.Open(cfg.Repository.Path, nil)
	db, _ := index.Open(r.IndexPath(), index.Options{})
	sum := sha256.Sum256(garbage)
	if _, err := db.Exec(`UPDATE blobs SET stored_sha256 = ?`, sum[:]); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	rep, _ := Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if !rep.OK() {
		t.Fatalf("the key-free check cannot see a self-consistent forgery: %+v", rep.Errors)
	}
	cfg.Encryption.Identity = keys.identity
	rep, err := Check(context.Background(), cfg, CheckOptions{Decrypt: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || kinds(rep.Errors)["corrupt-object"] != 1 {
		t.Fatalf("decrypting must catch it: %+v", rep.Errors)
	}
}

func TestCheckDecryptInPlainRepositoryNeedsNoKey(t *testing.T) {
	cfg := initRepo(t)
	addBlob(t, cfg, "plain one")
	rep, err := Check(context.Background(), cfg, CheckOptions{Decrypt: true})
	if err != nil || !rep.OK() || rep.Encrypted {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
}

func TestCheckPlainReadDataDetectsCorruptionOfOrphans(t *testing.T) {
	cfg := initRepo(t)
	r, _ := repo.Open(cfg.Repository.Path, nil)
	st, _ := openStore(r, "")
	k, _, _, _ := st.Put([]byte(strings.Repeat("orphan ", 50)))
	_ = st.Close()
	b, _ := os.ReadFile(objPath(cfg, k))
	b[len(b)/2] ^= 0xff
	_ = os.WriteFile(objPath(cfg, k), b, 0o600)
	rep, _ := Check(context.Background(), cfg, CheckOptions{ReadData: true})
	if rep.OK() || kinds(rep.Errors)["corrupt-object"] != 1 {
		t.Fatalf("plain repos can verify every object: %+v", rep.Errors)
	}
}

func TestCheckRefusesFormatOneRepository(t *testing.T) {
	cfg := initRepo(t)
	p := filepath.Join(cfg.Repository.Path, "repo.toml")
	b, _ := os.ReadFile(p)
	_ = os.WriteFile(p, []byte(strings.Replace(string(b), "format_version = 2", "format_version = 1", 1)), 0o600)
	_, err := Check(context.Background(), cfg, CheckOptions{})
	if !errors.Is(err, repo.ErrFormatVersion) || !strings.Contains(err.Error(), "re-create") {
		t.Fatalf("got %v", err)
	}
}
