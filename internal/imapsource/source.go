// Package imapsource implements mail.Source on top of go-imap v2.
//
// Backup is read-only on the server: folders are opened with EXAMINE and bodies
// fetched with BODY.PEEK[]. safety_test.go enforces that no write command and no
// non-peek body fetch can be added to this package without failing the build.
// It never enables go-imap's DebugWriter: raw traces contain credentials and
// message data.
package imapsource

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/michele/imapvault/internal/mail"
)

// Options configures a connection.
type Options struct {
	Host     string
	Port     int
	TLS      string // implicit | starttls | none
	Username string
	Password string
}

// String omits the password so Options is safe to print.
func (o Options) String() string {
	return fmt.Sprintf("%s@%s:%d (tls=%s)", o.Username, o.Host, o.Port, o.TLS)
}

func (o Options) validate() error {
	switch {
	case o.Host == "":
		return errors.New("imap host is required")
	case o.Port < 1 || o.Port > 65535:
		return fmt.Errorf("imap port %d out of range", o.Port)
	case o.Username == "":
		return errors.New("imap username is required")
	}
	_, err := dialFunc(o.TLS)
	return err
}

type dialer func(addr string, opts *imapclient.Options) (*imapclient.Client, error)

func dialFunc(mode string) (dialer, error) {
	switch mode {
	case "implicit":
		return imapclient.DialTLS, nil
	case "starttls":
		return imapclient.DialStartTLS, nil
	case "none":
		return imapclient.DialInsecure, nil
	}
	return nil, fmt.Errorf("unknown tls mode %q", mode)
}

// Batch sizes (vars so tests can force several batches).
var (
	listBatch  = 1000 // MessageInfo values handed to the callback at once
	fetchBatch = 100  // UIDs per UID FETCH BODY.PEEK[] command
)

// Source is a logged-in IMAP session.
type Source struct {
	c *imapclient.Client
}

var _ mail.Source = (*Source)(nil)

// Dial connects and logs in.
func Dial(ctx context.Context, o Options) (*Source, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dial, _ := dialFunc(o.TLS)
	addr := net.JoinHostPort(o.Host, strconv.Itoa(o.Port))
	c, err := dial(addr, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	if err := c.Login(o.Username, o.Password).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("login as %s failed: %w", o.Username, err)
	}
	return &Source{c: c}, nil
}

// Capabilities returns the post-login capability list, sorted.
func (s *Source) Capabilities(ctx context.Context) (mail.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	caps, err := s.c.Capability().Wait()
	if err != nil {
		return nil, fmt.Errorf("capability: %w", err)
	}
	out := make(mail.Capabilities, 0, len(caps))
	for c := range caps {
		out = append(out, string(c))
	}
	sort.Strings(out)
	return out, nil
}

