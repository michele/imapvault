// Package imaptarget is the only code in imapvault that may write to an IMAP
// server, and what it may write is exactly what the user approved.
//
// The surface is small on purpose. A Session reads (LIST, EXAMINE, BODY.PEEK).
// Changing the server needs a mail.TargetWriter, and the only way to get one is
// Session.Writer(fence), where the fence is built from an approved restore plan:
// the folders that may be created and, per folder, how many messages may be
// appended. The writer refuses anything else before it reaches the wire.
//
// Only CREATE and APPEND exist here. safety_test.go fails the build if any other
// write command appears, if CREATE or APPEND is called outside writer.go, or if
// a folder is opened read-write; importers_test.go fails it if any package other
// than internal/engine/restore.go imports this one. It never enables go-imap's
// DebugWriter: raw traces contain credentials and message data.
package imaptarget

import (
	"context"
	"fmt"
	"strings"

	"github.com/michele/imapvault/internal/mail"
)

// wire is the pair of commands that change a server, plus the LIST that proves a
// folder is new. session implements it with go-imap; tests implement it with a
// recorder, so the fence is checked without a server.
type wire interface {
	folderExists(ctx context.Context, folder string) (bool, error)
	create(ctx context.Context, folder string) error
	appendMessage(ctx context.Context, folder string, m mail.AppendMessage) error
}

// fencedWriter enforces a mail.Fence in front of a wire.
type fencedWriter struct {
	w          wire
	mayCreate  map[string]bool
	appendLeft map[string]int
}

var _ mail.TargetWriter = (*fencedWriter)(nil)

func newFencedWriter(w wire, f mail.Fence) *fencedWriter {
	fw := &fencedWriter{w: w, mayCreate: map[string]bool{}, appendLeft: map[string]int{}}
	for _, name := range f.Create {
		fw.mayCreate[name] = true
	}
	for name, n := range f.Append {
		if n > 0 {
			fw.appendLeft[name] = n
		}
	}
	return fw
}

// violation builds the error for a refused call. It names the folder (a folder
// name is allowed in logs) and nothing about the message.
func violation(what, folder string) error {
	return fmt.Errorf("%s %q: %w", what, folder, mail.ErrFenceViolation)
}

// CreateFolder creates a folder the fence lists, once, and only if LIST shows it
// does not exist: restore never touches a folder that is already there.
func (fw *fencedWriter) CreateFolder(ctx context.Context, folder string) error {
	if !fw.mayCreate[folder] {
		return violation("create folder", folder)
	}
	// Spent before the attempt: a failed CREATE is not retried behind the user's back.
	delete(fw.mayCreate, folder)
	exists, err := fw.w.folderExists(ctx, folder)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("create folder %q: it already exists on the server (the plan assumed it did not; run restore again to see a fresh plan)", folder)
	}
	return fw.w.create(ctx, folder)
}

// Append adds one message to a folder the fence lists, up to the counted number.
func (fw *fencedWriter) Append(ctx context.Context, folder string, m mail.AppendMessage) error {
	if fw.appendLeft[folder] <= 0 {
		return violation("append to folder", folder)
	}
	// Spent before the attempt, so an ambiguous failure can never turn into two copies.
	fw.appendLeft[folder]--
	m.Flags = safeFlags(m.Flags)
	return fw.w.appendMessage(ctx, folder, m)
}

// safeFlags drops the flags a restore must never set: \Deleted would queue the
// restored message for the next EXPUNGE of any client, and \Recent is the
// server's to set. The plan already drops them; this keeps the guarantee in the
// one place that talks to the wire.
func safeFlags(flags []string) []string {
	var out []string
	for _, f := range flags {
		if strings.EqualFold(f, `\Deleted`) || strings.EqualFold(f, `\Recent`) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// plainName reports whether folder can be passed to LIST as an exact name: the
// wildcards * and % would make it a pattern.
func plainName(folder string) bool {
	return folder != "" && !strings.ContainsAny(folder, "*%")
}
