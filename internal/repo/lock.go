//go:build unix

package repo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// LockMode selects how Lock claims the repository.
type LockMode int

const (
	// Shared is for readers (check, ls, show). Any number may hold it.
	Shared LockMode = iota
	// Exclusive is for anything that writes (backup, prune, forget, restore-side
	// bookkeeping). It excludes readers and other writers.
	Exclusive
)

func (m LockMode) String() string {
	if m == Exclusive {
		return "exclusive"
	}
	return "shared"
}

// ErrLocked means another process holds a conflicting lock.
var ErrLocked = errors.New("repository is locked")

// Lock takes the repository lock without waiting. The returned function releases
// it and is safe to call more than once. The lock is an flock on <repo>/lock, so
// the kernel drops it if the process dies; there are no stale locks to clean up.
// An exclusive holder also writes its pid, host and start time into the file so
// that a refused caller can say who is in the way.
func (r *Repo) Lock(mode LockMode) (unlock func(), err error) {
	path := filepath.Join(r.Path, "lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if mode == Exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		holder := readHolder(f)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s lock refused%s", ErrLocked, mode, holder)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if mode == Exclusive {
		host, _ := os.Hostname()
		info := fmt.Sprintf("pid %d on %s since %s\n", os.Getpid(), host, time.Now().UTC().Format(time.RFC3339))
		if err := f.Truncate(0); err == nil {
			_, _ = f.WriteAt([]byte(info), 0)
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if mode == Exclusive {
				_ = f.Truncate(0)
			}
			_ = f.Close() // closing the descriptor releases the flock
		})
	}, nil
}

// readHolder describes the exclusive holder recorded in the lock file, if any.
func readHolder(f *os.File) string {
	b, err := io.ReadAll(io.NewSectionReader(f, 0, 512))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return " (held by another imapvault process)"
	}
	return " (held by " + s + ")"
}
