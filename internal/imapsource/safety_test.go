package imapsource

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// CLAUDE.md safety rule 1: backup is read-only on the source. This test fails
// when a write command, a read-write SELECT or a non-PEEK body fetch appears in
// this package's production code. Restore (M6) must live in another package.
var forbiddenCalls = map[string]bool{
	"Store": true, "Expunge": true, "UIDExpunge": true, "Move": true, "UIDMove": true,
	"Copy": true, "UIDCopy": true, "Delete": true, "Rename": true, "Append": true,
	"Create": true, "Subscribe": true, "Unsubscribe": true, "Idle": true, "Enable": true,
}

func TestSourceIsReadOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pos := fset.Position(x.Pos())
				if forbiddenCalls[sel.Sel.Name] {
					t.Errorf("%s: forbidden call .%s() in a read-only source", pos, sel.Sel.Name)
				}
				if sel.Sel.Name == "Select" && !hasReadOnlyTrue(x) {
					t.Errorf("%s: Select without ReadOnly: true (must be EXAMINE)", pos)
				}
			case *ast.CompositeLit:
				if isType(x.Type, "FetchItemBodySection") && !fieldIsTrue(x, "Peek") {
					t.Errorf("%s: FetchItemBodySection without Peek: true would set \\Seen", fset.Position(x.Pos()))
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no production files scanned")
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

// The checker itself must catch what it claims to: run it over bad snippets.
func TestSafetyCheckerCatchesViolations(t *testing.T) {
	bad := map[string]string{
		"store":     `package p; func f(c C) { c.Store(nil, nil, nil) }`,
		"expunge":   `package p; func f(c C) { c.Expunge() }`,
		"select rw": `package p; func f(c C) { c.Select("x", nil) }`,
		"no peek":   `package p; var _ = &imap.FetchItemBodySection{}`,
	}
	for name, src := range bad {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "x.go", src, 0)
			if err != nil {
				t.Fatal(err)
			}
			hit := false
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (forbiddenCalls[sel.Sel.Name] || (sel.Sel.Name == "Select" && !hasReadOnlyTrue(x))) {
						hit = true
					}
				case *ast.CompositeLit:
					if isType(x.Type, "FetchItemBodySection") && !fieldIsTrue(x, "Peek") {
						hit = true
					}
				}
				return true
			})
			if !hit {
				t.Fatal("violation not detected")
			}
		})
	}
}
