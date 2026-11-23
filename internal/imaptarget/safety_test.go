package imaptarget

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// CLAUDE.md safety rule 9. This package may send exactly two commands that change
// a server, CREATE and APPEND, and only from writer.go, which is reachable only
// through the fenced writer. Every other write command, a read-write SELECT, and
// a body fetch that would set \Seen fail the build here.

// writeCalls are the IMAP commands that change a server.
var forbiddenCalls = map[string]bool{
	"Store": true, "Expunge": true, "UIDExpunge": true, "Move": true, "UIDMove": true,
	"Copy": true, "UIDCopy": true, "Delete": true, "Rename": true,
	"Subscribe": true, "Unsubscribe": true, "Idle": true, "Enable": true,
}

// allowedHere are the two commands restore needs, allowed in writer.go only.
var allowedHere = map[string]bool{"Append": true, "Create": true}

const writerFile = "writer.go"

// astViolation reports why n breaks the rules in the named file, or "".
func astViolation(file string, n ast.Node) string {
	switch x := n.(type) {
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		name := sel.Sel.Name
		if forbiddenCalls[name] {
			return "forbidden call ." + name + "(): restore may only CREATE and APPEND"
		}
		if allowedHere[name] && file != writerFile {
			return "." + name + "() outside " + writerFile + ": writes go through the fenced writer"
		}
		if name == "Select" && !hasReadOnlyTrue(x) {
			return "Select without ReadOnly: true (must be EXAMINE)"
		}
	case *ast.CompositeLit:
		if isType(x.Type, "FetchItemBodySection") && !fieldIsTrue(x, "Peek") {
			return `FetchItemBodySection without Peek: true would set \Seen`
		}
		if isType(x.Type, "SelectOptions") && !fieldIsTrue(x, "ReadOnly") {
			return "SelectOptions without ReadOnly: true"
		}
	}
	return ""
}

func TestTargetOnlyCreatesAndAppends(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked, sawWriter := 0, false
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		sawWriter = sawWriter || name == writerFile
		ast.Inspect(f, func(n ast.Node) bool {
			if msg := astViolation(name, n); msg != "" {
				t.Errorf("%s: %s", fset.Position(n.Pos()), msg)
			}
			return true
		})
	}
	if checked == 0 || !sawWriter {
		t.Fatalf("scanned %d files, writer.go seen: %v", checked, sawWriter)
	}
}

func isType(e ast.Expr, name string) bool {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		return t.Sel.Name == name
	case *ast.Ident:
		return t.Name == name
	}
	return false
}

func fieldIsTrue(lit *ast.CompositeLit, field string) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == field {
			v, ok := kv.Value.(*ast.Ident)
			return ok && v.Name == "true"
		}
	}
	return false
}

func hasReadOnlyTrue(call *ast.CallExpr) bool {
	for _, a := range call.Args {
		if u, ok := a.(*ast.UnaryExpr); ok {
			a = u.X
		}
		if lit, ok := a.(*ast.CompositeLit); ok && isType(lit.Type, "SelectOptions") && fieldIsTrue(lit, "ReadOnly") {
			return true
		}
	}
	return false
}

// The checker must catch what it claims to: run it over bad snippets.
func TestSafetyCheckerCatchesViolations(t *testing.T) {
	bad := map[string]struct{ file, src string }{
		"store":              {"session.go", `package p; func f(c C) { c.Store(nil, nil, nil) }`},
		"expunge in writer":  {writerFile, `package p; func f(c C) { c.Expunge() }`},
		"uid expunge":        {"session.go", `package p; func f(c C) { c.UIDExpunge(nil) }`},
		"move":               {"session.go", `package p; func f(c C) { c.Move(nil, "x") }`},
		"copy":               {"session.go", `package p; func f(c C) { c.Copy(nil, "x") }`},
		"delete folder":      {writerFile, `package p; func f(c C) { c.Delete("x") }`},
		"rename":             {writerFile, `package p; func f(c C) { c.Rename("a", "b") }`},
		"subscribe":          {"session.go", `package p; func f(c C) { c.Subscribe("a") }`},
		"append elsewhere":   {"session.go", `package p; func f(c C) { c.Append("x", 0, nil) }`},
		"create elsewhere":   {"fence.go", `package p; func f(c C) { c.Create("x", nil) }`},
		"select rw":          {"session.go", `package p; func f(c C) { c.Select("x", nil) }`},
		"select no readonly": {"session.go", `package p; func f(c C) { c.Select("x", &imap.SelectOptions{CondStore: true}) }`},
		"no peek":            {"session.go", `package p; var _ = &imap.FetchItemBodySection{}`},
	}
	for name, tc := range bad {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "x.go", tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			hit := false
			ast.Inspect(f, func(n ast.Node) bool {
				if astViolation(tc.file, n) != "" {
					hit = true
				}
				return true
			})
			if !hit {
				t.Fatal("violation not detected")
			}
		})
	}
	good := map[string]string{
		writerFile:   `package p; func f(c C) { c.Append("x", 1, nil); c.Create("y", nil) }`,
		"session.go": `package p; func f(c C) { c.Select("x", &imap.SelectOptions{ReadOnly: true}); c.List("", "*", nil); c.Fetch(nil, nil) }`,
	}
	for file, src := range good {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if msg := astViolation(file, n); msg != "" {
				t.Errorf("%s: false positive: %s", file, msg)
			}
			return true
		})
	}
}

func TestParseMessageID(t *testing.T) {
	cases := map[string]string{
		"Message-ID: <a@b>\r\n\r\n":          "<a@b>",
		"Message-ID: <a@b>\r\n":              "<a@b>", // no terminating blank line
		"message-id:    <a@b>  \r\n\r\n":     "<a@b>",
		"Message-ID:\r\n <folded@x>\r\n\r\n": "<folded@x>",
		"\r\n":                               "",
		"":                                   "",
		"garbage without colon":              "",
	}
	for in, want := range cases {
		if got := parseMessageID([]byte(in)); got != want {
			t.Errorf("parseMessageID(%q) = %q, want %q", in, got, want)
		}
	}
}
