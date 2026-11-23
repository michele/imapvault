package imaptarget

import (
	"context"
	"errors"
	"fmt"

	"github.com/emersion/go-imap/v2"

	"github.com/michele/imapvault/internal/mail"
)

// sessionWire is the go-imap side of the wire interface. This file is the only
// one in the repository that sends CREATE or APPEND, and it is reachable only
// through fencedWriter (safety_test.go pins both).
type sessionWire struct {
	s *Session
}

var _ wire = (*sessionWire)(nil)

func (w *sessionWire) folderExists(ctx context.Context, folder string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !plainName(folder) {
		return false, fmt.Errorf("folder name %q cannot be checked or created (empty, or contains * or %%)", folder)
	}
	list, err := w.s.c.List("", folder, nil).Collect()
	if err != nil {
		return false, fmt.Errorf("list %q: %w", folder, err)
	}
	for _, l := range list {
		if l.Mailbox == folder {
			return true, nil
		}
	}
	return false, nil
}

func (w *sessionWire) create(ctx context.Context, folder string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.s.c.Create(folder, nil).Wait(); err != nil {
		return fmt.Errorf("create %q: %w", folder, err)
	}
	return nil
}

func (w *sessionWire) appendMessage(ctx context.Context, folder string, m mail.AppendMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	opts := &imap.AppendOptions{Time: m.InternalDate}
	for _, f := range m.Flags {
		opts.Flags = append(opts.Flags, imap.Flag(f))
	}
	cmd := w.s.c.Append(folder, int64(len(m.Raw)), opts)
	_, werr := cmd.Write(m.Raw)
	cerr := cmd.Close()
	_, err := cmd.Wait()
	// Never put the message, or the server's echo of it, in the error: folder only.
	if err := errors.Join(werr, cerr, err); err != nil {
		return fmt.Errorf("append to %q: %w", folder, err)
	}
	return nil
}
