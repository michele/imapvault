package engine

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{"   ", nil, false},
		{"notify-me", []string{"notify-me"}, false},
		{"notify-me a  b\tc", []string{"notify-me", "a", "b", "c"}, false},
		{`/usr/local/bin/notify-me 'imapvault alert: {account} {summary}'`, []string{"/usr/local/bin/notify-me", "imapvault alert: {account} {summary}"}, false},
		{`say "hello world"`, []string{"say", "hello world"}, false},
		{`say "a \"quoted\" word"`, []string{"say", `a "quoted" word`}, false},
		{`say "back\\slash"`, []string{"say", `back\slash`}, false},
		{`say "keeps \n"`, []string{"say", `keeps \n`}, false},
		{`say 'no \ escapes "here"'`, []string{"say", `no \ escapes "here"`}, false},
		{`say a\ b`, []string{"say", "a b"}, false},
		{`say ''`, []string{"say", ""}, false},
		{`say a'b c'd`, []string{"say", "ab cd"}, false},
		{"say $(touch x) `id` ; rm -rf /", []string{"say", "$(touch", "x)", "`id`", ";", "rm", "-rf", "/"}, false},
		{`say 'open`, nil, true},
		{`say "open`, nil, true},
		{`say \`, nil, true},
	}
	for _, tc := range tests {
		got, err := splitCommand(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("splitCommand(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitCommand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHookArgsSubstituteAfterSplittingAndNeverInTheProgram(t *testing.T) {
	words, err := parseHook(`{account} 'alert {account}: {summary}' {summary}`)
	if err != nil {
		t.Fatal(err)
	}
	got := hookArgs(words, "me", `INBOX "x" $(touch pwned); a b`)
	want := []string{"{account}", `alert me: INBOX "x" $(touch pwned); a b`, `INBOX "x" $(touch pwned); a b`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if words[1] != "alert {account}: {summary}" {
		t.Error("hookArgs must not modify its input")
	}
}

func TestParseHook(t *testing.T) {
	if w, err := parseHook(""); err != nil || w != nil {
		t.Errorf("empty = %v, %v", w, err)
	}
	if _, err := parseHook(`x 'oops`); err == nil || !strings.Contains(err.Error(), "guard.on_alert") {
		t.Errorf("err = %v", err)
	}
}

func TestCleanText(t *testing.T) {
	if got := cleanText("a\nb\x1b[31mc\td"); strings.ContainsAny(got, "\n\x1b\t") {
		t.Errorf("control characters left in %q", got)
	}
}
