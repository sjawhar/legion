package testtmp

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// exceptions are the paths under the module root this check leaves unread, with the reason each
// places a temporary directory itself on purpose. The test logs every one it skips, so a run says
// what it did not read and nothing is left out quietly.
var exceptions = map[string]string{
	"internal/testtmp": "this package, where both shapes belong: Dir reads os.TempDir and makes the directory " +
		"itself, naming /tmp when it falls back, so that no other test has to",
	"internal/shim/socketpath_test.go": "roleTempDir builds a deliberately long directory, as long as the TMPDIR a " +
		"Legion role is told, so that its test proves a socket still binds where a role runs; a short one would take the proof away",
}

// Every test of this module that wants a temporary directory of its own takes it from this
// package, so that one place measures a socket path against what Linux holds and one place knows
// where to fall back to (docs/testing.md, "Unix sockets in Go tests"). The check reads the
// module's test files and fails on the two shapes the copied helpers were written in: an
// os.MkdirTemp whose first argument is a string literal naming a directory, which writes there
// wherever the test runs and whatever TMPDIR says, and an os.TempDir, which reads TMPDIR without
// measuring anything. os.MkdirTemp("", …) is not one of them: the empty first argument is the
// placement this package asks for, and the once-per-binary directories a TestMain owns and the
// fixtures rooted at t.TempDir() use it.
//
// It reads the source as the parser does rather than as text, so prose about either call — the
// comment on internal/daemon/idle_rig_pod_test.go's privateDir spells os.TempDir() in English —
// is not an offence.
//
// packages/envoy/internal/testtmp keeps the same test, with its own module's exceptions. Change
// one and change the other.
func TestNoTestFilePlacesATemporaryDirectoryOfItsOwn(t *testing.T) {
	offences := placements(t, "../..")
	if len(offences) == 0 {
		return
	}
	t.Fatalf("call sites that place a temporary directory of their own (%d):\n\t%s\n"+
		"take the directory from testtmp.Dir(t, longest), or the socket path from testtmp.Socket(t, name): it measures "+
		"the path against the %d bytes a unix socket path holds and falls back to %s for that length alone, saying so "+
		"(docs/testing.md, \"Unix sockets in Go tests\")",
		len(offences), strings.Join(offences, "\n\t"), maxSocketPath, fallbackBase)
}

// placements names, as file:line and the call, every place in the module's test files that puts a
// temporary directory somewhere of its own. Paths are written from the module root, where this
// package's own directory is two levels up.
func placements(t *testing.T, root string) []string {
	t.Helper()
	var offences []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		reason, excepted := exceptions[relative]
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || name == "vendor" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			if excepted {
				t.Logf("not read: %s (%s)", relative, reason)
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if excepted {
			t.Logf("not read: %s (%s)", relative, reason)
			return nil
		}
		offences = append(offences, placementsInFile(t, path, relative)...)
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's test files: %v", err)
	}
	slices.Sort(offences)
	return offences
}

// placementsInFile reads one test file, named relative from the module root in what it answers.
// A file whose bytes hold neither TempDir nor MkdirTemp is not parsed: an offence is reported
// from a selector whose Sel.Name is one of those two, copied from these same bytes, so such a
// file can hold none. Most of the module's test files hold neither and go unparsed, where
// parsing every one of them builds tens of megabytes of syntax tree on every run.
//
// One difference comes with the filter: a test file that does not parse fails this check only
// when it mentions one of the two names. The Go build is what reads the rest.
func placementsInFile(t *testing.T, path, relative string) []string {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	if !bytes.Contains(source, []byte("TempDir")) && !bytes.Contains(source, []byte("MkdirTemp")) {
		return nil
	}
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, path, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", relative, err)
	}
	var found []string
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "os" {
			return true
		}
		at := relative + ":" + strconv.Itoa(files.Position(call.Pos()).Line)
		switch selector.Sel.Name {
		case "TempDir":
			found = append(found, at+": os.TempDir()")
		case "MkdirTemp":
			if len(call.Args) == 0 {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			if dir, err := strconv.Unquote(literal.Value); err != nil || dir == "" {
				return true
			}
			found = append(found, at+": os.MkdirTemp("+literal.Value+", …)")
		}
		return true
	})
	return found
}
