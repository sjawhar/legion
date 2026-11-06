package fixtureenv_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// guardingPackages are the packages of this module whose tests run real git or jj — as child
// processes, in-process through the code under test, or both — so each one's TestMain calls
// fixtureenv.MustGuard before any test of it runs (docs/testing.md, "A test that starts jj or git
// inherits no pod's environment"). A package joins the list when a test of it starts git or jj: in
// a Legion issue pod an unguarded one inherits the pod's own LEGION_WORKSPACE, GIT_CONFIG_* and
// GitHub credential, and the shim's rescue test pushed the pod's own workspace to the real origin
// that way. Every package's tests run in a process of their own, so each entry here is one process
// to guard, and a guard deleted from one of them is invisible to every other package's tests.
//
// The check below reads these packages and no others. The module's other TestMains guard nothing
// today; bringing one of them under the guard is a change of its own, and that change adds it to
// this list.
var guardingPackages = []string{
	"cmd/legion",
	"internal/api",
	"internal/daemon",
	"internal/fixtureenv",
	"internal/launcher",
	"internal/repocache",
	"internal/rescue",
	"internal/runtime/sandbox",
	"internal/shim",
	"internal/workspace",
}

// fixtureenvPath is this package's import path, which a guarding TestMain's file imports.
const fixtureenvPath = "github.com/sjawhar/legion/daemon/internal/fixtureenv"

// Every package of guardingPackages guards its test process: its TestMain calls
// fixtureenv.MustGuard, in its own body where no branch can skip it, before anything in that body
// runs the tests. This is the one check for all nine, in place of a copy of a test per package,
// and it is a check of the source rather than of the running process: a test can only see the
// environment of the process it runs in, so no test of this package could see another package's
// TestMain drop its guard, and the deleted copies each saw only their own.
//
// It reads the TestMain of each package and refuses, naming the package: a TestMain with no
// MustGuard at all, one whose only MustGuard sits inside a branch (a run that takes another branch
// would reach the tests unguarded), and one that reaches the tests before it — below m.Run, or
// after a branch that runs them and falls through. The one shape it allows before the guard is the
// branch Guard's own doc asks for, where the test binary runs as a helper child and ends the
// process there: that child keeps the variables its test hands it and never reaches the guarded
// path.
func TestEveryGuardingPackagesTestMainGuardsItsProcess(t *testing.T) {
	root := moduleRoot(t)
	for _, pkg := range guardingPackages {
		if fault := packageGuardFault(root, pkg); fault != "" {
			t.Errorf("%s: %s", pkg, fault)
		}
	}
}

// The check sees the shapes it refuses, so it cannot pass by reading nothing: a TestMain that
// calls MustGuard nowhere, one that calls it behind a branch, one that calls it below m.Run, and
// one whose helper-child branch falls through into the tests instead of ending the process. The
// two shapes the nine packages are written in — the guard first, and the guard after a branch that
// exits — pass it.
func TestTheGuardCheckSeesTheShapesItRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"the guard first", "\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n", ""},
		{
			"the guard after a helper child's branch",
			"\tif os.Getenv(\"HELPER\") != \"\" {\n\t\tos.Exit(m.Run())\n\t}\n\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n",
			"",
		},
		{"no guard at all", "\tos.Exit(m.Run())\n", "calls no fixtureenv.MustGuard"},
		{
			"the guard behind a branch",
			"\tif os.Getenv(\"HELPER\") == \"\" {\n\t\tfixtureenv.MustGuard()\n\t}\n\tos.Exit(m.Run())\n",
			"inside a branch of its body",
		},
		{
			"the guard below m.Run",
			"\tcode := m.Run()\n\tfixtureenv.MustGuard()\n\tos.Exit(code)\n",
			"runs the tests at main_test.go:11:2, before the fixtureenv.MustGuard at main_test.go:12:2",
		},
		{
			"a branch that runs the tests and falls through",
			"\tif os.Getenv(\"HELPER\") != \"\" {\n\t\tm.Run()\n\t}\n\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n",
			"runs the tests at main_test.go:11:2, before the fixtureenv.MustGuard at main_test.go:14:2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fault := sourceGuardFault(t, tc.body)
			if tc.want == "" {
				if fault != "" {
					t.Fatalf("the check refused a guarding TestMain: %s", fault)
				}
				return
			}
			if !strings.Contains(fault, tc.want) {
				t.Fatalf("the check said %q, want it to name %q", fault, tc.want)
			}
		})
	}
}

