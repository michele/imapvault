package objects

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/quick"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "objects"), filepath.Join(root, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}

func TestPutGetRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	raw := []byte("From: a@example.com\r\nSubject: x\r\n\r\nbody\r\n")
	key, added, err := s.Put(raw)
	if err != nil || !added {
		t.Fatalf("put: added=%v err=%v", added, err)
	}
	if key != Key(sha256.Sum256(raw)) {
		t.Fatalf("key is not sha256 of raw bytes")
	}
	got, err := s.Get(key)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("get: %v", err)
	}
	if ok, err := s.Has(key); err != nil || !ok {
		t.Fatalf("has: %v %v", ok, err)
	}
}

func TestPutDedupes(t *testing.T) {
	s, _ := newStore(t)
	raw := []byte("same")
	k1, a1, _ := s.Put(raw)
	k2, a2, err := s.Put(raw)
	if err != nil || k1 != k2 || !a1 || a2 {
		t.Fatalf("dedupe: %v %v %v %v", k1 == k2, a1, a2, err)
	}
}

func TestEmptyAndBinaryPayloads(t *testing.T) {
	s, _ := newStore(t)
	for _, raw := range [][]byte{{}, {0}, bytes.Repeat([]byte{0xff}, 1<<20)} {
		k, _, err := s.Put(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(k)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("round trip len %d: %v", len(raw), err)
		}
	}
}

func TestRoundTripProperty(t *testing.T) {
	s, _ := newStore(t)
	f := func(raw []byte) bool {
		k, _, err := s.Put(raw)
		if err != nil {
			return false
		}
		got, err := s.Get(k)
		return err == nil && bytes.Equal(got, raw)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatal(err)
	}
}

func TestGetMissing(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Get(Key{1})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if ok, err := s.Has(Key{1}); ok || err != nil {
		t.Fatalf("has: %v %v", ok, err)
	}
}

func TestCorruptionDetected(t *testing.T) {
	raw := bytes.Repeat([]byte("corrupt me "), 500)
	// every single-byte flip of the stored file must be caught.
	s, _ := newStore(t)
	k, _, _ := s.Put(raw)
	path := s.path(k)
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range orig {
		bad := append([]byte(nil), orig...)
		bad[i] ^= 0x55
		if err := os.WriteFile(path, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := s.Get(k)
		if err == nil {
			t.Fatalf("flip at byte %d went unnoticed", i)
		}
		// byte 0 is the envelope: a flipped bit there is "unsupported", not "corrupt".
		want := ErrCorrupt
		if i == 0 {
			want = ErrUnsupported
		}
		if !errors.Is(err, want) {
			t.Fatalf("flip at byte %d: want %v, got %v", i, want, err)
		}
		if err := s.Verify(k); !errors.Is(err, want) {
			t.Fatalf("verify flip %d: %v", i, err)
		}
	}
}

func TestTruncationDetected(t *testing.T) {
	s, _ := newStore(t)
	k, _, _ := s.Put(bytes.Repeat([]byte("trunc "), 1000))
	orig, _ := os.ReadFile(s.path(k))
	for _, n := range []int{0, 1, len(orig) / 2, len(orig) - 1} {
		if err := os.WriteFile(s.path(k), orig[:n], 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(k); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("truncated to %d: want ErrCorrupt, got %v", n, err)
		}
	}
}

func TestUnknownEnvelopeBitsRejected(t *testing.T) {
	s, _ := newStore(t)
	k, _, _ := s.Put([]byte("hello"))
	orig, _ := os.ReadFile(s.path(k))
	for _, flags := range []byte{0x00, 0x02, 0x03, 0x04, 0x81, 0xff} {
		b := append([]byte(nil), orig...)
		b[0] = flags
		_ = os.WriteFile(s.path(k), b, 0o600)
		if _, err := s.Get(k); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("flags %#x: want ErrUnsupported, got %v", flags, err)
		}
	}
}

func TestVerifyOK(t *testing.T) {
	s, _ := newStore(t)
	k, _, _ := s.Put([]byte("fine"))
	if err := s.Verify(k); err != nil {
		t.Fatal(err)
	}
}

func TestWalk(t *testing.T) {
	s, _ := newStore(t)
	want := map[Key]bool{}
	for i := 0; i < 20; i++ {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		k, _, _ := s.Put(b)
		want[k] = true
	}
	// junk that is not an object must be skipped, not fail the walk.
	_ = os.WriteFile(filepath.Join(s.root, "README"), []byte("x"), 0o600)
	got := map[Key]bool{}
	var junk int
	err := s.Walk(func(k Key) error { got[k] = true; return nil }, func(string) { junk++ })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("walked %d keys, want %d", len(got), len(want))
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("missing %x", k)
		}
	}
	if junk != 1 {
		t.Fatalf("junk callback called %d times, want 1", junk)
	}
}

func TestLeftoverTmpDoesNotBreakPut(t *testing.T) {
	s, root := newStore(t)
	// a crash mid-write leaves a partial file in tmp/ and nothing in objects/.
	_ = os.WriteFile(filepath.Join(root, "tmp", "partial"), []byte{1, 2, 3}, 0o600)
	k, _, err := s.Put([]byte("after crash"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(k); err != nil {
		t.Fatal(err)
	}
}

func TestPutLeavesNoTmpFiles(t *testing.T) {
	s, root := newStore(t)
	for i := 0; i < 5; i++ {
		_, _, _ = s.Put([]byte{byte(i)})
	}
	ents, _ := os.ReadDir(filepath.Join(root, "tmp"))
	if len(ents) != 0 {
		t.Fatalf("tmp not empty: %d entries", len(ents))
	}
}

func TestLayoutAndModes(t *testing.T) {
	s, _ := newStore(t)
	k, _, _ := s.Put([]byte("layout"))
	hex := k.String()
	want := filepath.Join(s.root, hex[:2], hex[2:])
	if s.path(k) != want {
		t.Fatalf("path %s want %s", s.path(k), want)
	}
	fi, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestParseKey(t *testing.T) {
	k := Key(sha256.Sum256([]byte("x")))
	got, err := ParseKey(k.String())
	if err != nil || got != k {
		t.Fatalf("round trip: %v", err)
	}
	for _, bad := range []string{"", "zz", k.String()[:62], k.String() + "00"} {
		if _, err := ParseKey(bad); err == nil {
			t.Fatalf("ParseKey(%q) accepted", bad)
		}
	}
}

func FuzzGetNeverPanics(f *testing.F) {
	f.Add([]byte{0x01})
	f.Add([]byte{0x01, 0x28, 0xb5, 0x2f, 0xfd})
	f.Add([]byte{0xff, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		root := t.TempDir()
		s, err := Open(filepath.Join(root, "objects"), filepath.Join(root, "tmp"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		k := Key(sha256.Sum256(data))
		if err := os.MkdirAll(filepath.Dir(s.path(k)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.path(k), data, 0o600); err != nil {
			t.Fatal(err)
		}
		// must return an error or exactly the bytes whose hash is the key; never panic.
		if got, err := s.Get(k); err == nil && Key(sha256.Sum256(got)) != k {
			t.Fatal("Get returned data that does not match its key")
		}
	})
}
