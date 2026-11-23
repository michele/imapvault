package imaptarget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/michele/imapvault/internal/imapsource"
	imvmail "github.com/michele/imapvault/internal/mail"
)

// Session is a logged-in connection to the restore target. Everything on it
// except Writer is read-only.
type Session struct {
	c *imapclient.Client
}

var _ imvmail.Target = (*Session)(nil)

// Dial connects and logs in. It sends nothing that changes the server.
func Dial(ctx context.Context, o imapsource.Options) (*Session, error) {
	c, err := imapsource.Connect(ctx, o)
	if err != nil {
		return nil, err
	}
	return &Session{c: c}, nil
}

// ListFolders lists the folders that exist now, with their delimiter and, when
// the server says so, their message count.
func (s *Session) ListFolders(ctx context.Context) ([]imvmail.Folder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var opts *imap.ListOptions
	if s.c.Caps().Has(imap.CapListStatus) {
		opts = &imap.ListOptions{ReturnStatus: &imap.StatusOptions{NumMessages: true}}
	}
	list, err := s.c.List("", "*", opts).Collect()
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	out := make([]imvmail.Folder, 0, len(list))
	for _, l := range list {
		f := imvmail.Folder{Name: l.Mailbox, Delim: string(l.Delim)}
		for _, a := range l.Attrs {
			if strings.EqualFold(string(a), string(imap.MailboxAttrNoSelect)) ||
				strings.EqualFold(string(a), string(imap.MailboxAttrNonExistent)) {
				f.NoSelect = true
			}
		}
		if l.Status != nil && l.Status.NumMessages != nil {
			f.Messages = *l.Status.NumMessages
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Present lists what is already in folder: EXAMINE, then one FETCH of size,
// internal date and the Message-ID header with BODY.PEEK, so nothing is marked
// \Seen. A folder the server refuses to open is imvmail.ErrFolderGone.
func (s *Session) Present(ctx context.Context, folder string) ([]imvmail.Present, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := s.c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		var e *imap.Error
		if asIMAPError(err, &e) && e.Type == imap.StatusResponseTypeNo {
			return nil, fmt.Errorf("examine %q: %w", folder, imvmail.ErrFolderGone)
		}
		return nil, fmt.Errorf("examine %q: %w", folder, err)
	}
	if d.NumMessages == 0 {
		return nil, nil
	}
	section := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: []string{"Message-ID"},
		Peek:         true,
	}
	opts := &imap.FetchOptions{
		UID: true, RFC822Size: true, InternalDate: true,
		BodySection: []*imap.FetchItemBodySection{section},
	}
	var set imap.UIDSet
	set.AddRange(1, 0) // 1:*
	cmd := s.c.Fetch(set, opts)
	defer func() { _ = cmd.Close() }()

	var out []imvmail.Present
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", folder, err)
		}
		out = append(out, imvmail.Present{
			MessageID:    parseMessageID(buf.FindBodySection(section)),
			Size:         buf.RFC822Size,
			InternalDate: buf.InternalDate,
		})
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("list %q: %w", folder, err)
	}
	return out, nil
}

// Writer returns the only way to change the server, limited to fence.
func (s *Session) Writer(fence imvmail.Fence) imvmail.TargetWriter {
	return newFencedWriter(&sessionWire{s: s}, fence)
}

// Close logs out and closes the connection.
func (s *Session) Close() error {
	_ = s.c.Logout().Wait()
	return s.c.Close()
}

// parseMessageID reads the Message-ID out of a header section, trimmed exactly
// as the backup trims it when it stores the message's metadata, so the two
// compare equal. It returns "" when there is none or the section is unreadable.
func parseMessageID(section []byte) string {
	if len(section) == 0 {
		return ""
	}
	if !bytes.HasSuffix(section, []byte("\r\n\r\n")) && !bytes.HasSuffix(section, []byte("\n\n")) {
		section = append(append([]byte(nil), section...), '\r', '\n')
	}
	msg, err := mail.ReadMessage(bytes.NewReader(section))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(msg.Header.Get("Message-Id"))
}

func asIMAPError(err error, target **imap.Error) bool { return errors.As(err, target) }
