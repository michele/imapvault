// Package objects is the content-addressed blob store. A blob is one raw RFC 822
// message, keyed by the SHA-256 of its exact bytes and stored as
// objects/ab/cdef… with a 1-byte envelope header followed by a zstd frame, which
// in an encrypted repository is itself age-encrypted (compress, then encrypt).
//
// A store opened with a Sealer (public keys) can Put and VerifyStored; only one
// opened with an Opener (private key) can Get. That is what lets a cron job back
// up and check integrity with no private key on the machine.
//
// Durability: Put fsyncs the data and renames it into place (then fsyncs the
// directory) before returning, so a caller that commits an index row after Put
// can never reference a blob that is not on disk. A crash can leave orphan
// blobs or files in tmp/, never a half-written object.
package objects

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"

	"github.com/michele/imapvault/internal/seal"
)

// maxDecoded bounds decompression so a corrupt or hostile frame cannot exhaust memory.
const maxDecoded = 2 << 30

// Key is the SHA-256 of a message's raw bytes.
type Key [sha256.Size]byte

// String is the lower-case hex form used for file names.
func (k Key) String() string { return hex.EncodeToString(k[:]) }

// ParseKey parses the 64-character hex form.
func ParseKey(s string) (Key, error) {
	var k Key
	if len(s) != hex.EncodedLen(len(k)) {
		return k, fmt.Errorf("invalid object key %q", s)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return k, fmt.Errorf("invalid object key %q", s)
	}
	copy(k[:], b)
	return k, nil
}

// Options choose between plain and encrypted operation. The zero value is a
// plain store that cannot read encrypted objects.
type Options struct {
	Sealer *seal.Sealer // recipients: encrypts what Put writes; nil or no recipients = plain
	Opener *seal.Opener // identities: needed by Get/Verify of encrypted objects
}

// Store is a blob store rooted at one directory. It is safe for concurrent use.
type Store struct {
	root, tmp string
	enc       *zstd.Encoder
	dec       *zstd.Decoder
	sealer    *seal.Sealer
	opener    *seal.Opener
}

// Open creates (if needed) and opens the store. tmp must be on the same
// filesystem as root so that renames are atomic.
func Open(root, tmp string, opts Options) (*Store, error) {
	for _, d := range []string{root, tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxDecoded))
	if err != nil {
		_ = enc.Close()
		return nil, err
	}
	st := &Store{root: root, tmp: tmp, enc: enc, dec: dec, sealer: opts.Sealer, opener: opts.Opener}
	if st.sealer == nil {
		st.sealer = seal.NewSealer(nil)
	}
	if st.opener == nil {
		st.opener = seal.NewOpener(nil)
	}
	return st, nil
}

// Close releases the compression state.
func (s *Store) Close() error {
	s.dec.Close()
	return s.enc.Close()
}

func (s *Store) path(k Key) string {
	h := k.String()
	return filepath.Join(s.root, h[:2], h[2:])
}

// Has reports whether an object file exists for the key (it does not read it).
func (s *Store) Has(k Key) (bool, error) {
	_, err := os.Stat(s.path(k))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// Size is the size in bytes of the object file for the key (what it occupies on
// disk, not the message size). A missing object is ErrNotFound.
func (s *Store) Size(k Key) (int64, error) {
	st, err := os.Stat(s.path(k))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, k)
	}
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Delete removes the object file for the key and returns the bytes it freed. An
// object that is already gone is not an error (a prune interrupted after its
// index commit is resumed by running it again). The now empty shard directory is
// removed too. Only prune may call this, with the repository lock held and after
// the index no longer references the object.
func (s *Store) Delete(k Key) (int64, error) {
	p := s.path(k)
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	// Fails, harmlessly, while the shard still holds other objects.
	_ = os.Remove(filepath.Dir(p))
	return st.Size(), nil
}

