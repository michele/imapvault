package mailtest

import (
	"bytes"
	"context"
	"fmt"
	gomail "net/mail"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/michele/imapvault/internal/mail"
)

// TargetMessage is a message on the fake target.
type TargetMessage struct {
	Raw          []byte
	Flags        []string
	InternalDate time.Time
}

// FakeTarget is an in-memory mail.Target for engine tests. Its writer enforces
// the same fence rules as internal/imaptarget (a CREATE or APPEND outside the
// fence is mail.ErrFenceViolation) so a test fails if the engine asks for more
// than the approved plan. Every write is logged in Ops.
type FakeTarget struct {
	mu      sync.Mutex
	Delim   string
	Folders map[string][]TargetMessage
	NoSel   map[string]bool // folders that exist but hold no mail (\Noselect)
	Ops     []string        // "CREATE name", "APPEND name"
	// FailAppendAt makes the Nth append (1-based, counted over the target's life) fail.
	FailAppendAt int
	appends      int
	closed       bool
}

var _ mail.Target = (*FakeTarget)(nil)

// NewTarget returns a target with an empty INBOX, which every IMAP account has,
// and "/" as hierarchy delimiter.
func NewTarget() *FakeTarget {
	return &FakeTarget{Delim: "/", Folders: map[string][]TargetMessage{"INBOX": nil}, NoSel: map[string]bool{}}
}

// Add puts a message straight into a folder (creating it), as if another client had.
func (t *FakeTarget) Add(folder, raw string, flags ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Folders[folder] = append(t.Folders[folder], TargetMessage{Raw: []byte(raw), Flags: flags, InternalDate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
}

// Count is the number of messages in folder.
func (t *FakeTarget) Count(folder string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.Folders[folder])
}

// Writes returns the CREATE and APPEND operations so far.
func (t *FakeTarget) Writes() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.Ops...)
}

func (t *FakeTarget) ListFolders(ctx context.Context) ([]mail.Folder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []mail.Folder
	for name, msgs := range t.Folders {
		out = append(out, mail.Folder{Name: name, Delim: t.Delim, NoSelect: t.NoSel[name], Messages: uint32(len(msgs))})
	}
	for name := range t.NoSel {
		if _, ok := t.Folders[name]; !ok {
			out = append(out, mail.Folder{Name: name, Delim: t.Delim, NoSelect: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (t *FakeTarget) Present(ctx context.Context, folder string) ([]mail.Present, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	msgs, ok := t.Folders[folder]
	if !ok {
		return nil, fmt.Errorf("examine %q: %w", folder, mail.ErrFolderGone)
	}
	out := make([]mail.Present, 0, len(msgs))
	for _, m := range msgs {
		id := ""
		if parsed, err := gomail.ReadMessage(bytes.NewReader(m.Raw)); err == nil {
			id = strings.TrimSpace(parsed.Header.Get("Message-Id"))
		}
		out = append(out, mail.Present{MessageID: id, Size: int64(len(m.Raw)), InternalDate: m.InternalDate})
	}
	return out, nil
}

func (t *FakeTarget) Writer(f mail.Fence) mail.TargetWriter {
	w := &fakeWriter{t: t, create: map[string]bool{}, left: map[string]int{}}
	for _, name := range f.Create {
		w.create[name] = true
	}
	for name, n := range f.Append {
		w.left[name] = n
	}
	return w
}

func (t *FakeTarget) Close() error { t.closed = true; return nil }

type fakeWriter struct {
	t      *FakeTarget
	create map[string]bool
	left   map[string]int
}

func (w *fakeWriter) CreateFolder(ctx context.Context, folder string) error {
	if !w.create[folder] {
		return fmt.Errorf("create folder %q: %w", folder, mail.ErrFenceViolation)
	}
	delete(w.create, folder)
	w.t.mu.Lock()
	defer w.t.mu.Unlock()
	if _, ok := w.t.Folders[folder]; ok {
		return fmt.Errorf("create folder %q: it already exists on the server", folder)
	}
	w.t.Folders[folder] = nil
	w.t.Ops = append(w.t.Ops, "CREATE "+folder)
	return nil
}

func (w *fakeWriter) Append(ctx context.Context, folder string, m mail.AppendMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.left[folder] <= 0 {
		return fmt.Errorf("append to folder %q: %w", folder, mail.ErrFenceViolation)
	}
	w.left[folder]--
	w.t.mu.Lock()
	defer w.t.mu.Unlock()
	w.t.appends++
	if w.t.FailAppendAt == w.t.appends {
		return fmt.Errorf("append to %q: connection reset", folder)
	}
	if _, ok := w.t.Folders[folder]; !ok {
		return fmt.Errorf("append to %q: no such folder", folder)
	}
	// Mirror the real writer: \Deleted and \Recent never reach the server.
	var flags []string
	for _, f := range m.Flags {
		if !strings.EqualFold(f, `\Deleted`) && !strings.EqualFold(f, `\Recent`) {
			flags = append(flags, f)
		}
	}
	w.t.Folders[folder] = append(w.t.Folders[folder], TargetMessage{Raw: append([]byte(nil), m.Raw...), Flags: flags, InternalDate: m.InternalDate})
	w.t.Ops = append(w.t.Ops, "APPEND "+folder)
	return nil
}
