package config

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const identityText = "# created: 2026-11-23\n# public key: age1example\nAGE-SECRET-KEY-1TOPSECRETTOPSECRET\n"

func withEncryption(extra string) string { return validBase + "\n[encryption]\n" + extra }

func identCfg(t *testing.T, extra string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(withEncryption(extra)), pw)
	if err != nil {
		t.Fatal(err)
	}
	cfg.baseDir = "/etc/imapvault"
	return cfg
}

func TestParseEncryptionSection(t *testing.T) {
	cfg, err := Parse([]byte(withEncryption("recipients = [\"age1aaa\", \"age1bbb\"]\nidentity_file = \"/k\"")), pw)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Encryption.Recipients; len(got) != 2 || got[0] != "age1aaa" || got[1] != "age1bbb" {
		t.Fatalf("recipients: %v", got)
	}
	if cfg.Encryption.IdentityFile != "/k" || cfg.Encryption.Identity != "" {
		t.Fatalf("parsing must not read the identity: %+v", cfg.Encryption)
	}
	if !cfg.Encryption.Enabled() {
		t.Fatal("recipients present, encryption should be enabled")
	}

	plain, err := Parse([]byte(validBase), pw)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Encryption.Enabled() {
		t.Fatal("no [encryption] section means no encryption")
	}
}

func TestParseEncryptionRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"both identity sources": "identity_file = \"/k\"\nidentity_command = \"echo\"",
		"empty recipient":       "recipients = [\"\"]",
		"duplicate recipient":   "recipients = [\"age1aaa\", \"age1aaa\"]",
		"identity inline":       "identity = \"AGE-SECRET-KEY-1X\"",
	}
	for name, extra := range cases {
		if _, err := Parse([]byte(withEncryption(extra)), pw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := Parse([]byte(withEncryption("identity = \"AGE-SECRET-KEY-1TOPSECRET\"")), pw)
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("unknown-key error must name the key, never the value: %v", err)
	}
}

func TestIdentityEnvFor(t *testing.T) {
	if IdentityEnvFor("") != "IMAPVAULT_IDENTITY" || IdentityEnvFor("my-acct") != "IMAPVAULT_IDENTITY_MY_ACCT" {
		t.Fatalf("got %q %q", IdentityEnvFor(""), IdentityEnvFor("my-acct"))
	}
}

func TestIdentityFromEnvironment(t *testing.T) {
	getenv := env(map[string]string{"IMAPVAULT_IMAP_PASSWORD": "secret", "IMAPVAULT_IDENTITY": "AGE-SECRET-KEY-1PLAIN", "IMAPVAULT_IDENTITY_A": "AGE-SECRET-KEY-1ACCT"})
	cfg, err := ParseAccount([]byte(withEncryption("recipients = [\"age1aaa\"]")), getenv, "a")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Encryption.Identity != "AGE-SECRET-KEY-1ACCT" {
		t.Fatalf("per-account variable must win: %q", cfg.Encryption.Identity)
	}
	cfg, err = ParseAccount([]byte(withEncryption("recipients = [\"age1aaa\"]")), getenv, "b")
	if err != nil || cfg.Encryption.Identity != "AGE-SECRET-KEY-1PLAIN" {
		t.Fatalf("fallback to the plain variable: %q %v", cfg.Encryption.Identity, err)
	}
}

func TestResolveIdentityFile(t *testing.T) {
	good := fakeFile{data: identityText, mode: 0o600}
	cases := []struct {
		name    string
		extra   string
		files   fakeFS
		wantErr string
	}{
		{"absolute", `identity_file = "/run/key"`, fakeFS{"/run/key": good}, ""},
		{"relative to the config folder", `identity_file = "key.txt"`, fakeFS{"/etc/imapvault/key.txt": good}, ""},
		{"missing", `identity_file = "/run/none"`, fakeFS{}, "identity_file"},
		{"directory", `identity_file = "/run/d"`, fakeFS{"/run/d": {mode: 0o700, dir: true}}, "not a regular file"},
		{"empty", `identity_file = "/run/key"`, fakeFS{"/run/key": {data: "\n", mode: 0o600}}, "empty"},
		{"group readable", `identity_file = "/run/key"`, fakeFS{"/run/key": {data: identityText, mode: 0o640}}, "identity file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := identCfg(t, c.extra)
			cfg.Encryption.Identity = "from-the-environment" // the file must win
			_, err := cfg.resolveIdentity(context.Background(), c.files.deps(nil))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				if strings.Contains(err.Error(), "TOPSECRET") {
					t.Fatalf("error leaks the key: %v", err)
				}
				return
			}
			if err != nil || cfg.Encryption.Identity != identityText {
				t.Fatalf("identity = %q err = %v", cfg.Encryption.Identity, err)
			}
		})
	}
}

func TestIdentityFileContainerWarning(t *testing.T) {
	cfg := identCfg(t, `identity_file = "/run/key"`)
	files := fakeFS{"/run/key": {data: identityText, mode: 0o644}}
	warnings, err := cfg.resolveIdentity(context.Background(), files.deps(map[string]string{"IMAPVAULT_CONTAINER": "1"}))
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "identity file") {
		t.Fatalf("warnings=%v err=%v", warnings, err)
	}
}

func TestResolveIdentityCommand(t *testing.T) {
	cfg := identCfg(t, `identity_command = "pass show age-key"`)
	if _, err := cfg.resolveIdentity(context.Background(), runner(identityText, "", nil)); err != nil || cfg.Encryption.Identity != identityText {
		t.Fatalf("identity = %q err = %v", cfg.Encryption.Identity, err)
	}
	for name, d := range map[string]passwordDeps{
		"failure":      runner("TOPSECRET", "gpg: failed", errors.New("exit status 2")),
		"empty output": runner(" \n", "", nil),
	} {
		cfg := identCfg(t, `identity_command = "pass show age-key"`)
		_, err := cfg.resolveIdentity(context.Background(), d)
		if err == nil || strings.Contains(err.Error(), "TOPSECRET") || !strings.Contains(err.Error(), "identity_command") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestResolveIdentityWithoutAnySource(t *testing.T) {
	cfg := identCfg(t, `recipients = ["age1aaa"]`)
	if _, err := cfg.resolveIdentity(context.Background(), fakeFS{}.deps(nil)); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("want ErrNoIdentity, got %v", err)
	}
	cfg.Encryption.Identity = "AGE-SECRET-KEY-1ENV" // from the environment
	if _, err := cfg.resolveIdentity(context.Background(), fakeFS{}.deps(nil)); err != nil {
		t.Fatalf("environment identity: %v", err)
	}
}

func TestOfflineLoadNeverRunsIdentityCommand(t *testing.T) {
	cfg, err := Parse([]byte(withEncryption("identity_command = \"touch /should/not/run\"")), pw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Encryption.Identity != "" {
		t.Fatal("parse resolved the identity")
	}
}
