package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/mail"
	"github.com/michele/imapvault/internal/mail/mailtest"
	"github.com/michele/imapvault/internal/plan"
)

// restoreCfg is the repository's config with writing enabled, as the account's
// own config would have it.
func restoreCfg(cfg *config.Config) *config.Config {
	c := *cfg
	c.Restore.AllowAppend = true
	return &c
}

// mirror is a target holding exactly what the fake source holds now.
func mirror(f *mailtest.Fake) *mailtest.FakeTarget {
	t := mailtest.NewTarget()
	for _, fo := range f.Folders {
		t.Folders[fo.Name] = nil
		for _, m := range fo.Messages {
			t.Folders[fo.Name] = append(t.Folders[fo.Name], mailtest.TargetMessage{Raw: m.Raw, Flags: m.Flags, InternalDate: m.InternalDate})
		}
	}
	return t
}

func mustPlan(t *testing.T, cfg, tcfg *config.Config, tgt mail.Target, opts RestoreOptions) *RestorePreview {
	t.Helper()
	prev, err := PlanRestore(context.Background(), cfg, tcfg, tgt, opts)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	return prev
}

func approve(t *testing.T, prev *RestorePreview) plan.ApprovedRestore {
	t.Helper()
	a, err := plan.Approve(prev.Plan, prev.Token)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPlanRestoreWritesNothing(t *testing.T) {
	cfg := initRepo(t)
	runBackup(t, cfg, fakeMailbox())
	tgt := mailtest.NewTarget()
	prev := mustPlan(t, cfg, restoreCfg(cfg), tgt, RestoreOptions{})
	if prev.Plan.Appends != 3 || len(prev.Plan.Create) != 2 || prev.Token == "" {
		t.Fatalf("plan = %+v", prev.Plan)
	}
	if w := tgt.Writes(); len(w) != 0 || len(tgt.Folders) != 1 {
		t.Fatalf("planning wrote to the server: %v", w)
	}
}

func TestRestoreIntoPrefixThenSecondRunAppendsNothing(t *testing.T) {
	cfg := initRepo(t)
	src := fakeMailbox()
	runBackup(t, cfg, src)
	tcfg := restoreCfg(cfg)
	tgt := mirror(src) // the server as it is: restore must not touch these folders

	prev := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{})
	if got := prev.Plan.Create; len(got) != 2 || got[0] != "Restored/1/INBOX" || got[1] != "Restored/1/Sent" {
		t.Fatalf("Create = %v", got)
	}
	res, err := ApplyRestore(context.Background(), cfg, tcfg, tgt, approve(t, prev), RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Appended != 3 || len(res.Created) != 2 {
		t.Fatalf("result = %+v", res)
	}
	// Original folders untouched, restored ones hold the messages with flags and dates.
	if tgt.Count("INBOX") != 2 || tgt.Count("Sent") != 1 {
		t.Fatal("restore changed the user's own folders")
	}
	got := tgt.Folders["Restored/1/INBOX"]
	if len(got) != 2 || string(got[0].Raw) != string(src.Folders[0].Messages[0].Raw) {
		t.Fatalf("restored INBOX = %+v", got)
	}
	seen := 0
	for _, m := range got {
		for _, f := range m.Flags {
			if f == `\Seen` {
				seen++
			}
		}
		if m.InternalDate.IsZero() {
			t.Error("internal date lost")
		}
	}
	if seen != 1 {
		t.Fatalf("\\Seen flags restored = %d, want 1", seen)
	}
	before := len(tgt.Writes())

	again := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{})
	if !again.Plan.Empty() || again.Plan.Skipped != 3 {
		t.Fatalf("second plan = %+v, want empty with 3 skipped", again.Plan)
	}
	res, err = ApplyRestore(context.Background(), cfg, tcfg, tgt, plan.ApprovedRestore{}, RestoreOptions{})
	if err != nil || res.Appended != 0 || len(tgt.Writes()) != before {
		t.Fatalf("second run wrote: %+v %v", res, err)
	}
}

