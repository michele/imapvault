package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michele/imapvault/internal/config"
)

const topic = "SECRETTOPIC123"

// notifyConfig is a config whose alerts go to url (and to command, if set).
func notifyConfig(t *testing.T, url, command string) *config.Config {
	t.Helper()
	toml := "[imap]\nhost = \"imap.example.com\"\nusername = \"me@example.com\"\n[repository]\npath = \"/backup/me\"\n[guard]\n"
	if url != "" {
		toml += "on_alert_url = \"" + url + "\"\n"
	}
	if command != "" {
		toml += "on_alert = \"" + command + "\"\n"
	}
	cfg, err := config.Parse([]byte(toml), func(k string) string {
		if k == config.PasswordEnv {
			return "x"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type received struct {
	method, contentType, title, body string
}

func listener(t *testing.T, status int) (*httptest.Server, *[]received) {
	t.Helper()
	var got []received
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, received{r.Method, r.Header.Get("Content-Type"), r.Header.Get("Title"), string(b)})
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestNotifyPostsTheSummaryToTheURL(t *testing.T) {
	srv, got := listener(t, 200)
	cfg := notifyConfig(t, srv.URL+"/"+topic, "")
	cmdErr, urlErr := Notify(context.Background(), cfg, Notification{
		Event: EventGuard, Summary: "INBOX lost 120 of 900\nmessages \x1b[31m(13%)", AlertID: 4, Snapshot: 9,
	})
	if cmdErr != nil || urlErr != nil {
		t.Fatalf("errors: %v, %v", cmdErr, urlErr)
	}
	if len(*got) != 1 {
		t.Fatalf("got %d requests", len(*got))
	}
	r := (*got)[0]
	if r.method != "POST" || !strings.HasPrefix(r.contentType, "text/plain") {
		t.Errorf("request %+v", r)
	}
	if !strings.Contains(r.title, "imapvault") || !strings.Contains(r.title, "me@example.com") {
		t.Errorf("title %q", r.title)
	}
	if r.body != "INBOX lost 120 of 900 messages  [31m(13%)" && !strings.Contains(r.body, "INBOX lost 120 of 900 messages") {
		t.Errorf("body %q", r.body)
	}
	if strings.ContainsAny(r.body, "\x1b\n") {
		t.Errorf("control characters reached the notification: %q", r.body)
	}
}

func TestNotifyErrorsNeverContainTheURL(t *testing.T) {
	srv, _ := listener(t, 500)
	cfg := notifyConfig(t, srv.URL+"/"+topic, "")
	_, urlErr := Notify(context.Background(), cfg, Notification{Event: EventGuard, Summary: "x"})
	if urlErr == nil || !strings.Contains(urlErr.Error(), "500") {
		t.Fatalf("want a status error, got %v", urlErr)
	}
	if strings.Contains(urlErr.Error(), topic) {
		t.Errorf("the error repeats the URL: %v", urlErr)
	}

	srv.Close() // now the connection itself fails
	_, urlErr = Notify(context.Background(), cfg, Notification{Event: EventGuard, Summary: "x"})
	if urlErr == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(urlErr.Error(), topic) {
		t.Errorf("a connection error repeats the URL: %v", urlErr)
	}
}

func TestNotifyDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	_, urlErr := Notify(context.Background(), notifyConfig(t, srv.URL+"/"+topic, ""), Notification{Event: EventGuard, Summary: "x"})
	if urlErr == nil {
		t.Fatal("a redirect is not a delivery")
	}
	if elsewhere.Load() != 0 {
		t.Error("followed a redirect to another host")
	}
}

func TestNotifyGivesUpOnASlowServer(t *testing.T) {
	old := webhookTimeout
	webhookTimeout = 100 * time.Millisecond
	t.Cleanup(func() { webhookTimeout = old })
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-done }))
	defer srv.Close()
	defer close(done)
	start := time.Now()
	_, urlErr := Notify(context.Background(), notifyConfig(t, srv.URL+"/"+topic, ""), Notification{Event: EventGuard, Summary: "x"})
	if urlErr == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("err %v after %v", urlErr, time.Since(start))
	}
}

func TestNotifyTriesBothChannelsEvenIfOneFails(t *testing.T) {
	srv, got := listener(t, 200)
	var argv []string
	old := runHook
	runHook = func(_ context.Context, a, env []string) error {
		argv = append(a, env...)
		return errors.New("notify-me: not found")
	}
	t.Cleanup(func() { runHook = old })
	cfg := notifyConfig(t, srv.URL+"/x", "notify-me '{account}: {summary}'")
	cmdErr, urlErr := Notify(context.Background(), cfg, Notification{Event: EventStale, Summary: "no backup for 24h"})
	if cmdErr == nil || urlErr != nil {
		t.Fatalf("cmdErr %v, urlErr %v", cmdErr, urlErr)
	}
	if len(*got) != 1 {
		t.Fatal("the webhook was skipped because the command failed")
	}
	joined := strings.Join(argv, "|")
	for _, want := range []string{"me@example.com: no backup for 24h", "IMAPVAULT_EVENT=stale", "IMAPVAULT_SUMMARY=no backup for 24h"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the command lacks %q: %s", want, joined)
		}
	}
}

func TestNotifyWithNothingConfiguredDoesNothing(t *testing.T) {
	cmdErr, urlErr := Notify(context.Background(), notifyConfig(t, "", ""), Notification{Event: EventGuard, Summary: "x"})
	if cmdErr != nil || urlErr != nil {
		t.Fatalf("%v, %v", cmdErr, urlErr)
	}
}

func TestBackupRefusesABrokenAlertURLSourceUpFront(t *testing.T) {
	cfg := initRepo(t)
	cfg.Guard.OnAlertURLFile = "/nonexistent/alert_url" // found now, not after a mass deletion
	f := fakeMailbox()
	_, err := Backup(context.Background(), cfg, f, BackupOptions{})
	if err == nil || !strings.Contains(err.Error(), "on_alert_url_file") {
		t.Fatalf("got %v", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("the server was touched before the config was checked: %v", f.Calls)
	}
}
