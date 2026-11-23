package objects

import (
	"errors"
	"fmt"
)

// Envelope flag bits (byte 0 of every object file).
const (
	flagZstd      byte = 1 << 0
	flagEncrypted byte = 1 << 1 // reserved: v1 never writes it and refuses to read it

	knownFlags = flagZstd
)

var (
	// ErrNotFound means no object exists for the key.
	ErrNotFound = errors.New("object not found")
	// ErrCorrupt means the object file does not decode to bytes that hash to its key.
	ErrCorrupt = errors.New("object is corrupt")
	// ErrUnsupported means the envelope uses bits this version does not understand
	// (for example encryption written by a newer imapvault).
	ErrUnsupported = errors.New("object uses an unsupported envelope")
)

// checkFlags rejects any envelope bit this version cannot interpret.
func checkFlags(flags byte) error {
	if flags&^knownFlags != 0 {
		return fmt.Errorf("%w: flags %#02x", ErrUnsupported, flags)
	}
	if flags&flagZstd == 0 {
		// v1 always compresses; an uncompressed object is not something we wrote.
		return fmt.Errorf("%w: flags %#02x (no compression bit)", ErrUnsupported, flags)
	}
	return nil
}