// The "undo the hack" case: everything deleted from the server, restored in place.
func TestRestoreDeletedSinceInPlace(t *testing.T) {
	cfg := initRepo(t)
	src := fakeMailbox()
	runBackup(t, cfg, src)
	for _, fo := range src.Folders {
		for len(fo.Messages) > 0 {
			fo.Delete(fo.Messages[0].UID)
		}
	}
	runBackup(t, cfg, src)
	tcfg := restoreCfg(cfg)
	tgt := mirror(src) // folders exist and are empty

	opts := RestoreOptions{DeletedSince: time.Hour, InPlace: true}
	prev := mustPlan(t, cfg, tcfg, tgt, opts)
	if prev.Plan.Appends != 3 || len(prev.Plan.Create) != 0 {
		t.Fatalf("plan = %+v", prev.Plan)
	}
	if _, err := ApplyRestore(context.Background(), cfg, tcfg, tgt, approve(t, prev), opts); err != nil {
		t.Fatal(err)
	}
	if tgt.Count("INBOX") != 2 || tgt.Count("Sent") != 1 {
		t.Fatalf("INBOX %d, Sent %d", tgt.Count("INBOX"), tgt.Count("Sent"))
	}
	if again := mustPlan(t, cfg, tcfg, tgt, opts); !again.Plan.Empty() {
		t.Fatalf("second restore would append %d", again.Plan.Appends)
	}
}

func TestDeletedSinceIgnoresMovedAndExistingMail(t *testing.T) {
	cfg := initRepo(t)
	src := fakeMailbox()
	runBackup(t, cfg, src)
	src.Folders[0].Delete(1) // really deleted
	// moved: message 2 leaves INBOX and arrives in Sent
	raw := string(src.Folders[0].Messages[len(src.Folders[0].Messages)-1].Raw)
	src.Folders[0].Delete(src.Folders[0].Messages[len(src.Folders[0].Messages)-1].UID)
	src.Folders[1].Add(raw, `\Seen`)
	runBackup(t, cfg, src)

	prev := mustPlan(t, cfg, restoreCfg(cfg), mirror(src), RestoreOptions{DeletedSince: time.Hour, InPlace: true})
	if prev.Plan.Appends != 1 {
		t.Fatalf("appends = %d, want only the deleted message (the moved one is on the server)", prev.Plan.Appends)
	}
}

func TestApplyRefusesWithoutAllowAppend(t *testing.T) {
	cfg := initRepo(t)
	runBackup(t, cfg, fakeMailbox())
	tgt := mailtest.NewTarget()
	prev := mustPlan(t, cfg, cfg, tgt, RestoreOptions{}) // cfg has no [restore]
	if prev.AllowAppend {
		t.Fatal("AllowAppend must be false")
	}
	if prev.Plan.Appends != 3 {
		t.Fatal("the preview still shows what would happen")
	}
	_, err := ApplyRestore(context.Background(), cfg, cfg, tgt, approve(t, prev), RestoreOptions{})
	if !errors.Is(err, ErrRestoreNotAllowed) {
		t.Fatalf("err = %v", err)
	}
	if len(tgt.Writes()) != 0 {
		t.Fatal("a refused restore wrote")
	}
}

func TestApplyRefusesAnotherTargetOrRepository(t *testing.T) {
	cfg := initRepo(t)
	runBackup(t, cfg, fakeMailbox())
	tcfg := restoreCfg(cfg)
	tgt := mailtest.NewTarget()
	prev := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{})
	approved := approve(t, prev)

	other := *tcfg
	other.IMAP.Username = "someone-else@example.com"
	if _, err := ApplyRestore(context.Background(), cfg, &other, tgt, approved, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "not for this account") {
		t.Fatalf("approved plan applied to another account: %v", err)
	}

	// A plan made from some other repository, approved by its own token.
	foreign := prev.Plan
	foreign.RepoID = "some-other-repo"
	a, err := plan.Approve(foreign, foreign.Token())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyRestore(context.Background(), cfg, tcfg, tgt, a, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "different repository") {
		t.Fatalf("plan from another repository applied: %v", err)
	}
	if len(tgt.Writes()) != 0 {
		t.Fatalf("writes happened: %v", tgt.Writes())
	}
}

func TestZeroApprovalWritesNothing(t *testing.T) {
	cfg := initRepo(t)
	runBackup(t, cfg, fakeMailbox())
	tgt := mailtest.NewTarget()
	res, err := ApplyRestore(context.Background(), cfg, restoreCfg(cfg), tgt, plan.ApprovedRestore{}, RestoreOptions{})
	if err != nil || res.Appended != 0 || len(tgt.Writes()) != 0 {
		t.Fatalf("%+v %v %v", res, err, tgt.Writes())
	}
}

