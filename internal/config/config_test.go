package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validBase = `
[imap]
host = "imap.example.com"
username = "me@example.com"

[repository]
path = "/backup/imapvault/me"
`

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var pw = env(map[string]string{"IMAPVAULT_IMAP_PASSWORD": "secret"})

func TestParseValidAndDefaults(t *testing.T) {
	cfg, err := Parse([]byte(validBase), pw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAP.Port != 993 || cfg.IMAP.TLS != TLSImplicit {
		t.Errorf("imap defaults: port=%d tls=%q", cfg.IMAP.Port, cfg.IMAP.TLS)
	}
	if cfg.IMAP.Password != "secret" {
		t.Error("password not taken from env")
	}
	if got := cfg.Folders.Include; len(got) != 1 || got[0] != "*" {
		t.Errorf("folders.include default = %v", got)
	}
	r := cfg.Retention
	if r.KeepLast != 3 || r.KeepHourly != 24 || r.KeepDaily != 14 || r.KeepWeekly != 8 ||
		r.KeepMonthly != 24 || r.KeepYearly != 10 {
		t.Errorf("retention defaults = %+v", r)
	}
	if r.DeletedGrace != "180d" || r.DeletedGraceDur != 180*24*time.Hour {
		t.Errorf("deleted_grace = %q (%v)", r.DeletedGrace, r.DeletedGraceDur)
	}
	if cfg.Guard.MaxDeletedPercent != 10 || cfg.Guard.MaxDeletedCount != 500 || cfg.Guard.OnAlert != "" {
		t.Errorf("guard defaults = %+v", cfg.Guard)
	}
}

func TestStartTLSDefaultPort(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(validBase, `username`, "tls = \"starttls\"\nusername", 1)), pw)
	if err != nil || cfg.IMAP.Port != 143 {
		t.Fatalf("port = %v, err = %v", cfg, err)
	}
}

func TestParseErrors(t *testing.T) {
	with := func(section, body string) string { return validBase + "\n[" + section + "]\n" + body + "\n" }
	cases := []struct {
		name string
		toml string
		env  func(string) string
		want string
	}{
		{"missing password", validBase, env(nil), "IMAPVAULT_IMAP_PASSWORD"},
		{"password in file", strings.Replace(validBase, `username = "me@example.com"`, `username = "me"`+"\npassword = \"x\"", 1), pw, "password"},
		{"unknown section", with("extra", "foo = 1"), pw, "extra"},
		{"unknown key", with("guard", "nonsense = 1"), pw, "guard.nonsense"},
		{"no host", "[imap]\nusername = \"me\"\n[repository]\npath = \"/r\"\n", pw, "host"},
		{"no username", "[imap]\nhost = \"h\"\n[repository]\npath = \"/r\"\n", pw, "username"},
		{"bad tls", strings.Replace(validBase, `username`, "tls = \"bogus\"\nusername", 1), pw, "tls"},
		{"plaintext remote", strings.Replace(validBase, `username`, "tls = \"none\"\nusername", 1), pw, "loopback"},
		{"both password sources", strings.Replace(validBase, `username`, "password_file = \"/p\"\npassword_command = \"x\"\nusername", 1), pw, "cannot both"},
		{"no repository", "[imap]\nhost = \"h\"\nusername = \"me\"\n", pw, "repository.path"},
		{"relative repository", strings.Replace(validBase, "/backup/imapvault/me", "backup/me", 1), pw, "absolute"},
		{"unset var in repository", strings.Replace(validBase, "/backup/imapvault/me", "$NOPE/me", 1), pw, "NOPE"},
		{"empty include pattern", with("folders", `include = [""]`), pw, "folders.include"},
		{"negative keep", with("retention", "keep_daily = -1"), pw, "keep_daily"},
		{"bad grace", with("retention", `deleted_grace = "soon"`), pw, "deleted_grace"},
		{"zero grace", with("retention", `deleted_grace = "0d"`), pw, "deleted_grace"},
		{"percent too big", with("guard", "max_deleted_percent = 101"), pw, "max_deleted_percent"},
		{"percent negative", with("guard", "max_deleted_percent = -1"), pw, "max_deleted_percent"},
		{"negative count", with("guard", "max_deleted_count = -5"), pw, "max_deleted_count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.toml), tc.env)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error leaks password: %q", err)
			}
		})
	}
}

