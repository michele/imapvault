//go:build integration

package imapsource

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/michele/imapvault/internal/mail"
)

func dialTest(t *testing.T) *Source {
	t.Helper()
	pass := os.Getenv("IMAPVAULT_IMAP_PASSWORD")
	if pass == "" {
		t.Skip("IMAPVAULT_IMAP_PASSWORD is not set; run `make itest`")
	}
	s, err := Dial(context.Background(), Options{Host: "127.0.0.1", Port: 1143, TLS: "none", Username: "me@imapvault.test", Password: pass})
	if err != nil {
		t.Fatalf("dial (is `make itest-up` running?): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSourceAgainstDovecot(t *testing.T) {
	ctx := context.Background()
	s := dialTest(t)

	caps, err := s.Capabilities(ctx)
	if err != nil || !caps.Has("CONDSTORE") {
		t.Fatalf("caps %v err %v", caps, err)
	}
	folders, err := s.ListFolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var inbox *mail.Folder
	for i := range folders {
		if folders[i].Name == "INBOX" {
			inbox = &folders[i]
		}
	}
	if inbox == nil || inbox.Messages != 4 {
		t.Fatalf("INBOX in %+v", folders)
	}

	before, err := s.Examine(ctx, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if before.UIDValidity == 0 || before.NumMessages != 4 {
		t.Fatalf("state %+v", before)
	}

	var infos []mail.MessageInfo
	if err := s.ListMessages(ctx, "INBOX", func(b []mail.MessageInfo) error { infos = append(infos, b...); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(infos) != 4 || !slices.IsSortedFunc(infos, func(a, b mail.MessageInfo) int { return int(a.UID) - int(b.UID) }) {
		t.Fatalf("infos %+v", infos)
	}
	var uids []uint32
	for _, m := range infos {
		if m.Size <= 0 || m.InternalDate.IsZero() {
			t.Errorf("uid %d lacks size/date: %+v", m.UID, m)
		}
		uids = append(uids, m.UID)
	}

	// force several fetch batches
	old := fetchBatch
	fetchBatch = 3
	defer func() { fetchBatch = old }()
	got := map[uint32]int{}
	if err := s.FetchBodies(ctx, "INBOX", uids, func(uid uint32, raw []byte) error { got[uid] = len(raw); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, m := range infos {
		if int64(got[m.UID]) != m.Size {
			t.Errorf("uid %d: body is %d bytes, RFC822.SIZE %d", m.UID, got[m.UID], m.Size)
		}
	}

	// read-only proof: fetching bodies changed neither flags nor MODSEQ.
	after, err := s.Examine(ctx, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if after.HighestModSeq != before.HighestModSeq {
		t.Errorf("HIGHESTMODSEQ moved %d -> %d: something wrote to the mailbox", before.HighestModSeq, after.HighestModSeq)
	}
	var again []mail.MessageInfo
	_ = s.ListMessages(ctx, "INBOX", func(b []mail.MessageInfo) error { again = append(again, b...); return nil })
	for i := range infos {
		if !slices.Equal(infos[i].Flags, again[i].Flags) {
			t.Errorf("uid %d flags changed %v -> %v", infos[i].UID, infos[i].Flags, again[i].Flags)
		}
	}
}
