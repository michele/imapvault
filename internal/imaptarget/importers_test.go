package imaptarget

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The import wall. Which code can reach a server, and which code can write to
// one, is decided by imports, so it is checked on imports:
//
//   - go-imap may be imported only by internal/imapsource (read-only, enforced by
//     its own test) and internal/imaptarget (CREATE and APPEND, fenced);
//   - internal/imaptarget may be imported only by internal/engine/restore.go.
//
// One named exception: tools/seed is a dev tool (package main, so nothing can
// import it) that loads fixtures into the throwaway docker server and refuses
// any non-loopback address.
//
// So a backup, prune or check path cannot be handed a connection that writes.
const (
	imapLib    = "github.com/emersion/go-imap"
	targetPkg  = "github.com/michele/imapvault/internal/imaptarget"
	readOnlyPk = "internal/imapsource"
	writerPk   = "internal/imaptarget"
	restoreGo  = "internal/engine/restore.go"
	seedTool   = "tools/seed"
)

// wallViolations reports what a non-test file at slash-separated path (relative
// to the module root) may not import.
func wallViolations(path string, imports []string) []string {
	var out []string
	dir := path[:max(strings.LastIndex(path, "/"), 0)]
	for _, imp := range imports {
		if (imp == imapLib || strings.HasPrefix(imp, imapLib+"/")) && dir != readOnlyPk && dir != writerPk && dir != seedTool {
			out = append(out, path+" imports "+imp+": only "+readOnlyPk+" and "+writerPk+" may talk IMAP")
		}
		if imp == targetPkg && dir != writerPk && path != restoreGo {
			out = append(out, path+" imports "+imp+": only "+restoreGo+" may write to a server")
		}
	}
	return out
}

func TestImportWall(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	scanned := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n != "." && n != ".." && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		var imports []string
		for _, is := range f.Imports {
			s, err := strconv.Unquote(is.Path.Value)
			if err != nil {
				return err
			}
			imports = append(imports, s)
		}
		scanned++
		for _, v := range wallViolations(filepath.ToSlash(rel), imports) {
			t.Error(v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 20 {
		t.Fatalf("only %d files scanned: the walk is not reaching the module", scanned)
	}
}

func TestImportWallCatchesViolations(t *testing.T) {
	imap := imapLib + "/v2/imapclient"
	cases := []struct {
		path    string
		imports []string
		bad     bool
	}{
		{"internal/engine/backup.go", []string{imap}, true},
		{"cmd/imapvault/restore.go", []string{imap}, true},
		{"internal/engine/backup.go", []string{targetPkg}, true},
		{"cmd/imapvault/restore.go", []string{targetPkg}, true},
		{"internal/imapsource/source.go", []string{targetPkg}, true},
		{"internal/imapsource/source.go", []string{imap}, false},
		{"internal/imaptarget/session.go", []string{imap}, false},
		{"internal/engine/restore.go", []string{targetPkg}, false},
		{"internal/engine/restore.go", []string{imap}, true}, // the engine never touches the library
		{"internal/engine/backup.go", []string{"net/mail"}, false},
		{"tools/seed/main.go", []string{imap}, false},
		{"tools/seed/main.go", []string{targetPkg}, true}, // the exception is for fixtures, not for restore
		{"tools/other/main.go", []string{imap}, true},
	}
	for _, c := range cases {
		if got := len(wallViolations(c.path, c.imports)) > 0; got != c.bad {
			t.Errorf("%s importing %v: violation = %v, want %v", c.path, c.imports, got, c.bad)
		}
	}
}
