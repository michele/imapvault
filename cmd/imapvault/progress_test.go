package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/michele/imapvault/internal/engine"
)

const mib = 1 << 20

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestProgress(mode progressMode) (*progressRenderer, *bytes.Buffer, *fakeClock) {
	var buf bytes.Buffer
	clk := &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	return newProgress(&buf, mode, clk.now), &buf, clk
}

func startFolder(r *progressRenderer, name string, idx, total, msgs int, bytes int64) {
	r.Event(engine.ProgressEvent{Kind: engine.ProgressFolderStart, Folder: name, Index: idx, Folders: total})
	r.Event(engine.ProgressEvent{Kind: engine.ProgressFolderFetch, Folder: name, Index: idx, Folders: total, Messages: msgs, Bytes: bytes})
}

func msgDone(r *progressRenderer, name string, idx, total int, bytes int64) {
	r.Event(engine.ProgressEvent{Kind: engine.ProgressMessageDone, Folder: name, Index: idx, Folders: total, Bytes: bytes})
}

func folderDone(r *progressRenderer, name string, idx, total int, rep engine.FolderReport) {
	r.Event(engine.ProgressEvent{Kind: engine.ProgressFolderDone, Folder: name, Index: idx, Folders: total, Report: &rep})
}

func TestLogModeGolden(t *testing.T) {
	r, buf, clk := newTestProgress(progressLog)
	r.Event(engine.ProgressEvent{Kind: engine.ProgressRunStart, Folders: 23})
	startFolder(r, "INBOX", 4, 23, 100, 100*mib)
	for range 29 { // 29 messages in the first 29 s: under the 30 s interval, so silent
		msgDone(r, "INBOX", 4, 23, mib)
	}
	clk.advance(29 * time.Second)
	msgDone(r, "INBOX", 4, 23, mib) // still 29 s since the "fetching" line: silent
	clk.advance(time.Second)
	msgDone(r, "INBOX", 4, 23, mib) // 30 s: one line, 31 messages done
	clk.advance(70 * time.Second)
	folderDone(r, "INBOX", 4, 23, engine.FolderReport{Name: "INBOX", Mode: "full", Messages: 100, New: 100})
	r.Close()

	want := "INBOX [4/23]: fetching 100 messages (100.0 MiB)\n" +
		"INBOX [4/23]: 31/100 (31%), 31.0/100.0 MiB, 1.0 MiB/s, ETA 1m07s\n" +
		"INBOX [4/23]: done, full, 100 messages, +100 -0 ~0 (31.0 MiB in 1m40s)\n"
	if got := buf.String(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.ContainsAny(buf.String(), "\r\x1b") {
		t.Fatal("log mode must not emit control characters")
	}
}

func TestLogModeSaysNothingForAnUnchangedFolder(t *testing.T) {
	r, buf, _ := newTestProgress(progressLog)
	r.Event(engine.ProgressEvent{Kind: engine.ProgressRunStart, Folders: 2})
	r.Event(engine.ProgressEvent{Kind: engine.ProgressFolderStart, Folder: "INBOX", Index: 1, Folders: 2})
	folderDone(r, "INBOX", 1, 2, engine.FolderReport{Name: "INBOX", Mode: "unchanged", Messages: 10})
	r.Close()
	if buf.Len() != 0 {
		t.Fatalf("an unchanged run printed %q", buf.String())
	}
}

func TestLogModeEmailIDOnlyFolderHasNoRateOrEta(t *testing.T) {
	r, buf, clk := newTestProgress(progressLog)
	startFolder(r, "B", 2, 2, 1, 0)
	msgDone(r, "B", 2, 2, 0)
	clk.advance(time.Second)
	folderDone(r, "B", 2, 2, engine.FolderReport{Name: "B", Mode: "incremental", Messages: 1, New: 1})
	want := "B [2/2]: fetching 1 message (0 B)\nB [2/2]: done, incremental, 1 message, +1 -0 ~0 (0 B in 1s)\n"
	if buf.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestBarModeDrawsAtMostEvery200ms(t *testing.T) {
	r, buf, clk := newTestProgress(progressBar)
	startFolder(r, "INBOX", 4, 23, 100, 100*mib)
	msgDone(r, "INBOX", 4, 23, mib)
	n := buf.Len()
	if n == 0 {
		t.Fatal("the first message should draw the bar")
	}
	clk.advance(100 * time.Millisecond)
	msgDone(r, "INBOX", 4, 23, mib)
	if buf.Len() != n {
		t.Fatal("redrew within 200 ms")
	}
	clk.advance(150 * time.Millisecond)
	msgDone(r, "INBOX", 4, 23, mib)
	if buf.Len() == n {
		t.Fatal("did not redraw after 250 ms")
	}
}

func TestBarModeLineShape(t *testing.T) {
	r, buf, clk := newTestProgress(progressBar)
	startFolder(r, "INBOX", 4, 23, 48000, 4096*mib)
	clk.advance(10 * time.Second)
	for range 3200 {
		msgDone(r, "INBOX", 4, 23, mib/2)
	}
	clk.advance(time.Second)
	msgDone(r, "INBOX", 4, 23, mib/2)

	drawn := buf.String()
	last := drawn[strings.LastIndex(drawn, "\r\x1b[K")+len("\r\x1b[K"):]
	for _, want := range []string{"[ 4/23] INBOX", "3,201/48,000", "1.6/4.0 GiB", "MiB/s", "ETA"} {
		if !strings.Contains(last, want) {
			t.Errorf("bar %q lacks %q", last, want)
		}
	}
	if strings.ContainsAny(last, "\n") {
		t.Errorf("the live bar must not end a line: %q", last)
	}
	if n := utf8.RuneCountInString(last); n > 79 {
		t.Errorf("bar is %d columns wide", n)
	}
}

func TestBarModeTruncatesLongAndHostileFolderNames(t *testing.T) {
	r, buf, _ := newTestProgress(progressBar)
	name := "Archive/" + strings.Repeat("very-long-folder-name/", 8) + "\x1b[2J"
	startFolder(r, name, 12, 99, 10, 10*mib)
	msgDone(r, name, 12, 99, mib)
	if strings.Contains(buf.String(), "\x1b[2J") {
		t.Fatal("a folder name must not smuggle an escape sequence")
	}
	drawn := buf.String()
	last := drawn[strings.LastIndex(drawn, "\r\x1b[K")+len("\r\x1b[K"):]
	if n := utf8.RuneCountInString(last); n > 79 {
		t.Errorf("bar is %d columns wide: %q", n, last)
	}
	if !strings.Contains(last, "…") {
		t.Errorf("a long name should be shortened with an ellipsis: %q", last)
	}
}

func TestBarModeKeepsALineForAFolderThatDownloaded(t *testing.T) {
	r, buf, clk := newTestProgress(progressBar)
	startFolder(r, "INBOX", 4, 23, 2, 2*mib)
	msgDone(r, "INBOX", 4, 23, mib)
	msgDone(r, "INBOX", 4, 23, mib)
	clk.advance(2 * time.Second)
	folderDone(r, "INBOX", 4, 23, engine.FolderReport{Name: "INBOX", Mode: "full", Messages: 2, New: 2})
	if want := "\r\x1b[KINBOX: full, 2 messages, +2 -0 ~0 (2.0 MiB in 2s)\n"; !strings.HasSuffix(buf.String(), want) {
		t.Fatalf("got %q", buf.String())
	}
}

func TestBarModeLeavesNothingBehindForAnUnchangedFolder(t *testing.T) {
	r, buf, _ := newTestProgress(progressBar)
	r.Event(engine.ProgressEvent{Kind: engine.ProgressFolderStart, Folder: "Sent", Index: 2, Folders: 2})
	folderDone(r, "Sent", 2, 2, engine.FolderReport{Name: "Sent", Mode: "unchanged", Messages: 5})
	r.Close()
	if strings.Contains(buf.String(), "\n") {
		t.Fatalf("an unchanged folder left a line: %q", buf.String())
	}
	if rest := strings.ReplaceAll(buf.String(), "\r\x1b[K", ""); strings.TrimSpace(rest) != "" && !strings.Contains(rest, "Sent") {
		t.Fatalf("unexpected output %q", buf.String())
	}
	if !strings.HasSuffix(buf.String(), "\r\x1b[K") {
		t.Fatalf("Close should leave the line cleared: %q", buf.String())
	}
}

func TestBarModeLogfClearsAndRedraws(t *testing.T) {
	r, buf, clk := newTestProgress(progressBar)
	startFolder(r, "INBOX", 1, 1, 10, 10*mib)
	msgDone(r, "INBOX", 1, 1, mib)
	clk.advance(time.Second)
	r.Logf("INBOX uid %d sha256 %s", 7, "ab12cd34")
	got := buf.String()
	i := strings.Index(got, "\r\x1b[KINBOX uid 7 sha256 ab12cd34\n")
	if i < 0 {
		t.Fatalf("verbose line missing or not on a cleared line: %q", got)
	}
	if !strings.Contains(got[i:], "[1/1] INBOX") {
		t.Fatalf("the bar was not redrawn after the verbose line: %q", got[i:])
	}
}

func TestLogModeLogfPassesThrough(t *testing.T) {
	r, buf, _ := newTestProgress(progressLog)
	r.Logf("INBOX: unchanged (%d messages)", 3)
	if buf.String() != "INBOX: unchanged (3 messages)\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	r, buf, _ := newTestProgress(progressBar)
	r.Close()
	r.Close()
	if strings.Count(buf.String(), "\r\x1b[K") > 1 {
		t.Fatalf("got %q", buf.String())
	}
}

func TestResolveProgressMode(t *testing.T) {
	tty := func(string) string { return "xterm-256color" }
	dumb := func(env string) string {
		if env == "TERM" {
			return "dumb"
		}
		return ""
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = null.Close() }()
	pipe := &bytes.Buffer{}

	cases := []struct {
		flag    string
		isTerm  bool
		jsonOut bool
		env     func(string) string
		want    progressMode
		bad     bool
	}{
		{"auto", true, false, tty, progressBar, false},
		{"auto", false, false, tty, progressLog, false},
		{"auto", true, false, dumb, progressLog, false},
		{"", true, false, tty, progressBar, false},
		{"bar", false, false, tty, progressBar, false},
		{"log", true, false, tty, progressLog, false},
		{"none", true, false, tty, progressNone, false},
		{"fancy", true, false, tty, 0, true},
		// --json means a machine reads the output, often with stderr merged in:
		// auto stays quiet, an explicit choice is honoured.
		{"auto", true, true, tty, progressNone, false},
		{"auto", false, true, tty, progressNone, false},
		{"log", false, true, tty, progressLog, false},
		{"bar", true, true, tty, progressBar, false},
	}
	for _, c := range cases {
		got, err := resolveProgressMode(c.flag, c.isTerm, c.jsonOut, c.env)
		if c.bad != (err != nil) || (!c.bad && got != c.want) {
			t.Errorf("%+v: got %v, %v", c, got, err)
		}
	}
	// The terminal test itself: a pipe or buffer is not one, /dev/null is not one.
	if isTerminalFile(pipe) || isTerminalFile(null) {
		t.Error("isTerminal accepted a non-terminal")
	}
}

func TestProgressFormatters(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 48000: "48,000", 1234567: "1,234,567"} {
		if got := commas(in); got != want {
			t.Errorf("commas(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{
		0: "0s", 45 * time.Second: "45s", 59*time.Second + 600*time.Millisecond: "1m00s",
		12*time.Minute + 5*time.Second: "12m05s", 3*time.Hour + 7*time.Minute: "3h07m",
	} {
		if got := formatDuration(in); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", in, got, want)
		}
	}
	for _, c := range []struct {
		got, total int64
		want       string
	}{
		{1600 * mib, 4096 * mib, "1.6/4.0 GiB"},
		{900 * mib, 4096 * mib, "900.0 MiB/4.0 GiB"},
		{0, 500, "0/500 B"},
	} {
		if got := bytesPair(c.got, c.total); got != c.want {
			t.Errorf("bytesPair(%d, %d) = %q, want %q", c.got, c.total, got, c.want)
		}
	}
}
