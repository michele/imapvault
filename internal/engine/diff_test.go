package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/seal"
)

func TestDiffCountsEveryKind(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	first := runBackup(t, cfg, f)

	inbox, sent := f.Folders[0], f.Folders[1]
	inbox.Add(msg("4", "Fresh unique-subject-4", "body four")) // added
	inbox.SetFlags(1, `\Seen`)                                 // reflagged
	inbox.Delete(2)                                            // deleted
	moved := string(sent.Messages[0].Raw)
	sent.Delete(1)                             // moved out of Sent...
	f.AddFolder("Archive").Add(moved, `\Seen`) // ...and into a new folder
	second := runBackup(t, cfg, f)

	res, err := Diff(context.Background(), cfg, "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	if res.From != first.Snapshot || res.To != second.Snapshot || res.FromTime.IsZero() || res.ToTime.IsZero() {
		t.Fatalf("result header %+v", res)
	}
	got := map[string]DiffCounts{}
	for _, fd := range res.Folders {
		got[fd.Name] = fd.DiffCounts
	}
	want := map[string]DiffCounts{
		"INBOX":   {Added: 1, Deleted: 1, Reflagged: 1},
		"Sent":    {MovedOut: 1},
		"Archive": {MovedIn: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("folders %+v\nwant %+v", got, want)
	}
	if res.Totals != (DiffCounts{Added: 1, Deleted: 1, MovedIn: 1, MovedOut: 1, Reflagged: 1}) {
		t.Fatalf("totals %+v", res.Totals)
	}
	if res.Folders[0].Name != "Archive" || res.Folders[2].Name != "Sent" {
		t.Fatalf("folders are not sorted by name: %+v", res.Folders)
	}
	// the same pair through the other spellings
	for _, pair := range [][2]string{{"latest~1", "latest"}, {"1", "latest"}, {"latest~1", "2"}} {
		again, err := Diff(context.Background(), cfg, pair[0], pair[1])
		if err != nil || !reflect.DeepEqual(again.Folders, res.Folders) {
			t.Fatalf("%v: %+v err %v", pair, again, err)
		}
	}
}

func TestDiffUIDValidityResetIsRenumbered(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	inbox := f.Folders[0]
	raws := []string{string(inbox.Messages[0].Raw), string(inbox.Messages[1].Raw)}
	inbox.Recreate()
	inbox.Add(raws[0])
	inbox.Add(raws[1])
	runBackup(t, cfg, f)
	res, err := Diff(context.Background(), cfg, "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Folders) != 1 || res.Folders[0].Name != "INBOX" || res.Folders[0].DiffCounts != (DiffCounts{Renumbered: 2}) {
		t.Fatalf("%+v", res.Folders)
	}
}

func TestDiffFolderRenameIsMovesOutAndIn(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	f.RenameFolder("Sent", "Sent Items")
	runBackup(t, cfg, f)
	res, err := Diff(context.Background(), cfg, "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]DiffCounts{}
	for _, fd := range res.Folders {
		got[fd.Name] = fd.DiffCounts
	}
	if !reflect.DeepEqual(got, map[string]DiffCounts{"Sent": {MovedOut: 1}, "Sent Items": {MovedIn: 1}}) {
		t.Fatalf("%+v", got)
	}
}

func TestDiffOfIdenticalSnapshotsIsEmpty(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	runBackup(t, cfg, f)
	res, err := Diff(context.Background(), cfg, "1", "2")
	if err != nil || !res.Empty() || res.Folders == nil || res.Totals != (DiffCounts{}) {
		t.Fatalf("%+v err %v", res, err)
	}
}

func TestDiffRefusesBadSnapshotSpecs(t *testing.T) {
	cfg := initRepo(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	runBackup(t, cfg, f)
	for _, c := range []struct {
		from, to, want string
	}{
		{"2", "1", "older"},
		{"1", "1", "older"},
		{"latest", "latest", "older"},
		{"latest~5", "latest", "does not exist"},
		{"latest~x", "latest", "latest~N"},
		{"latest~-1", "latest", "latest~N"},
		{"0", "latest", "snapshot number"},
		{"1", "99", "does not exist"},
	} {
		if _, err := Diff(context.Background(), cfg, c.from, c.to); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("diff %s %s: err %v, want it to mention %q", c.from, c.to, err, c.want)
		}
	}
	empty := initRepo(t)
	if _, err := Diff(context.Background(), empty, "1", "latest"); err == nil {
		t.Fatal("diff on a repository with no snapshots succeeded")
	}
}

// An encrypted repository: the backup machine (no private key) still gets the
// counts, with folders shown by number; with the key it gets the names.
func TestDiffInAnEncryptedRepository(t *testing.T) {
	cfg, keys := initEncrypted(t)
	f := fakeMailbox()
	runBackup(t, cfg, f)
	f.Folders[0].Add(msg("4", "Fresh unique-subject-4", "body four"))
	runBackup(t, cfg, f)

	res, err := Diff(context.Background(), cfg, "1", "2")
	if err != nil {
		t.Fatalf("diff without a key: %v", err)
	}
	if len(res.Folders) != 1 || !strings.HasPrefix(res.Folders[0].Name, "#") || res.Folders[0].Added != 1 {
		t.Fatalf("%+v", res.Folders)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "no private key") {
		t.Fatalf("warnings %v", res.Warnings)
	}
	for _, secret := range []string{"INBOX", "Sent"} {
		if strings.Contains(res.Folders[0].Name, secret) {
			t.Fatal("a folder name leaked without the key")
		}
	}

	cfg.Encryption.Identity = keys.identity
	res, err = Diff(context.Background(), cfg, "1", "2")
	if err != nil || len(res.Folders) != 1 || res.Folders[0].Name != "INBOX" || len(res.Warnings) != 0 {
		t.Fatalf("with the key: %+v err %v", res, err)
	}

	cfg.Encryption.Identity = newEncKeys(t).identity
	if _, err := Diff(context.Background(), cfg, "1", "2"); err == nil || !strings.Contains(err.Error(), seal.ErrWrongIdentity.Error()) {
		t.Fatalf("with the wrong key: %v", err)
	}
}
