package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/mail"
	"github.com/michele/imapvault/internal/mail/mailtest"
	"github.com/michele/imapvault/internal/plan"
	"github.com/michele/imapvault/internal/repo"
)

var runT0 = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

// runHarness drives Run with a fake clock and sleep, so no test ever waits.
type runHarness struct {
	t         *testing.T
	cfg       *config.Config
	fake      *mailtest.Fake
	now       time.Time
	sleeps    []time.Duration
	dials     []time.Time
	logs      []string
	stopAfter int // cancel after this many sleeps
	onDial    func(n int)
	dialErr   func(n int) error
	onSleep   func(n int)
	cancel    context.CancelFunc
}

// withRetention gives a hand-built test config the defaults that Parse would.
func withRetention(cfg *config.Config) *config.Config {
	cfg.Retention = config.Retention{KeepLast: 3, KeepHourly: 24, KeepDaily: 14, DeletedGrace: "180d", DeletedGraceDur: 180 * 24 * time.Hour}
	return cfg
}

func newRunHarness(t *testing.T, cfg *config.Config, f *mailtest.Fake) *runHarness {
	withRetention(cfg)
	return &runHarness{t: t, cfg: cfg, fake: f, now: runT0, stopAfter: 3}
}

func (h *runHarness) logged() string { return strings.Join(h.logs, "\n") }

func (h *runHarness) run(s plan.Schedule) error {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	defer cancel()
	return Run(ctx, RunOptions{
		Schedule:    s,
		LoadOnline:  func() (*config.Config, error) { return h.cfg, nil },
		LoadOffline: func() (*config.Config, error) { return h.cfg, nil },
		Dial: func(context.Context, *config.Config) (mail.Source, error) {
			n := len(h.dials) + 1
			h.dials = append(h.dials, h.now)
			if h.onDial != nil {
				h.onDial(n)
			}
			if h.dialErr != nil {
				if err := h.dialErr(n); err != nil {
					return nil, err
				}
			}
			return h.fake, nil
		},
		Logf: func(format string, args ...any) { h.logs = append(h.logs, fmt.Sprintf(format, args...)) },
		Now:  func() time.Time { return h.now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			h.sleeps = append(h.sleeps, d)
			h.now = h.now.Add(d)
			if h.onSleep != nil {
				h.onSleep(len(h.sleeps))
			}
			if len(h.sleeps) >= h.stopAfter {
				cancel()
			}
			return ctx.Err()
		},
	})
}

func hourly() plan.Schedule {
	return plan.Schedule{Every: time.Hour, ForgetEvery: 24 * time.Hour, StaleAfter: 0}
}

func TestRunBacksUpOnTheIntervalPicksUpNewMailAndForgetsOnce(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	h := newRunHarness(t, cfg, f)
	h.onDial = func(n int) {
		if n == 2 {
			f.Folders[0].Add(msg("9", "late", "arrived between runs"))
		}
	}
	if err := h.run(hourly()); err != nil {
		t.Fatal(err)
	}
	if len(h.dials) != 3 {
		t.Fatalf("%d backups, want 3: %s", len(h.dials), h.logged())
	}
	for i, at := range h.dials {
		if want := runT0.Add(time.Duration(i) * time.Hour); !at.Equal(want) {
			t.Errorf("backup %d at %v, want %v", i+1, at, want)
		}
	}
	for _, d := range h.sleeps {
		if d != time.Hour {
			t.Errorf("slept %v, want 1h", d)
		}
	}
	log := h.logged()
	if !strings.Contains(log, "+1 new") {
		t.Errorf("the second run should report the new message:\n%s", log)
	}
	if n := strings.Count(log, "run: forget: kept"); n != 1 || strings.Contains(log, "failed") {
		t.Errorf("forget ran %d times in 2 hours, want once:\n%s", n, log)
	}
	if strings.Contains(strings.ToLower(log), "subject") {
		t.Errorf("a log line mentions a subject:\n%s", log)
	}
}

func TestRunStopsCleanlyWhenToldTo(t *testing.T) {
	cfg := initRepo(t)
	h := newRunHarness(t, cfg, fakeMailbox())
	h.stopAfter = 1
	if err := h.run(hourly()); err != nil {
		t.Fatalf("a stop is not an error: %v", err)
	}
}

func TestRunBacksOffAfterFailuresAndRecovers(t *testing.T) {
	cfg := initRepo(t)
	h := newRunHarness(t, cfg, fakeMailbox())
	h.dialErr = func(n int) error {
		if n <= 4 {
			return errors.New("dial: connection refused")
		}
		return nil
	}
	h.stopAfter = 5
	if err := h.run(hourly()); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, time.Hour}
	if fmt.Sprint(h.sleeps) != fmt.Sprint(want) {
		t.Fatalf("sleeps %v, want %v\n%s", h.sleeps, want, h.logged())
	}
	if !strings.Contains(h.logged(), "backup failed: dial: connection refused") {
		t.Errorf("the failure is not logged:\n%s", h.logged())
	}
}

