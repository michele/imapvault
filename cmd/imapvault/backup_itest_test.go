//go:build integration

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/michele/imapvault/internal/engine"
	"github.com/michele/imapvault/internal/plan"
)

// itestAccount writes an account config for the docker test server and returns
// the repository path. The password comes from IMAPVAULT_IMAP_PASSWORD.
func itestAccount(t *testing.T, extra string) string {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	repoPath := filepath.Join(t.TempDir(), "repo")
	dir := filepath.Join(cfgHome, "imapvault")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[imap]\nhost = \"127.0.0.1\"\nport = 1143\ntls = \"none\"\nusername = \"me@imapvault.test\"\n\n[repository]\npath = \"" + repoPath + "\"\n" + extra
	if err := os.WriteFile(filepath.Join(dir, "it.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return repoPath
}

// guardThresholdsOff is for tests that delete a large share of a tiny folder on
// purpose and are not about the guard: with both thresholds at 0 only the
// vanished-folder rule is left.
const guardThresholdsOff = "\n[guard]\nmax_deleted_percent = 0\nmax_deleted_count = 0\n"

func run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := execRoot(t, append([]string{"--account", "it"}, args...)...)
	if err != nil {
		t.Fatalf("imapvault %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// serverMsg is one message as the server reports it, read independently of imapvault.
type serverMsg struct {
	UID    uint32
	Flags  string // normalised like the index stores them
	Size   int64
	SHA256 string
	Raw    []byte
}

// serverState reads folder names -> messages with a separate client, EXAMINE only.
func serverState(t *testing.T, c *imapclient.Client, only ...string) map[string][]serverMsg {
	t.Helper()
	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]serverMsg{}
	for _, b := range boxes {
		if len(only) > 0 && !slices.Contains(only, b.Mailbox) {
			continue
		}
		sel, err := c.Select(b.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			t.Fatalf("examine %s: %v", b.Mailbox, err)
		}
		msgs := []serverMsg{}
		if sel.NumMessages > 0 {
			section := &imap.FetchItemBodySection{Peek: true}
			var all imap.UIDSet
			all.AddRange(1, 0)
			bufs, err := c.Fetch(all, &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
			if err != nil {
				t.Fatalf("fetch %s: %v", b.Mailbox, err)
			}
			for _, m := range bufs {
				raw := m.FindBodySection(section)
				var fl []string
				for _, f := range m.Flags {
					fl = append(fl, string(f))
				}
				sum := sha256.Sum256(raw)
				msgs = append(msgs, serverMsg{UID: uint32(m.UID), Flags: plan.NormalizeFlags(fl), Size: m.RFC822Size, SHA256: fmt.Sprintf("%x", sum), Raw: raw})
			}
			slices.SortFunc(msgs, func(a, b serverMsg) int { return int(a.UID) - int(b.UID) })
		}
		out[b.Mailbox] = msgs
	}
	return out
}

func lsJSON(t *testing.T, snap, folder string) *engine.LsResult {
	t.Helper()
	args := []string{"--json", "ls", snap}
	if folder != "" {
		args = append(args, folder)
	}
	var res engine.LsResult
	if err := json.Unmarshal([]byte(run(t, args...)), &res); err != nil {
		t.Fatal(err)
	}
	return &res
}

// requireLsMatches fails unless `ls` of the snapshot reproduces the server exactly:
// the same folders and counts, and per message UID, flags, size and SHA-256.
func requireLsMatches(t *testing.T, snap string, want map[string][]serverMsg) {
	t.Helper()
	folders := lsJSON(t, snap, "")
	if len(folders.Folders) != len(want) {
		t.Errorf("snapshot %s lists %d folders, the server has %d", snap, len(folders.Folders), len(want))
	}
	for _, f := range folders.Folders {
		if w, ok := want[f.Name]; !ok || f.Messages != len(w) {
			t.Errorf("folder %q: snapshot says %d messages, server %v", f.Name, f.Messages, len(w))
		}
	}
	for name, msgs := range want {
		got := lsJSON(t, snap, name).Messages
		if len(got) != len(msgs) {
			t.Errorf("%s: %d messages in the snapshot, %d on the server", name, len(got), len(msgs))
			continue
		}
		for i, m := range msgs {
			g := got[i]
			if g.UID != m.UID || g.Flags != m.Flags || g.Size != m.Size || g.SHA256 != m.SHA256 {
				t.Errorf("%s uid %d: snapshot {%d %q %d %s} server {%d %q %d %s}", name, m.UID, g.UID, g.Flags, g.Size, g.SHA256[:12], m.UID, m.Flags, m.Size, m.SHA256[:12])
			}
		}
	}
}

func TestBackupMatchesServer(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plain"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			c := dialTestServer(t)
			extra, identity := "", ""
			if encrypted {
				keyFile := filepath.Join(t.TempDir(), "it.key")
				out, err := execRoot(t, "keygen", "--out", keyFile)
				if err != nil {
					t.Fatal(err)
				}
				extra = "\n[encryption]\nrecipients = [\"" + strings.Fields(out[strings.Index(out, "age1"):])[0] + "\"]\n"
				identity = mustRead(t, keyFile)
			}
			repoPath := itestAccount(t, extra)
			// The backup machine has no private key.
			t.Setenv("IMAPVAULT_IDENTITY", "")
			run(t, "init")

			before := serverState(t, c)
			modseq := map[string]uint64{}
			for name := range before {
				// With CondStore: a server reports no HIGHESTMODSEQ (0) for a mailbox until
				// some client turns CONDSTORE on. The backup does (EXAMINE (CONDSTORE)),
				// so the baseline must be taken with it on too, or the first read would
				// differ from the second for that reason alone.
				sel, err := c.Select(name, &imap.SelectOptions{ReadOnly: true, CondStore: true}).Wait()
				if err != nil {
					t.Fatal(err)
				}
				modseq[name] = sel.HighestModSeq
			}

			var rep engine.BackupReport
			if err := json.Unmarshal([]byte(run(t, "--json", "backup")), &rep); err != nil {
				t.Fatal(err)
			}
			t.Logf("first backup: snapshot %d, %d messages, %d new, %d blobs written, %d reused", rep.Snapshot, rep.Messages, rep.New, rep.BlobsAdded, rep.BlobsReused)
			total := 0
			for _, m := range before {
				total += len(m)
			}
			if rep.Messages != total || rep.New != total || len(rep.Warnings) != 0 {
				t.Fatalf("report %+v, server has %d messages", rep, total)
			}
			// The seed has Archive/002-newsletter-copy duplicating INBOX/004-newsletter:
			// same bytes, stored once.
			if rep.BlobsAdded >= total {
				t.Errorf("%d blobs for %d messages: duplicates were not deduplicated", rep.BlobsAdded, total)
			}

			// The backup changed nothing on the server: flags and MODSEQ identical.
			after := serverState(t, c)
			for name, msgs := range before {
				for i := range msgs {
					if after[name][i].Flags != msgs[i].Flags {
						t.Errorf("%s uid %d: flags %q -> %q during backup", name, msgs[i].UID, msgs[i].Flags, after[name][i].Flags)
					}
				}
				sel, _ := c.Select(name, &imap.SelectOptions{ReadOnly: true, CondStore: true}).Wait()
				if sel.HighestModSeq != modseq[name] {
					t.Errorf("%s: HIGHESTMODSEQ %d -> %d: backup wrote to the mailbox", name, modseq[name], sel.HighestModSeq)
				}
			}
			if unseen := 0; true {
				for _, m := range after["INBOX"] {
					if !strings.Contains(m.Flags, `\Seen`) {
						unseen++
					}
				}
				if unseen == 0 {
					t.Error("the seed's unseen INBOX messages are gone: BODY.PEEK must not set \\Seen")
				}
			}

			t.Setenv("IMAPVAULT_IDENTITY", identity) // reading needs the key (only when encrypted)
			requireLsMatches(t, "latest", before)

			// show --raw reproduces the exact server bytes, tricky ones included.
			for _, folder := range []string{"Tricky", "INBOX"} {
				for _, m := range before[folder] {
					out, err := execRoot(t, "--account", "it", "show", "--raw", m.SHA256[:16])
					if err != nil {
						t.Fatalf("show %s: %v", m.SHA256[:16], err)
					}
					if out != string(m.Raw) {
						t.Errorf("%s uid %d: show --raw differs from the server bytes", folder, m.UID)
					}
				}
			}
			// Two messages sharing a Message-ID but with different bodies are two blobs.
			tr := before["Tricky"]
			if len(tr) != 4 || tr[2].SHA256 == tr[3].SHA256 {
				t.Errorf("Tricky: %+v", tr)
			}

			// A second run with no server change records nothing and downloads nothing new.
			t.Setenv("IMAPVAULT_IDENTITY", "")
			var rep2 engine.BackupReport
			if err := json.Unmarshal([]byte(run(t, "--json", "backup")), &rep2); err != nil {
				t.Fatal(err)
			}
			if rep2.Snapshot != rep.Snapshot+1 || rep2.New+rep2.Closed+rep2.Reflagged != 0 || rep2.BlobsAdded != 0 || rep2.Messages != total {
				t.Fatalf("second run %+v", rep2)
			}
			t.Setenv("IMAPVAULT_IDENTITY", identity)
			requireLsMatches(t, "2", before)

			// the repository passes its own checks (and --decrypt with the key).
			run(t, "check", "--read-data")
			if encrypted {
				run(t, "check", "--decrypt")
			}
			if _, err := os.Stat(filepath.Join(repoPath, "objects")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// appendMsg writes a message to the server with a test client. Only tests do this,
// to change the mailbox between two backups.
func appendMsg(t *testing.T, c *imapclient.Client, folder, subject string) {
	t.Helper()
	body := fmt.Sprintf("From: t@example.test\r\nTo: me@imapvault.test\r\nSubject: %s\r\nMessage-ID: <%s@itest>\r\nDate: Mon, 02 Mar 2026 09:00:00 +0000\r\n\r\nbody of %s\r\n", subject, subject, subject)
	cmd := c.Append(folder, int64(len(body)), &imap.AppendOptions{Time: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)})
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

func TestRescanSeesChangesAndKeepsHistory(t *testing.T) {
	c := dialTestServer(t)
	folder := fmt.Sprintf("itest-rescan-%d", time.Now().UnixNano())
	if err := c.Create(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(folder).Wait() })
	for _, s := range []string{"one", "two", "three"} {
		appendMsg(t, c, folder, s)
	}

	itestAccount(t, "\n[folders]\ninclude = [\""+folder+"\"]\n"+guardThresholdsOff)
	run(t, "init")
	var rep1 engine.BackupReport
	if err := json.Unmarshal([]byte(run(t, "--json", "backup")), &rep1); err != nil {
		t.Fatal(err)
	}
	state1 := serverState(t, c, folder)
	requireLsMatches(t, "1", state1)

	// change the mailbox: flag uid 1, expunge uid 2, add a message.
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	var one, two imap.UIDSet
	one.AddNum(1)
	two.AddNum(2)
	if err := c.Store(one, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagFlagged, imap.FlagSeen}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(two, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge().Close(); err != nil {
		t.Fatal(err)
	}
	appendMsg(t, c, folder, "four")
	_ = c.Unselect().Wait()

	var rep2 engine.BackupReport
	if err := json.Unmarshal([]byte(run(t, "--json", "backup")), &rep2); err != nil {
		t.Fatal(err)
	}
	t.Logf("second backup: +%d -%d ~%d", rep2.New, rep2.Closed, rep2.Reflagged)
	if rep2.New != 1 || rep2.Closed != 1 || rep2.Reflagged != 1 || rep2.Messages != 3 {
		t.Fatalf("report %+v", rep2)
	}
	state2 := serverState(t, c, folder)
	requireLsMatches(t, "2", state2)
	requireLsMatches(t, "1", state1) // history is intact

	// the folder disappears from the server: later snapshots drop it, old ones keep it.
	if err := c.Delete(folder).Wait(); err != nil {
		t.Fatal(err)
	}
	// ... and since it took three messages with it, the guard says so (exit 3).
	rep3 := backupJSONAlerted(t)
	if rep3.GoneFolders != 1 || rep3.Messages != 0 || rep3.Alert == nil || rep3.Alert.Kind != "folder-vanished" {
		t.Fatalf("report %+v", rep3)
	}
	requireLsMatches(t, "3", map[string][]serverMsg{})
	requireLsMatches(t, "2", state2)
	run(t, "check", "--read-data")
}

func TestDoctorAgainstServer(t *testing.T) {
	dialTestServer(t)
	itestAccount(t, "\n[folders]\ninclude = [\"*\"]\nexclude = [\"Trash\"]\n")
	var rep engine.DoctorReport
	if err := json.Unmarshal([]byte(run(t, "--json", "doctor")), &rep); err != nil {
		t.Fatal(err)
	}
	feat := map[string]bool{}
	for _, f := range rep.Features {
		feat[f.Name] = f.Supported
	}
	if !feat["CONDSTORE"] || !feat["QRESYNC"] || !feat["UIDPLUS"] {
		t.Errorf("features %v", feat)
	}
	sel := map[string]bool{}
	for _, f := range rep.Folders {
		sel[f.Name] = f.Selected
	}
	if !sel["INBOX"] || sel["Trash"] {
		t.Errorf("selection %v", sel)
	}
	t.Log(run(t, "doctor"))
}
