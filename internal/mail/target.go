package mail

import (
	"context"
	"errors"
	"time"
)

// ErrFenceViolation is returned by a TargetWriter that was asked to do something
// the approved restore plan did not list: create a folder it did not name,
// append into a folder it did not name, or append more messages than it
// counted. It is a bug in the caller, never a server condition, and the restore
// stops at once.
var ErrFenceViolation = errors.New("refused: not part of the approved restore plan")

// Present is one message already in a target folder, reduced to what restore
// needs to avoid appending it a second time. MessageID is empty when the
// message has none.
type Present struct {
	MessageID    string
	Size         int64
	InternalDate time.Time
}

// Fence is the exact set of writes the user approved. A TargetWriter refuses
// everything outside it. Create lists the folders that may be created (and that
// must not exist yet); Append gives, per folder, the most messages that may be
// appended.
type Fence struct {
	Create []string
	Append map[string]int
}

// AppendMessage is one message to append, with the flags and internal date it
// had on the original server.
type AppendMessage struct {
	Flags        []string
	InternalDate time.Time
	Raw          []byte
}

// Target is a server restore can write to. Everything on it except Writer is
// read-only (LIST, EXAMINE, BODY.PEEK). Like Source it has no IMAP types, so the
// engine and the pure plan never see a library.
type Target interface {
	// ListFolders lists the folders that exist now, with delimiter and counts.
	ListFolders(ctx context.Context) ([]Folder, error)
	// Present lists the messages already in folder (EXAMINE, then a read-only
	// FETCH of size, internal date and the Message-ID header). A folder that does
	// not exist yields ErrFolderGone.
	Present(ctx context.Context, folder string) ([]Present, error)
	// Writer returns the only way to change the server, limited to fence.
	Writer(fence Fence) TargetWriter
	Close() error
}

// TargetWriter creates folders and appends messages, and nothing else, within
// the Fence it was built from.
type TargetWriter interface {
	// CreateFolder creates folder. It fails with ErrFenceViolation unless the
	// fence lists the folder, and with a plain error if it already exists.
	CreateFolder(ctx context.Context, folder string) error
	// Append adds one message to folder. It fails with ErrFenceViolation unless
	// the fence lists the folder and has appends left for it.
	Append(ctx context.Context, folder string, m AppendMessage) error
}
