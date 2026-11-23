package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/mail"
	"github.com/michele/imapvault/internal/mail/mailtest"
	"github.com/michele/imapvault/internal/objects"
	"github.com/michele/imapvault/internal/plan"
	"github.com/michele/imapvault/internal/repo"
)

// putOrphan writes a blob object and no index row: what a run killed between
// the two leaves behind.
func putOrphan(t *testing.T, cfg *config.Config, raw string) {
	t.Helper()
	r, err := repo.Open(cfg.Repository.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := objects.Open(r.ObjectsDir(), r.TmpDir(), objects.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, _, _, err := st.Put([]byte(raw)); err != nil {
		t.Fatal(err)
	}
}

// serverView is the fake server's state in the format snapshotOf reads back.
func serverView(f *mailtest.Fake) map[string][]string {
	out := map[string][]string{}
	for _, fo := range f.Folders {
		if fo.NoSelect {
			continue
		}
		msgs := slices.Clone(fo.Messages)
		slices.SortFunc(msgs, func(a, b mailtest.Message) int { return int(a.UID) - int(b.UID) })
		list := []string{}
		for _, m := range msgs {
			list = append(list, fmt.Sprintf("%d [%s] %s", m.UID, plan.NormalizeFlags(m.Flags), sum4(string(m.Raw))))
		}
		out[fo.Name] = list
	}
	return out
}

func requireMatchesServer(t *testing.T, cfg *config.Config, rep *BackupReport, f *mailtest.Fake) {
	t.Helper()
	if got, want := snapshotOf(t, cfg, rep.Snapshot, ""), serverView(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot %d does not match the server\n got %v\nwant %v", rep.Snapshot, got, want)
	}
}

func modeOf(t *testing.T, rep *BackupReport, folder string) string {
	t.Helper()
	for _, fr := range rep.Folders {
		if fr.Name == folder {
			return fr.Mode
		}
	}
	t.Fatalf("folder %q not in the report %+v", folder, rep.Folders)
	return ""
}

func hasCall(f *mailtest.Fake, prefix string) bool { return f.Count(prefix) > 0 }

func TestFirstRunIsFullAndSecondReadsNothingButExamine(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	first := runBackup(t, cfg, f)
	for _, fr := range first.Folders {
		if fr.Mode != "full" {
			t.Fatalf("first run, %s: mode %s", fr.Name, fr.Mode)
		}
	}
	f.Reset()
	second := runBackup(t, cfg, f)
	for _, fr := range second.Folders {
		if fr.Mode != "unchanged" {
			t.Fatalf("second run, %s: mode %s", fr.Name, fr.Mode)
		}
	}
	for _, c := range f.Calls {
		name, _, _ := strings.Cut(c, " ")
		if name != "capabilities" && name != "list" && name != "examine" && name != "close" {
			t.Fatalf("an unchanged run made call %q", c)
		}
	}
	if second.Messages != 3 || second.Fetched != 0 || second.Status != "complete" {
		t.Fatalf("second %+v", second)
	}
	requireMatchesServer(t, cfg, second, f)
}

func TestNewMailIsFetchedIncrementally(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	inbox := f.Folders[0]
	inbox.Add(msg("4", "Four unique-subject-4", "body four"))
	inbox.Add(msg("5", "Five unique-subject-5", "body five"), `\Seen`)
	f.Reset()
	rep := runBackup(t, cfg, f)
	if modeOf(t, rep, "INBOX") != "incremental" || modeOf(t, rep, "Sent") != "unchanged" {
		t.Fatalf("modes: %+v", rep.Folders)
	}
	if rep.New != 2 || rep.Fetched != 2 || rep.Closed != 0 || rep.Messages != 5 {
		t.Fatalf("report %+v", rep)
	}
	if !hasCall(f, "list-messages INBOX from 3") || hasCall(f, "list-messages INBOX from 1") {
		t.Fatalf("an incremental run listed more than the new range: %v", f.Calls)
	}
	if hasCall(f, "uids") {
		t.Fatalf("nothing was deleted, so no UID list was needed: %v", f.Calls)
	}
	requireMatchesServer(t, cfg, rep, f)
}

func TestFlagChangeIsFoundWithChangedSince(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	f.Folders[0].SetFlags(1, `\Seen`, `\Flagged`)
	f.Reset()
	rep := runBackup(t, cfg, f)
	if modeOf(t, rep, "INBOX") != "incremental" || rep.Reflagged != 1 || rep.Fetched != 0 || rep.New != 0 {
		t.Fatalf("report %+v", rep)
	}
	if !hasCall(f, "changed-flags INBOX upto 2") || hasCall(f, "uids") || hasCall(f, "fetch-bodies") {
		t.Fatalf("calls %v", f.Calls)
	}
	requireMatchesServer(t, cfg, rep, f)
}

// Plain CONDSTORE does not promise to bump HIGHESTMODSEQ on EXPUNGE; the message
// count gives the expunge away either way, and only then is the UID list asked for.
func TestExpungeIsFoundByCountingAndOnlyThenListsUIDs(t *testing.T) {
	for _, bump := range []bool{false, true} {
		t.Run(fmt.Sprintf("bump=%v", bump), func(t *testing.T) {
			cfg := initRepo(t)
			f := fakeMailbox()
			runBackup(t, cfg, f)
			f.Folders[0].Delete(2)
			if bump {
				f.Folders[0].BumpOnExpunge()
			}
			f.Reset()
			rep := runBackup(t, cfg, f)
			if modeOf(t, rep, "INBOX") != "incremental" || rep.Closed != 1 || rep.Fetched != 0 || rep.Messages != 2 {
				t.Fatalf("report %+v", rep)
			}
			if !hasCall(f, "uids INBOX upto 2") {
				t.Fatalf("no UID list: %v", f.Calls)
			}
			requireMatchesServer(t, cfg, rep, f)
		})
	}
}

func TestExpungeAndNewMailAndFlagTogether(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	inbox := f.Folders[0]
	inbox.Delete(1)
	inbox.SetFlags(2, `\Answered`)
	inbox.Add(msg("9", "Nine unique-subject-9", "body nine"))
	rep := runBackup(t, cfg, f)
	if rep.New != 1 || rep.Closed != 1 || rep.Reflagged != 1 || modeOf(t, rep, "INBOX") != "incremental" {
		t.Fatalf("report %+v", rep)
	}
	requireMatchesServer(t, cfg, rep, f)
}

func TestFlagChangeOnAMessageThatIsThenExpunged(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	inbox := f.Folders[0]
	inbox.SetFlags(1, `\Seen`)
	inbox.SetFlags(2, `\Seen`)
	inbox.Delete(2)
	rep := runBackup(t, cfg, f)
	if rep.Closed != 1 || rep.Reflagged != 1 {
		t.Fatalf("report %+v", rep)
	}
	requireMatchesServer(t, cfg, rep, f)
}

func TestWithoutCondstoreEveryRunIsFull(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	f.Caps = mail.Capabilities{"UIDPLUS"}
	runBackup(t, cfg, f)
	f.Folders[0].Add(msg("4", "Four unique-subject-4", "body four"))
	f.Folders[0].SetFlags(1, `\Seen`)
	f.Reset()
	rep := runBackup(t, cfg, f)
	for _, fr := range rep.Folders {
		if fr.Mode != "full" {
			t.Fatalf("%s: %s", fr.Name, fr.Mode)
		}
	}
	if hasCall(f, "changed-flags") || hasCall(f, "uids") {
		t.Fatalf("CONDSTORE calls on a server without it: %v", f.Calls)
	}
	if rep.New != 1 || rep.Reflagged != 1 {
		t.Fatalf("report %+v", rep)
	}
	requireMatchesServer(t, cfg, rep, f)
}

func TestFullOptionForcesARescan(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	f.Reset()
	rep, err := Backup(context.Background(), cfg, f, BackupOptions{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, fr := range rep.Folders {
		if fr.Mode != "full" {
			t.Fatalf("%s: %s", fr.Name, fr.Mode)
		}
	}
	if !hasCall(f, "list-messages INBOX from 1") || rep.Fetched != 0 {
		t.Fatalf("calls %v report %+v", f.Calls, rep)
	}
	requireMatchesServer(t, cfg, rep, f)
}

// A server whose HIGHESTMODSEQ or UIDNEXT moves backwards was restored or is
// broken: nothing it says about changes can be trusted.
func TestServerGoingBackwardsForcesAFullRescan(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	f.Folders[0].ModSeq = 1
	rep := runBackup(t, cfg, f)
	if modeOf(t, rep, "INBOX") != "full" {
		t.Fatalf("mode %s", modeOf(t, rep, "INBOX"))
	}
}

func TestUIDValidityResetClosesTheOldGenerationAndReadsTheNew(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	first := runBackup(t, cfg, f)
	inbox := f.Folders[0]
	same := string(inbox.Messages[0].Raw)
	inbox.Recreate()
	inbox.Add(same) // the same message, now UID 1 of generation 2
	inbox.Add(msg("8", "Eight unique-subject-8", "body eight"))
	rep := runBackup(t, cfg, f)
	if modeOf(t, rep, "INBOX") != "full" || rep.Closed != 2 || rep.New != 2 {
		t.Fatalf("report %+v", rep)
	}
	if rep.BlobsAdded != 1 || rep.BlobsReused != 1 {
		t.Fatalf("the unchanged message must reuse its blob, the new one adds one: %+v", rep)
	}
	requireMatchesServer(t, cfg, rep, f)
	if got := snapshotOf(t, cfg, first.Snapshot, "")["INBOX"]; len(got) != 2 {
		t.Fatalf("snapshot 1 changed: %v", got)
	}
	// and the reset is not repeated: the next run sees an unchanged folder.
	again := runBackup(t, cfg, f)
	if modeOf(t, again, "INBOX") != "unchanged" || again.Closed != 0 {
		t.Fatalf("again %+v", again)
	}
}

// A reset that a failed run left half done: some entries of the new generation
// are open next to all of the old one. The next run finishes it without
// fetching again what was stored.
func TestHalfDoneUIDValidityResetResumes(t *testing.T) {
	old := commitEvery
	commitEvery = 1
	t.Cleanup(func() { commitEvery = old })

	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	inbox := f.Folders[0]
	inbox.Recreate()
	for i := 0; i < 4; i++ {
		inbox.Add(msg(fmt.Sprint(20+i), fmt.Sprintf("Gen2 unique-subject-%d", 20+i), "gen two"))
	}
	f.FailAfter = map[string]int{"INBOX": 2}
	if _, err := Backup(context.Background(), cfg, f, BackupOptions{}); err == nil {
		t.Fatal("expected the injected failure")
	}
	if n := snapshotsCount(t, cfg); n != 1 {
		t.Fatalf("%d snapshots after the failed run", n)
	}
	if chk, _ := Check(context.Background(), cfg, CheckOptions{}); !chk.OK() || len(chk.Warnings) != 0 {
		t.Fatalf("check after the failed run: %+v", chk)
	}

	f.FailAfter = nil
	f.Reset()
	rep := runBackup(t, cfg, f)
	if rep.Fetched != 2 || rep.Closed != 2 || modeOf(t, rep, "INBOX") != "full" {
		t.Fatalf("resumed run %+v (only the 2 missing bodies may be fetched)", rep)
	}
	requireMatchesServer(t, cfg, rep, f)
}

// When the cheap view does not add up (here a message arrives between EXAMINE
// and the listing) the run falls back to a full rescan before committing anything.
func TestInconsistentIncrementalViewFallsBackToAFullRescan(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	inbox := f.Folders[0]
	inbox.Add(msg("3", "Three unique-subject-3", "body three"))
	armed := true
	f.FailAt = func(call string) error {
		if armed && call == "list-messages INBOX from 3" {
			armed = false
			inbox.Add(msg("4", "Four unique-subject-4", "arrived mid-run"))
		}
		return nil
	}
	rep := runBackup(t, cfg, f)
	if modeOf(t, rep, "INBOX") != "full" || rep.New != 2 {
		t.Fatalf("report %+v", rep)
	}
	requireMatchesServer(t, cfg, rep, f)
	f.FailAt = nil
	again := runBackup(t, cfg, f)
	requireMatchesServer(t, cfg, again, f)
	if again.New != 0 || again.Closed != 0 || again.Reflagged != 0 {
		t.Fatalf("the next run should find nothing new: %+v", again)
	}
}

func TestEmailIDLetsAMovedMessageSkipItsDownload(t *testing.T) {
	cfg := initRepo(t)
	f := mailtest.New()
	a, b := f.AddFolder("A"), f.AddFolder("B")
	raw := msg("1", "Moves unique-subject-1", "same bytes")
	a.AddWithEmailID(raw, "E1")
	runBackup(t, cfg, f)

	// a server-side move: new UID in B, same EMAILID, gone from A.
	b.AddWithEmailID(raw, "E1")
	a.Delete(1)
	f.Reset()
	rep := runBackup(t, cfg, f)
	if rep.SkippedByEmailID != 1 || rep.Fetched != 0 || rep.BlobsAdded != 0 || rep.New != 1 || rep.Closed != 1 {
		t.Fatalf("report %+v", rep)
	}
	if hasCall(f, "fetch-bodies") {
		t.Fatalf("a body was downloaded: %v", f.Calls)
	}
	requireMatchesServer(t, cfg, rep, f)
	if chk, _ := Check(context.Background(), cfg, CheckOptions{ReadData: true}); !chk.OK() {
		t.Fatalf("check: %+v", chk)
	}

	// EMAILID is a hint, never the key: the same id with a different size is fetched.
	b.AddWithEmailID(raw+"tampered\r\n", "E1")
	f.Reset()
	rep = runBackup(t, cfg, f)
	if rep.SkippedByEmailID != 0 || rep.Fetched != 1 || rep.BlobsAdded != 1 {
		t.Fatalf("report %+v", rep)
	}
	requireMatchesServer(t, cfg, rep, f)
}

func TestRunStoppedAfterSomeFoldersIsPartial(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	f.AddFolder("Archive").Add(msg("5", "Old unique-subject-5", "archived"))
	runBackup(t, cfg, f)

	f.Folders[2].Add(msg("6", "Archived two unique-subject-6", "more"))       // Archive: backed up first
	f.Folders[0].Add(msg("7", "Inbox mail unique-subject-7", "inbox"))        // INBOX: fails
	f.Folders[1].Add(msg("8", "Sent mail unique-subject-8", "never reached")) // Sent: never reached
	f.FailAt = func(call string) error {
		if strings.HasPrefix(call, "fetch-bodies INBOX") {
			return errors.New("connection reset")
		}
		return nil
	}
	rep, err := Backup(context.Background(), cfg, f, BackupOptions{})
	if err != nil {
		t.Fatalf("a run that backed up some folders must end partial, not fail: %v", err)
	}
	if !rep.Partial() || rep.Status != "partial" || rep.Snapshot == 0 {
		t.Fatalf("report %+v", rep)
	}
	if len(rep.Warnings) == 0 || !strings.Contains(strings.Join(rep.Warnings, "\n"), "connection reset") {
		t.Fatalf("warnings %v", rep.Warnings)
	}
	snap := snapshotOf(t, cfg, rep.Snapshot, "")
	if len(snap["Archive"]) != 2 || len(snap["INBOX"]) != 2 || len(snap["Sent"]) != 1 {
		t.Fatalf("a partial snapshot holds what was verified and keeps the rest as it was: %v", snap)
	}
	snaps := statusOf(t, cfg)
	if snaps[rep.Snapshot] != "partial" {
		t.Fatalf("status %v", snaps)
	}

	// the next run finishes the job.
	f.FailAt = nil
	f.Reset()
	next := runBackup(t, cfg, f)
	if next.Status != "complete" || modeOf(t, next, "Archive") != "unchanged" || modeOf(t, next, "INBOX") != "incremental" {
		t.Fatalf("next %+v", next)
	}
	requireMatchesServer(t, cfg, next, f)
}

func statusOf(t *testing.T, cfg *config.Config) map[int64]string {
	t.Helper()
	acct := accountOf(cfg)
	r, err := repo.Open(cfg.Repository.Path, &acct)
	if err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(r.IndexPath(), index.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	snaps, err := db.Snapshots()
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]string{}
	for _, s := range snaps {
		out[s.ID] = s.Status
	}
	return out
}

func TestPartialRunNeverClosesFoldersThatLookGone(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	f.AddFolder("Archive").Add(msg("5", "Old unique-subject-5", "archived"))
	runBackup(t, cfg, f)

	f.RemoveFolder("Sent")
	f.Folders[0].Add(msg("7", "Inbox mail unique-subject-7", "inbox"))
	f.FailAt = func(call string) error {
		if strings.HasPrefix(call, "fetch-bodies INBOX") {
			return errors.New("connection reset")
		}
		return nil
	}
	rep, err := Backup(context.Background(), cfg, f, BackupOptions{})
	if err != nil || !rep.Partial() || rep.GoneFolders != 0 {
		t.Fatalf("report %+v err %v", rep, err)
	}
	if got := snapshotOf(t, cfg, rep.Snapshot, "")["Sent"]; len(got) != 1 {
		t.Fatalf("a partial run closed a folder it could not verify: %v", got)
	}

	f.FailAt = nil
	done := runBackup(t, cfg, f)
	if done.Status != "complete" || done.GoneFolders != 1 {
		t.Fatalf("done %+v", done)
	}
	requireMatchesServer(t, cfg, done, f)
}

func TestFolderThatVanishesBetweenListAndExamineIsSkipped(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	f.FailAt = func(call string) error {
		if call == "examine Sent" {
			f.RemoveFolder("Sent") // deleted by someone else just now
		}
		return nil
	}
	rep, err := Backup(context.Background(), cfg, f, BackupOptions{})
	if err != nil || !rep.Partial() {
		t.Fatalf("report %+v err %v", rep, err)
	}
	if !strings.Contains(strings.Join(rep.Warnings, "\n"), `"Sent"`) {
		t.Fatalf("warnings %v", rep.Warnings)
	}
	if got := snapshotOf(t, cfg, rep.Snapshot, "")["Sent"]; len(got) != 1 {
		t.Fatalf("Sent in the partial snapshot: %v", got)
	}
	f.FailAt = nil
	next := runBackup(t, cfg, f)
	if next.Status != "complete" || next.GoneFolders != 1 {
		t.Fatalf("next %+v", next)
	}
	requireMatchesServer(t, cfg, next, f)
}

func TestInterruptingTheRunMakesItPartial(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.FailAt = func(call string) error {
		if call == "examine Sent" {
			cancel() // Ctrl-C after INBOX was committed
		}
		return nil
	}
	rep, err := Backup(ctx, cfg, f, BackupOptions{})
	if err != nil || !rep.Partial() {
		t.Fatalf("report %+v err %v", rep, err)
	}
	if got := snapshotOf(t, cfg, rep.Snapshot, ""); len(got["INBOX"]) != 2 {
		t.Fatalf("INBOX was committed before the interrupt: %v", got)
	}
	f.FailAt = nil
	next := runBackup(t, cfg, f)
	requireMatchesServer(t, cfg, next, f)
}

func TestRunFailsWhenNoFolderCouldBeRead(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	f.FailAt = func(call string) error {
		if strings.HasPrefix(call, "examine") {
			return errors.New("server unavailable")
		}
		return nil
	}
	if _, err := Backup(context.Background(), cfg, f, BackupOptions{}); err == nil {
		t.Fatal("a run that read nothing must fail")
	}
	if n := snapshotsCount(t, cfg); n != 0 {
		t.Fatalf("%d snapshots", n)
	}
	// a server that refuses every folder is not a partial success either
	f.FailAt = func(call string) error {
		if strings.HasPrefix(call, "examine") {
			return mail.ErrFolderGone
		}
		return nil
	}
	if _, err := Backup(context.Background(), cfg, f, BackupOptions{}); err == nil {
		t.Fatal("expected an error when every folder was refused")
	}
}

// Whatever happens on the server between runs, and whichever way a run is
// interrupted, the snapshot of a completed run is exactly the server's state.
func TestRandomHistoryAlwaysConvergesToTheServer(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		for _, condstore := range []bool{true, false} {
			t.Run(fmt.Sprintf("seed%d/condstore=%v", seed, condstore), func(t *testing.T) { randomHistory(t, seed, condstore) })
		}
	}
}

func randomHistory(t *testing.T, seed int64, condstore bool) {
	rng := rand.New(rand.NewSource(seed))
	old := commitEvery
	t.Cleanup(func() { commitEvery = old })

	cfg := initRepo(t)
	f := mailtest.New()
	if !condstore {
		f.Caps = mail.Capabilities{"UIDPLUS"}
	}
	counter := 0
	newMsg := func() string {
		counter++
		return msg(fmt.Sprint(counter), fmt.Sprintf("Random unique-subject-%d", counter), fmt.Sprintf("body %d", counter))
	}
	flagSets := [][]string{nil, {`\Seen`}, {`\Seen`, `\Flagged`}, {`\Answered`}, {`$Label`}, {`\Recent`, `\Seen`}}
	addFolder := func() *mailtest.Folder {
		counter++
		fo := f.AddFolder(fmt.Sprintf("F%d", counter))
		fo.UIDValidity = uint32(1000 + counter) // a new folder never reuses a UIDVALIDITY
		return fo
	}
	addFolder()
	addFolder()
	pick := func() *mailtest.Folder { return f.Folders[rng.Intn(len(f.Folders))] }

	for step := 0; step < 40; step++ {
		for k := rng.Intn(4) + 1; k > 0; k-- {
			fo := pick()
			switch rng.Intn(11) {
			case 0, 1, 2:
				fo.Add(newMsg(), flagSets[rng.Intn(len(flagSets))]...)
			case 3: // copy, sometimes a move: the same bytes in another folder
				if len(fo.Messages) > 0 {
					m := fo.Messages[rng.Intn(len(fo.Messages))]
					pick().Add(string(m.Raw), m.Flags...)
					if rng.Intn(2) == 0 {
						fo.Delete(m.UID)
					}
				}
			case 4, 5:
				if len(fo.Messages) > 0 {
					fo.SetFlags(fo.Messages[rng.Intn(len(fo.Messages))].UID, flagSets[rng.Intn(len(flagSets))]...)
				}
			case 6, 7:
				if len(fo.Messages) > 0 {
					fo.Delete(fo.Messages[rng.Intn(len(fo.Messages))].UID)
					if rng.Intn(2) == 0 {
						fo.BumpOnExpunge()
					}
				}
			case 8:
				if rng.Intn(3) == 0 {
					fo.Recreate()
				}
			case 9:
				if len(f.Folders) > 1 {
					f.RemoveFolder(fo.Name)
				} else {
					addFolder()
				}
			case 10:
				if rng.Intn(2) == 0 {
					addFolder()
				} else {
					counter++
					f.RenameFolder(fo.Name, fmt.Sprintf("R%d", counter))
				}
			}
		}
		commitEvery = []int{1, 2, 100}[rng.Intn(3)]
		if rng.Intn(4) == 0 {
			f.FailAfter = map[string]int{pick().Name: rng.Intn(3)}
		}
		opts := BackupOptions{Full: rng.Intn(8) == 0}
		rep, err := Backup(context.Background(), cfg, f, opts)
		f.FailAfter = nil
		for tries := 0; (err != nil || rep.Partial()) && tries < 3; tries++ {
			rep, err = Backup(context.Background(), cfg, f, opts)
		}
		if err != nil || rep.Partial() {
			t.Fatalf("step %d: run did not complete: %v %+v", step, err, rep)
		}
		requireMatchesServer(t, cfg, rep, f)
		if step%10 == 9 {
			if chk, err := Check(context.Background(), cfg, CheckOptions{ReadData: true}); err != nil || !chk.OK() {
				t.Fatalf("step %d: check %+v err %v", step, chk, err)
			}
		}
	}
}
