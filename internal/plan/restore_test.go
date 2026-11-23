package plan

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func msg(folder string, b byte, size int64, id string, flags ...string) RestoreMessage {
	return RestoreMessage{Folder: folder, SrcDelim: "/", Blob: blob(b), Size: size, MessageID: id, Flags: flags, InternalDate: t0.Add(time.Duration(b) * time.Minute)}
}

func baseInput(msgs ...RestoreMessage) RestoreInput {
	return RestoreInput{
		Target:   RestoreTarget{Host: "imap.example.com", Port: 993, Username: "me@example.com"},
		RepoID:   "repo-1",
		Source:   "snapshot 12",
		Prefix:   "Restored/12",
		TgtDelim: "/",
		Existing: map[string][]PresentMessage{},
		Messages: msgs,
	}
}

func TestTargetFolderName(t *testing.T) {
	cases := []struct {
		prefix, src, tgt, folder, want string
		wantErr                        bool
	}{
		{"Restored/12", "/", "/", "INBOX", "Restored/12/INBOX", false},
		{"Restored/12", "/", "/", "Clients/Acme", "Restored/12/Clients/Acme", false},
		{"", "/", "/", "Clients/Acme", "Clients/Acme", false},
		{"Restored/12", "/", ".", "Clients/Acme", "Restored.12.Clients.Acme", false},
		{"", ".", "/", "INBOX.Sent", "INBOX/Sent", false},
		{"", "", "/", "Flat", "Flat", false},
		{"Restored/12", "/", ".", "a.b/c", "", true}, // a component holds the target delimiter
		{"", "/", "/", "a//b", "", true},             // empty component
		{"", "/", "/", "a*", "", true},               // wildcard
		{"Restored/12", "/", "", "INBOX", "", true},  // target with no hierarchy
		{"", "/", "", "INBOX", "INBOX", false},
	}
	for _, c := range cases {
		got, err := TargetFolderName(c.prefix, c.src, c.tgt, c.folder)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("TargetFolderName(%q,%q,%q,%q) = %q, %v; want %q (err %v)", c.prefix, c.src, c.tgt, c.folder, got, err, c.want, c.wantErr)
		}
		if err != nil && !errors.Is(err, ErrFolderName) {
			t.Errorf("error is not ErrFolderName: %v", err)
		}
	}
}

func TestRestoreIntoPrefixCreatesFolders(t *testing.T) {
	p, err := Restore(baseInput(msg("INBOX", 1, 100, "<a>"), msg("INBOX", 2, 200, "<b>"), msg("Sent", 3, 50, "<c>")))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Create, []string{"Restored/12/INBOX", "Restored/12/Sent"}) {
		t.Fatalf("Create = %v", p.Create)
	}
	if p.Appends != 3 || p.Bytes != 350 || p.Skipped != 0 || len(p.Folders) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	if p.Folders[0].Exists || p.Folders[0].Name != "Restored/12/INBOX" {
		t.Fatalf("folder 0 = %+v", p.Folders[0])
	}
}

func TestRestoreInPlaceAppendsIntoExistingAndCreatesMissing(t *testing.T) {
	in := baseInput(msg("INBOX", 1, 100, "<a>"), msg("Archive", 2, 200, "<b>"))
	in.InPlace, in.Prefix = true, ""
	in.Existing = map[string][]PresentMessage{"INBOX": {{MessageID: "<other>", Size: 5, InternalDate: t0}}}
	p, err := Restore(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Create, []string{"Archive"}) {
		t.Fatalf("Create = %v (INBOX exists and must not be created)", p.Create)
	}
	var inbox RestoreFolder
	for _, f := range p.Folders {
		if f.Name == "INBOX" {
			inbox = f
		}
	}
	if !inbox.Exists || inbox.ExistingMessages != 1 || len(inbox.Append) != 1 {
		t.Fatalf("INBOX = %+v", inbox)
	}
}

