package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/mail"
	"github.com/michele/imapvault/internal/mail/mailtest"
)

const allowAppend = "\n[restore]\nallow_append = true\n"

// restoreSetup backs up a small mailbox into account "test" and makes the
// restore command talk to tgt instead of a real server.
func restoreSetup(t *testing.T, extra string, tgt *mailtest.FakeTarget) {
	t.Helper()
	t.Setenv("IMAPVAULT_IMAP_PASSWORD", "secret")
	backedUp(t, extra)
	old := dialTarget
	dialTarget = func(context.Context, *config.Config) (mail.Target, error) { return tgt, nil }
	t.Cleanup(func() { dialTarget = old })
	oldTerm := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return false }
	t.Cleanup(func() { stdinIsTerminal = oldTerm })
}

func execRootIn(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

var tokenRE = regexp.MustCompile(`Plan token: ([0-9a-f]{5}-[0-9a-f]{5})`)

func tokenOf(t *testing.T, out string) string {
	t.Helper()
	m := tokenRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no plan token in:\n%s", out)
	}
	return m[1]
}

func noWrites(t *testing.T, tgt *mailtest.FakeTarget) {
	t.Helper()
	if w := tgt.Writes(); len(w) != 0 {
		t.Fatalf("the server was written to: %v", w)
	}
}

func TestRestoreDryRunPreviewsAndWritesNothing(t *testing.T) {
	tgt := mailtest.NewTarget()
	restoreSetup(t, allowAppend, tgt)
	out, err := execRootIn(t, "", "--account", "test", "restore", "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Nothing has been written yet", "me@example.com@imap.example.com", `"Restored/1"`, "Restored/1/INBOX", "new: will be created",
		"Will create 1 folder and add 2 messages", "Nothing is deleted, moved, re-flagged or overwritten", "Dry run: nothing was written"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	tokenOf(t, out)
	if strings.Contains(out, "\x1b") {
		t.Fatalf("a terminal escape reached the output: %q", out)
	}
	noWrites(t, tgt)
}

func TestRestoreWithoutApprovalOffTerminalWritesNothing(t *testing.T) {
	tgt := mailtest.NewTarget()
	restoreSetup(t, allowAppend, tgt)
	out, err := execRootIn(t, "", "--account", "test", "restore")
	if err == nil || !strings.Contains(err.Error(), "--confirm "+tokenOf(t, out)) || !strings.Contains(err.Error(), "Nothing was written") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	noWrites(t, tgt)
}

// Typing something on stdin is not approval when stdin is not a terminal: a
// script that pipes "yes" must not write.
func TestRestorePipedYesIsNotApproval(t *testing.T) {
	tgt := mailtest.NewTarget()
	restoreSetup(t, allowAppend, tgt)
	for _, in := range []string{"yes\n", "y\n", "--confirm\n"} {
		if _, err := execRootIn(t, in, "--account", "test", "restore"); err == nil {
			t.Fatalf("restore with %q on a pipe succeeded", in)
		}
	}
	noWrites(t, tgt)
}

func TestRestoreWrongOrStaleConfirmWritesNothing(t *testing.T) {
	tgt := mailtest.NewTarget()
	restoreSetup(t, allowAppend, tgt)
	for _, bad := range []string{"00000-00000", "yes", "7f3a", "0"} {
		out, err := execRootIn(t, "", "--account", "test", "restore", "--confirm", bad)
		if err == nil || !strings.Contains(err.Error(), "does not match") || !strings.Contains(err.Error(), "Nothing was written") {
			t.Fatalf("--confirm %q: err = %v\n%s", bad, err, out)
		}
		tokenOf(t, out) // the fresh preview, with the token to approve
	}
	noWrites(t, tgt)

	// A token from a preview that has since gone stale: the target changed.
	out, err := execRootIn(t, "", "--account", "test", "restore", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	old := tokenOf(t, out)
	tgt.Folders["Restored/1/INBOX"] = []mailtest.TargetMessage{{Raw: []byte("Subject: x\r\n\r\ny\r\n")}}
	if out, err = execRootIn(t, "", "--account", "test", "restore", "--confirm", old); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a stale token approved a changed plan: %v\n%s", err, out)
	}
	noWrites(t, tgt)
}

func TestRestoreConfirmWritesThenSecondRunHasNothingToDo(t *testing.T) {
	tgt := mailtest.NewTarget()
	restoreSetup(t, allowAppend, tgt)
	out, err := execRootIn(t, "", "--account", "test", "restore", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	tok := tokenOf(t, out)
	out, err = execRootIn(t, "", "--account", "test", "restore", "--confirm", tok)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Restored 2 messages") || tgt.Count("Restored/1/INBOX") != 2 || tgt.Count("INBOX") != 0 {
		t.Fatalf("out:\n%s\nrestored folder has %d, INBOX has %d", out, tgt.Count("Restored/1/INBOX"), tgt.Count("INBOX"))
	}
	n := len(tgt.Writes())
	out, err = execRootIn(t, "", "--account", "test", "restore", "--confirm", tok)
	if err != nil || !strings.Contains(out, "Nothing to restore") || len(tgt.Writes()) != n {
		t.Fatalf("second run: %v\n%s\nwrites %d -> %d", err, out, n, len(tgt.Writes()))
	}
}

func TestRestoreRefusedWithoutAllowAppend(t *testing.T) {
	tgt := mailtest.NewTarget()
	restoreSetup(t, "", tgt)
	out, err := execRootIn(t, "", "--account", "test", "restore")
	if err == nil || !strings.Contains(err.Error(), "allow_append = true") || !strings.Contains(err.Error(), "test.toml") || !strings.Contains(err.Error(), "Nothing was written") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, "Will create") {
		t.Fatalf("the preview must still be shown:\n%s", out)
	}
	// Not even a correct token gets past it.
	if _, err := execRootIn(t, "", "--account", "test", "restore", "--confirm", tokenOf(t, out)); err == nil {
		t.Fatal("--confirm bypassed allow_append")
	}
	// A dry run is only information: it works, and says how to enable writing.
	out, err = execRootIn(t, "", "--account", "test", "restore", "--dry-run")
	if err != nil || !strings.Contains(out, "allow_append = true") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	noWrites(t, tgt)
}

