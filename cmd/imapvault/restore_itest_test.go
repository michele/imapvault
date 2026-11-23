//go:build integration

package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/michele/imapvault/internal/plan"
)

// appendFull appends a message with the flags and internal date given, so the
// tests can see that restore brings both back.
func appendFull(t *testing.T, c *imapclient.Client, folder, subject string, flags []imap.Flag, date time.Time) {
	t.Helper()
	body := fmt.Sprintf("From: t@example.test\r\nTo: me@imapvault.test\r\nSubject: %s\r\nMessage-ID: <%s@itest>\r\nDate: Mon, 02 Mar 2026 09:00:00 +0000\r\n\r\nbody of %s\r\n", subject, subject, subject)
	cmd := c.Append(folder, int64(len(body)), &imap.AppendOptions{Flags: flags, Time: date})
	if _, err := io.WriteString(cmd, body); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

// dump describes every message in a folder as "sha flags internaldate", sorted,
// read with EXAMINE and BODY.PEEK by a client of its own.
func dump(t *testing.T, c *imapclient.Client, folder string) []string {
	t.Helper()
	sel, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatalf("examine %s: %v", folder, err)
	}
	// A selected mailbox cannot be deleted, and the cleanups delete folders.
	defer func() { _ = c.Unselect().Wait() }()
	if sel.NumMessages == 0 {
		return nil
	}
	section := &imap.FetchItemBodySection{Peek: true}
	var all imap.UIDSet
	all.AddRange(1, 0)
	bufs, err := c.Fetch(all, &imap.FetchOptions{UID: true, Flags: true, InternalDate: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range bufs {
		var fl []string
		for _, f := range m.Flags {
			fl = append(fl, string(f))
		}
		sum := sha256.Sum256(m.FindBodySection(section))
		out = append(out, fmt.Sprintf("%x %s %s", sum[:8], plan.NormalizeFlags(fl), m.InternalDate.UTC().Format(time.RFC3339)))
	}
	sort.Strings(out)
	return out
}

// fingerprint is the whole account as far as a write could change it: every
// folder with its message count, UIDNEXT and UIDVALIDITY. A CREATE adds a line,
// an APPEND moves UIDNEXT, any expunge or renumbering shows too.
func fingerprint(t *testing.T, c *imapclient.Client) string {
	t.Helper()
	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, b := range boxes {
		line := b.Mailbox
		if st, err := c.Status(b.Mailbox, &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true}).Wait(); err == nil {
			line += fmt.Sprintf(" messages=%d uidnext=%d uidvalidity=%d", *st.NumMessages, st.UIDNext, st.UIDValidity)
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// foldersUnder lists the server's folders named prefix or below it.
func foldersUnder(t *testing.T, c *imapclient.Client, prefix string) []string {
	t.Helper()
	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range boxes {
		if b.Mailbox == prefix || strings.HasPrefix(b.Mailbox, prefix+"/") {
			out = append(out, b.Mailbox)
		}
	}
	sort.Strings(out)
	return out
}

// cleanupUnder deletes what a restore created under prefix, deepest first.
func cleanupUnder(t *testing.T, c *imapclient.Client, prefix string) {
	t.Cleanup(func() {
		names := foldersUnder(t, c, prefix)
		sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
		for _, n := range names {
			_ = c.Delete(n).Wait()
		}
	})
}

func sameLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s differ:\n got:  %q\n want: %q", what, got, want)
	}
}

// The scenario the tool exists for: everything is deleted and the trash emptied,
// the next backup raises the alarm, and restore puts the mail back.
func TestRestoreUndoTheHackAgainstServer(t *testing.T) {
	c := dialTestServer(t)
	inbox, trash := uniq("itest-undo-inbox"), uniq("itest-undo-trash")
	createFolder(t, c, inbox)
	createFolder(t, c, trash)
	d := func(day int) time.Time { return time.Date(2026, 2, day, 8, 30, 0, 0, time.UTC) }
	appendFull(t, c, inbox, "plain", nil, d(1))
	appendFull(t, c, inbox, "read", []imap.Flag{imap.FlagSeen}, d(2))
	appendFull(t, c, inbox, "starred", []imap.Flag{imap.FlagSeen, imap.FlagFlagged, "$MyTag"}, d(3))
	appendFull(t, c, inbox, "marked-deleted", []imap.Flag{imap.FlagDeleted}, d(4))
	appendFull(t, c, trash, "old", []imap.Flag{imap.FlagSeen}, d(5))
	appendFull(t, c, trash, "older", nil, d(6))
	wantInbox, wantTrash := dump(t, c, inbox), dump(t, c, trash)
	// \Deleted is the one flag restore leaves out on purpose: put back, a message
	// marked deleted would be expunged by the next client that cleans up.
	for i := range wantInbox {
		wantInbox[i] = strings.ReplaceAll(wantInbox[i], `\Deleted`, "")
	}

	itestAccount(t, include(inbox, trash)+allowAppend)
	run(t, "init")
	bin := buildBinary(t)
	if out, code := runBinary(t, bin, "backup"); code != 0 {
		t.Fatalf("first backup exited %d:\n%s", code, out)
	}

	expungeAll(t, c, inbox)
	expungeAll(t, c, trash)
	if out, code := runBinary(t, bin, "backup"); code != 3 {
		t.Fatalf("backup after the hack exited %d, want 3:\n%s", code, out)
	}
	if len(dump(t, c, inbox))+len(dump(t, c, trash)) != 0 {
		t.Fatal("the hack did not empty the server")
	}
	empty := fingerprint(t, c)

	// 1. No approval: the preview, an error, and a server that has not moved.
	out, code := runBinary(t, bin, "restore", "--deleted-since", "1d", "--in-place")
	t.Logf("restore without approval (exit %d):\n%s", code, out)
	if code == 0 || !strings.Contains(out, "--confirm ") || !strings.Contains(out, "Nothing was written") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if fingerprint(t, c) != empty {
		t.Fatal("a restore without approval changed the server")
	}

	// 2. Approved with the token from the preview.
	pre, code := runBinary(t, bin, "restore", "--deleted-since", "1d", "--in-place", "--dry-run")
	if code != 0 || !strings.Contains(pre, "IN PLACE") || !strings.Contains(pre, "Will create 0 folders and add 6 messages") {
		t.Fatalf("dry run exit %d:\n%s", code, pre)
	}
	out, code = runBinary(t, bin, "restore", "--deleted-since", "1d", "--in-place", "--confirm", tokenOf(t, pre))
	t.Logf("approved restore (exit %d):\n%s", code, out)
	if code != 0 || !strings.Contains(out, "Restored 6 messages") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	sameLines(t, "INBOX after restore", dump(t, c, inbox), wantInbox)
	sameLines(t, "Trash after restore", dump(t, c, trash), wantTrash)
	for _, line := range dump(t, c, inbox) {
		if strings.Contains(line, `\Deleted`) {
			t.Fatalf(`\Deleted was restored: %s`, line)
		}
	}
	restored := fingerprint(t, c)

	// 3. The same restore again appends nothing.
	out, code = runBinary(t, bin, "restore", "--deleted-since", "1d", "--in-place")
	if code != 0 || !strings.Contains(out, "Nothing to restore") {
		t.Fatalf("second restore exited %d:\n%s", code, out)
	}
	if fingerprint(t, c) != restored {
		t.Fatal("the second restore changed the server")
	}

	// 4. The restored mail is on the server again, so a new backup sees it as
	// present and there is nothing left that counts as deleted.
	if out, code := runBinary(t, bin, "backup"); code != 0 {
		t.Fatalf("backup after the restore exited %d:\n%s", code, out)
	}
	out, code = runBinary(t, bin, "restore", "--deleted-since", "1d", "--in-place")
	if code != 0 || !strings.Contains(out, "Nothing to restore") {
		t.Fatalf("restore after the re-backup exited %d:\n%s", code, out)
	}
	run(t, "check", "--read-data")
}

// Every way of not approving leaves the account exactly as it was.
func TestRestoreGuardRailsAgainstServer(t *testing.T) {
	c := dialTestServer(t)
	inbox := uniq("itest-rails-inbox")
	createFolder(t, c, inbox)
	appendN(t, c, inbox, "r", 3)
	into := uniq("itest-rs") // restore target prefix, cleaned up afterwards
	cleanupUnder(t, c, into)

	itestAccount(t, include(inbox)+allowAppend)
	run(t, "init")
	bin := buildBinary(t)
	if out, code := runBinary(t, bin, "backup"); code != 0 {
		t.Fatalf("backup exited %d:\n%s", code, out)
	}
	args := []string{"restore", "--into", into + "/{snapshot}"}
	before := fingerprint(t, c)

	unchanged := func(what string) {
		t.Helper()
		if got := fingerprint(t, c); got != before {
			t.Fatalf("%s changed the server:\nbefore:\n%s\nafter:\n%s", what, before, got)
		}
	}
	pre, code := runBinary(t, bin, append(args, "--dry-run")...)
	if code != 0 {
		t.Fatalf("dry run exited %d:\n%s", code, pre)
	}
	token := tokenOf(t, pre)
	unchanged("--dry-run")

	if out, code := runBinary(t, bin, args...); code == 0 || !strings.Contains(out, "--confirm "+token) {
		t.Fatalf("no --confirm, not a terminal: exit %d:\n%s", code, out)
	}
	unchanged("restore with no approval")

	for _, bad := range []string{"00000-00000", "yes", "y", token + "0", token[:5]} {
		if out, code := runBinary(t, bin, append(args, "--confirm", bad)...); code == 0 || !strings.Contains(out, "Nothing was written") {
			t.Fatalf("--confirm %q: exit %d:\n%s", bad, code, out)
		}
	}
	unchanged("a wrong token")

	// The plan changes under the user's feet: the token from the preview is stale.
	if err := c.Create(into+"/1/"+inbox, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	stale := fingerprint(t, c)
	if out, code := runBinary(t, bin, append(args, "--confirm", token)...); code == 0 || !strings.Contains(out, "does not match") {
		t.Fatalf("stale token: exit %d:\n%s", code, out)
	}
	if fingerprint(t, c) != stale {
		t.Fatal("a stale token changed the server")
	}
	for _, n := range []string{into + "/1/" + inbox, into + "/1", into} {
		_ = c.Delete(n).Wait()
	}
	unchanged("a stale token (after removing the folder the test made)")

	// allow_append off: even the right token is refused.
	cfgPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "imapvault", "it.toml")
	orig, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(orig), "allow_append = true", "allow_append = false", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runBinary(t, bin, append(args, "--confirm", token)...)
	if code == 0 || !strings.Contains(out, "allow_append = true") || !strings.Contains(out, "Nothing was written") {
		t.Fatalf("allow_append off: exit %d:\n%s", code, out)
	}
	unchanged("allow_append = false")
	if err := os.WriteFile(cfgPath, orig, 0o600); err != nil {
		t.Fatal(err)
	}

	// Finally the approved restore: it creates folders under the prefix and
	// touches nothing else.
	out, code = runBinary(t, bin, append(args, "--confirm", token)...)
	t.Logf("approved restore (exit %d):\n%s", code, out)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	after := strings.Split(fingerprint(t, c), "\n")
	for _, line := range after {
		name, _, _ := strings.Cut(line, " ")
		if !strings.Contains(before, line) && name != into && !strings.HasPrefix(name, into+"/") {
			t.Fatalf("restore changed a folder outside %s: %q", into, line)
		}
	}
	sameLines(t, "restored folder", dump(t, c, into+"/1/"+inbox), dump(t, c, inbox))
}

// A snapshot restored under a prefix: nested folders keep their shape, and every
// message comes back byte for byte with its flags and date. The server's own
// folders are not touched.
func TestRestoreIntoPrefixAgainstServer(t *testing.T) {
	c := dialTestServer(t)
	parent := uniq("itest-nest")
	child := parent + "/sub"
	createFolder(t, c, parent)
	createFolder(t, c, child)
	d := func(day int) time.Time { return time.Date(2025, 12, day, 17, 45, 30, 0, time.UTC) }
	appendFull(t, c, parent, "top-1", []imap.Flag{imap.FlagSeen, imap.FlagAnswered}, d(1))
	appendFull(t, c, child, "deep-1", []imap.Flag{"$Project"}, d(2))
	appendFull(t, c, child, "deep-2", nil, d(3))
	into := uniq("itest-restored")
	cleanupUnder(t, c, into)

	itestAccount(t, include(parent, child)+allowAppend)
	run(t, "init")
	bin := buildBinary(t)
	if out, code := runBinary(t, bin, "backup"); code != 0 {
		t.Fatalf("backup exited %d:\n%s", code, out)
	}
	before := fingerprint(t, c)
	srcParent, srcChild := dump(t, c, parent), dump(t, c, child)

	args := []string{"restore", "--snapshot", "latest", "--into", into + "/{snapshot}"}
	pre, code := runBinary(t, bin, append(args, "--dry-run")...)
	if code != 0 || !strings.Contains(pre, into+"/1/"+child) || !strings.Contains(pre, "Will create 2 folders and add 3 messages") {
		t.Fatalf("dry run exit %d:\n%s", code, pre)
	}
	t.Logf("preview:\n%s", pre)
	out, code := runBinary(t, bin, append(args, "--confirm", tokenOf(t, pre))...)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	sameLines(t, "restored "+parent, dump(t, c, into+"/1/"+parent), srcParent)
	sameLines(t, "restored "+child, dump(t, c, into+"/1/"+child), srcChild)
	sameLines(t, parent, dump(t, c, parent), srcParent) // the originals are as they were
	sameLines(t, child, dump(t, c, child), srcChild)

	// Restoring twice is harmless.
	again := fingerprint(t, c)
	out, code = runBinary(t, bin, args...)
	if code != 0 || !strings.Contains(out, "Nothing to restore") || fingerprint(t, c) != again {
		t.Fatalf("second restore exited %d:\n%s", code, out)
	}
	if again == before {
		t.Fatal("the first restore wrote nothing")
	}

	// One message only, picked by hash, into a fresh prefix.
	ls := lsJSON(t, "1", child)
	one := []string{"restore", "--snapshot", "1", "--folder", child, "--message", ls.Messages[0].SHA256[:12], "--into", into + "/one"}
	pre, _ = runBinary(t, bin, append(one, "--dry-run")...)
	if !strings.Contains(pre, "add 1 message") {
		t.Fatalf("--message preview:\n%s", pre)
	}
	if out, code := runBinary(t, bin, append(one, "--confirm", tokenOf(t, pre))...); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := dump(t, c, into+"/one/"+child); len(got) != 1 {
		t.Fatalf("one message wanted, got %q", got)
	}
}
