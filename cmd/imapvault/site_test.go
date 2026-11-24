package main

import (
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	siteCodeBlock = regexp.MustCompile(`(?s)<pre><code>(.*?)</code></pre>`)
	siteTag       = regexp.MustCompile(`</?span[^>]*>`)
)

// The getting-started guide is what a new user types, line by line. Every
// `imapvault ...` line in its code blocks must name a real command and only
// flags that command has, so a renamed command or flag fails the build instead
// of misleading a reader.
func TestSiteGuideCommandsExist(t *testing.T) {
	page := filepath.Join("..", "..", "site", "guide", "index.html")
	data, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, m := range siteCodeBlock.FindAllStringSubmatch(string(data), -1) {
		for _, line := range strings.Split(html.UnescapeString(siteTag.ReplaceAllString(m[1], "")), "\n") {
			line = strings.TrimSpace(line)
			if i := strings.Index(line, " #"); i > 0 { // a trailing comment
				line = strings.TrimSpace(line[:i])
			}
			if !strings.HasPrefix(line, "imapvault ") {
				continue
			}
			args := strings.Fields(line)[1:]
			for i := range args {
				args[i] = strings.Trim(args[i], `"'`)
			}
			root := newRootCmd()
			cmd, rest, err := root.Find(args)
			if err != nil {
				t.Errorf("%q: %v", line, err)
				continue
			}
			if cmd == root {
				if len(rest) != 1 || (rest[0] != "--version" && rest[0] != "--help") {
					t.Errorf("%q does not name a command", line)
				}
				checked++
				continue
			}
			if err := cmd.ParseFlags(rest); err != nil {
				t.Errorf("%q: %v", line, err)
			}
			checked++
		}
	}
	if checked < 15 {
		t.Errorf("only %d imapvault command lines found in the guide: is the test reading the right page?", checked)
	}
}
