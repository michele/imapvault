package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseInterval(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"0", 0, true},
		{"90s", 90 * time.Second, true},
		{"15m", 15 * time.Minute, true},
		{"1h", time.Hour, true},
		{"1h30m", 90 * time.Minute, true},
		{"24h", 24 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"2w", 14 * 24 * time.Hour, true},
		{"", 0, false},
		{"-1h", 0, false},
		{"soon", 0, false},
		{"1.5d", 0, false},
	}
	for _, c := range cases {
		got, err := parseInterval(c.in)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("parseInterval(%q) = %v, %v; want %v ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func TestRunRejectsBadFlagsBeforeTouchingAnything(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no config exists: the flags must fail first
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--every", "1m"}, "too short"},
		{[]string{"--every", "never"}, "invalid interval"},
		{[]string{"--prune-every", "7d", "--forget-every", "0"}, "--forget-every"},
		{[]string{"--progress", "bar"}, "--progress"},
		{[]string{"--progress", "fancy"}, "--progress"},
		{[]string{"--stale-after", "-1h"}, "invalid interval"},
	}
	for _, c := range cases {
		out, err := execRoot(t, append([]string{"--account", "x", "run"}, c.args...)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("run %v: err %v, out %q; want an error containing %q", c.args, err, out, c.want)
		}
	}
}

func TestRunIsDocumented(t *testing.T) {
	out, err := execRoot(t, "run", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--every", "--forget-every", "--prune-every", "--stale-after", "SIGTERM"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}
