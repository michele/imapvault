//go:build unix

package repo

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Init(filepath.Join(t.TempDir(), "repo"), acct)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExclusiveLockExcludesEverything(t *testing.T) {
	r := newRepo(t)
	unlock, err := r.Lock(Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	// flock conflicts between distinct open file descriptions even within one process,
	// so this exercises the same path a second imapvault process would take.
	for _, mode := range []LockMode{Exclusive, Shared} {
		_, err := r.Lock(mode)
		if !errors.Is(err, ErrLocked) {
			t.Fatalf("mode %v while exclusive held: want ErrLocked, got %v", mode, err)
		}
	}
	unlock()
	u2, err := r.Lock(Exclusive)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	u2()
}

func TestSharedLocksCoexistButBlockExclusive(t *testing.T) {
	r := newRepo(t)
	u1, err := r.Lock(Shared)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := r.Lock(Shared)
	if err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	if _, err := r.Lock(Exclusive); !errors.Is(err, ErrLocked) {
		t.Fatalf("exclusive while shared held: want ErrLocked, got %v", err)
	}
	u1()
	if _, err := r.Lock(Exclusive); !errors.Is(err, ErrLocked) {
		t.Fatalf("exclusive with one shared left: want ErrLocked, got %v", err)
	}
	u2()
	u3, err := r.Lock(Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	u3()
}

func TestLockErrorNamesHolder(t *testing.T) {
	r := newRepo(t)
	unlock, err := r.Lock(Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = r.Lock(Exclusive)
	if err == nil || !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("error should name the holder pid: %v", err)
	}
}

func TestUnlockIsIdempotent(t *testing.T) {
	r := newRepo(t)
	unlock, err := r.Lock(Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	unlock() // must not panic or release someone else's lock
	u2, err := r.Lock(Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	unlock() // stale unlock must not drop u2's lock
	if _, err := r.Lock(Exclusive); !errors.Is(err, ErrLocked) {
		t.Fatalf("stale unlock released a newer lock: %v", err)
	}
	u2()
}
