package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/engine"
	"github.com/michele/imapvault/internal/mail/mailtest"
)

// alerted sets up an account whose second backup trips the guard: 20 messages,
// then 15 deleted. It returns the second run's report.
func alerted(t *testing.T, extra string) *engine.BackupReport {
	t.Helper()
	setupAccountWith(t, "\n[guard]\nmax_deleted_percent = 10\nmax_deleted_count = 500\n"+extra)
	if out, err := execRoot(t, "--account", "test", "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	path, err := config.AccountConfigPath(os.Getenv, "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAccountOffline(path, "test")
	if err != nil {
		t.Fatal(err)
	}
	f := mailtest.New()
	inbox := f.AddFolder("INBOX")
	for i := range 20 {
		inbox.Add(fmt.Sprintf("Message-ID: <m%d@example.com>\r\nSubject: s%d\r\n\r\nbody %d\r\n", i, i, i))
	}
	if _, err := engine.Backup(context.Background(), cfg, f, engine.BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, m := range append([]mailtest.Message(nil), inbox.Messages[:15]...) {
		inbox.Delete(m.UID)
	}
	inbox.BumpOnExpunge()
	rep, err := engine.Backup(context.Background(), cfg, f, engine.BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Alerted() {
		t.Fatalf("the guard did not trip: %+v", rep)
	}
	return rep
}

func TestBackupExitCodes(t *testing.T) {
	tests := []struct {
		name string
		rep  engine.BackupReport
		code int
	}{
		{"complete", engine.BackupReport{Status: "complete"}, 0},
		{"partial", engine.BackupReport{Status: "partial"}, exitPartial},
		{"alert", engine.BackupReport{Status: "complete", Alert: &engine.AlertReport{ID: 4}}, exitAlert},
		{"alert beats partial", engine.BackupReport{Status: "partial", Alert: &engine.AlertReport{ID: 4}}, exitAlert},
	}
	for _, tc := range tests {
		err := backupExitError(&tc.rep)
		if tc.code == 0 {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		var ec *exitCodeError
		if !errors.As(err, &ec) || ec.code != tc.code {
			t.Errorf("%s: err = %v, want exit code %d", tc.name, err, tc.code)
		}
	}
	if exitAlert != 3 {
		t.Errorf("the alert exit code is part of the interface: got %d, want 3", exitAlert)
	}
}

func TestPrintBackupShowsTheAlert(t *testing.T) {
	rep := alerted(t, "")
	var b bytes.Buffer
	printBackup(&b, rep, "test")
	out := b.String()
	for _, want := range []string{"ALERT 1 (mass-deletion)", "15 of 20 messages deleted", "INBOX 15", "snapshot 2 is flagged", "snapshot 1", "pinned",
		"imapvault --account test diff 1 2", "imapvault --account test ack 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	var quiet bytes.Buffer
	printBackup(&quiet, &engine.BackupReport{Status: "complete"}, "")
	if strings.Contains(quiet.String(), "ALERT") {
		t.Errorf("no alert, yet:\n%s", quiet.String())
	}
}

func TestAlertsAckPinCommands(t *testing.T) {
	alerted(t, "")

	out, err := execRoot(t, "--account", "test", "alerts")
	if err != nil || !strings.Contains(out, "mass-deletion") || !strings.Contains(out, "open") || !strings.Contains(out, "15 of 20 messages deleted") {
		t.Fatalf("alerts: %v\n%s", err, out)
	}
	js, err := execRoot(t, "--account", "test", "--json", "alerts")
	if err != nil {
		t.Fatal(err)
	}
	var res engine.AlertsResult
	if err := json.Unmarshal([]byte(js), &res); err != nil || len(res.Alerts) != 1 || res.Alerts[0].Snapshot != 2 || res.Alerts[0].PinnedSnapshot != 1 || res.Alerts[0].AcknowledgedAt != nil {
		t.Fatalf("alerts --json: %v\n%s", err, js)
	}

	out, err = execRoot(t, "--account", "test", "snapshots")
	if err != nil || !strings.Contains(out, "pinned") || !strings.Contains(out, "flagged") {
		t.Fatalf("snapshots: %v\n%s", err, out)
	}

	if _, err := execRoot(t, "--account", "test", "unpin", "1"); err == nil || !strings.Contains(err.Error(), "ack 1") {
		t.Fatalf("unpin before ack: %v", err)
	}
	if _, err := execRoot(t, "--account", "test", "ack"); err == nil {
		t.Error("ack without ids or --all must fail")
	}
	if _, err := execRoot(t, "--account", "test", "ack", "x"); err == nil {
		t.Error("ack x must fail")
	}
	if _, err := execRoot(t, "--account", "test", "ack", "9"); err == nil {
		t.Error("ack of an unknown alert must fail")
	}
	out, err = execRoot(t, "--account", "test", "ack", "1")
	if err != nil || !strings.Contains(out, "Acknowledged alert 1") {
		t.Fatalf("ack: %v\n%s", err, out)
	}
	out, err = execRoot(t, "--account", "test", "alerts")
	if err != nil || !strings.Contains(out, "No unacknowledged alerts.") {
		t.Fatalf("alerts after ack: %v\n%s", err, out)
	}
	out, err = execRoot(t, "--account", "test", "alerts", "--all")
	if err != nil || !strings.Contains(out, "acked ") {
		t.Fatalf("alerts --all: %v\n%s", err, out)
	}
	if out, err = execRoot(t, "--account", "test", "ack", "--all"); err != nil || !strings.Contains(out, "No unacknowledged alerts.") {
		t.Fatalf("ack --all with none open: %v\n%s", err, out)
	}

	if out, err = execRoot(t, "--account", "test", "unpin", "1"); err != nil || !strings.Contains(out, "Snapshot 1 is no longer pinned") {
		t.Fatalf("unpin: %v\n%s", err, out)
	}
	if out, err = execRoot(t, "--account", "test", "pin", "latest"); err != nil || !strings.Contains(out, "Snapshot 2 is pinned") {
		t.Fatalf("pin: %v\n%s", err, out)
	}
	if _, err := execRoot(t, "--account", "test", "pin", "7"); err == nil {
		t.Error("pin of a missing snapshot must fail")
	}
}

func TestEncryptedAlertsListWithoutAKeyAndShowTheSummaryWithOne(t *testing.T) {
	keyFile := t.TempDir() + "/k.key"
	out, err := execRoot(t, "keygen", "--out", keyFile)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	recipient := strings.Fields(out[strings.Index(out, "age1"):])[0]
	alerted(t, "\n[encryption]\nrecipients = [\""+recipient+"\"]\n")

	out, err = execRoot(t, "--account", "test", "alerts")
	if err != nil || !strings.Contains(out, "mass-deletion") || strings.Contains(out, "15 of 20") {
		t.Fatalf("without a key: %v\n%s", err, out)
	}
	t.Setenv("IMAPVAULT_IDENTITY", mustRead(t, keyFile))
	out, err = execRoot(t, "--account", "test", "alerts")
	if err != nil || !strings.Contains(out, "15 of 20 messages deleted") {
		t.Fatalf("with the key: %v\n%s", err, out)
	}
}
