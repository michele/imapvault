// Package mailtest is an in-memory mail.Source for engine tests.
package mailtest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/michele/imapvault/internal/mail"
)

// Message is one fake message.
type Message struct {
	UID          uint32
	Flags        []string
	Raw          []byte
	InternalDate time.Time
	EmailID      string
	ModSeq       uint64 // mod-sequence of the last change to this message
}

// Folder is one fake folder.
type Folder struct {
	Name        string
	SpecialUse  string
	NoSelect    bool
	UIDValidity uint32
	UIDNext     uint32
	ModSeq      uint64 // HIGHESTMODSEQ
	Messages    []Message
}

// Fake is a mail.Source over in-memory folders. It records every call, so a
// test can assert that backup only ever uses the read-only surface.
type Fake struct {
	mu      sync.Mutex
	Caps    mail.Capabilities
	Folders []*Folder
	Calls   []string
	// FailBodies, when set, makes FetchBodies return it for that folder.
	FailBodies map[string]error
	// FailAfter, when set for a folder, makes FetchBodies deliver that many
	// bodies and then fail, like a connection dropping mid-folder.
	FailAfter map[string]int
	// FailAt, when set, is asked before every call with the call's record
	// ("examine INBOX", "fetch-bodies INBOX [3 4]", ...). A non-nil result is
	// returned as the call's error and the call does nothing else.
	FailAt func(call string) error
}

var _ mail.Source = (*Fake)(nil)

// New returns an empty fake advertising CONDSTORE and UIDPLUS.
func New() *Fake { return &Fake{Caps: mail.Capabilities{"CONDSTORE", "UIDPLUS"}} }

// AddFolder adds (or returns) a folder with UIDVALIDITY 1.
func (f *Fake) AddFolder(name string) *Folder {
	for _, fo := range f.Folders {
		if fo.Name == name {
			return fo
		}
	}
	fo := &Folder{Name: name, UIDValidity: 1, UIDNext: 1, ModSeq: 1}
	f.Folders = append(f.Folders, fo)
	return fo
}

// RemoveFolder deletes a folder, like DELETE on the server.
func (f *Fake) RemoveFolder(name string) {
	for i, fo := range f.Folders {
		if fo.Name == name {
			f.Folders = append(f.Folders[:i], f.Folders[i+1:]...)
			return
		}
	}
}

// RenameFolder renames a folder keeping its messages, UIDs and UIDVALIDITY, as
// RENAME does on most servers.
func (f *Fake) RenameFolder(from, to string) {
	for _, fo := range f.Folders {
		if fo.Name == from {
			fo.Name = to
			return
		}
	}
}

// Recreate empties the folder and starts a new UID generation: UIDVALIDITY goes
// up and UIDs restart at 1, as after a DELETE and CREATE of the same name.
func (fo *Folder) Recreate() {
	fo.UIDValidity++
	fo.UIDNext = 1
	fo.Messages = nil
	fo.ModSeq++
}

// Add appends a message with the next UID and returns that UID.
func (fo *Folder) Add(raw string, flags ...string) uint32 {
	return fo.AddWithEmailID(raw, "", flags...)
}

// AddWithEmailID is Add for a server that has OBJECTID.
func (fo *Folder) AddWithEmailID(raw, emailID string, flags ...string) uint32 {
	uid := fo.UIDNext
	fo.UIDNext++
	fo.ModSeq++
	fo.Messages = append(fo.Messages, Message{UID: uid, Flags: flags, Raw: []byte(raw), InternalDate: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), EmailID: emailID, ModSeq: fo.ModSeq})
	return uid
}

// Delete removes the message with uid (an expunge). Like a server that does not
// promise to bump HIGHESTMODSEQ on EXPUNGE, it leaves ModSeq alone unless
// BumpOnExpunge is used.
func (fo *Folder) Delete(uid uint32) {
	for i, m := range fo.Messages {
		if m.UID == uid {
			fo.Messages = append(fo.Messages[:i], fo.Messages[i+1:]...)
			return
		}
	}
}

// BumpOnExpunge raises HIGHESTMODSEQ, as a QRESYNC server does on EXPUNGE.
func (fo *Folder) BumpOnExpunge() { fo.ModSeq++ }

// SetFlags replaces the flags of uid.
func (fo *Folder) SetFlags(uid uint32, flags ...string) {
	for i := range fo.Messages {
		if fo.Messages[i].UID == uid {
			fo.ModSeq++
			fo.Messages[i].Flags = flags
			fo.Messages[i].ModSeq = fo.ModSeq
		}
	}
}

// Count returns how many recorded calls start with prefix.
func (f *Fake) Count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// Reset forgets the recorded calls.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = nil
}

func (f *Fake) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf(format, args...))
}

// step records a call and returns the error FailAt injects for it, if any.
func (f *Fake) step(format string, args ...any) error {
	call := fmt.Sprintf(format, args...)
	f.mu.Lock()
	f.Calls = append(f.Calls, call)
	hook := f.FailAt
	f.mu.Unlock()
	if hook != nil {
		return hook(call)
	}
	return nil
}