// Put stores raw and returns its key and the SHA-256 of the exact file bytes
// ("stored", which the index records so integrity can be checked without the
// private key). added is false when the blob already existed (deduplication); the
// stored hash of the existing file is then recomputed, so callers should consult
// the index first and call Put only for blobs they do not already have. The
// object is durable when Put returns.
func (s *Store) Put(raw []byte) (key Key, stored [32]byte, added bool, err error) {
	key = Key(sha256.Sum256(raw))
	if ok, err := s.Has(key); err != nil {
		return key, stored, false, err
	} else if ok {
		stored, err = s.fileHash(key)
		return key, stored, false, err
	}

	flags := flagZstd
	body := s.enc.EncodeAll(raw, nil)
	if s.sealer.Encrypting() {
		if body, err = s.sealer.Encrypt(body); err != nil {
			return key, stored, false, err
		}
		flags |= flagEncrypted
	}
	payload := append([]byte{flags}, body...)
	stored = sha256.Sum256(payload)

	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return key, stored, false, err
	}
	tmpPath := filepath.Join(s.tmp, hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return key, stored, false, err
	}
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		cleanup()
		return key, stored, false, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return key, stored, false, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return key, stored, false, err
	}

	dst := s.path(key)
	dir := filepath.Dir(dst)
	if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			cleanup()
			return key, stored, false, err
		}
		if err := syncDir(s.root); err != nil {
			cleanup()
			return key, stored, false, err
		}
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		cleanup()
		return key, stored, false, err
	}
	if err := syncDir(dir); err != nil {
		return key, stored, false, err
	}
	return key, stored, true, nil
}

// Get returns the raw message for key, verifying its hash. It returns
// ErrNotFound, ErrCorrupt or ErrUnsupported (wrapped) on failure.
func (s *Store) Get(k Key) ([]byte, error) {
	data, err := os.ReadFile(s.path(k))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, k)
	}
	if err != nil {
		return nil, err
	}
	if len(data) < 1 {
		return nil, fmt.Errorf("%w: %s: empty file", ErrCorrupt, k)
	}
	if err := checkFlags(data[0]); err != nil {
		return nil, fmt.Errorf("%s: %w", k, err)
	}
	body := data[1:]
	if data[0]&flagEncrypted != 0 {
		if body, err = s.opener.Decrypt(body); err != nil {
			if errors.Is(err, seal.ErrCorrupt) {
				return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, k, err)
			}
			return nil, fmt.Errorf("%s: %w", k, err)
		}
	}
	raw, err := s.dec.DecodeAll(body, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, k, err)
	}
	if Key(sha256.Sum256(raw)) != k {
		return nil, fmt.Errorf("%w: %s: hash mismatch", ErrCorrupt, k)
	}
	return raw, nil
}

// Verify re-reads, decrypts if needed, and re-hashes one object. It needs the
// private key for encrypted objects; VerifyStored does not.
func (s *Store) Verify(k Key) error {
	_, err := s.Get(k)
	return err
}

// VerifyStored checks that the object file still has the SHA-256 recorded at Put
// time. It reads the file but needs no key, so a backup machine that holds only
// public keys can detect bit rot and truncation. It cannot detect an attacker who
// rewrites both the file and its recorded hash; decrypting (Verify) does.
func (s *Store) VerifyStored(k Key, want [32]byte) error {
	got, err := s.fileHash(k)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: %s: file does not match its recorded hash", ErrCorrupt, k)
	}
	return nil
}

// Encrypted reports whether the object's envelope says it is encrypted. It reads
// one byte.
func (s *Store) Encrypted(k Key) (bool, error) {
	f, err := os.Open(s.path(k))
	if errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("%w: %s", ErrNotFound, k)
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return false, fmt.Errorf("%w: %s: empty file", ErrCorrupt, k)
	}
	if err := checkFlags(b[0]); err != nil {
		return false, fmt.Errorf("%s: %w", k, err)
	}
	return b[0]&flagEncrypted != 0, nil
}

func (s *Store) fileHash(k Key) ([32]byte, error) {
	data, err := os.ReadFile(s.path(k))
	if errors.Is(err, fs.ErrNotExist) {
		return [32]byte{}, fmt.Errorf("%w: %s", ErrNotFound, k)
	}
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

// Walk calls fn for every well-formed object file (<2 hex>/<62 hex>). Anything
// else under the root is reported to junk (which may be nil) and skipped.
func (s *Store) Walk(fn func(Key) error, junk func(path string)) error {
	top, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	report := func(p string) {
		if junk != nil {
			junk(p)
		}
	}
	for _, d := range top {
		p := filepath.Join(s.root, d.Name())
		if !d.IsDir() || len(d.Name()) != 2 {
			report(p)
			continue
		}
		files, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		for _, f := range files {
			k, err := ParseKey(d.Name() + f.Name())
			if err != nil || f.IsDir() {
				report(filepath.Join(p, f.Name()))
				continue
			}
			if err := fn(k); err != nil {
				return err
			}
		}
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
