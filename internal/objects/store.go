// Package objects is the content-addressed blob store. A blob is one raw RFC 822
// message, keyed by the SHA-256 of its exact bytes and stored as
// objects/ab/cdef… with a 1-byte envelope header followed by a zstd frame.
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
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
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

// Store is a blob store rooted at one directory. It is safe for concurrent use.
type Store struct {
	root, tmp string
	enc       *zstd.Encoder
	dec       *zstd.Decoder
}

// Open creates (if needed) and opens the store. tmp must be on the same
// filesystem as root so that renames are atomic.
func Open(root, tmp string) (*Store, error) {
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
	return &Store{root: root, tmp: tmp, enc: enc, dec: dec}, nil
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

// Put stores raw and returns its key. added is false when the blob already
// existed (deduplication). The object is durable when Put returns.
func (s *Store) Put(raw []byte) (key Key, added bool, err error) {
	key = Key(sha256.Sum256(raw))
	if ok, err := s.Has(key); err != nil || ok {
		return key, false, err
	}

	payload := make([]byte, 1, 1+len(raw)/2+64)
	payload[0] = flagZstd
	payload = s.enc.EncodeAll(raw, payload)

	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return key, false, err
	}
	tmpPath := filepath.Join(s.tmp, hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return key, false, err
	}
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		cleanup()
		return key, false, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return key, false, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return key, false, err
	}

	dst := s.path(key)
	dir := filepath.Dir(dst)
	if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			cleanup()
			return key, false, err
		}
		if err := syncDir(s.root); err != nil {
			cleanup()
			return key, false, err
		}
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		cleanup()
		return key, false, err
	}
	if err := syncDir(dir); err != nil {
		return key, false, err
	}
	return key, true, nil
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
	raw, err := s.dec.DecodeAll(data[1:], nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, k, err)
	}
	if Key(sha256.Sum256(raw)) != k {
		return nil, fmt.Errorf("%w: %s: hash mismatch", ErrCorrupt, k)
	}
	return raw, nil
}

// Verify re-reads and re-hashes one object.
func (s *Store) Verify(k Key) error {
	_, err := s.Get(k)
	return err
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