func TestRestoreInteractiveApproval(t *testing.T) {
	tgt := mailtest.NewTarget()
	restoreSetup(t, allowAppend, tgt)
	stdinIsTerminal = func(io.Reader) bool { return true }

	out, err := execRootIn(t, "yes\n", "--account", "test", "restore")
	if err == nil || !strings.Contains(err.Error(), "aborted") || !strings.Contains(out, "Type the token to write") {
		t.Fatalf("wrong answer: %v\n%s", err, out)
	}
	if _, err := execRootIn(t, "", "--account", "test", "restore"); err == nil { // EOF
		t.Fatal("EOF at the prompt approved")
	}
	noWrites(t, tgt)

	dry, _ := execRootIn(t, "", "--account", "test", "restore", "--dry-run")
	tok := tokenOf(t, dry)
	out, err = execRootIn(t, tok+"\n", "--account", "test", "restore")
	if err != nil || tgt.Count("Restored/1/INBOX") != 2 {
		t.Fatalf("typed token: %v\n%s", err, out)
	}
}

func TestRestoreInPlaceIsLoud(t *testing.T) {
	tgt := mailtest.NewTarget()
	tgt.Add("INBOX", "Subject: already here\r\n\r\nx\r\n")
	restoreSetup(t, allowAppend, tgt)
	out, err := execRootIn(t, "", "--account", "test", "restore", "--in-place", "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"IN PLACE", "EXISTS: has 1 message", "Will create 0 folders and add 2 messages"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	// The token covers the mode: a prefix-mode token cannot approve an in-place run.
	prefixOut, _ := execRootIn(t, "", "--account", "test", "restore", "--dry-run")
	if _, err := execRootIn(t, "", "--account", "test", "restore", "--in-place", "--confirm", tokenOf(t, prefixOut)); err == nil {
		t.Fatal("a prefix-mode token approved an in-place restore")
	}
	noWrites(t, tgt)
}

func TestRestoreToAnotherAccountIsFlagged(t *testing.T) {
	tgt := mailtest.NewTarget()
	restoreSetup(t, allowAppend, tgt)
	path, err := config.AccountConfigPath(os.Getenv, "other")
	if err != nil {
		t.Fatal(err)
	}
	body := "[imap]\nhost = \"imap.example.org\"\nusername = \"new@example.org\"\n[repository]\npath = \"/tmp/unused\"\n" + allowAppend
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := execRootIn(t, "", "--account", "test", "restore", "--to", "other", "--dry-run")
	if err != nil || !strings.Contains(out, "DIFFERENT ACCOUNT") || !strings.Contains(out, "new@example.org@imap.example.org") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := execRootIn(t, "", "--account", "test", "restore", "--to", "nope", "--dry-run"); err == nil {
		t.Fatal("unknown --to account must fail")
	}
	noWrites(t, tgt)
}

func TestRestoreJSON(t *testing.T) {
	tgt := mailtest.NewTarget()
	restoreSetup(t, allowAppend, tgt)
	out, err := execRootIn(t, "", "--account", "test", "--json", "restore", "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var j restoreJSON
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if j.Appends != 2 || j.Written || !j.AllowAppend || len(j.Token) != 11 || len(j.Create) != 1 {
		t.Fatalf("%+v", j)
	}
	noWrites(t, tgt)
}

func TestRestoreFlagErrors(t *testing.T) {
	tgt := mailtest.NewTarget()
	restoreSetup(t, allowAppend, tgt)
	for _, args := range [][]string{
		{"--deleted-since", "soon"},
		{"--deleted-since", "7d", "--snapshot", "1"},
		{"--in-place", "--into", "X"},
		{"--folder", "Nope"},
		{"--snapshot", "9"},
		{"--message", "abc"},
	} {
		if out, err := execRootIn(t, "", append([]string{"--account", "test", "restore", "--dry-run"}, args...)...); err == nil {
			t.Errorf("%v succeeded:\n%s", args, out)
		}
	}
	noWrites(t, tgt)
}

func TestIsTerminal(t *testing.T) {
	if isTerminal(strings.NewReader("")) || isTerminal(&bytes.Buffer{}) {
		t.Fatal("a reader is not a terminal")
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = null.Close() }()
	if isTerminal(null) {
		t.Fatal("/dev/null is not a terminal: nobody can answer")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	if isTerminal(r) {
		t.Fatal("a pipe is not a terminal")
	}
}

func TestRestoreMaxMessages(t *testing.T) {
	tgt := mailtest.NewTarget()
	restoreSetup(t, allowAppend, tgt) // the plan adds 2 messages
	base := []string{"--account", "test", "restore"}

	dry, err := execRootIn(t, "", append(base, "--max-messages", "1", "--dry-run")...)
	if err != nil || !strings.Contains(dry, "OVER THE LIMIT") || !strings.Contains(dry, "--max-messages 2") {
		t.Fatalf("dry run: %v\n%s", err, dry)
	}
	tok := tokenOf(t, dry)

	// A correct token does not get past the cap.
	out, err := execRootIn(t, "", append(base, "--max-messages", "1", "--confirm", tok)...)
	if err == nil || !strings.Contains(err.Error(), "more than --max-messages 1") || !strings.Contains(err.Error(), "Nothing was written") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	noWrites(t, tgt)
	if _, err := execRootIn(t, "", append(base, "--max-messages", "-3", "--dry-run")...); err == nil {
		t.Fatal("a negative cap must fail")
	}

	// Raising it, or switching it off, lets the same plan through.
	for _, max := range []string{"2", "0"} {
		restoreSetup(t, allowAppend, mailtest.NewTarget())
		out, err := execRootIn(t, "", append(base, "--max-messages", max, "--dry-run")...)
		if err != nil || strings.Contains(out, "OVER THE LIMIT") {
			t.Fatalf("max %s: %v\n%s", max, err, out)
		}
	}
	// The default is 10000.
	if defaultMaxMessages != 10000 {
		t.Fatalf("default cap = %d", defaultMaxMessages)
	}
}
