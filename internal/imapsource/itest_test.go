//go:build integration

package imapsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

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
	if err := s.ListMessages(ctx, "INBOX", 1, func(b []mail.MessageInfo) error { infos = append(infos, b...); return nil }); err != nil {
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
	_ = s.ListMessages(ctx, "INBOX", 1, func(b []mail.MessageInfo) error { again = append(again, b...); return nil })
	for i := range infos {
		if !slices.Equal(infos[i].Flags, again[i].Flags) {
			t.Errorf("uid %d flags changed %v -> %v", infos[i].UID, infos[i].Flags, again[i].Flags)
		}
	}
}

// TestSourceIncrementalAgainstDovecot exercises the M3 surface on a scratch
// folder: ranged listing (including the "n:*" quirk), CHANGEDSINCE, UID SEARCH
// and the refusal mapping. The writer client is test-side only.
func TestSourceIncrementalAgainstDovecot(t *testing.T) {
	ctx := context.Background()
	s := dialTest(t)

	w, err := imapclient.DialInsecure("127.0.0.1:1143", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if err := w.Login("me@imapvault.test", os.Getenv("IMAPVAULT_IMAP_PASSWORD")).Wait(); err != nil {
		t.Fatal(err)
	}
	folder := fmt.Sprintf("itest-source-%d", time.Now().UnixNano())
	if err := w.Create(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Delete(folder).Wait() })
	for i := 1; i <= 3; i++ {
		body := fmt.Sprintf("Subject: m%d\r\nMessage-ID: <m%d@itest>\r\n\r\nbody %d\r\n", i, i, i)
		cmd := w.Append(folder, int64(len(body)), nil)
		if _, err := io.WriteString(cmd, body); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}

	collect := func(from uint32) []uint32 {
		t.Helper()
		var out []uint32
		if err := s.ListMessages(ctx, folder, from, func(b []mail.MessageInfo) error {
			for _, m := range b {
				out = append(out, m.UID)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	st, err := s.Examine(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if st.NumMessages != 3 || st.HighestModSeq == 0 || st.UIDNext != 4 {
		t.Fatalf("state %+v (HighestModSeq 0 means CONDSTORE was not requested)", st)
	}
	if got := collect(1); !slices.Equal(got, []uint32{1, 2, 3}) {
		t.Fatalf("from 1: %v", got)
	}
	if got := collect(3); !slices.Equal(got, []uint32{3}) {
		t.Fatalf("from 3: %v", got)
	}
	// "4:*" makes the server return the last message (uid 3); it must be dropped.
	if got := collect(4); len(got) != 0 {
		t.Fatalf("from 4 (nothing new): %v", got)
	}

	// change the folder with the writer: flag uid 2, expunge uid 1.
	if _, err := w.Select(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	var two, one imap.UIDSet
	two.AddNum(2)
	one.AddNum(1)
	if err := w.Store(two, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Store(one, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Expunge().Close(); err != nil {
		t.Fatal(err)
	}
	_ = w.Unselect().Wait()

	st2, err := s.Examine(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if st2.HighestModSeq <= st.HighestModSeq || st2.NumMessages != 2 {
		t.Fatalf("after changes: %+v (was %+v)", st2, st)
	}
	var changed []mail.MessageInfo
	if err := s.ChangedFlags(ctx, folder, 3, st.HighestModSeq, func(b []mail.MessageInfo) error { changed = append(changed, b...); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].UID != 2 || !slices.Contains(changed[0].Flags, `\Seen`) {
		t.Fatalf("changed = %+v", changed)
	}
	uids, err := s.UIDs(ctx, folder, 3)
	if err != nil || !slices.Equal(uids, []uint32{2, 3}) {
		t.Fatalf("uids = %v err %v", uids, err)
	}
	if none, err := s.UIDs(ctx, folder, 0); err != nil || len(none) != 0 {
		t.Fatalf("uids upto 0 = %v err %v", none, err)
	}

	// still read-only: none of the above moved the mod-sequence.
	st3, err := s.Examine(ctx, folder)
	if err != nil || st3.HighestModSeq != st2.HighestModSeq {
		t.Fatalf("HIGHESTMODSEQ moved %d -> %d (err %v): the source wrote", st2.HighestModSeq, st3.HighestModSeq, err)
	}

	if _, err := s.Examine(ctx, folder+"-does-not-exist"); !errors.Is(err, mail.ErrFolderGone) {
		t.Fatalf("examine of a missing folder: %v", err)
	}
}
