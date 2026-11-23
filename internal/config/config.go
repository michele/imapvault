// Package config loads and validates the imapvault TOML configuration.
// Secrets never come from the file: the IMAP password comes from password_file,
// password_command or the IMAPVAULT_IMAP_PASSWORD environment variable.
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// PasswordEnv is the environment variable holding the IMAP password.
const PasswordEnv = "IMAPVAULT_IMAP_PASSWORD"

// TLS modes.
const (
	TLSImplicit = "implicit"
	TLSStartTLS = "starttls"
	TLSNone     = "none"
)

// IMAP holds connection settings. The password is never in the file itself: it
// comes from password_file, password_command or the environment (see password.go).
type IMAP struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	Username string `toml:"username"`
	TLS      string `toml:"tls"`
	// PasswordFile is a path to a file holding the password; PasswordCommand is
	// a shell command printing it. At most one may be set; either wins over the
	// environment variables.
	PasswordFile    string `toml:"password_file"`
	PasswordCommand string `toml:"password_command"`
	Password        string `toml:"-"`
}

// Repository locates the backup repository on disk. One account is bound to
// exactly one repository.
type Repository struct {
	Path string `toml:"path"`
}

// Folders selects which server folders are backed up. Patterns are globs matched
// against the full folder name: '*' matches any run of characters, including the
// hierarchy delimiter ("Archive*" matches "Archive/2025"), and '?' matches one
// character. A folder is backed up when it matches an include pattern and no
// exclude pattern.
type Folders struct {
	Include []string `toml:"include"`
	Exclude []string `toml:"exclude"`
}

// Selected reports whether a folder is in scope.
func (f Folders) Selected(name string) bool {
	match := func(patterns []string) bool {
		for _, p := range patterns {
			if globMatch(p, name) {
				return true
			}
		}
		return false
	}
	return match(f.Include) && !match(f.Exclude)
}

// globMatch reports whether name matches pattern ('*' any run, '?' one rune).
func globMatch(pattern, name string) bool {
	p, n := []rune(pattern), []rune(name)
	pi, ni, star, mark := 0, 0, -1, 0
	for ni < len(n) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == n[ni]):
			pi++
			ni++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ni
			pi++
		case star >= 0:
			pi, mark = star+1, mark+1
			ni = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// Retention is the snapshot retention policy (restic semantics: a keep_* of 0
// switches that rule off) and the grace period for mail deleted on the server.
// DeletedGrace is written as a string ("180d") and parsed into DeletedGraceDur.
type Retention struct {
	KeepLast     int    `toml:"keep_last"`
	KeepHourly   int    `toml:"keep_hourly"`
	KeepDaily    int    `toml:"keep_daily"`
	KeepWeekly   int    `toml:"keep_weekly"`
	KeepMonthly  int    `toml:"keep_monthly"`
	KeepYearly   int    `toml:"keep_yearly"`
	DeletedGrace string `toml:"deleted_grace"`

	DeletedGraceDur time.Duration `toml:"-"`
}

func defaultRetention() Retention {
	return Retention{
		KeepLast: 3, KeepHourly: 24, KeepDaily: 14, KeepWeekly: 8, KeepMonthly: 24, KeepYearly: 10,
		DeletedGrace: "180d",
	}
}

// Guard configures the mass-deletion guard. A threshold of 0 switches that
// threshold off. OnAlert is a shell command run when the guard trips; {account}
// and {summary} are substituted by the engine.
type Guard struct {
	MaxDeletedPercent float64 `toml:"max_deleted_percent"`
	MaxDeletedCount   int     `toml:"max_deleted_count"`
	OnAlert           string  `toml:"on_alert"`
}

func defaultGuard() Guard { return Guard{MaxDeletedPercent: 10, MaxDeletedCount: 500} }

// Restore is the opt-in for writing to this account's server. Every other
// command is read-only on the server; `restore` refuses to append anything to an
// account whose config does not set allow_append. A backup-only config (cron, a
// NAS) therefore can never write, even if restore is run against it by mistake.
type Restore struct {
	AllowAppend bool `toml:"allow_append"`
}

// Config is the whole configuration file.
type Config struct {
	IMAP       IMAP       `toml:"imap"`
	Repository Repository `toml:"repository"`
	Folders    Folders    `toml:"folders"`
	Retention  Retention  `toml:"retention"`
	Guard      Guard      `toml:"guard"`
	Encryption Encryption `toml:"encryption"`
	Restore    Restore    `toml:"restore"`

	// Account is the --account name this configuration was loaded for ("" when
	// loaded by path). It only affects the password lookup; it is never read
	// from the file.
	Account string `toml:"-"`

	// PasswordWarnings are shown by every command that loads the config (a
	// password file readable by others, inside a container).
	PasswordWarnings []string `toml:"-"`
	// baseDir is the folder of the config file: relative password_file paths
	// resolve against it. Empty when the config was parsed from bytes.
	baseDir string
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) { return LoadAccount(path, "") }

