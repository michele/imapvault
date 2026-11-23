package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

// hookTimeout bounds a guard.on_alert command: a hung notifier must not keep the
// backup (and its repository lock) alive.
const hookTimeout = 60 * time.Second

// splitCommand splits a command line into words the way a POSIX shell would for
// the simple cases: whitespace separates words, single quotes keep everything
// literally, double quotes keep everything except \" and \\, and a backslash
// outside quotes escapes the next character. Nothing else is special: there is
// no expansion, substitution, pipe or redirection, because on_alert is executed
// directly and never through a shell.
func splitCommand(s string) ([]string, error) {
	var (
		words   []string
		cur     strings.Builder
		inWord  bool
		quote   rune // 0, '\'' or '"'
		escaped bool
	)
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for _, r := range s {
		switch {
		case escaped:
			if quote == '"' && r != '"' && r != '\\' {
				cur.WriteRune('\\') // a backslash that escapes nothing stays
			}
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case unicode.IsSpace(r):
			flush()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	switch {
	case quote != 0:
		return nil, errors.New("unterminated quote")
	case escaped:
		return nil, errors.New("trailing backslash")
	}
	flush()
	return words, nil
}

// parseHook validates guard.on_alert. An empty command is fine (no hook).
func parseHook(cmdline string) ([]string, error) {
	if strings.TrimSpace(cmdline) == "" {
		return nil, nil
	}
	words, err := splitCommand(cmdline)
	if err != nil {
		return nil, fmt.Errorf("guard.on_alert: %w", err)
	}
	if len(words) == 0 {
		return nil, errors.New("guard.on_alert: empty command")
	}
	return words, nil
}

// cleanText replaces control characters (a folder name can hold newlines or
// escape sequences) so the text is safe in a terminal, a log or a notification.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// hookArgs substitutes {account} and {summary} into the words after the program
// name. It substitutes after splitting, so a value can never add or remove a
// word, and never in the program name itself.
func hookArgs(words []string, account, summary string) []string {
	out := make([]string, len(words))
	copy(out, words)
	rep := strings.NewReplacer("{account}", account, "{summary}", summary)
	for i := 1; i < len(out); i++ {
		out[i] = rep.Replace(out[i])
	}
	return out
}

// runHook executes argv directly, with extra added to the environment. It is a
// variable so tests can observe the call without running anything.
var runHook = func(ctx context.Context, argv, extra []string) error {
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), extra...)
	// Output is dropped: it is the hook's own business and could echo the summary.
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s did not finish within %s", argv[0], hookTimeout)
		}
		return fmt.Errorf("%s: %w", argv[0], err)
	}
	return nil
}