// A restore that dies half way leaves a partial result; planning again appends
// only what is missing, never a second copy.
func TestRestoreResumesAfterFailure(t *testing.T) {
	cfg := initRepo(t)
	runBackup(t, cfg, fakeMailbox())
	tcfg := restoreCfg(cfg)
	tgt := mailtest.NewTarget()
	tgt.FailAppendAt = 2

	prev := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{})
	res, err := ApplyRestore(context.Background(), cfg, tcfg, tgt, approve(t, prev), RestoreOptions{})
	if err == nil || res.Appended != 1 {
		t.Fatalf("result %+v err %v, want 1 appended then an error", res, err)
	}
	tgt.FailAppendAt = 0
	prev = mustPlan(t, cfg, tcfg, tgt, RestoreOptions{})
	if prev.Plan.Appends != 2 || prev.Plan.Skipped != 1 {
		t.Fatalf("resume plan: %d appends, %d skipped", prev.Plan.Appends, prev.Plan.Skipped)
	}
	if _, err := ApplyRestore(context.Background(), cfg, tcfg, tgt, approve(t, prev), RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	total := tgt.Count("Restored/1/INBOX") + tgt.Count("Restored/1/Sent")
	if total != 3 {
		t.Fatalf("messages on the target = %d, want exactly 3", total)
	}
}

func TestRestoreCancelledBetweenAppends(t *testing.T) {
	cfg := initRepo(t)
	runBackup(t, cfg, fakeMailbox())
	tcfg := restoreCfg(cfg)
	tgt := mailtest.NewTarget()
	prev := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	opts := RestoreOptions{Log: func(f string, _ ...any) {
		if strings.HasPrefix(f, "appended") {
			cancel()
		}
	}}
	res, err := ApplyRestore(ctx, cfg, tcfg, tgt, approve(t, prev), opts)
	if !errors.Is(err, context.Canceled) || res.Appended != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRestoreFiltersAndOptions(t *testing.T) {
	cfg := initRepo(t)
	src := fakeMailbox()
	runBackup(t, cfg, src)
	tcfg, tgt := restoreCfg(cfg), mailtest.NewTarget()

	if p := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{Folder: "Sent"}); p.Plan.Appends != 1 || p.Plan.Create[0] != "Restored/1/Sent" {
		t.Fatalf("--folder: %+v", p.Plan)
	}
	ls, err := Ls(context.Background(), cfg, "1", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if p := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{Message: ls.Messages[1].SHA256[:12]}); p.Plan.Appends != 1 || p.Plan.Folders[0].Name != "Restored/1/INBOX" {
		t.Fatalf("--message: %+v", p.Plan)
	}
	if p := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{Into: "Backup/{snapshot}/x"}); p.Plan.Create[0] != "Backup/1/x/INBOX" {
		t.Fatalf("--into: %v", p.Plan.Create)
	}

	for name, o := range map[string]RestoreOptions{
		"unknown folder":      {Folder: "Nope"},
		"short hash":          {Message: "abc"},
		"unknown hash":        {Message: "ffffffffff"},
		"in place and into":   {InPlace: true, Into: "X"},
		"snapshot and window": {Snapshot: "1", DeletedSince: time.Hour},
		"negative window":     {DeletedSince: -time.Hour},
		"unknown snapshot":    {Snapshot: "9"},
		"empty prefix":        {Into: "/"},
	} {
		if _, err := PlanRestore(context.Background(), cfg, tcfg, tgt, o); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if len(tgt.Writes()) != 0 {
		t.Fatal("planning wrote")
	}
}

func TestRestoreRefusesANoselectTarget(t *testing.T) {
	cfg := initRepo(t)
	runBackup(t, cfg, fakeMailbox())
	tgt := mailtest.NewTarget()
	tgt.NoSel["Restored/1/INBOX"] = true
	if _, err := PlanRestore(context.Background(), cfg, restoreCfg(cfg), tgt, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "Noselect") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreToADifferentAccountIsFlagged(t *testing.T) {
	cfg := initRepo(t)
	runBackup(t, cfg, fakeMailbox())
	same := mustPlan(t, cfg, restoreCfg(cfg), mailtest.NewTarget(), RestoreOptions{})
	if same.DifferentAccount {
		t.Fatal("same account flagged")
	}
	other := restoreCfg(cfg)
	other.IMAP.Username = "new@example.org"
	prev := mustPlan(t, cfg, other, mailtest.NewTarget(), RestoreOptions{})
	if !prev.DifferentAccount || prev.TargetAccount != "new@example.org@imap.example.com" {
		t.Fatalf("preview = %+v", prev)
	}
	if prev.Token == same.Token {
		t.Fatal("the target is not part of the token")
	}
}

func TestRestoreSnapshotByTime(t *testing.T) {
	cfg := initRepo(t)
	src := fakeMailbox()
	runBackup(t, cfg, src)
	mid := time.Now()
	time.Sleep(1100 * time.Millisecond) // snapshot times have one-second resolution
	src.Folders[0].Delete(1)
	runBackup(t, cfg, src)

	tcfg, tgt := restoreCfg(cfg), mailtest.NewTarget()
	p := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{Snapshot: mid.Format("2006-01-02T15:04:05")})
	if p.Plan.Source != "snapshot 1" || p.Plan.Appends != 3 {
		t.Fatalf("at a time between the runs: %+v", p.Plan)
	}
	if _, err := PlanRestore(context.Background(), cfg, tcfg, tgt, RestoreOptions{Snapshot: "2001-01-01"}); err == nil {
		t.Fatal("a time before every snapshot must fail")
	}
}

