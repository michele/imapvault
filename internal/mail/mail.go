// Package mail defines the provider-neutral mailbox types and the Source
// interface the engine reads from. It must not import any IMAP library.
package mail

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrFolderGone is returned by Examine when the server refuses to open a folder
// it listed a moment ago (deleted or renamed between LIST and EXAMINE, or no
// longer readable). The engine skips that folder and the run becomes partial.
var ErrFolderGone = errors.New("folder is no longer available on the server")

// Capabilities is the server's advertised capability list.
type Capabilities []string

// Has reports whether the capability is advertised, case-insensitively.
func (c Capabilities) Has(name string) bool {
	for _, have := range c {
		if strings.EqualFold(have, name) {
			return true
		}
	}
	return false
}

// String is the stored form (sorted by the source, space-joined).
func (c Capabilities) String() string { return strings.Join(c, " ") }

// Folder describes one mailbox on the server.
type Folder struct {
	Name       string
	Delim      string
	SpecialUse string // e.g. \Sent, \Trash; empty when none
	NoSelect   bool   // a container that holds no mail (\Noselect, \NonExistent)
	Messages   uint32 // 0 when the server did not say
}

// FolderState is what EXAMINE reports. UIDs are comparable only while
// UIDValidity is unchanged.
type FolderState struct {
	UIDValidity   uint32
	UIDNext       uint32
	NumMessages   uint32
	HighestModSeq uint64 // 0 when the server has no CONDSTORE
}

// MessageInfo is the listing view of one message: no body.
type MessageInfo struct {
	UID          uint32
	Flags        []string
	Size         int64
	InternalDate time.Time
	EmailID      string // RFC 8474; empty without OBJECTID. A download hint only.
}

// Source is the read-only mailbox surface used by backup. Nothing here changes
// a flag or a message on the server.
type Source interface {
	Capabilities(ctx context.Context) (Capabilities, error)
	ListFolders(ctx context.Context) ([]Folder, error)
	// Examine opens folder read-only (EXAMINE, with CONDSTORE when the server has
	// it, so HighestModSeq is set) and returns its state. A server refusal is
	// reported as ErrFolderGone.
	Examine(ctx context.Context, folder string) (FolderState, error)
	// ListMessages streams UID, flags, size and internal date of the messages of
	// the examined folder with UID >= fromUID, in UID order and in batches.
	// fromUID 1 lists everything.
	ListMessages(ctx context.Context, folder string, fromUID uint32, fn func([]MessageInfo) error) error
	// ChangedFlags streams UID and flags of the messages with UID <= uptoUID whose
	// metadata changed after sinceModSeq (CONDSTORE: UID FETCH CHANGEDSINCE).
	ChangedFlags(ctx context.Context, folder string, uptoUID uint32, sinceModSeq uint64, fn func([]MessageInfo) error) error
	// UIDs returns the UIDs of the messages with UID <= uptoUID (UID SEARCH), to
	// find which of the messages we hold were expunged.
	UIDs(ctx context.Context, folder string, uptoUID uint32) ([]uint32, error)
	// FetchBodies streams the raw RFC 822 bytes of the given UIDs (BODY.PEEK[]).
	// raw is only valid during the call.
	FetchBodies(ctx context.Context, folder string, uids []uint32, fn func(uid uint32, raw []byte) error) error
	Close() error
}