func TestRunTreatsALockedRepositoryAsBusyNotAsAFailure(t *testing.T) {
	cfg := initRepo(t)
	acct := accountOf(cfg)
	r, err := repo.Open(cfg.Repository.Path, &acct)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := r.Lock(repo.Exclusive) // somebody's manual prune
	if err != nil {
		t.Fatal(err)
	}
	h := newRunHarness(t, cfg, fakeMailbox())
	h.onSleep = func(n int) {
		if n == 1 {
			unlock()
		}
	}
	h.stopAfter = 2
	if err := h.run(plan.Schedule{Every: time.Hour, StaleAfter: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if h.sleeps[0] != time.Minute {
		t.Errorf("busy retry in %v, want 1m", h.sleeps[0])
	}
	if !strings.Contains(h.logged(), "busy") || strings.Contains(h.logged(), "backup failed") {
		t.Errorf("a locked repository must read as busy:\n%s", h.logged())
	}
	if len(h.dials) != 2 || !strings.Contains(h.logged(), "backup done") {
		t.Errorf("the retry should have succeeded:\n%s", h.logged())
	}
}

func TestRunTellsOnceWhenBackupsHaveBeenFailingForTooLong(t *testing.T) {
	var posts atomic.Int32
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		b := make([]byte, 512)
		n, _ := r.Body.Read(b)
		body.Store(string(b[:n]))
	}))
	defer srv.Close()
	cfg := initRepo(t)
	cfg.Guard.OnAlertURL = srv.URL + "/" + topic
	h := newRunHarness(t, cfg, fakeMailbox())
	h.dialErr = func(int) error { return errors.New("dial: no route to host") }
	h.stopAfter = 40
	err := h.run(plan.Schedule{Every: 10 * time.Minute, StaleAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 {
		t.Fatalf("%d notifications, want exactly one per streak\n%s", posts.Load(), h.logged())
	}
	if got, _ := body.Load().(string); !strings.Contains(got, "no successful backup for 1h") || !strings.Contains(got, "no route to host") {
		t.Errorf("notification body %q", got)
	}
	if strings.Contains(h.logged(), topic) {
		t.Errorf("the URL leaked into the log:\n%s", h.logged())
	}
}

func TestRunKeepsGoingAfterAGuardAlertAndPruneStaysRefused(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { posts.Add(1) }))
	defer srv.Close()
	cfg := guardConfig(t)
	cfg.Guard.OnAlertURL = srv.URL + "/x"
	f := bigMailbox(20)
	h := newRunHarness(t, cfg, f)
	h.onDial = func(n int) {
		if n == 2 {
			deleteFirst(f.Folders[0], 15)
		}
	}
	h.stopAfter = 4
	err := h.run(plan.Schedule{Every: time.Hour, ForgetEvery: 24 * time.Hour, PruneEvery: 3 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	log := h.logged()
	if len(h.dials) != 4 {
		t.Fatalf("%d backups, want 4: the loop must go on after an alert\n%s", len(h.dials), log)
	}
	if posts.Load() != 1 {
		t.Errorf("%d notifications, want 1: later runs compare with the flagged snapshot and stay quiet", posts.Load())
	}
	if !strings.Contains(log, "GUARD ALERT") {
		t.Errorf("an alert must be loud in the log:\n%s", log)
	}
	if !strings.Contains(log, "run: prune") || !strings.Contains(log, "unacknowledged") {
		t.Errorf("prune should be refused and say why:\n%s", log)
	}
	n, err := UnackedAlerts(cfg)
	if err != nil || n != 1 {
		t.Fatalf("unacknowledged alerts: %d, %v", n, err)
	}
}

func TestRunCancelledInTheMiddleOfABackupReturnsNil(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	h := newRunHarness(t, cfg, f)
	f.FailAt = func(call string) error {
		if strings.HasPrefix(call, "fetch-bodies") {
			h.cancel()
			return context.Canceled
		}
		return nil
	}
	if err := h.run(hourly()); err != nil {
		t.Fatalf("a stop mid-backup is not an error: %v", err)
	}
	if len(h.dials) != 1 || len(h.sleeps) != 0 {
		t.Errorf("dials %d, sleeps %d", len(h.dials), len(h.sleeps))
	}
	if strings.Contains(h.logged(), "backup failed") {
		t.Errorf("an interrupted run is not a failure:\n%s", h.logged())
	}
}

func TestRunRefusesToStartWithoutARepository(t *testing.T) {
	cfg := testConfig(t) // never initialised
	h := newRunHarness(t, cfg, fakeMailbox())
	err := h.run(hourly())
	if err == nil || !strings.Contains(err.Error(), "repo.toml") {
		t.Fatalf("got %v", err)
	}
	if len(h.dials) != 0 {
		t.Error("it dialled the server with nowhere to put the mail")
	}
}

func TestRunRefusesToStartWhenTheConfigDoesNotLoadOnline(t *testing.T) {
	cfg := initRepo(t)
	h := newRunHarness(t, cfg, fakeMailbox())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := Run(ctx, RunOptions{
		Schedule:    hourly(),
		LoadOffline: func() (*config.Config, error) { return cfg, nil },
		LoadOnline:  func() (*config.Config, error) { return nil, errors.New("imap.password_file: permission denied") },
		Logf:        func(string, ...any) {},
	})
	_ = h
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("got %v", err)
	}
}

func TestHumanDuration(t *testing.T) {
	for in, want := range map[time.Duration]string{
		0: "0s", -time.Second: "0s", 400 * time.Millisecond: "0s", 15 * time.Second: "15s", 59*time.Second + 600*time.Millisecond: "1m",
		time.Minute: "1m", 90 * time.Second: "1m30s", 12 * time.Minute: "12m", time.Hour: "1h", time.Hour + 5*time.Minute: "1h05m",
		24 * time.Hour: "24h", 24*time.Hour + 30*time.Minute: "24h30m",
	} {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}
