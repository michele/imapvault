package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"
)

// The alert URL is where imapvault POSTs a notification when the mass-deletion
// guard trips or a long-running `run` has had no good backup for a while. It is
// for hosts that cannot run a guard.on_alert program (the container image has
// none). A topic URL such as https://ntfy.sh/<topic> works as a password, so it
// can come from a file like the IMAP password, and no error ever contains it.

var alertURLKind = secretKind{fileKey: "guard.on_alert_url_file", noun: "alert URL file"}

func (c *Config) validateAlertURL(add func(string, ...any)) {
	g := &c.Guard
	if g.OnAlertURL != "" && g.OnAlertURLFile != "" {
		add("guard.on_alert_url and guard.on_alert_url_file cannot both be set")
		return
	}
	if g.OnAlertURL != "" {
		if err := checkAlertURL(g.OnAlertURL); err != nil {
			add("guard.on_alert_url %v", err)
		}
	}
}

// AlertURL returns the URL notifications are sent to ("" when none is
// configured), reading guard.on_alert_url_file if that is the source. The
// warnings are for the user (a file readable by others inside a container).
func (c *Config) AlertURL() (string, []string, error) { return c.alertURL(realPasswordDeps()) }

func (c *Config) alertURL(d passwordDeps) (string, []string, error) {
	switch {
	case c.Guard.OnAlertURL != "":
		return c.Guard.OnAlertURL, nil, nil // checked when the config was parsed
	case c.Guard.OnAlertURLFile != "":
		data, warnings, err := readSecretFile(alertURLKind, c.Guard.OnAlertURLFile, c.baseDir, d)
		if err != nil {
			return "", nil, err
		}
		raw, err := singleLine(string(data), "guard.on_alert_url_file")
		if err != nil {
			return "", nil, err
		}
		if err := checkAlertURL(raw); err != nil {
			return "", nil, fmt.Errorf("the URL in guard.on_alert_url_file %v", err)
		}
		return raw, warnings, nil
	}
	return "", nil, nil
}

// checkAlertURL accepts https URLs, and http only for an address that never
// leaves the local network: loopback, a private or link-local IP, or a host name
// without dots (a compose service name). The message is deliberately generic: it
// must not repeat the URL.
func checkAlertURL(raw string) error {
	bad := errors.New("must be an https URL (http is allowed only for a local address)")
	if strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return bad
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return bad
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if localAddress(u.Hostname()) {
			return nil
		}
	}
	return bad
}

func localAddress(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return !strings.Contains(host, ".")
}