// sourceGuardFault runs the check over a TestMain with body, in a file shaped like the guarding
// packages' own: this package's import, os, testing, and TestMain's opening line on line 10, so a
// case can name the line it expects the fault at.
func sourceGuardFault(t *testing.T, body string) string {
	t.Helper()
	src := "package main_test\n\nimport (\n\t\"os\"\n\t\"testing\"\n\n\t\"" + fixtureenvPath + "\"\n)\n\n" +
		"func TestMain(m *testing.M) {\n" + body + "}\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main_test.go", src, 0)
	if err != nil {
		t.Fatalf("parse the case's source: %v", err)
	}
	fault, found := testMainFault(fset, file)
	if !found {
		t.Fatalf("the case's source declares no TestMain:\n%s", src)
	}
	return fault
}

// moduleRoot is the daemon module's root directory, found by walking up from the directory the
// tests run in, which is this package's own, to the one holding go.mod. Every path in
// guardingPackages is read from there.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("read the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above the working directory")
		}
		dir = parent
	}
}

// packageGuardFault is why the package at pkg, under the module root, does not guard its test
// process, as a sentence that follows the package's name, and "" when it does. Every test file of
// the package is read, the e2e-tagged ones included, since build tags decide which tests run and
// not which TestMain guards them.
func packageGuardFault(root, pkg string) string {
	entries, err := os.ReadDir(filepath.Join(root, pkg))
	if err != nil {
		return fmt.Sprintf("its directory cannot be read (%v); a package of this list that moved or was renamed moves here too", err)
	}
	fset := token.NewFileSet()
	var faults, files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, pkg, name))
		if err != nil {
			return fmt.Sprintf("%s cannot be read: %v", name, err)
		}
		// The file is parsed under its path from the module root, so every position a fault names
		// is the path a reader opens.
		file, err := parser.ParseFile(fset, filepath.Join(pkg, name), src, 0)
		if err != nil {
			return fmt.Sprintf("%s does not parse: %v", name, err)
		}
		fault, found := testMainFault(fset, file)
		if !found {
			continue
		}
		faults = append(faults, fault)
		files = append(files, name)
	}
	switch len(faults) {
	case 0:
		return "no test file of it declares func TestMain(m *testing.M), and a package of this list guards its test process in one"
	case 1:
		return faults[0]
	default:
		return fmt.Sprintf("it declares TestMain in more than one file (%s)", strings.Join(files, ", "))
	}
}

// testMainFault is why file's TestMain does not guard its test process, and "" when it guards.
// found is whether file declares a TestMain at all.
func testMainFault(fset *token.FileSet, file *ast.File) (fault string, found bool) {
	fn := testMainIn(file)
	if fn == nil {
		return "", false
	}
	main := testMain{
		fset:  fset,
		fn:    fn,
		guard: importName(file, fixtureenvPath),
		os:    importName(file, "os"),
		tests: testsParameter(fn),
	}
	return main.fault(), true
}

// testMainIn is file's func TestMain(m *testing.M), nil when it declares none.
func testMainIn(file *ast.File) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "TestMain" || fn.Recv != nil || fn.Body == nil {
			continue
		}
		if len(fn.Type.Params.List) == 1 && isSelector(fn.Type.Params.List[0].Type, "testing", "M") {
			return fn
		}
	}
	return nil
}

// testMain is one package's TestMain as the check reads it: the declaration, the names its file
// imports this package and os under, and the name its *testing.M parameter carries.
type testMain struct {
	fset      *token.FileSet
	fn        *ast.FuncDecl
	guard, os string
	tests     string
}

// fault is why this TestMain does not guard its test process, and "" when it does: it holds a
// fixtureenv.MustGuard call in its own body, and nothing before that call runs the tests except a
// branch that ends the process where it stands.
func (m testMain) fault() string {
	guard := -1
	for i, stmt := range m.fn.Body.List {
		if m.guardCall(stmt) {
			guard = i
			break
		}
	}
	if guard < 0 {
		if nested := m.firstNestedGuard(); nested != token.NoPos {
			return fmt.Sprintf("TestMain (%s) calls fixtureenv.MustGuard at %s, inside a branch of its body, where a run that takes another branch reaches the tests unguarded",
				m.at(m.fn.Pos()), m.at(nested))
		}
		return fmt.Sprintf("TestMain (%s) calls no fixtureenv.MustGuard, so its tests run on a process that still holds the pod's workspace, git configuration and GitHub credential",
			m.at(m.fn.Pos()))
	}
	for _, stmt := range m.fn.Body.List[:guard] {
		if !m.mentionsTests(stmt) || m.escapes(stmt) {
			continue
		}
		return fmt.Sprintf("TestMain (%s) runs the tests at %s, before the fixtureenv.MustGuard at %s",
			m.at(m.fn.Pos()), m.at(stmt.Pos()), m.at(m.fn.Body.List[guard].Pos()))
	}
	return ""
}

