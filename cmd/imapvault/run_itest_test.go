//go:build integration

package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func TestRunBacksUpOnAnIntervalAndStopsCleanly(t *testing.T) {
	itestAccount(t, "")
	t.Setenv("IMAPVAULT_IDENTITY", "")
	run(t, "init")

	old := minRunEvery
	minRunEvery = time.Second
	t.Cleanup(func() { minRunEvery = old })

	root := newRootCmd()
	out := &lockedBuffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--account", "it", "run", "--every", "2s", "--forget-every", "1h"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	deadline := time.After(90 * time.Second)
	for strings.Count(out.String(), "run: backup done") < 2 {
		select {
		case err := <-done:
			t.Fatalf("run ended early: %v\n%s", err, out.String())
		case <-deadline:
			t.Fatalf("no second backup in time:\n%s", out.String())
		case <-time.After(200 * time.Millisecond):
		}
	}
	cancel() // what SIGTERM does
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stopping is not an error: %v\n%s", err, out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("run did not stop:\n%s", out.String())
	}

	log := out.String()
	t.Logf("run's log:\n%s", log)
	for _, want := range []string{"run: started", "run: backup done: snapshot 1, 13 messages", "run: forget: kept", "+0 new", "run: stopping"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q", want)
		}
	}
	if strings.Contains(log, "failed") {
		t.Errorf("something failed:\n%s", log)
	}
	if snaps := run(t, "snapshots"); !strings.Contains(snaps, "2 ") {
		t.Errorf("snapshots: %s", snaps)
	}
}
