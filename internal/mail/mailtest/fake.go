// Package mailtest is an in-memory mail.Source for engine tests.
package mailtest

import (
	"context"
	"fmt"
	"sort"
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
}

// Folder is one fake folder.
type Folder struct {
	Name        string
	SpecialUse  string
	NoSelect    bool
	UIDValidity uint32
	UIDNext     uint32
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
	fo := &Folder{Name: name, UIDValidity: 1, UIDNext: 1}
	f.Folders = append(f.Folders, fo)
	return fo
}

// Add appends a message with the next UID and returns that UID.
func (fo *Folder) Add(raw string, flags ...string) uint32 {
	uid := fo.UIDNext
	fo.UIDNext++
	fo.Messages = append(fo.Messages, Message{UID: uid, Flags: flags, Raw: []byte(raw), InternalDate: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)})
	return uid
}

// Delete removes the message with uid (an expunge).
func (fo *Folder) Delete(uid uint32) {
	for i, m := range fo.Messages {
		if m.UID == uid {
			fo.Messages = append(fo.Messages[:i], fo.Messages[i+1:]...)
			return
		}
	}
}

// SetFlags replaces the flags of uid.
func (fo *Folder) SetFlags(uid uint32, flags ...string) {
	for i := range fo.Messages {
		if fo.Messages[i].UID == uid {
			fo.Messages[i].Flags = flags
		}
	}
}

func (f *Fake) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf(format, args...))
}

func (f *Fake) folder(name string) (*Folder, error) {
	for _, fo := range f.Folders {
		if fo.Name == name {
			return fo, nil
		}
	}
	return nil, fmt.Errorf("no such folder %q", name)
}

func (f *Fake) Capabilities(ctx context.Context) (mail.Capabilities, error) {
	f.record("capabilities")
	return f.Caps, ctx.Err()
}

func (f *Fake) ListFolders(ctx context.Context) ([]mail.Folder, error) {
	f.record("list")
	var out []mail.Folder
	for _, fo := range f.Folders {
		out = append(out, mail.Folder{Name: fo.Name, Delim: "/", SpecialUse: fo.SpecialUse, NoSelect: fo.NoSelect, Messages: uint32(len(fo.Messages))})
	}
	return out, ctx.Err()
}

func (f *Fake) Examine(ctx context.Context, folder string) (mail.FolderState, error) {
	f.record("examine %s", folder)
	fo, err := f.folder(folder)
	if err != nil {
		return mail.FolderState{}, err
	}
	return mail.FolderState{UIDValidity: fo.UIDValidity, UIDNext: fo.UIDNext, NumMessages: uint32(len(fo.Messages))}, ctx.Err()
}

func (f *Fake) ListMessages(ctx context.Context, folder string, fn func([]mail.MessageInfo) error) error {
	f.record("list-messages %s", folder)
	fo, err := f.folder(folder)
	if err != nil {
		return err
	}
	msgs := append([]Message(nil), fo.Messages...)
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].UID < msgs[j].UID })
	// two per batch, so tests exercise batching.
	for i := 0; i < len(msgs); i += 2 {
		if err := ctx.Err(); err != nil {
			return err
		}
		var batch []mail.MessageInfo
		for _, m := range msgs[i:min(i+2, len(msgs))] {
			batch = append(batch, mail.MessageInfo{UID: m.UID, Flags: append([]string(nil), m.Flags...), Size: int64(len(m.Raw)), InternalDate: m.InternalDate, EmailID: m.EmailID})
		}
		if err := fn(batch); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fake) FetchBodies(ctx context.Context, folder string, uids []uint32, fn func(uint32, []byte) error) error {
	f.record("fetch-bodies %s %v", folder, uids)
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
