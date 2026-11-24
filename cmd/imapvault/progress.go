package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/michele/imapvault/internal/engine"
)

// progressMode is how backup progress is shown. It all goes to stderr, so
// stdout (the summary, --json) stays clean.
type progressMode int

const (
	progressNone progressMode = iota
	progressBar               // one live line, redrawn in place (a terminal)
	progressLog               // plain periodic lines (docker logs, journald, cron)
)

const (
	barRedrawEvery = 200 * time.Millisecond
	logEvery       = 30 * time.Second
	barWidth       = 79 // columns; no terminal-size lookup, so no dependency
	clearLine      = "\r\x1b[K"
)

// resolveProgressMode turns the --progress flag into a mode. "auto" is the live
// bar on a terminal and plain lines otherwise (or when TERM=dumb). With --json a
// program is reading the output, often with stderr merged in, so "auto" shows
// nothing; asking for bar or log explicitly still works.
func resolveProgressMode(flag string, isTerm, jsonOut bool, getenv func(string) string) (progressMode, error) {
	switch flag {
	case "", "auto":
		if jsonOut {
			return progressNone, nil
		}
		if isTerm && getenv("TERM") != "dumb" {
			return progressBar, nil
		}
		return progressLog, nil
	case "bar":
		return progressBar, nil
	case "log":
		return progressLog, nil
	case "none":
		return progressNone, nil
	}
	return progressNone, fmt.Errorf("--progress %q: want auto, bar, log or none", flag)
}

// progressRenderer draws the engine's progress events. It shows folder names,
// counts and sizes only, never message content (safety rule 6).
type progressRenderer struct {
	mu   sync.Mutex
	w    io.Writer
	mode progressMode
	now  func() time.Time

	// the folder being read
	folder      string
	idx, total  int
	fetching    bool // a FolderFetch arrived: something is being downloaded
	msgs, done  int
	bytes, got  int64
	fetchedAt   time.Time
	lastDraw    time.Time
	lastLog     time.Time
	barOnScreen bool
}

func newProgress(w io.Writer, mode progressMode, now func() time.Time) *progressRenderer {
	return &progressRenderer{w: w, mode: mode, now: now}
}

// Event is the engine's BackupOptions.Progress callback.
func (r *progressRenderer) Event(e engine.ProgressEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	switch e.Kind {
	case engine.ProgressFolderStart:
		r.folder, r.idx, r.total = clean(e.Folder), e.Index, e.Folders
		r.fetching, r.msgs, r.done, r.bytes, r.got = false, 0, 0, 0, 0
		r.draw(now, true) // "listing…", while a big folder is being read
	case engine.ProgressFolderFetch:
		r.fetching, r.msgs, r.bytes, r.fetchedAt = true, e.Messages, e.Bytes, now
		if r.mode == progressLog {
			r.printf("%s [%d/%d]: fetching %s (%s)\n", r.folder, r.idx, r.total, plural(e.Messages, "message"), humanBytes(e.Bytes))
			r.lastLog = now
		} else {
			r.draw(now, true)
		}
	case engine.ProgressMessageDone:
		r.done++
		r.got += e.Bytes
		switch r.mode {
		case progressBar:
			r.draw(now, false)
		case progressLog:
			if now.Sub(r.lastLog) >= logEvery {
				r.printf("%s [%d/%d]: %s\n", r.folder, r.idx, r.total, r.progressText(now, true))
				r.lastLog = now
			}
		}
	case engine.ProgressFolderDone:
		if !r.fetching || e.Report == nil {
			// Nothing was downloaded here: an unchanged folder leaves no trace.
			r.clear()
			return
		}
		rep := e.Report
		took := formatDuration(now.Sub(r.fetchedAt))
		summary := fmt.Sprintf("%s, %s, +%d -%d ~%d (%s in %s)", rep.Mode, plural(rep.Messages, "message"), rep.New, rep.Closed, rep.Reflagged, humanBytes(r.got), took)
		if r.mode == progressLog {
			r.printf("%s [%d/%d]: done, %s\n", r.folder, r.idx, r.total, summary)
		} else {
			r.clear()
			r.printf("%s: %s\n", r.folder, summary)
		}
		r.fetching = false
	}
}

// Logf passes a --verbose line through, keeping the live bar intact.
func (r *progressRenderer) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	redraw := r.barOnScreen
	r.clear()
	r.printf(format+"\n", args...)
	if redraw {
		r.draw(r.now(), true)
	}
}

