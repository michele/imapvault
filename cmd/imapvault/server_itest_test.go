//go:build integration

package main

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// The M0 itest only proves the harness: the docker server is up, seeded, and
// offers the capabilities the backup fast path relies on. It uses go-imap
// directly because internal/imapsource does not exist yet (M2).

func dialTestServer(t *testing.T) *imapclient.Client {
	t.Helper()
	pass := os.Getenv("IMAPVAULT_IMAP_PASSWORD")
	if pass == "" {
		t.Skip("IMAPVAULT_IMAP_PASSWORD is not set; run `make itest`")
	}
	c, err := imapclient.DialInsecure("127.0.0.1:1143", nil)
	if err != nil {
		t.Fatalf("dial test server (is `make itest-up` running?): %v", err)
	}
	// LOGOUT first: a bare close is not noticed by the server through the container's
	// port forward, and its per-user connection limit (10) then starves later packages.
	t.Cleanup(func() { _ = c.Logout().Wait(); _ = c.Close() })
	if err := c.Login("me@imapvault.test", pass).Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c
}

func TestServerCapabilities(t *testing.T) {
	c := dialTestServer(t)
	caps := c.Caps()
	for _, need := range []imap.Cap{imap.CapCondStore, imap.CapQResync, imap.CapUIDPlus} {
		if !caps.Has(need) {
			t.Errorf("test server lacks %s: enable it in testdata/dovecot/99-test.conf", need)
		}
	}
	// Informational: M3's EMAILID download skip needs OBJECTID, and folder roles
	// use SPECIAL-USE. Record what the test server offers.
	for _, opt := range []imap.Cap{imap.CapObjectID, imap.CapSpecialUse} {
		t.Logf("%-12s advertised: %v", opt, caps.Has(opt))
	}
}

func TestSeededFolders(t *testing.T) {
	c := dialTestServer(t)
	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	var all []string
	for _, b := range boxes {
		all = append(all, b.Mailbox)
		// Count messages with EXAMINE (read-only), as the backup will.
		sel, err := c.Select(b.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			t.Fatalf("examine %s: %v", b.Mailbox, err)
		}
		got[b.Mailbox] = int(sel.NumMessages)
	}
	sort.Strings(all)
	t.Logf("folders: %s", strings.Join(all, ", "))

	nested := ""
	for name := range got {
		if strings.HasPrefix(name, "Archive") && name != "Archive" {
			nested = name
		}
	}
	want := map[string]int{"INBOX": 4, "Sent": 1, "Archive": 2, "Trash": 1, "Tricky": 4}
	if nested == "" {
		t.Error("nested folder Archive/2025 was not seeded")
	} else {
		want[nested] = 1
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s has %d messages, want %d", name, got[name], n)
		}
	}
}