// guardCall reports whether stmt is a call of fixtureenv.MustGuard and nothing else: a statement
// of TestMain's own body, which every run of it makes.
func (m testMain) guardCall(stmt ast.Stmt) bool {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expr.X.(*ast.CallExpr)
	return ok && isSelector(call.Fun, m.guard, "MustGuard")
}

// firstNestedGuard is where TestMain calls fixtureenv.MustGuard somewhere other than its own body
// — inside a branch, a loop or a function literal — so a TestMain that moved its guard out of
// reach is told apart from one that has none.
func (m testMain) firstNestedGuard() token.Pos {
	found := token.NoPos
	ast.Inspect(m.fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, m.guard, "MustGuard") || found != token.NoPos {
			return found == token.NoPos
		}
		found = call.Pos()
		return false
	})
	return found
}

// escapes reports whether every run of stmt that reaches the tests ends the test process there,
// instead of falling through to the rest of TestMain: the branch Guard's doc asks a guarding
// TestMain to put its guard after, where the test binary runs as a helper child of a guarded test
// process and keeps the variables that test hands it. A statement that runs the tests and goes on
// — a bare call, a branch with no exit — does not escape, and the check refuses it before the
// guard. The condition of such a branch may not run the tests itself, and a block that ends any
// other way than os.Exit or a return (a panic, a call that exits for it) counts as falling
// through, since the check reads the statement rather than guessing what it does.
func (m testMain) escapes(stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case *ast.IfStmt:
		if m.mentionsTests(s.Init) || m.mentionsTests(s.Cond) {
			return false
		}
		if m.mentionsTests(s.Body) && !m.endsTheProcess(s.Body) {
			return false
		}
		if block, ok := s.Else.(*ast.BlockStmt); ok {
			return !m.mentionsTests(block) || m.endsTheProcess(block)
		}
		return s.Else == nil || !m.mentionsTests(s.Else) || m.escapes(s.Else)
	case *ast.ForStmt:
		return m.everyStatementEscapes(s.Body)
	case *ast.RangeStmt:
		return m.everyStatementEscapes(s.Body)
	case *ast.BlockStmt:
		return m.everyStatementEscapes(s)
	}
	return false
}

// everyStatementEscapes reports whether every statement of block that reaches the tests escapes.
func (m testMain) everyStatementEscapes(block *ast.BlockStmt) bool {
	for _, stmt := range block.List {
		if m.mentionsTests(stmt) && !m.escapes(stmt) {
			return false
		}
	}
	return true
}

// endsTheProcess reports whether a run of block ends TestMain where block ends — os.Exit, or a
// return from TestMain itself — so nothing after it in the body runs.
func (m testMain) endsTheProcess(block *ast.BlockStmt) bool {
	if len(block.List) == 0 {
		return false
	}
	switch last := block.List[len(block.List)-1].(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.ExprStmt:
		call, ok := last.X.(*ast.CallExpr)
		return ok && isSelector(call.Fun, m.os, "Exit")
	}
	return false
}

// mentionsTests reports whether node names TestMain's *testing.M parameter, the one way anything
// in the body can run the package's tests: m.Run, or m handed to a helper that runs it
// (testpg.Main, testbuild.Main, testnats.Main).
func (m testMain) mentionsTests(node ast.Node) bool {
	if node == nil || m.tests == "" || m.tests == "_" {
		return false
	}
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && ident.Name == m.tests {
			found = true
		}
		return !found
	})
	return found
}

// at is where pos is, as the path from the module root and the line.
func (m testMain) at(pos token.Pos) string {
	return m.fset.Position(pos).String()
}

// testsParameter is the name TestMain's *testing.M parameter carries, "" when it has none.
func testsParameter(fn *ast.FuncDecl) string {
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) != 1 {
		return ""
	}
	return params[0].Names[0].Name
}

// importName is the name file imports path under: its own name where it renames it, the path's
// last element otherwise, and "" where it does not import it at all.
func importName(file *ast.File, path string) string {
	for _, spec := range file.Imports {
		if spec.Path.Value != strconv.Quote(path) {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}

// isSelector reports whether expr is pkg.name, or *pkg.name, read as written.
func isSelector(expr ast.Expr, pkg, name string) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || pkg == "" || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}
