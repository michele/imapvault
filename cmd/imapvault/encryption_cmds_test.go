package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var recipientRE = regexp.MustCompile(`age1[a-z0-9]+`)

// keygenTo runs `keygen --out` and returns the file path and printed recipient.
func keygenTo(t *testing.T, name string) (path, recipient string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), name)
	out, err := execRoot(t, "keygen", "--out", path)
	if err != nil {
		t.Fatalf("keygen: %v\n%s", err, out)
	}
	if strings.Contains(out, "AGE-SECRET-KEY") {
		t.Fatalf("keygen printed the private key:\n%s", out)
	}
	m := recipientRE.FindString(out)
	if m == "" {
		t.Fatalf("keygen printed no recipient:\n%s", out)
	}
	return path, m
}

func encToml(recipient, identityFile string) string {
	s := "\n[encryption]\nrecipients = [\"" + recipient + "\"]\n"
	if identityFile != "" {
		s += "identity_file = \"" + identityFile + "\"\n"
	}
	return s
}

func TestKeygenWritesKeyAndExplains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k", "personal.key")
	out, err := execRoot(t, "keygen", "--out", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{path, "recipients = [", "identity_file", "second", "back it up"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("keygen output lacks %q:\n%s", want, out)
		}
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", err, fi)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# public key: "+recipientRE.FindString(out)) {
		t.Fatalf("printed recipient does not match the file:\n%s\n%s", out, data)
	}
}

func TestKeygenRefusesOverwriteAndNeedsOut(t *testing.T) {
	path, _ := keygenTo(t, "a.key")
	before, _ := os.ReadFile(path)
	if _, err := execRoot(t, "keygen", "--out", path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("key file changed")
	}
	if _, err := execRoot(t, "keygen"); err == nil {
		t.Fatal("keygen without --out accepted")
	}
}

func TestKeygenJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.key")
	out, err := execRoot(t, "--json", "keygen", "--out", path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not json: %v\n%s", err, out)
	}
	if got["identity_file"] != path || !strings.HasPrefix(got["recipient"], "age1") || strings.Contains(out, "AGE-SECRET-KEY") {
		t.Fatalf("json: %v", got)
	}
}

func TestInitPlainWarnsItIsNotEncrypted(t *testing.T) {
	setupAccount(t)
	out, err := execRoot(t, "--account", "test", "init")
	if err != nil || !strings.Contains(out, "NOT encrypted") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

func TestEncryptedFlow(t *testing.T) {
	keyFile, recipient := keygenTo(t, "me.key")
	repoPath := setupAccountWith(t, encToml(recipient, keyFile))
	t.Setenv("IMAPVAULT_IDENTITY", "")

	out, err := execRoot(t, "--account", "test", "init")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "encrypted to 1 recipient") || strings.Contains(out, "NOT encrypted") {
		t.Fatalf("init output:\n%s", out)
	}
	toml, _ := os.ReadFile(filepath.Join(repoPath, "repo.toml"))
	if !strings.Contains(string(toml), recipient) || strings.Contains(string(toml), "AGE-SECRET-KEY") {
		t.Fatalf("repo.toml:\n%s", toml)
	}

	for _, args := range [][]string{{"check"}, {"check", "--read-data"}, {"check", "--decrypt"}} {
		out, err := execRoot(t, append([]string{"--account", "test"}, args...)...)
		if err != nil || !strings.Contains(out, "Check passed") || !strings.Contains(out, "encrypted") {
			t.Fatalf("%v: err=%v\n%s", args, err, out)
		}
	}
}

func TestCheckDecryptFailsClearlyWithoutOrWithWrongKey(t *testing.T) {
	_, recipient := keygenTo(t, "me.key")
	setupAccountWith(t, encToml(recipient, "")) // the cron box: no identity configured
	t.Setenv("IMAPVAULT_IDENTITY", "")
	if _, err := execRoot(t, "--account", "test", "init"); err != nil {
		t.Fatal(err)
	}
	// plain check works with no key at all
	if _, err := execRoot(t, "--account", "test", "check", "--read-data"); err != nil {
		t.Fatalf("key-free check: %v", err)
	}
	_, err := execRoot(t, "--account", "test", "check", "--decrypt")
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("no identity: %v", err)
	}
	other, _ := keygenTo(t, "other.key")
	data, _ := os.ReadFile(other)
	t.Setenv("IMAPVAULT_IDENTITY", string(data))
	_, err = execRoot(t, "--account", "test", "check", "--decrypt")
	if err == nil || !strings.Contains(err.Error(), "does not match") || strings.Contains(err.Error(), "AGE-SECRET-KEY") {
		t.Fatalf("wrong identity: %v", err)
	}
}

func TestInitRejectsInvalidRecipientInConfig(t *testing.T) {
	repoPath := setupAccountWith(t, encToml("age1definitelynotakey", ""))
	if _, err := execRoot(t, "--account", "test", "init"); err == nil {
		t.Fatal("invalid recipient accepted")
	}
	if _, err := os.Stat(filepath.Join(repoPath, "repo.toml")); err == nil {
		t.Fatal("a failed init left a repository behind")
	}
}
