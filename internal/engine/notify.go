package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/michele/imapvault/internal/config"
)

// Event says why a notification is sent.
type Event string

const (
	// EventGuard: the mass-deletion guard tripped during a backup.
	EventGuard Event = "guard"
	// EventStale: `run` has had no successful backup for too long.
	EventStale Event = "stale"
)

// Notification is one message for the user. Summary holds folder names and
// counts only (safety rule 6).
type Notification struct {
	Event    Event
	Summary  string
	AlertID  int64 // EventGuard only
	Snapshot int64 // EventGuard only
}

// webhookTimeout bounds a delivery: a hung notification service must not keep a
// backup (and the repository lock it holds) alive. A variable for tests.
var webhookTimeout = 15 * time.Second

// Notify delivers n through guard.on_alert (a program) and guard.on_alert_url (a
// plain-text POST), whichever are configured, and tries both even if the first
// fails. It never fails its caller: the two errors are for the caller to report
// as warnings. A failing notification must never fail a backup, since the alert
// is already recorded. Nothing it returns contains the URL.
func Notify(ctx context.Context, cfg *config.Config, n Notification) (cmdErr, urlErr error) {
	account := cfg.Account
	if account == "" {
		account = cfg.IMAP.Username
	}
	if words, err := parseHook(cfg.Guard.OnAlert); err == nil && words != nil {
		env := []string{
			"IMAPVAULT_ACCOUNT=" + account,
			"IMAPVAULT_SUMMARY=" + n.Summary,
			"IMAPVAULT_EVENT=" + string(n.Event),
		}
		if n.Event == EventGuard {
			env = append(env, fmt.Sprintf("IMAPVAULT_ALERT_ID=%d", n.AlertID), fmt.Sprintf("IMAPVAULT_SNAPSHOT=%d", n.Snapshot))
		}
		cmdErr = runHook(ctx, hookArgs(words, account, n.Summary), env)
	}
	target, _, err := cfg.AlertURL()
	switch {
	case err != nil:
		urlErr = err
	case target != "":
		urlErr = postWebhook(ctx, target, title(n.Event, account), cleanText(n.Summary))
	}
	return cmdErr, urlErr
}

func title(e Event, account string) string {
	what := "alert"
	switch e {
	case EventGuard:
		what = "mass deletion alert"
	case EventStale:
		what = "no recent backup"
	}
	// HTTP header values are ASCII here.
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, "imapvault: "+what+" ("+account+")")
}

// postWebhook sends body as text/plain. It follows no redirect (a notification
// service does not need one, and following would send the message elsewhere)
// and returns errors that leave out the URL, which can hold a secret topic.
func postWebhook(ctx context.Context, target, title, body string) error {
	client := &http.Client{
		Timeout:       webhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		return errors.New("guard.on_alert_url: cannot build the request")
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", title)
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the *url.Error wrapper carries the full URL
		}
		return fmt.Errorf("guard.on_alert_url: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("guard.on_alert_url: the server answered %s", resp.Status)
	}
	return nil
}