// ListFolders lists all folders, with special-use roles and message counts
// (LIST-STATUS when offered, else one STATUS per selectable folder).
func (s *Source) ListFolders(ctx context.Context) ([]mail.Folder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	caps := s.c.Caps()
	_, listStatus := caps[imap.CapListStatus]
	_, specialUse := caps[imap.CapSpecialUse]

	var opts *imap.ListOptions
	if listStatus || specialUse {
		opts = &imap.ListOptions{ReturnSpecialUse: specialUse}
		if listStatus {
			opts.ReturnStatus = &imap.StatusOptions{NumMessages: true}
		}
	}
	list, err := s.c.List("", "*", opts).Collect()
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	out := make([]mail.Folder, 0, len(list))
	for _, l := range list {
		f := mail.Folder{Name: l.Mailbox, Delim: string(l.Delim), NoSelect: isUnselectable(l.Attrs), SpecialUse: specialUseOf(l.Attrs)}
		switch {
		case f.NoSelect:
		case l.Status != nil && l.Status.NumMessages != nil:
			f.Messages = *l.Status.NumMessages
		default:
			st, err := s.c.Status(l.Mailbox, &imap.StatusOptions{NumMessages: true}).Wait()
			switch {
			case err == nil:
				if st.NumMessages != nil {
					f.Messages = *st.NumMessages
				}
			case isServerRefusal(err):
				// listed but refuses STATUS (Gmail's "[Gmail]"): no count.
			default:
				return nil, fmt.Errorf("status %q: %w", l.Mailbox, err)
			}
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Examine opens folder read-only (EXAMINE). With CONDSTORE on the server it
// asks for (CONDSTORE) so the reply carries HIGHESTMODSEQ. A server NO is
// reported as mail.ErrFolderGone.
func (s *Source) Examine(ctx context.Context, folder string) (mail.FolderState, error) {
	if err := ctx.Err(); err != nil {
		return mail.FolderState{}, err
	}
	d, err := s.c.Select(folder, &imap.SelectOptions{ReadOnly: true, CondStore: s.c.Caps().Has(imap.CapCondStore)}).Wait()
	if err != nil {
		if isServerRefusal(err) {
			return mail.FolderState{}, fmt.Errorf("examine %q: %w", folder, mail.ErrFolderGone)
		}
		return mail.FolderState{}, fmt.Errorf("examine %q: %w", folder, err)
	}
	return mail.FolderState{
		UIDValidity:   d.UIDValidity,
		UIDNext:       uint32(d.UIDNext),
		NumMessages:   d.NumMessages,
		HighestModSeq: d.HighestModSeq,
	}, nil
}

// ListMessages streams UID, flags, size and internal date of the messages of
// the examined folder with UID >= fromUID, in UID order. It does not read the
// folder into memory at once: results are handed to fn listBatch at a time.
//
// EMAILID (RFC 8474) is not fetched: go-imap v2 beta.9 cannot request it.
func (s *Source) ListMessages(ctx context.Context, folder string, fromUID uint32, fn func([]mail.MessageInfo) error) error {
	if fromUID == 0 {
		fromUID = 1
	}
	var set imap.UIDSet
	set.AddRange(imap.UID(fromUID), 0) // fromUID:*
	opts := &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, InternalDate: true}
	return s.streamInfos(ctx, folder, set, opts, fromUID, 0, fn)
}

// ChangedFlags streams UID and flags of the messages with UID <= uptoUID that
// changed after sinceModSeq: UID FETCH 1:uptoUID (UID FLAGS) (CHANGEDSINCE m).
// Needs CONDSTORE (the engine only asks when the server has it).
func (s *Source) ChangedFlags(ctx context.Context, folder string, uptoUID uint32, sinceModSeq uint64, fn func([]mail.MessageInfo) error) error {
	if uptoUID == 0 {
		return ctx.Err()
	}
	var set imap.UIDSet
	set.AddRange(1, imap.UID(uptoUID))
	opts := &imap.FetchOptions{UID: true, Flags: true, ChangedSince: sinceModSeq}
	return s.streamInfos(ctx, folder, set, opts, 1, uptoUID, fn)
}

// UIDs returns the UIDs <= uptoUID in the examined folder (UID SEARCH).
func (s *Source) UIDs(ctx context.Context, folder string, uptoUID uint32) ([]uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if uptoUID == 0 {
		return nil, nil
	}
	var set imap.UIDSet
	set.AddRange(1, imap.UID(uptoUID))
	data, err := s.c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("search %q: %w", folder, err)
	}
	var out []uint32
	for _, u := range data.AllUIDs() {
		if u >= 1 && uint32(u) <= uptoUID {
			out = append(out, uint32(u))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// streamInfos runs one UID FETCH and hands the results to fn in batches. Only
// UIDs in [minUID, maxUID] (maxUID 0 = no limit) are kept: "n:*" returns the
// last message of the folder even when its UID is below n.
func (s *Source) streamInfos(ctx context.Context, folder string, set imap.UIDSet, opts *imap.FetchOptions, minUID, maxUID uint32, fn func([]mail.MessageInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := s.c.Fetch(set, opts)
	defer func() { _ = cmd.Close() }()

	batch := make([]mail.MessageInfo, 0, listBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := fn(batch)
		batch = batch[:0]
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			return fmt.Errorf("list %q: %w", folder, err)
		}
		// A UID of 0 means the server sent no UID.
		if buf.UID == 0 {
			return fmt.Errorf("list %q: server returned a message without a UID", folder)
		}
		if uint32(buf.UID) < minUID || (maxUID != 0 && uint32(buf.UID) > maxUID) {
			continue
		}
		batch = append(batch, mail.MessageInfo{
			UID:          uint32(buf.UID),
			Flags:        flagStrings(buf.Flags),
			Size:         buf.RFC822Size,
			InternalDate: buf.InternalDate,
		})
		if len(batch) >= listBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("list %q: %w", folder, err)
	}
	return flush()
}

// FetchBodies streams the raw bytes of uids with BODY.PEEK[], so no flag (\Seen)
// is set. Messages arrive one at a time, so memory is bounded by the largest
// message, not by the batch.
func (s *Source) FetchBodies(ctx context.Context, folder string, uids []uint32, fn func(uint32, []byte) error) error {
	section := &imap.FetchItemBodySection{Peek: true}
	opts := &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}
	for start := 0; start < len(uids); start += fetchBatch {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+fetchBatch, len(uids))
		want := make(map[uint32]bool, end-start)
		set := imap.UIDSet{}
		for _, u := range uids[start:end] {
			want[u] = true
			set.AddNum(imap.UID(u))
		}
		if err := s.fetchChunk(ctx, folder, set, opts, section, want, fn); err != nil {
			return err
		}
		if len(want) > 0 {
			return fmt.Errorf("fetch %q: server did not return %d requested message(s)", folder, len(want))
		}
	}
	return nil
}

func (s *Source) fetchChunk(ctx context.Context, folder string, set imap.UIDSet, opts *imap.FetchOptions, section *imap.FetchItemBodySection, want map[uint32]bool, fn func(uint32, []byte) error) error {
	cmd := s.c.Fetch(set, opts)
	defer func() { _ = cmd.Close() }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			return fmt.Errorf("fetch %q: %w", folder, err)
		}
		uid := uint32(buf.UID)
		if !want[uid] {
			continue
		}
		raw := buf.FindBodySection(section)
		if raw == nil {
			return fmt.Errorf("fetch %q uid %d: server returned no body", folder, uid)
		}
		delete(want, uid)
		if err := fn(uid, raw); err != nil {
			return err
		}
	}
	return cmd.Close()
}