// LoadAccount is Load for a named account: the password may come from
// IMAPVAULT_IMAP_PASSWORD_<NAME>.
func LoadAccount(path, account string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := ParseAccount(data, os.Getenv, account)
	if err != nil {
		return nil, err
	}
	// Files and commands are touched here only, never while parsing.
	cfg.baseDir = filepath.Dir(path)
	if cfg.PasswordWarnings, err = cfg.ResolvePassword(context.Background()); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadOffline is Load for commands that never connect to IMAP: everything is
// validated except that the password may be unset.
func LoadOffline(path string) (*Config, error) { return LoadAccountOffline(path, "") }

// LoadAccountOffline is LoadOffline for a named account.
func LoadAccountOffline(path, account string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return parse(data, os.Getenv, account, false)
}

// Parse decodes and validates config bytes. getenv supplies the password.
func Parse(data []byte, getenv func(string) string) (*Config, error) {
	return parse(data, getenv, "", true)
}

// ParseAccount is Parse for a named account (see LoadAccount).
func ParseAccount(data []byte, getenv func(string) string, account string) (*Config, error) {
	return parse(data, getenv, account, true)
}

func parse(data []byte, getenv func(string) string, account string, requirePassword bool) (*Config, error) {
	cfg := Config{Retention: defaultRetention(), Guard: defaultGuard(), Account: account}
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var se *toml.StrictMissingError
		if errors.As(err, &se) {
			keys := make([]string, 0, len(se.Errors))
			for _, e := range se.Errors {
				keys = append(keys, strings.Join(e.Key(), "."))
			}
			// Report key names only, never values: the file may hold a secret.
			return nil, fmt.Errorf("unknown config key(s): %s", strings.Join(keys, ", "))
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			return nil, fmt.Errorf("invalid TOML at line %d, column %d", row, col)
		}
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	cfg.IMAP.Password = getenv(PasswordEnvFor(account))
	if cfg.IMAP.Password == "" && account != "" {
		cfg.IMAP.Password = getenv(PasswordEnv)
	}
	cfg.Encryption.Identity = getenv(IdentityEnvFor(account))
	if cfg.Encryption.Identity == "" && account != "" {
		cfg.Encryption.Identity = getenv(IdentityEnv)
	}
	cfg.applyDefaults()
	if err := cfg.validate(getenv, requirePassword); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.IMAP.TLS == "" {
		c.IMAP.TLS = TLSImplicit
	}
	if c.IMAP.Port == 0 {
		if c.IMAP.TLS == TLSImplicit {
			c.IMAP.Port = 993
		} else {
			c.IMAP.Port = 143
		}
	}
	if len(c.Folders.Include) == 0 {
		c.Folders.Include = []string{"*"}
	}
}

func (c *Config) validate(getenv func(string) string, requirePassword bool) error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.IMAP.Host == "" {
		add("imap.host is required")
	}
	if c.IMAP.Username == "" {
		add("imap.username is required")
	}
	switch c.IMAP.TLS {
	case TLSImplicit, TLSStartTLS:
	case TLSNone:
		if !isLoopback(c.IMAP.Host) {
			add("imap.tls = %q is only allowed for loopback hosts, not %q", TLSNone, c.IMAP.Host)
		}
	default:
		add("imap.tls must be one of implicit, starttls, none (got %q)", c.IMAP.TLS)
	}
	if c.IMAP.PasswordFile != "" && c.IMAP.PasswordCommand != "" {
		add("imap.password_file and imap.password_command cannot both be set")
	}
	if requirePassword && c.IMAP.Password == "" && !c.hasPasswordSource() {
		if c.Account != "" {
			add("%s (or %s) is not set", PasswordEnvFor(c.Account), PasswordEnv)
		} else {
			add("%s is not set", PasswordEnv)
		}
	}

	c.validateRepository(getenv, add)
	c.validateFolders(add)
	c.validateRetention(add)
	c.validateGuard(add)
	c.validateEncryption(add)
	return errors.Join(errs...)
}

func (c *Config) validateRepository(getenv func(string) string, add func(string, ...any)) {
	if c.Repository.Path == "" {
		add("repository.path is required")
		return
	}
	// ~ and $VAR are expanded; the result must be absolute so a repository never
	// depends on the directory the command happens to run in.
	p, err := expandPath(c.Repository.Path, "", getenv)
	if err != nil {
		add("repository.path: %v", err)
		return
	}
	if !filepath.IsAbs(p) {
		add("repository.path must be an absolute path (got %q)", c.Repository.Path)
		return
	}
	c.Repository.Path = filepath.Clean(p)
}

func (c *Config) validateFolders(add func(string, ...any)) {
	for _, list := range []struct {
		key      string
		patterns []string
	}{{"include", c.Folders.Include}, {"exclude", c.Folders.Exclude}} {
		for _, p := range list.patterns {
			if p == "" {
				add("folders.%s: empty pattern", list.key)
			}
		}
	}
}

func (c *Config) validateRetention(add func(string, ...any)) {
	r := &c.Retention
	for _, k := range []struct {
		key string
		n   int
	}{
		{"keep_last", r.KeepLast}, {"keep_hourly", r.KeepHourly}, {"keep_daily", r.KeepDaily},
		{"keep_weekly", r.KeepWeekly}, {"keep_monthly", r.KeepMonthly}, {"keep_yearly", r.KeepYearly},
	} {
		if k.n < 0 {
			add("retention.%s must not be negative (got %d)", k.key, k.n)
		}
	}
	d, err := ParseDuration(r.DeletedGrace)
	if err != nil {
		add("retention.deleted_grace: %v", err)
	}
	r.DeletedGraceDur = d
}

func (c *Config) validateGuard(add func(string, ...any)) {
	g := &c.Guard
	if g.MaxDeletedPercent < 0 || g.MaxDeletedPercent > 100 {
		add("guard.max_deleted_percent must be between 0 and 100 (got %v); 0 turns it off", g.MaxDeletedPercent)
	}
	if g.MaxDeletedCount < 0 {
		add("guard.max_deleted_count must not be negative (got %d); 0 turns it off", g.MaxDeletedCount)
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