func (f *Fake) folder(name string) (*Folder, error) {
	for _, fo := range f.Folders {
		if fo.Name == name {
			return fo, nil
		}
	}
	return nil, fmt.Errorf("no such folder %q", name)
}

func (f *Fake) condstore() bool { return f.Caps.Has("CONDSTORE") }

func (f *Fake) Capabilities(ctx context.Context) (mail.Capabilities, error) {
	f.record("capabilities")
	return f.Caps, ctx.Err()
}

func (f *Fake) ListFolders(ctx context.Context) ([]mail.Folder, error) {
	if err := f.step("list"); err != nil {
		return nil, err
	}
	var out []mail.Folder
	for _, fo := range f.Folders {
		out = append(out, mail.Folder{Name: fo.Name, Delim: "/", SpecialUse: fo.SpecialUse, NoSelect: fo.NoSelect, Messages: uint32(len(fo.Messages))})
	}
	return out, ctx.Err()
}

func (f *Fake) Examine(ctx context.Context, folder string) (mail.FolderState, error) {
	if err := f.step("examine %s", folder); err != nil {
		return mail.FolderState{}, err
	}
	fo, err := f.folder(folder)
	if err != nil {
		return mail.FolderState{}, fmt.Errorf("examine %q: %w", folder, mail.ErrFolderGone)
	}
	st := mail.FolderState{UIDValidity: fo.UIDValidity, UIDNext: fo.UIDNext, NumMessages: uint32(len(fo.Messages))}
	if f.condstore() {
		st.HighestModSeq = fo.ModSeq
	}
	return st, ctx.Err()
}

func (f *Fake) sorted(fo *Folder) []Message {
	msgs := append([]Message(nil), fo.Messages...)
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].UID < msgs[j].UID })
	return msgs
}

func info(m Message) mail.MessageInfo {
	return mail.MessageInfo{UID: m.UID, Flags: append([]string(nil), m.Flags...), Size: int64(len(m.Raw)), InternalDate: m.InternalDate, EmailID: m.EmailID}
}

// stream hands msgs to fn two at a time, so tests exercise batching.
func stream(ctx context.Context, msgs []Message, fn func([]mail.MessageInfo) error) error {
	for i := 0; i < len(msgs); i += 2 {
		if err := ctx.Err(); err != nil {
			return err
		}
		var batch []mail.MessageInfo
		for _, m := range msgs[i:min(i+2, len(msgs))] {
			batch = append(batch, info(m))
		}
		if err := fn(batch); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fake) ListMessages(ctx context.Context, folder string, fromUID uint32, fn func([]mail.MessageInfo) error) error {
	if err := f.step("list-messages %s from %d", folder, fromUID); err != nil {
		return err
	}
	fo, err := f.folder(folder)
	if err != nil {
		return err
	}
	var msgs []Message
	for _, m := range f.sorted(fo) {
		if m.UID >= fromUID {
			msgs = append(msgs, m)
		}
	}
	return stream(ctx, msgs, fn)
}

func (f *Fake) ChangedFlags(ctx context.Context, folder string, uptoUID uint32, sinceModSeq uint64, fn func([]mail.MessageInfo) error) error {
	if err := f.step("changed-flags %s upto %d since %d", folder, uptoUID, sinceModSeq); err != nil {
		return err
	}
	if !f.condstore() {
		return errors.New("fake: CHANGEDSINCE needs CONDSTORE")
	}
	fo, err := f.folder(folder)
	if err != nil {
		return err
	}
	var msgs []Message
	for _, m := range f.sorted(fo) {
		if m.UID <= uptoUID && m.ModSeq > sinceModSeq {
			msgs = append(msgs, m)
		}
	}
	return stream(ctx, msgs, fn)
}

func (f *Fake) UIDs(ctx context.Context, folder string, uptoUID uint32) ([]uint32, error) {
	if err := f.step("uids %s upto %d", folder, uptoUID); err != nil {
		return nil, err
	}
	fo, err := f.folder(folder)
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, m := range f.sorted(fo) {
		if m.UID <= uptoUID {
			out = append(out, m.UID)
		}
	}
	return out, ctx.Err()
}

func (f *Fake) FetchBodies(ctx context.Context, folder string, uids []uint32, fn func(uint32, []byte) error) error {
	if err := f.step("fetch-bodies %s %v", folder, uids); err != nil {
		return err
	}
	if err := f.FailBodies[folder]; err != nil {
		return err
	}
	fo, err := f.folder(folder)
	if err != nil {
		return err
	}
	delivered := 0
	for _, uid := range uids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n, ok := f.FailAfter[folder]; ok && delivered >= n {
			return fmt.Errorf("injected failure after %d bodies", n)
		}
		delivered++
		found := false
		for _, m := range fo.Messages {
			if m.UID == uid {
				found = true
				if err := fn(uid, m.Raw); err != nil {
					return err
				}
			}
		}
		if !found {
			return fmt.Errorf("uid %d not in %s", uid, folder)
		}
	}
	return nil
}

func (f *Fake) Close() error { f.record("close"); return nil }