// Close logs out and closes the connection.
func (s *Source) Close() error {
	_ = s.c.Logout().Wait()
	return s.c.Close()
}

func flagStrings(flags []imap.Flag) []string {
	if len(flags) == 0 {
		return nil
	}
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = string(f)
	}
	return out
}

// specialUseOf returns the RFC 6154 role among the LIST attributes, if any.
func specialUseOf(attrs []imap.MailboxAttr) string {
	for _, a := range attrs {
		switch strings.ToLower(string(a)) {
		case `\all`, `\archive`, `\drafts`, `\flagged`, `\junk`, `\sent`, `\trash`:
			return string(a)
		}
	}
	return ""
}

// isUnselectable reports whether LIST attributes mark a container that holds no
// mail (\Noselect, or \NonExistent which implies it).
func isUnselectable(attrs []imap.MailboxAttr) bool {
	for _, a := range attrs {
		if strings.EqualFold(string(a), string(imap.MailboxAttrNoSelect)) ||
			strings.EqualFold(string(a), string(imap.MailboxAttrNonExistent)) {
			return true
		}
	}
	return false
}

// isServerRefusal reports whether err is the server answering NO.
func isServerRefusal(err error) bool {
	var e *imap.Error
	return errors.As(err, &e) && e.Type == imap.StatusResponseTypeNo
}