func TestErrorsAreJoined(t *testing.T) {
	_, err := Parse([]byte("[imap]\n[repository]\npath = \"rel\"\n"), pw)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"imap.host", "imap.username", "absolute"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should report %q", err, want)
		}
	}
}

func TestPlaintextLoopbackAllowed(t *testing.T) {
	src := "[imap]\nhost = \"127.0.0.1\"\nport = 1143\nusername = \"me\"\ntls = \"none\"\n[repository]\npath = \"/r\"\n"
	if _, err := Parse([]byte(src), pw); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryPathExpansion(t *testing.T) {
	g := env(map[string]string{"IMAPVAULT_IMAP_PASSWORD": "x", "HOME": "/home/u", "BACKUPS": "/mnt/b"})
	cases := map[string]string{
		"~/vault":             "/home/u/vault",
		"$BACKUPS/me":         "/mnt/b/me",
		"/data//vault/../me/": "/data/me",
	}
	for in, want := range cases {
		cfg, err := Parse([]byte(strings.Replace(validBase, "/backup/imapvault/me", in, 1)), g)
		if err != nil || cfg.Repository.Path != want {
			t.Errorf("%q -> %q, %v; want %q", in, cfg.Repository.Path, err, want)
		}
	}
}

func TestOverrides(t *testing.T) {
	src := validBase + `
[folders]
include = ["INBOX", "Archive*"]
exclude = ["Spam"]

[retention]
keep_last = 0
keep_yearly = 1
deleted_grace = "2w"

[guard]
max_deleted_percent = 0
max_deleted_count = 50
on_alert = "notify {summary}"
`
	cfg, err := Parse([]byte(src), pw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retention.KeepLast != 0 || cfg.Retention.KeepYearly != 1 || cfg.Retention.KeepDaily != 14 {
		t.Errorf("retention = %+v (an explicit 0 must stick, unset keys keep defaults)", cfg.Retention)
	}
	if cfg.Retention.DeletedGraceDur != 14*24*time.Hour {
		t.Errorf("grace = %v", cfg.Retention.DeletedGraceDur)
	}
	if cfg.Guard.MaxDeletedPercent != 0 || cfg.Guard.MaxDeletedCount != 50 || cfg.Guard.OnAlert != "notify {summary}" {
		t.Errorf("guard = %+v", cfg.Guard)
	}
}

func TestFoldersSelected(t *testing.T) {
	f := Folders{Include: []string{"*"}, Exclude: []string{"Spam", "Junk", "+QuietBlackHole", "Archive/old*"}}
	for name, want := range map[string]bool{
		"INBOX": true, "Archive/2025": true, "Spam": false, "Junk": false, "+QuietBlackHole": false,
		"Spam2": true, "Archive/old": false, "Archive/older/x": false, "Archive/new": true,
	} {
		if got := f.Selected(name); got != want {
			t.Errorf("Selected(%q) = %v, want %v", name, got, want)
		}
	}
	only := Folders{Include: []string{"INBOX", "Arch?ve*"}}
	for name, want := range map[string]bool{"INBOX": true, "Archive": true, "Archive.2025": true, "Sent": false, "inbox": false} {
		if got := only.Selected(name); got != want {
			t.Errorf("only.Selected(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLoadOfflineDoesNotNeedPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte(validBase), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PasswordEnv, "")
	if _, err := Load(path); err == nil {
		t.Fatal("Load must still require the password")
	}
	cfg, err := LoadOffline(path)
	if err != nil {
		t.Fatalf("LoadOffline: %v", err)
	}
	if cfg.Repository.Path == "" {
		t.Errorf("cfg = %+v", cfg)
	}
	// Everything else is still validated.
	bad := strings.Replace(validBase, `path = "/backup/imapvault/me"`, `path = "rel"`, 1)
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOffline(path); err == nil {
		t.Error("LoadOffline must still validate the repository path")
	}
}
