package engine

import (
	"context"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/mail/mailtest"
)

// collect runs a backup and returns every progress event in order.
func collect(t *testing.T, cfg *config.Config, f *mailtest.Fake) ([]ProgressEvent, *BackupReport, error) {
	t.Helper()
	var evs []ProgressEvent
	rep, err := Backup(context.Background(), cfg, f, BackupOptions{Progress: func(e ProgressEvent) { evs = append(evs, e) }})
	return evs, rep, err
}

func kindsOf(evs []ProgressEvent) []ProgressKind {
	out := make([]ProgressKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func TestProgressEventsForAFirstBackup(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	evs, _, err := collect(t, cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	want := []ProgressKind{
		ProgressRunStart,
		ProgressFolderStart, ProgressFolderFetch, ProgressMessageDone, ProgressMessageDone, ProgressFolderDone, // INBOX
		ProgressFolderStart, ProgressFolderFetch, ProgressMessageDone, ProgressFolderDone, // Sent
	}
	got := kindsOf(evs)
	if len(got) != len(want) {
		t.Fatalf("kinds %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds %v, want %v", got, want)
		}
	}
	if evs[0].Folders != 2 {
		t.Errorf("RunStart folders = %d, want 2", evs[0].Folders)
	}
	inbox := f.Folders[0].Messages
	wantBytes := int64(len(inbox[0].Raw) + len(inbox[1].Raw))

	start, fetch := evs[1], evs[2]
	if start.Folder != "INBOX" || start.Index != 1 || start.Folders != 2 {
		t.Errorf("FolderStart %+v", start)
	}
	if fetch.Folder != "INBOX" || fetch.Index != 1 || fetch.Messages != 2 || fetch.Bytes != wantBytes {
		t.Errorf("FolderFetch %+v, want 2 messages and %d bytes", fetch, wantBytes)
	}
	if evs[3].Bytes != int64(len(inbox[0].Raw)) || evs[4].Bytes != int64(len(inbox[1].Raw)) {
		t.Errorf("MessageDone bytes %d, %d", evs[3].Bytes, evs[4].Bytes)
	}
	done := evs[5]
	if done.Report == nil || done.Report.Name != "INBOX" || done.Report.Mode != "full" || done.Report.New != 2 {
		t.Errorf("FolderDone %+v", done.Report)
	}
	if evs[6].Folder != "Sent" || evs[6].Index != 2 {
		t.Errorf("second FolderStart %+v", evs[6])
	}
}

func TestProgressUnchangedFolderHasNoFetchEvents(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	evs, _, err := collect(t, cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == ProgressFolderFetch || e.Kind == ProgressMessageDone {
			t.Fatalf("unchanged run emitted %v", kindsOf(evs))
		}
	}
	var done int
	for _, e := range evs {
		if e.Kind == ProgressFolderDone {
			done++
			if e.Report.Mode != "unchanged" {
				t.Errorf("mode %q, want unchanged", e.Report.Mode)
			}
		}
	}
	if done != 2 {
		t.Errorf("FolderDone events = %d, want 2", done)
	}
}

func TestProgressEmailIDSkipCountsAsDoneWithoutBytes(t *testing.T) {
	cfg := initRepo(t)
	f := mailtest.New()
	a := f.AddFolder("A")
	raw := msg("m", "moved", "body")
	a.AddWithEmailID(raw, "E1")
	runBackup(t, cfg, f)

	// The same message shows up in B under a new UID: its body is not downloaded.
	b := f.AddFolder("B")
	b.AddWithEmailID(raw, "E1")
	evs, rep, err := collect(t, cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SkippedByEmailID != 1 {
		t.Fatalf("report %+v", rep)
	}
	var fetch, mdone *ProgressEvent
	for i := range evs {
		switch {
		case evs[i].Kind == ProgressFolderFetch && evs[i].Folder == "B":
			fetch = &evs[i]
		case evs[i].Kind == ProgressMessageDone && evs[i].Folder == "B":
			mdone = &evs[i]
		}
	}
	if fetch == nil || mdone == nil {
		t.Fatalf("missing events: %v", kindsOf(evs))
	}
	if fetch.Messages != 1 || fetch.Bytes != 0 {
		t.Errorf("FolderFetch %+v: want 1 message to account for and 0 bytes to download", fetch)
	}
	if mdone.Bytes != 0 {
		t.Errorf("EMAILID skip reported %d bytes", mdone.Bytes)
	}
}

func TestProgressFailureMidFolderHasNoFolderDone(t *testing.T) {
	cfg := initRepo(t)
	f := mailtest.New()
	f.AddFolder("INBOX").Add(msg("1", "one", "x"))
	sent := f.AddFolder("Sent")
	sent.Add(msg("2", "two", "y"))
	sent.Add(msg("3", "three", "z"))
	f.FailAfter = map[string]int{"Sent": 1}
	// FetchBodies delivers one body, then fails like a dropped connection.
	evs, rep, err := collect(t, cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Partial() {
		t.Fatalf("status %q, want partial", rep.Status)
	}
	var sentDone, sentMsgs int
	for _, e := range evs {
		if e.Folder != "Sent" {
			continue
		}
		switch e.Kind {
		case ProgressFolderDone:
			sentDone++
		case ProgressMessageDone:
			sentMsgs++
		}
	}
	if sentDone != 0 {
		t.Errorf("a folder that failed emitted FolderDone")
	}
	if sentMsgs != 1 {
		t.Errorf("MessageDone before the failure = %d, want 1", sentMsgs)
	}
}

func TestProgressNilCallbackIsFine(t *testing.T) {
	cfg := initRepo(t)
	runBackup(t, cfg, fakeMailbox()) // BackupOptions{} has no Progress
}