func TestEncryptedRestoreNeedsTheKey(t *testing.T) {
	cfg, keys := initEncrypted(t)
	src := fakeMailbox()
	runBackup(t, cfg, src)
	tcfg := restoreCfg(cfg)
	tgt := mailtest.NewTarget()

	if _, err := PlanRestore(context.Background(), cfg, tcfg, tgt, RestoreOptions{}); !errors.Is(err, config.ErrNoIdentity) {
		t.Fatalf("without a key: %v", err)
	}
	cfg.Encryption.Identity, tcfg.Encryption.Identity = keys.identity, keys.identity
	prev := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{})
	if _, err := ApplyRestore(context.Background(), cfg, tcfg, tgt, approve(t, prev), RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := tgt.Folders["Restored/1/INBOX"]; len(got) != 2 || string(got[0].Raw) != string(src.Folders[0].Messages[0].Raw) {
		t.Fatalf("restored = %+v", got)
	}
}

func TestRestoreNeverSendsDeletedFlag(t *testing.T) {
	cfg := initRepo(t)
	src := mailtest.New()
	src.AddFolder("INBOX").Add(msg("1", "s", "b"), `\Seen`, `\Deleted`)
	runBackup(t, cfg, src)
	tcfg, tgt := restoreCfg(cfg), mailtest.NewTarget()
	prev := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{})
	if prev.Plan.DeletedFlagDropped != 1 {
		t.Fatalf("DeletedFlagDropped = %d", prev.Plan.DeletedFlagDropped)
	}
	if _, err := ApplyRestore(context.Background(), cfg, tcfg, tgt, approve(t, prev), RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, f := range tgt.Folders["Restored/1/INBOX"][0].Flags {
		if f == `\Deleted` {
			t.Fatal(`\Deleted was appended`)
		}
	}
}

// The cap is a second check on top of the token: a plan over it is previewed
// and flagged but never applied, whatever approved it.
func TestRestoreMessageCap(t *testing.T) {
	cfg := initRepo(t)
	runBackup(t, cfg, fakeMailbox()) // 3 messages
	tcfg := restoreCfg(cfg)

	over := mustPlan(t, cfg, tcfg, mailtest.NewTarget(), RestoreOptions{MaxMessages: 2})
	if !over.OverLimit || over.MaxMessages != 2 || over.Plan.Appends != 3 {
		t.Fatalf("preview = %+v", over)
	}
	tgt := mailtest.NewTarget()
	_, err := ApplyRestore(context.Background(), cfg, tcfg, tgt, approve(t, over), RestoreOptions{MaxMessages: 2})
	if !errors.Is(err, ErrOverLimit) || len(tgt.Writes()) != 0 {
		t.Fatalf("err = %v, writes = %v", err, tgt.Writes())
	}

	for _, max := range []int{3, 0} { // exactly at the cap, and no cap
		tgt := mailtest.NewTarget()
		prev := mustPlan(t, cfg, tcfg, tgt, RestoreOptions{MaxMessages: max})
		if prev.OverLimit {
			t.Fatalf("max %d: flagged over the limit", max)
		}
		if res, err := ApplyRestore(context.Background(), cfg, tcfg, tgt, approve(t, prev), RestoreOptions{MaxMessages: max}); err != nil || res.Appended != 3 {
			t.Fatalf("max %d: %+v %v", max, res, err)
		}
	}
	if _, err := PlanRestore(context.Background(), cfg, tcfg, mailtest.NewTarget(), RestoreOptions{MaxMessages: -1}); err == nil {
		t.Fatal("a negative cap must be refused")
	}
}