func TestRestoreSkipsMessagesAlreadyPresent(t *testing.T) {
	m1, m2 := msg("INBOX", 1, 100, "<a>"), msg("INBOX", 2, 200, "<b>")
	in := baseInput(m1, m2)
	in.InPlace, in.Prefix = true, ""
	in.Existing = map[string][]PresentMessage{"INBOX": {{MessageID: "<a>", Size: 100, InternalDate: t0.Add(99 * time.Hour)}}}
	p, err := Restore(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Skipped != 1 || p.Appends != 1 || p.Folders[0].Append[0].Blob != blob(2) {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPresentMatching(t *testing.T) {
	m := RestoreMessage{Size: 100, MessageID: "<a>", InternalDate: t0}
	cases := []struct {
		name string
		p    PresentMessage
		m    RestoreMessage
		want bool
	}{
		{"same id and size", PresentMessage{"<a>", 100, t0.Add(time.Hour)}, m, true},
		{"same id, other size", PresentMessage{"<a>", 101, t0}, m, false},
		{"other id, same size and date", PresentMessage{"<z>", 100, t0}, m, false},
		{"no id on server: size and date", PresentMessage{"", 100, t0}, m, true},
		{"no id on server, date differs", PresentMessage{"", 100, t0.Add(time.Minute)}, m, false},
		{"no id on message: size and date", PresentMessage{"<a>", 100, t0}, RestoreMessage{Size: 100, InternalDate: t0}, true},
		{"sub-second date difference", PresentMessage{"", 100, t0.Add(300 * time.Millisecond)}, RestoreMessage{Size: 100, InternalDate: t0}, true},
	}
	for _, c := range cases {
		if got := presentMatches(c.p, c.m); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Two identical messages in the snapshot need two on the server.
func TestMatchesAreConsumedOneForOne(t *testing.T) {
	a := msg("INBOX", 1, 100, "<dup>")
	b := msg("INBOX", 1, 100, "<dup>")
	in := baseInput(a, b)
	in.InPlace, in.Prefix = true, ""
	in.Existing = map[string][]PresentMessage{"INBOX": {{MessageID: "<dup>", Size: 100, InternalDate: t0}}}
	p, err := Restore(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Skipped != 1 || p.Appends != 1 {
		t.Fatalf("one existing copy must cover only one of two: %+v", p)
	}
}

func TestRestoreNeverSendsDeletedOrRecent(t *testing.T) {
	p, err := Restore(baseInput(
		msg("INBOX", 1, 10, "<a>", `\Seen`, `\Deleted`, `\Recent`, `$Label`),
		msg("INBOX", 2, 10, "<b>", `\DELETED`),
		msg("INBOX", 3, 10, "<c>"),
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range p.Folders[0].Append {
		for _, f := range it.Flags {
			if f == `\Deleted` || f == `\Recent` {
				t.Fatalf("flag %s would be appended", f)
			}
		}
	}
	if p.DeletedFlagDropped != 2 {
		t.Fatalf("DeletedFlagDropped = %d, want 2", p.DeletedFlagDropped)
	}
	if got := p.Folders[0].Append[0].Flags; !reflect.DeepEqual(got, []string{`$Label`, `\Seen`}) {
		t.Fatalf("flags = %v", got)
	}
}

func TestRestoreModeErrors(t *testing.T) {
	in := baseInput(msg("INBOX", 1, 1, ""))
	in.InPlace = true // prefix still set
	if _, err := Restore(in); err == nil {
		t.Error("prefix + in-place must be refused")
	}
	in = baseInput(msg("INBOX", 1, 1, ""))
	in.Prefix = ""
	if _, err := Restore(in); err == nil {
		t.Error("no prefix and not in place must be refused")
	}
}

func TestRestoreOfNothingIsEmpty(t *testing.T) {
	p, err := Restore(baseInput())
	if err != nil || !p.Empty() || p.Token() == "" {
		t.Fatalf("plan = %+v, err = %v", p, err)
	}
}

// Property: apply a plan to the target as the engine would, plan again, and the
// second plan is empty. Randomised over folders, duplicates, missing Message-IDs
// and pre-existing mail.
func TestApplyThenReplanIsEmpty(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		folders := []string{"INBOX", "Sent", "Clients/Acme", "Archive/2025"}
		var msgs []RestoreMessage
		for i, n := 0, rng.Intn(30); i < n; i++ {
			// The blob is the hash of the message, so one blob has one size and one
			// Message-ID (or none); only the internal date and the folder vary.
			b := byte(rng.Intn(8))
			id := ""
			if b%4 != 0 {
				id = "<id" + string(rune('a'+b)) + ">"
			}
			m := msg(folders[rng.Intn(len(folders))], b, int64(10+b%3), id, `\Seen`)
			m.InternalDate = t0.Add(time.Duration(rng.Intn(3)) * time.Second * 3600)
			msgs = append(msgs, m)
		}
		in := baseInput(msgs...)
		if rng.Intn(2) == 0 {
			in.InPlace, in.Prefix = true, ""
			// some mail already there
			for _, f := range folders[:2] {
				if rng.Intn(2) == 0 {
					in.Existing[f] = []PresentMessage{{MessageID: "<idz>", Size: 99, InternalDate: t0}}
				}
			}
		}
		p1, err := Restore(in)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		// Apply: every append lands in the target with its message's id, size and date.
		lookup := map[[32]byte][]RestoreMessage{}
		for _, m := range msgs {
			lookup[m.Blob] = append(lookup[m.Blob], m)
		}
		next := map[string][]PresentMessage{}
		for k, v := range in.Existing {
			next[k] = append([]PresentMessage(nil), v...)
		}
		for _, f := range p1.Folders {
			if _, ok := next[f.Name]; !ok {
				next[f.Name] = []PresentMessage{}
			}
			for _, it := range f.Append {
				var src RestoreMessage
				for _, c := range lookup[it.Blob] {
					if c.InternalDate.Equal(it.InternalDate) && c.Size == it.Size {
						src = c
					}
				}
				next[f.Name] = append(next[f.Name], PresentMessage{MessageID: src.MessageID, Size: it.Size, InternalDate: it.InternalDate})
			}
		}
		in.Existing = next
		p2, err := Restore(in)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if !p2.Empty() || len(p2.Create) != 0 || p2.Appends != 0 {
			t.Fatalf("seed %d: second plan is not empty: %+v (first: %d appends)", seed, p2, p1.Appends)
		}
	}
}

func TestTokenChangesWithEveryPartOfThePlan(t *testing.T) {
	base := func() RestoreInput {
		return baseInput(msg("INBOX", 1, 100, "<a>", `\Seen`), msg("Sent", 2, 50, "<b>"))
	}
	token := func(in RestoreInput) string {
		p, err := Restore(in)
		if err != nil {
			t.Fatal(err)
		}
		return p.Token()
	}
	want := token(base())
	if token(base()) != want {
		t.Fatal("token is not deterministic")
	}
	// Message order must not matter.
	rev := base()
	rev.Messages[0], rev.Messages[1] = rev.Messages[1], rev.Messages[0]
	if token(rev) != want {
		t.Error("token depends on input order")
	}
	mutations := map[string]func(*RestoreInput){
		"host":      func(in *RestoreInput) { in.Target.Host = "evil.example.com" },
		"port":      func(in *RestoreInput) { in.Target.Port = 143 },
		"username":  func(in *RestoreInput) { in.Target.Username = "other@example.com" },
		"repo":      func(in *RestoreInput) { in.RepoID = "repo-2" },
		"source":    func(in *RestoreInput) { in.Source = "snapshot 13" },
		"prefix":    func(in *RestoreInput) { in.Prefix = "Restored/13" },
		"in place":  func(in *RestoreInput) { in.InPlace, in.Prefix = true, "" },
		"extra msg": func(in *RestoreInput) { in.Messages = append(in.Messages, msg("INBOX", 9, 1, "<z>")) },
		"fewer msg": func(in *RestoreInput) { in.Messages = in.Messages[:1] },
		"blob":      func(in *RestoreInput) { in.Messages[0].Blob = blob(77) },
		"size":      func(in *RestoreInput) { in.Messages[0].Size++ },
		"flags":     func(in *RestoreInput) { in.Messages[0].Flags = []string{`\Seen`, `\Flagged`} },
		"date":      func(in *RestoreInput) { in.Messages[0].InternalDate = in.Messages[0].InternalDate.Add(time.Second) },
		"folder":    func(in *RestoreInput) { in.Messages[0].Folder = "Other" },
		"now existing": func(in *RestoreInput) {
			in.Existing = map[string][]PresentMessage{"Restored/12/INBOX": {{MessageID: "<q>", Size: 1, InternalDate: t0}}}
		},
	}
	for name, mutate := range mutations {
		in := base()
		mutate(&in)
		if got := token(in); got == want {
			t.Errorf("token unchanged after changing %s", name)
		}
	}
}

func TestTokenFormat(t *testing.T) {
	p, _ := Restore(baseInput(msg("INBOX", 1, 1, "<a>")))
	tok := p.Token()
	if len(tok) != 11 || tok[5] != '-' {
		t.Fatalf("token = %q, want xxxxx-xxxxx", tok)
	}
}

func TestApprove(t *testing.T) {
	p, _ := Restore(baseInput(msg("INBOX", 1, 1, "<a>")))
	tok := p.Token()
	for _, good := range []string{tok, " " + tok + "\n", upper(tok), tok[:5] + tok[6:]} {
		a, err := Approve(p, good)
		if err != nil {
			t.Errorf("Approve(%q): %v", good, err)
			continue
		}
		if a.Plan().Token() != tok {
			t.Error("approved plan differs")
		}
	}
	for _, bad := range []string{"", "00000-00000", tok[:5], tok + "0", "approve", "yes", "y"} {
		a, err := Approve(p, bad)
		if !errors.Is(err, ErrNotApproved) {
			t.Errorf("Approve(%q) err = %v, want ErrNotApproved", bad, err)
		}
		if !a.Plan().Empty() {
			t.Errorf("Approve(%q) returned a usable plan with an error", bad)
		}
	}
	// A token from a different plan does not approve this one.
	other, _ := Restore(baseInput(msg("INBOX", 2, 1, "<a>")))
	if _, err := Approve(p, other.Token()); !errors.Is(err, ErrNotApproved) {
		t.Errorf("a token of another plan approved this one: %v", err)
	}
}

func TestApproveRefusesAnEmptyPlan(t *testing.T) {
	p, _ := Restore(baseInput())
	if _, err := Approve(p, p.Token()); err == nil {
		t.Fatal("an empty plan must not be approvable")
	}
}

// The zero ApprovedRestore (all a caller can make without Approve) carries no work.
func TestZeroApprovedRestoreIsEmpty(t *testing.T) {
	var a ApprovedRestore
	if !a.Plan().Empty() || len(a.Plan().Create) != 0 {
		t.Fatal("the zero value must carry an empty plan")
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}
