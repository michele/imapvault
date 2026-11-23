package engine

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/michele/imapvault/internal/seal"
)

// MessageMeta is the descriptive metadata of one message. It is stored sealed
// (index blobs.meta_sealed): a backup machine writes it and never reads it.
// It must never be logged.
type MessageMeta struct {
	MessageID    string    `json:"message_id,omitempty"`
	Date         string    `json:"date,omitempty"` // the header as written, not parsed
	From         string    `json:"from,omitempty"`
	To           string    `json:"to,omitempty"`
	Subject      string    `json:"subject,omitempty"`
	InternalDate time.Time `json:"internal_date,omitzero"`
}

var wordDecoder = &mime.WordDecoder{}

// parseMeta reads the header block of raw. A message with unparseable headers
// still gets whatever was read: the blob is the source of truth, this is only a
// convenience for listing.
func parseMeta(raw []byte, internalDate time.Time) MessageMeta {
	m := MessageMeta{InternalDate: internalDate.UTC()}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return m
	}
	h := msg.Header
	dec := func(key string) string {
		v := strings.TrimSpace(h.Get(key))
		if d, err := wordDecoder.DecodeHeader(v); err == nil {
			v = d
		}
		return v
	}
	m.MessageID = strings.TrimSpace(h.Get("Message-Id"))
	m.Date = strings.TrimSpace(h.Get("Date"))
	m.From = dec("From")
	m.To = dec("To")
	m.Subject = dec("Subject")
	return m
}

func sealMeta(s *seal.Sealer, m MessageMeta) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return s.Seal(b)
}

func openMeta(o *seal.Opener, sealed []byte) (MessageMeta, error) {
	var m MessageMeta
	if len(sealed) == 0 {
		return m, nil
	}
	b, err := o.Open(sealed)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, seal.ErrCorrupt
	}
	return m, nil
}
