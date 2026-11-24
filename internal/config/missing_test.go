package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readDeps(readErr error, container bool) passwordDeps {
	d := passwordDeps{
		getenv:   func(string) string { return "" },
		exists:   func(p string) bool { return container && p == "/.dockerenv" },
		readFile: func(string) ([]byte, error) { return nil, readErr },
	}
	return d
}

func TestMissingAccountConfigSaysWhereToPutIt(t *testing.T) {
	path := "/config/imapvault/fastmail.toml"
	_, err := readConfigFile(path, "fastmail", readDeps(&fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}, false))
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the error must still be fs.ErrNotExist: %v", err)
	}
	var me *MissingConfigError
	if !errors.As(err, &me) {
		t.Fatalf("want a *MissingConfigError, got %T", err)
	}
	msg := err.Error()
	for _, want := range []string{`account "fastmail"`, path, "XDG_CONFIG_HOME", "examples/"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "mount") {
		t.Errorf("outside a container there is nothing to mount: %s", msg)
	}
	if strings.Contains(msg, "no such file or directory") {
		t.Errorf("the raw OS error should not leak into the message: %s", msg)
	}
}

func TestMissingAccountConfigInAContainerSaysWhatToMount(t *testing.T) {
	path := "/config/imapvault/fastmail.toml"
	_, err := readConfigFile(path, "fastmail", readDeps(fs.ErrNotExist, true))
	msg := err.Error()
	if !strings.Contains(msg, "mount") || !strings.Contains(msg, "/config/imapvault") {
		t.Errorf("a container user needs to hear what to mount where: %s", msg)
	}
}

func TestMissingConfigWithoutAnAccountNamesThePath(t *testing.T) {
	_, err := readConfigFile("/etc/x/config.toml", "", readDeps(fs.ErrNotExist, false))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/etc/x/config.toml") || strings.Contains(msg, "account") {
		t.Errorf("message: %s", msg)
	}
}

func TestOtherReadErrorsKeepTheirText(t *testing.T) {
	_, err := readConfigFile("/c/a.toml", "a", readDeps(fs.ErrPermission, false))
	var me *MissingConfigError
	if errors.As(err, &me) {
		t.Fatal("a permission error is not a missing file")
	}
	if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "read config") {
		t.Errorf("got %v", err)
	}
}

func TestLoadAccountOfflineReportsAMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadAccountOffline(filepath.Join(dir, "imapvault", "nope.toml"), "nope")
	var me *MissingConfigError
	if !errors.As(err, &me) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v", err)
	}
	_, err = LoadAccount(filepath.Join(dir, "imapvault", "nope.toml"), "nope")
	if !errors.As(err, &me) {
		t.Fatalf("LoadAccount: got %v", err)
	}
}
