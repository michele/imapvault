package plan

import (
	"reflect"
	"testing"
)

func TestNormalizeFlags(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"empty", nil, ""},
		{"recent dropped", []string{`\Recent`, `\Seen`}, `\Seen`},
		{"recent only", []string{`\RECENT`}, ""},
		{"sorted", []string{`\Seen`, `\Answered`, `$Label`}, `$Label \Answered \Seen`},
		{"system case folded and deduped", []string{`\seen`, `\SEEN`, `\Seen`}, `\Seen`},
		{"keywords are case sensitive", []string{"$a", "$A"}, "$A $a"},
		{"empty strings skipped", []string{"", `\Seen`}, `\Seen`},
		{"unknown system flag kept", []string{`\Custom`}, `\Custom`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeFlags(tc.in); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func h(b byte) [32]byte { var x [32]byte; x[0] = b; return x }

func TestDiffFolders(t *testing.T) {
	known := []KnownFolder{{ID: 2, NameHash: h(2)}, {ID: 1, NameHash: h(1)}, {ID: 3, NameHash: h(3)}}
	tests := []struct {
		name               string
		known              []KnownFolder
		listed             []ListedFolder
		wantNew            []string
		wantSame, wantGone []int64
	}{
		{"empty", nil, nil, nil, nil, nil},
		{"all new, sorted by name", nil, []ListedFolder{{NameHash: h(9), Name: "b"}, {NameHash: h(8), Name: "a"}}, []string{"a", "b"}, nil, nil},
		{"all same", known, []ListedFolder{{NameHash: h(1)}, {NameHash: h(2)}, {NameHash: h(3)}}, nil, []int64{1, 2, 3}, nil},
		{"all gone", known, nil, nil, nil, []int64{1, 2, 3}},
		{"mixed (excluded since last run counts as gone)", known, []ListedFolder{{NameHash: h(2)}, {NameHash: h(7), Name: "x"}}, []string{"x"}, []int64{2}, []int64{1, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := DiffFolders(tc.known, tc.listed)
			var gotNew []string
			for _, n := range p.New {
				gotNew = append(gotNew, n.Name)
			}
			var gotSame, gotGone []int64
			for _, s := range p.Same {
				gotSame = append(gotSame, s.ID)
			}
			for _, g := range p.Gone {
				gotGone = append(gotGone, g.ID)
			}
			if !reflect.DeepEqual(gotNew, tc.wantNew) || !reflect.DeepEqual(gotSame, tc.wantSame) || !reflect.DeepEqual(gotGone, tc.wantGone) {
				t.Fatalf("new=%v same=%v gone=%v; want %v %v %v", gotNew, gotSame, gotGone, tc.wantNew, tc.wantSame, tc.wantGone)
			}
		})
	}
}

func TestDiffMessages(t *testing.T) {
	tests := []struct {
		name    string
		open    []OpenEntry
		server  []ServerMsg
		want    MessagePlan
		wantErr bool
	}{
		{name: "empty"},
		{name: "all new, unsorted input", server: []ServerMsg{{UID: 5}, {UID: 2}}, want: MessagePlan{Fetch: []uint32{2, 5}}},
		{name: "all gone", open: []OpenEntry{{EntryID: 9, UID: 1}, {EntryID: 4, UID: 2}}, want: MessagePlan{Close: []int64{4, 9}}},
		{name: "unchanged", open: []OpenEntry{{EntryID: 1, UID: 1, Flags: `\Seen`}}, server: []ServerMsg{{UID: 1, Flags: []string{`\Seen`}}}},
		{name: "recent is noise", open: []OpenEntry{{EntryID: 1, UID: 1, Flags: `\Seen`}}, server: []ServerMsg{{UID: 1, Flags: []string{`\Recent`, `\Seen`}}}},
		{name: "flag change", open: []OpenEntry{{EntryID: 1, UID: 1, Flags: ""}}, server: []ServerMsg{{UID: 1, Flags: []string{`\Seen`}}},
			want: MessagePlan{Reflag: []Reflag{{EntryID: 1, UID: 1, Flags: `\Seen`}}}},
		{name: "flag cleared", open: []OpenEntry{{EntryID: 1, UID: 1, Flags: `\Seen`}}, server: []ServerMsg{{UID: 1}},
			want: MessagePlan{Reflag: []Reflag{{EntryID: 1, UID: 1, Flags: ""}}}},
		{name: "everything at once",
			open:   []OpenEntry{{EntryID: 1, UID: 1}, {EntryID: 2, UID: 2}, {EntryID: 3, UID: 3, Flags: `\Seen`}},
			server: []ServerMsg{{UID: 3, Flags: []string{`\Seen`, `\Flagged`}}, {UID: 4}, {UID: 1}},
			want:   MessagePlan{Fetch: []uint32{4}, Close: []int64{2}, Reflag: []Reflag{{EntryID: 3, UID: 3, Flags: `\Flagged \Seen`}}}},
		{name: "server duplicate uid", server: []ServerMsg{{UID: 1}, {UID: 1}}, wantErr: true},
		{name: "index duplicate uid", open: []OpenEntry{{EntryID: 1, UID: 1}, {EntryID: 2, UID: 1}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DiffMessages(tc.open, tc.server)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
			if got.Empty() != (len(tc.want.Fetch)+len(tc.want.Close)+len(tc.want.Reflag) == 0) {
				t.Fatal("Empty() disagrees")
			}
		})
	}
}
