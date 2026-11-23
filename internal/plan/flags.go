// Package plan holds pure functions: sync diffing, and later retention and the
// mass-deletion guard. No I/O.
package plan

import (
	"slices"
	"strings"
)

// NormalizeFlags is the stored form of a message's flags: \Recent dropped (the
// server sets it per session, so it is noise), duplicates removed (system flags
// compare case-insensitively and are canonicalised, keywords are case-sensitive
// as stored), sorted, space-joined.
func NormalizeFlags(flags []string) string {
	seen := map[string]bool{}
	var out []string
	for _, f := range flags {
		if f == "" {
			continue
		}
		if strings.HasPrefix(f, `\`) {
			f = canonicalSystem(f)
		}
		if f == `\Recent` || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

func canonicalSystem(f string) string {
	switch strings.ToLower(f) {
	case `\seen`:
		return `\Seen`
	case `\answered`:
		return `\Answered`
	case `\flagged`:
		return `\Flagged`
	case `\deleted`:
		return `\Deleted`
	case `\draft`:
		return `\Draft`
	case `\recent`:
		return `\Recent`
	}
	return f
}