// Close wipes the live bar so the next thing printed starts on a clean line.
func (r *progressRenderer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clear()
}

func (r *progressRenderer) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.w, format, args...)
}

func (r *progressRenderer) clear() {
	if r.mode == progressBar && r.barOnScreen {
		r.printf(clearLine)
		r.barOnScreen = false
	}
}

func (r *progressRenderer) draw(now time.Time, force bool) {
	if r.mode != progressBar || r.folder == "" {
		return
	}
	if !force && now.Sub(r.lastDraw) < barRedrawEvery {
		return
	}
	r.lastDraw = now
	r.printf("%s%s", clearLine, r.barLine(now))
	r.barOnScreen = true
}

// barLine is "[ 4/23] INBOX        ██████░░░░░░  3,200/48,000  1.2/4.0 GiB  2.1 MiB/s  ETA 12m", at most barWidth columns.
func (r *progressRenderer) barLine(now time.Time) string {
	head := fmt.Sprintf("[%*d/%d] %s", len(strconv.Itoa(r.total)), r.idx, r.total, padRunes(r.folder, 12))
	if !r.fetching {
		return fitRunes(head+"  listing…", barWidth)
	}
	tail := "  " + r.progressText(now, false)
	// The bar takes what is left, up to 12 cells, and goes if it would be tiny.
	cells := min(barWidth-utf8.RuneCountInString(head)-utf8.RuneCountInString(tail)-1, 12)
	if cells < 4 {
		return fitRunes(head+tail, barWidth)
	}
	filled := 0
	if r.msgs > 0 {
		filled = cells * r.done / r.msgs
	}
	return fitRunes(head+" "+strings.Repeat("█", filled)+strings.Repeat("░", cells-filled)+tail, barWidth)
}

// progressText is the counters part: "3,200/48,000 (6%), 1.2/4.0 GiB, 2.1 MiB/s, ETA 12m" in
// log mode and the same without the percentage in the bar. Rate and ETA wait for
// a few seconds of data and are left out when nothing is downloaded.
func (r *progressRenderer) progressText(now time.Time, log bool) string {
	var parts []string
	if log {
		parts = append(parts, fmt.Sprintf("%d/%d (%d%%)", r.done, r.msgs, percent(r.done, r.msgs)))
	} else {
		parts = append(parts, fmt.Sprintf("%s/%s", commas(r.done), commas(r.msgs)))
	}
	if r.bytes > 0 {
		parts = append(parts, bytesPair(r.got, r.bytes))
		elapsed := now.Sub(r.fetchedAt)
		if (elapsed >= 5*time.Second || r.got >= 1<<20) && elapsed > 0 && r.got > 0 {
			rate := float64(r.got) / elapsed.Seconds()
			parts = append(parts, humanBytes(int64(rate))+"/s")
			if left := r.bytes - r.got; left > 0 {
				parts = append(parts, "ETA "+formatDuration(time.Duration(float64(left)/rate*float64(time.Second))))
			}
		}
	}
	sep := "  "
	if log {
		sep = ", "
	}
	return strings.Join(parts, sep)
}

func percent(done, total int) int {
	if total <= 0 {
		return 0
	}
	return done * 100 / total
}

// bytesPair writes "1.6/4.0 GiB" when both sizes use the same unit, and
// "900 MiB/4.0 GiB" style otherwise.
func bytesPair(got, total int64) string {
	a, b := humanBytes(got), humanBytes(total)
	if i, j := strings.LastIndexByte(a, ' '), strings.LastIndexByte(b, ' '); i > 0 && j > 0 && a[i:] == b[j:] {
		return a[:i] + "/" + b
	}
	return a + "/" + b
}

// commas writes 48000 as 48,000.
func commas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// formatDuration is 45s, 12m05s or 3h07m, rounded to the second.
func formatDuration(d time.Duration) string {
	secs := int(d.Round(time.Second) / time.Second)
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
	}
	return fmt.Sprintf("%dh%02dm", secs/3600, secs%3600/60)
}

// padRunes pads or shortens s to exactly n columns, with an ellipsis if cut.
func padRunes(s string, n int) string {
	if c := utf8.RuneCountInString(s); c > n {
		return string([]rune(s)[:n-1]) + "…"
	} else if c < n {
		return s + strings.Repeat(" ", n-c)
	}
	return s
}

// fitRunes cuts s to at most n columns.
func fitRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
