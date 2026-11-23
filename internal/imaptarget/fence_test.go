package imaptarget

import (
	"context"
	"errors"
	"testing"

	"github.com/michele/imapvault/internal/mail"
)

// recorder is a wire that remembers every command that reached it.
type recorder struct {
	existing map[string]bool
	creates  []string
	appends  []string
	flags    [][]string
	failNext error
}

func (r *recorder) folderExists(_ context.Context, f string) (bool, error) { return r.existing[f], nil }

func (r *recorder) create(_ context.Context, f string) error {
	if r.failNext != nil {
		err := r.failNext
		r.failNext = nil
		return err
	}
	r.creates = append(r.creates, f)
	return nil
}

func (r *recorder) appendMessage(_ context.Context, f string, m mail.AppendMessage) error {
	if r.failNext != nil {
		err := r.failNext
		r.failNext = nil
		return err
	}
	r.appends = append(r.appends, f)
	r.flags = append(r.flags, m.Flags)
	return nil
}

var ctx = context.Background()

func TestFenceRefusesCreateOutsidePlan(t *testing.T) {
	r := &recorder{}
	w := newFencedWriter(r, mail.Fence{Create: []string{"Restored/1/INBOX"}})
	for _, f := range []string{"INBOX", "Restored/1", "Restored/1/Other", ""} {
		if err := w.CreateFolder(ctx, f); !errors.Is(err, mail.ErrFenceViolation) {
			t.Errorf("create %q: err = %v, want fence violation", f, err)
		}
	}
	if len(r.creates) != 0 {
		t.Fatalf("a CREATE reached the wire: %v", r.creates)
	}
	if err := w.CreateFolder(ctx, "Restored/1/INBOX"); err != nil {
		t.Fatal(err)
	}
	if len(r.creates) != 1 {
		t.Fatalf("creates = %v", r.creates)
	}
}

func TestFenceCreatesAFolderOnlyOnce(t *testing.T) {
	r := &recorder{}
	w := newFencedWriter(r, mail.Fence{Create: []string{"A"}})
	if err := w.CreateFolder(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	if err := w.CreateFolder(ctx, "A"); !errors.Is(err, mail.ErrFenceViolation) {
		t.Fatalf("second create: %v", err)
	}
}

func TestFenceNeverCreatesAnExistingFolder(t *testing.T) {
	r := &recorder{existing: map[string]bool{"A": true}}
	w := newFencedWriter(r, mail.Fence{Create: []string{"A"}})
	err := w.CreateFolder(ctx, "A")
	if err == nil || errors.Is(err, mail.ErrFenceViolation) {
		t.Fatalf("err = %v, want a plain 'already exists' error", err)
	}
	if len(r.creates) != 0 {
		t.Fatal("CREATE sent for a folder that exists")
	}
}

func TestFenceRefusesAppendOutsidePlanAndBeyondCount(t *testing.T) {
	r := &recorder{}
	w := newFencedWriter(r, mail.Fence{Append: map[string]int{"A": 2, "Zero": 0}})
	m := mail.AppendMessage{Raw: []byte("x")}
	for _, f := range []string{"B", "Zero", "INBOX", "a"} {
		if err := w.Append(ctx, f, m); !errors.Is(err, mail.ErrFenceViolation) {
			t.Errorf("append %q: err = %v, want fence violation", f, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := w.Append(ctx, "A", m); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := w.Append(ctx, "A", m); !errors.Is(err, mail.ErrFenceViolation) {
		t.Fatalf("third append into a folder planned for 2: %v", err)
	}
	if len(r.appends) != 2 {
		t.Fatalf("appends that reached the wire: %v", r.appends)
	}
}

// A failed attempt still uses up its slot: after an ambiguous failure the writer
// must not be able to send a second copy.
func TestFenceCountsFailedAttempts(t *testing.T) {
	r := &recorder{failNext: errors.New("connection reset")}
	w := newFencedWriter(r, mail.Fence{Append: map[string]int{"A": 1}})
	m := mail.AppendMessage{Raw: []byte("x")}
	if err := w.Append(ctx, "A", m); err == nil {
		t.Fatal("expected the wire error")
	}
	if err := w.Append(ctx, "A", m); !errors.Is(err, mail.ErrFenceViolation) {
		t.Fatalf("retry after failure: %v", err)
	}
	if len(r.appends) != 0 {
		t.Fatal("a retry reached the wire")
	}
}

func TestEmptyFenceRefusesEverything(t *testing.T) {
	r := &recorder{}
	w := newFencedWriter(r, mail.Fence{})
	if !errors.Is(w.CreateFolder(ctx, "A"), mail.ErrFenceViolation) || !errors.Is(w.Append(ctx, "A", mail.AppendMessage{}), mail.ErrFenceViolation) {
		t.Fatal("an empty fence must refuse every write")
	}
}

func TestPlainName(t *testing.T) {
	for name, want := range map[string]bool{"INBOX": true, "Restored/1/A b": true, "": false, "a*": false, "50%": false} {
		if plainName(name) != want {
			t.Errorf("plainName(%q) = %v", name, !want)
		}
	}
}

func TestAppendNeverSendsDeletedOrRecent(t *testing.T) {
	r := &recorder{}
	w := newFencedWriter(r, mail.Fence{Append: map[string]int{"A": 1}})
	m := mail.AppendMessage{Flags: []string{`\Seen`, `\deleted`, `\Recent`, `$Label`, `\Flagged`}, Raw: []byte("x")}
	if err := w.Append(ctx, "A", m); err != nil {
		t.Fatal(err)
	}
	got := r.flags[0]
	want := []string{`\Seen`, `$Label`, `\Flagged`}
	if len(got) != len(want) {
		t.Fatalf("flags sent = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("flags sent = %v, want %v", got, want)
		}
	}
	if len(m.Flags) != 5 {
		t.Fatal("the caller's flags were modified")
	}
}
