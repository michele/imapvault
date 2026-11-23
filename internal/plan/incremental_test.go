package plan

import (
	"errors"
	"reflect"
	"testing"
)

func TestChooseMode(t *testing.T) {
	known := FolderSync{UIDValidity: 7, LastUID: 10, ModSeq: 50, OpenCurrent: 10}
	srv := ServerFolder{UIDValidity: 7, UIDNext: 11, NumMessages: 10, HighestModSeq: 50}
	tests := []struct {
		name      string
		known     FolderSync
		srv       ServerFolder
		condstore bool
		force     bool
		want      Mode
	}{
		{"nothing changed", known, srv, true, false, ModeUnchanged},
		{"forced", known, srv, true, true, ModeFull},
		{"no condstore", known, srv, false, false, ModeFull},
		{"never synced (modseq 0)", FolderSync{UIDValidity: 7, LastUID: 10, OpenCurrent: 10}, srv, true, false, ModeFull},
		{"server reports no modseq", known, ServerFolder{UIDValidity: 7, UIDNext: 11, NumMessages: 10}, true, false, ModeFull},
		{"uidvalidity changed", known, ServerFolder{UIDValidity: 8, UIDNext: 11, NumMessages: 10, HighestModSeq: 50}, true, false, ModeFull},
		{"entries of another generation still open", FolderSync{UIDValidity: 7, LastUID: 10, ModSeq: 50, OpenCurrent: 10, OpenOther: 2}, srv, true, false, ModeFull},
		{"modseq moved (flag change)", known, ServerFolder{UIDValidity: 7, UIDNext: 11, NumMessages: 10, HighestModSeq: 60}, true, false, ModeIncremental},
		{"new mail", known, ServerFolder{UIDValidity: 7, UIDNext: 12, NumMessages: 11, HighestModSeq: 55}, true, false, ModeIncremental},
		{"expunge without a modseq bump", known, ServerFolder{UIDValidity: 7, UIDNext: 11, NumMessages: 9, HighestModSeq: 50}, true, false, ModeIncremental},
		{"expunge plus append, same count and modseq", known, ServerFolder{UIDValidity: 7, UIDNext: 12, NumMessages: 10, HighestModSeq: 50}, true, false, ModeIncremental},
		{"index has fewer entries than server", FolderSync{UIDValidity: 7, LastUID: 10, ModSeq: 50, OpenCurrent: 8}, srv, true, false, ModeIncremental},
		{"empty folder, nothing changed", FolderSync{UIDValidity: 7, ModSeq: 5}, ServerFolder{UIDValidity: 7, UIDNext: 1, HighestModSeq: 5}, true, false, ModeUnchanged},
		{"server without uidnext", known, ServerFolder{UIDValidity: 7, NumMessages: 10, HighestModSeq: 50}, true, false, ModeIncremental},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChooseMode(tc.known, tc.srv, tc.condstore, tc.force); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestModeString(t *testing.T) {
	for m, want := range map[Mode]string{ModeFull: "full", ModeIncremental: "incremental", ModeUnchanged: "unchanged", Mode(9): "mode(9)"} {
		if m.String() != want {
			t.Errorf("%d: %q want %q", int(m), m.String(), want)
		}
	}
}

func TestSplitGenerations(t *testing.T) {
	open := []OpenEntry{
		{EntryID: 9, UID: 1, UIDValidity: 5},
		{EntryID: 2, UID: 1, UIDValidity: 6},
		{EntryID: 4, UID: 2, UIDValidity: 6},
		{EntryID: 3, UID: 7, UIDValidity: 4},
	}
	cur, closeIDs := SplitGenerations(open, 6)
	if got := []int64{cur[0].EntryID, cur[1].EntryID}; len(cur) != 2 || !reflect.DeepEqual(got, []int64{2, 4}) {
		t.Fatalf("current = %+v", cur)
	}
	if !reflect.DeepEqual(closeIDs, []int64{3, 9}) {
		t.Fatalf("close = %v", closeIDs)
	}
	cur, closeIDs = SplitGenerations(nil, 6)
	if cur != nil || closeIDs != nil {
		t.Fatalf("empty input gave %v %v", cur, closeIDs)
	}
	// A reset: nothing matches, everything closes.
	cur, closeIDs = SplitGenerations(open, 99)
	if len(cur) != 0 || len(closeIDs) != 4 {
		t.Fatalf("reset: %v %v", cur, closeIDs)
	}
}

func TestDiffIncremental(t *testing.T) {
	oe := func(id int64, uid uint32, flags string) OpenEntry {
		return OpenEntry{EntryID: id, UID: uid, Flags: flags}
	}
	tests := []struct {
		name         string
		in           IncrementalInput
		want         MessagePlan
		wantNeedUIDs bool
		wantErr      bool
		wantInconsis bool
	}{
		{name: "nothing", in: IncrementalInput{Open: []OpenEntry{oe(1, 1, ""), oe(2, 2, "")}, LastUID: 2, NumMessages: 2}},
		{name: "new mail",
			in: IncrementalInput{Open: []OpenEntry{oe(1, 1, "")}, LastUID: 1, NumMessages: 3,
				NewRange: []ServerMsg{{UID: 2}, {UID: 3}}},
			want: MessagePlan{Fetch: []uint32{2, 3}}},
		{name: "range returns the last old message too (n:* quirk)",
			in: IncrementalInput{Open: []OpenEntry{oe(1, 1, "")}, LastUID: 1, NumMessages: 1,
				NewRange: []ServerMsg{{UID: 1}}}},
		{name: "flag change on old message",
			in: IncrementalInput{Open: []OpenEntry{oe(1, 1, ""), oe(2, 2, "")}, LastUID: 2, NumMessages: 2,
				Changed: []ServerMsg{{UID: 2, Flags: []string{`\Seen`}}}},
			want: MessagePlan{Reflag: []Reflag{{EntryID: 2, UID: 2, Flags: `\Seen`}}}},
		{name: "changed but flags already match (changed during the previous run)",
			in: IncrementalInput{Open: []OpenEntry{oe(1, 1, `\Seen`)}, LastUID: 1, NumMessages: 1,
				Changed: []ServerMsg{{UID: 1, Flags: []string{`\Seen`}}}}},
		{name: "changed message above last_uid is left to the new range",
			in: IncrementalInput{Open: []OpenEntry{oe(1, 1, "")}, LastUID: 1, NumMessages: 2,
				NewRange: []ServerMsg{{UID: 2}}, Changed: []ServerMsg{{UID: 2, Flags: []string{`\Seen`}}}},
			want: MessagePlan{Fetch: []uint32{2}}},
		{name: "expunge needs the UID list",
			in:           IncrementalInput{Open: []OpenEntry{oe(1, 1, ""), oe(2, 2, "")}, LastUID: 2, NumMessages: 1},
			wantNeedUIDs: true},
		{name: "expunge hidden by new mail still needs the UID list",
			in: IncrementalInput{Open: []OpenEntry{oe(1, 1, ""), oe(2, 2, "")}, LastUID: 2, NumMessages: 2,
				NewRange: []ServerMsg{{UID: 3}}},
			want: MessagePlan{Fetch: []uint32{3}}, wantNeedUIDs: true},
		{name: "entries left by an interrupted run above last_uid are diffed as in full mode",
			in: IncrementalInput{Open: []OpenEntry{oe(1, 1, ""), oe(5, 2, ""), oe(6, 3, "")}, LastUID: 1, NumMessages: 4,
				NewRange: []ServerMsg{{UID: 2, Flags: []string{`\Seen`}}, {UID: 4}, {UID: 5}}},
			want: MessagePlan{Fetch: []uint32{4, 5}, Close: []int64{6}, Reflag: []Reflag{{EntryID: 5, UID: 2, Flags: `\Seen`}}}},
		{name: "server holds more messages than index and new range can account for",
			in:      IncrementalInput{Open: []OpenEntry{oe(1, 1, "")}, LastUID: 1, NumMessages: 5},
			wantErr: true, wantInconsis: true},
		{name: "changed uid with no open entry",
			in: IncrementalInput{Open: []OpenEntry{oe(1, 1, "")}, LastUID: 3, NumMessages: 1,
				Changed: []ServerMsg{{UID: 3}}},
			wantErr: true, wantInconsis: true},
		{name: "more messages in the index than the server count allows is fine (expunge)",
			in:           IncrementalInput{Open: []OpenEntry{oe(1, 1, ""), oe(2, 2, ""), oe(3, 3, "")}, LastUID: 3, NumMessages: 0},
			wantNeedUIDs: true},
		{name: "server lists a uid twice in changed", in: IncrementalInput{Open: []OpenEntry{oe(1, 1, "")}, LastUID: 1, NumMessages: 1,
			Changed: []ServerMsg{{UID: 1}, {UID: 1}}}, wantErr: true},
		{name: "index has two open entries for one uid", in: IncrementalInput{Open: []OpenEntry{oe(1, 1, ""), oe(2, 1, "")}, LastUID: 1, NumMessages: 1},
			wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DiffIncremental(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if tc.wantInconsis && !errors.Is(err, ErrInconsistent) {
					t.Fatalf("err = %v, want ErrInconsistent", err)
				}
				return
			}
			if !reflect.DeepEqual(got.MessagePlan, tc.want) {
				t.Fatalf("plan = %+v want %+v", got.MessagePlan, tc.want)
			}
			if got.NeedUIDs != tc.wantNeedUIDs {
				t.Fatalf("NeedUIDs = %v want %v", got.NeedUIDs, tc.wantNeedUIDs)
			}
		})
	}
}

func TestDiffIncrementalImpossibleCount(t *testing.T) {
	// The new range alone is bigger than the server's count: the listing and
	// EXAMINE disagree (a message arrived between them, or a buggy server).
	_, err := DiffIncremental(IncrementalInput{LastUID: 0, NumMessages: 1, NewRange: []ServerMsg{{UID: 1}, {UID: 2}}})
	if !errors.Is(err, ErrInconsistent) {
		t.Fatalf("err = %v, want ErrInconsistent", err)
	}
}

func TestWithExpunges(t *testing.T) {
	oe := func(id int64, uid uint32, flags string) OpenEntry {
		return OpenEntry{EntryID: id, UID: uid, Flags: flags}
	}
	open := []OpenEntry{oe(1, 1, ""), oe(2, 2, ""), oe(3, 3, "")}
	tests := []struct {
		name    string
		in      IncrementalInput
		uids    []uint32
		want    MessagePlan
		wantErr bool
	}{
		{name: "one expunged",
			in:   IncrementalInput{Open: open, LastUID: 3, NumMessages: 2},
			uids: []uint32{1, 3},
			want: MessagePlan{Close: []int64{2}}},
		{name: "all expunged",
			in:   IncrementalInput{Open: open, LastUID: 3, NumMessages: 0},
			uids: nil,
			want: MessagePlan{Close: []int64{1, 2, 3}}},
		{name: "expunge and new mail together",
			in:   IncrementalInput{Open: open, LastUID: 3, NumMessages: 4, NewRange: []ServerMsg{{UID: 4}, {UID: 5}}},
			uids: []uint32{2, 3},
			want: MessagePlan{Fetch: []uint32{4, 5}, Close: []int64{1}}},
		{name: "a flag change on a message that was then expunged: the close wins",
			in:   IncrementalInput{Open: open, LastUID: 3, NumMessages: 2, Changed: []ServerMsg{{UID: 2, Flags: []string{`\Seen`}}, {UID: 3, Flags: []string{`\Seen`}}}},
			uids: []uint32{1, 3},
			want: MessagePlan{Close: []int64{2}, Reflag: []Reflag{{EntryID: 3, UID: 3, Flags: `\Seen`}}}},
		{name: "server holds a uid the index lacks",
			in:      IncrementalInput{Open: open, LastUID: 3, NumMessages: 2},
			uids:    []uint32{1, 2, 3},
			wantErr: true},
		{name: "UID list disagrees with the counts",
			in:      IncrementalInput{Open: open, LastUID: 3, NumMessages: 2},
			uids:    []uint32{3},
			wantErr: true},
		{name: "uids above last_uid in the list are ignored",
			in:   IncrementalInput{Open: open, LastUID: 3, NumMessages: 2},
			uids: []uint32{1, 3, 9},
			want: MessagePlan{Close: []int64{2}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := DiffIncremental(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !p.NeedUIDs {
				t.Fatal("test case needs NeedUIDs")
			}
			got, err := p.WithExpunges(tc.uids)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrInconsistent) {
					t.Fatalf("err = %v, want ErrInconsistent", err)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}

	t.Run("no expunges expected: passes the plan through", func(t *testing.T) {
		p, err := DiffIncremental(IncrementalInput{Open: open, LastUID: 3, NumMessages: 3})
		if err != nil || p.NeedUIDs {
			t.Fatalf("err=%v need=%v", err, p.NeedUIDs)
		}
		got, err := p.WithExpunges(nil)
		if err != nil || !got.Empty() {
			t.Fatalf("got %+v err %v", got, err)
		}
	})
}

func TestMatchEmailID(t *testing.T) {
	known := map[string]KnownBlob{"M1": {SHA: h(1), Size: 100}}
	tests := []struct {
		name string
		id   string
		size int64
		ok   bool
	}{
		{"hit", "M1", 100, true},
		{"unknown id", "M2", 100, false},
		{"same id, different size is not a match", "M1", 101, false},
		{"no emailid", "", 100, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, ok := MatchEmailID(tc.id, tc.size, known)
			if ok != tc.ok {
				t.Fatalf("ok = %v want %v", ok, tc.ok)
			}
			if ok && b.SHA != h(1) {
				t.Fatalf("wrong blob %x", b.SHA[:2])
			}
		})
	}
	if _, ok := MatchEmailID("M1", 100, nil); ok {
		t.Fatal("nil map matched")
	}
}
