package mailtest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/michele/imapvault/internal/mail"
)

func uidsOf(t *testing.T, f *Fake, folder string, from uint32) []uint32 {
	t.Helper()
	var out []uint32
	if err := f.ListMessages(context.Background(), folder, from, func(b []mail.MessageInfo) error {
		for _, m := range b {
			out = append(out, m.UID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFakeModSeqAndChangedFlags(t *testing.T) {
	ctx := context.Background()
	f := New()
	fo := f.AddFolder("INBOX")
	fo.Add("a")
	fo.Add("b")
	fo.Add("c")
	st, _ := f.Examine(ctx, "INBOX")
	if st.HighestModSeq == 0 {
		t.Fatal("CONDSTORE fake reports no HIGHESTMODSEQ")
	}
	since := st.HighestModSeq

	fo.SetFlags(2, `\Seen`)
	fo.Add("d")
	var changed []uint32
	if err := f.ChangedFlags(ctx, "INBOX", 3, since, func(b []mail.MessageInfo) error {
		for _, m := range b {
			changed = append(changed, m.UID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changed, []uint32{2}) { // uid 4 is above uptoUID, uid 1 and 3 did not change
		t.Fatalf("changed = %v", changed)
	}
	if st2, _ := f.Examine(ctx, "INBOX"); st2.HighestModSeq <= since {
		t.Fatal("HIGHESTMODSEQ did not move")
	}
}

func TestFakeExpungeKeepsModSeqUnlessAsked(t *testing.T) {
	ctx := context.Background()
	f := New()
	fo := f.AddFolder("INBOX")
	fo.Add("a")
	before, _ := f.Examine(ctx, "INBOX")
	fo.Delete(1)
	after, _ := f.Examine(ctx, "INBOX")
	if after.HighestModSeq != before.HighestModSeq || after.NumMessages != 0 {
		t.Fatalf("before %+v after %+v", before, after)
	}
	fo.BumpOnExpunge()
	if bumped, _ := f.Examine(ctx, "INBOX"); bumped.HighestModSeq <= before.HighestModSeq {
		t.Fatal("BumpOnExpunge did nothing")
	}
}

func TestFakeListFromAndUIDs(t *testing.T) {
	f := New()
	fo := f.AddFolder("INBOX")
	for i := 0; i < 5; i++ {
		fo.Add("m")
	}
	fo.Delete(2)
	if got := uidsOf(t, f, "INBOX", 1); !reflect.DeepEqual(got, []uint32{1, 3, 4, 5}) {
		t.Fatalf("from 1: %v", got)
	}
	if got := uidsOf(t, f, "INBOX", 4); !reflect.DeepEqual(got, []uint32{4, 5}) {
		t.Fatalf("from 4: %v", got)
	}
	got, err := f.UIDs(context.Background(), "INBOX", 4)
	if err != nil || !reflect.DeepEqual(got, []uint32{1, 3, 4}) {
		t.Fatalf("uids upto 4: %v %v", got, err)
	}
}

func TestFakeNoCondstore(t *testing.T) {
	ctx := context.Background()
	f := New()
	f.Caps = mail.Capabilities{"UIDPLUS"}
	f.AddFolder("INBOX").Add("a")
	if st, _ := f.Examine(ctx, "INBOX"); st.HighestModSeq != 0 {
		t.Fatalf("HighestModSeq = %d without CONDSTORE", st.HighestModSeq)
	}
	if err := f.ChangedFlags(ctx, "INBOX", 1, 1, func([]mail.MessageInfo) error { return nil }); err == nil {
		t.Fatal("ChangedFlags worked without CONDSTORE")
	}
}

func TestFakeRecreateRenameRemove(t *testing.T) {
	ctx := context.Background()
	f := New()
	fo := f.AddFolder("A")
	fo.Add("x")
	uv := fo.UIDValidity
	fo.Recreate()
	st, _ := f.Examine(ctx, "A")
	if st.UIDValidity != uv+1 || st.UIDNext != 1 || st.NumMessages != 0 {
		t.Fatalf("after recreate: %+v", st)
	}
	f.RenameFolder("A", "B")
	if _, err := f.Examine(ctx, "A"); !errors.Is(err, mail.ErrFolderGone) {
		t.Fatalf("examine old name: %v", err)
	}
	if _, err := f.Examine(ctx, "B"); err != nil {
		t.Fatal(err)
	}
	f.RemoveFolder("B")
	if _, err := f.Examine(ctx, "B"); !errors.Is(err, mail.ErrFolderGone) {
		t.Fatalf("examine removed: %v", err)
	}
}

func TestFakeFailAtAndCount(t *testing.T) {
	ctx := context.Background()
	f := New()
	f.AddFolder("INBOX").Add("a")
	boom := errors.New("boom")
	f.FailAt = func(call string) error {
		if call == "examine INBOX" {
			return boom
		}
		return nil
	}
	if _, err := f.Examine(ctx, "INBOX"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if f.Count("examine") != 1 {
		t.Fatalf("count = %d", f.Count("examine"))
	}
	f.Reset()
	if f.Count("examine") != 0 {
		t.Fatal("Reset kept calls")
	}
}
