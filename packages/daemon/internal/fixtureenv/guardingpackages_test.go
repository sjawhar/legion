package fixtureenv_test

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
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

// guardingPackages are the packages of this module whose tests run real git or jj — as child
// processes, in-process through the code under test, or both — so each one's TestMain calls
// fixtureenv.MustGuard before any test of it runs (docs/testing.md, "A test that starts jj or git
// inherits no pod's environment"). A package joins the list when a test of it starts git or jj: in
// a Legion issue pod an unguarded one inherits the pod's own LEGION_WORKSPACE, GIT_CONFIG_* and
// GitHub credential, and the shim's rescue test pushed the pod's own workspace to the real origin
// that way. Every package's tests run in a process of their own, so each entry here is one process
// to guard, and a guard deleted from one of them is invisible to every other package's tests.
//
// The check below reads these packages, and walks the module's own test files for a TestMain that
// guards outside the list: a package that takes up the guard joins the list in the change that
// gives it one, and the walk names it where it has not. The module's other TestMains guard nothing
// today, and bringing one of them under the guard is a change of its own.
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
// runs the tests. This is the one check for every package of the list, in place of a copy of a
// test per package, and it is a check of the source rather than of the running process: a test
// can only see the environment of the process it runs in, so no test of this package could see
// another package's TestMain drop its guard, and the deleted copies each saw only their own.
//
// It reads the TestMain of each package and refuses, naming the package: a TestMain with no
// MustGuard at all, one whose only MustGuard sits inside a branch (a run that takes another branch
// would reach the tests unguarded), one that reaches the tests before it — below m.Run, or after a
// branch that runs them and falls through, or under a condition that does not say a helper-child
// marker is set — and one a build constraint keeps out of the package's default build. The one
// shape it allows before the guard is the branch Guard's own doc asks for, where the test binary
// runs as a helper child and ends the process there: that child keeps the variables its test hands
// it and never reaches the guarded path.
//
// The list is kept by hand, so the module's own test files are walked as well and every package
// whose TestMain guards and which the list does not hold is named: a package that takes up the
// guard without joining the list would be read by nothing here, and the guard it dropped later
// would be no package's fault.
func TestEveryGuardingPackagesTestMainGuardsItsProcess(t *testing.T) {
	root := moduleRoot(t)
	for _, pkg := range guardingPackages {
		if fault := packageGuardFault(root, pkg); fault != "" {
			t.Errorf("%s: %s", pkg, fault)
		}
	}
	outside, err := packagesGuardingOutsideTheList(root, guardingPackages)
	if err != nil {
		t.Fatalf("read the module's test files: %v", err)
	}
	for _, pkg := range outside {
		t.Errorf("%s: its TestMain calls fixtureenv.MustGuard and guardingPackages does not hold it, so this check reads neither it nor the day that guard goes; add the package to the list", pkg)
	}
}

// The check sees the shapes it refuses, so it cannot pass by reading nothing: a TestMain that
// calls MustGuard nowhere, one that calls it behind a branch, one that calls it below m.Run, one
// whose helper-child branch falls through into the tests instead of ending the process, one whose
// branch runs the tests under a condition that does not say a helper-child marker is set or in
// the else where it is not, and one a build constraint keeps out of the package's default build.
// The shapes the listed packages are written in — the guard first, and the guard after
// helper-child branches that each end the process, whether the branch stands on its own or in a
// loop over the package's markers — pass it.
func TestTheGuardCheckSeesTheShapesItRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		// constraint is the build constraint the case's file carries, "" where it carries none;
		// it stands above the package clause and moves every line of the body below it.
		constraint string
		body       string
		want       string
	}{
		{name: "the guard first", body: "\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n"},
		{
			name: "the guard after a helper child's branch",
			body: "\tif os.Getenv(\"HELPER\") != \"\" {\n\t\tos.Exit(m.Run())\n\t}\n\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n",
		},
		{
			name: "the guard after a helper child's LookupEnv branch",
			body: "\tif _, ok := os.LookupEnv(\"HELPER\"); ok {\n\t\tos.Exit(m.Run())\n\t}\n\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n",
		},
		{
			name: "the guard after a loop over the package's helper-child markers",
			body: "\tfor _, name := range []string{\"HELPER\"} {\n\t\tif os.Getenv(name) != \"\" {\n\t\t\tos.Exit(m.Run())\n\t\t}\n\t}\n\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n",
		},
		{
			name: "no guard at all",
			body: "\tos.Exit(m.Run())\n",
			want: "calls no fixtureenv.MustGuard",
		},
		{
			name: "the guard behind a branch",
			body: "\tif os.Getenv(\"HELPER\") == \"\" {\n\t\tfixtureenv.MustGuard()\n\t}\n\tos.Exit(m.Run())\n",
			want: "inside a branch of its body",
		},
		{
			name: "the guard below m.Run",
			body: "\tcode := m.Run()\n\tfixtureenv.MustGuard()\n\tos.Exit(code)\n",
			want: "runs the tests at main_test.go:11:2, before the fixtureenv.MustGuard at main_test.go:12:2",
		},
		{
			name: "a branch that runs the tests and falls through",
			body: "\tif os.Getenv(\"HELPER\") != \"\" {\n\t\tm.Run()\n\t}\n\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n",
			want: "runs the tests at main_test.go:11:2, before the fixtureenv.MustGuard at main_test.go:14:2",
		},
		{
			name: "a branch that runs the tests where the marker is unset",
			body: "\tif os.Getenv(\"HELPER\") == \"\" {\n\t\tos.Exit(m.Run())\n\t}\n\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n",
			want: "runs the tests at main_test.go:11:2 under a condition (main_test.go:11:5) that does not say a helper-child marker is set",
		},
		{
			name: "a loop whose branch runs the tests on something other than a marker",
			body: "\tfor _, name := range []string{\"HELPER\"} {\n\t\tif name != \"\" {\n\t\t\tos.Exit(m.Run())\n\t\t}\n\t}\n\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n",
			want: "runs the tests at main_test.go:11:2 under a condition (main_test.go:12:6) that does not say a helper-child marker is set",
		},
		{
			name: "an else that runs the tests where the marker is unset",
			body: "\tif os.Getenv(\"HELPER\") != \"\" {\n\t\tos.Exit(m.Run())\n\t} else {\n\t\tos.Exit(m.Run())\n\t}\n\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n",
			want: "runs the tests at main_test.go:11:2, before the fixtureenv.MustGuard at main_test.go:16:2",
		},
		{
			name:       "a TestMain a build constraint keeps out of the default build",
			constraint: "//go:build e2e",
			body:       "\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n",
			want:       "TestMain (main_test.go:12:1) is in a file that carries the build constraint //go:build e2e",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fault := sourceGuardFault(t, tc.constraint, tc.body)
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
// case can name the line it expects the fault at. A case that passes a build constraint puts it
// above the package clause, where it moves every line below it down by two.
func sourceGuardFault(t *testing.T, constraint, body string) string {
	t.Helper()
	src := "package main_test\n\nimport (\n\t\"os\"\n\t\"testing\"\n\n\t\"" + fixtureenvPath + "\"\n)\n\n" +
		"func TestMain(m *testing.M) {\n" + body + "}\n"
	if constraint != "" {
		src = constraint + "\n\n" + src
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main_test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse the case's source: %v", err)
	}
	fault, found := testMainFault(fset, file)
	if !found {
		t.Fatalf("the case's source declares no TestMain:\n%s", src)
	}
	return fault
}

// The walk names a package that guards its test process and is not on the list, which is the one
// way a package that takes up the guard without joining the list is caught, and names neither a
// package the list holds nor one whose TestMain guards nothing. It reads a module of its own, so
// it refuses nothing of the real one: the directories it skips carry a guarding TestMain each, so
// a skip that reached a package of the module would name one of them here.
func TestTheWalkNamesAGuardingPackageTheListDoesNotHold(t *testing.T) {
	root := t.TempDir()
	for _, pkg := range []string{
		"internal/listed", "internal/unlisted", "internal/deep/under/it",
		"internal/testdata", "vendor/example.com/other", ".git/hooks", "_ignored",
	} {
		writeTestMain(t, root, pkg, "\tfixtureenv.MustGuard()\n\tos.Exit(m.Run())\n")
	}
	writeTestMain(t, root, "internal/unguarded", "\tos.Exit(m.Run())\n")
	outside, err := packagesGuardingOutsideTheList(root, []string{"internal/listed"})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	want := []string{"internal/deep/under/it", "internal/unlisted"}
	if !slices.Equal(outside, want) {
		t.Fatalf("the walk named %q, want %q", outside, want)
	}
}

// writeTestMain writes pkg/main_test.go under root, a test file whose TestMain has body, so a
// case can give a package of a module of its own a TestMain that guards or one that does not.
func writeTestMain(t *testing.T, root, pkg, body string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(pkg))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make %s: %v", pkg, err)
	}
	src := "package " + filepath.Base(pkg) + "_test\n\nimport (\n\t\"os\"\n\t\"testing\"\n\n\t\"" +
		fixtureenvPath + "\"\n)\n\nfunc TestMain(m *testing.M) {\n" + body + "}\n"
	if err := os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write %s: %v", pkg, err)
	}
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
// the package is read, the e2e-tagged ones included, since a TestMain guards every test of the
// package and not the tests of its own file; a TestMain the file's own build constraint keeps out
// of the default build is a fault of its own, since the build a plain go test makes then declares
// no TestMain at all and guards nothing.
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
		file, err := parser.ParseFile(fset, filepath.Join(pkg, name), src, parser.ParseComments)
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

// packagesGuardingOutsideTheList are the packages under root whose TestMain calls
// fixtureenv.MustGuard and which listed does not hold, as paths from root in the order the walk
// reads them. Every directory under root is read but the ones that hold no package of the module
// — testdata, whose sources are a test's material and no package, and a vendored tree, which
// holds other modules' — and the ones the go command itself leaves out of a build, those whose
// name begins with "." or "_". A TestMain that calls MustGuard under no import of this package is
// this package's own, which the list holds.
func packagesGuardingOutsideTheList(root string, listed []string) ([]string, error) {
	fset := token.NewFileSet()
	var outside []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && (name == "testdata" || name == "vendor" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("%s does not parse: %w", path, err)
		}
		if !guardsItsTestProcess(file) {
			return nil
		}
		dir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(dir)
		if slices.Contains(listed, pkg) || slices.Contains(outside, pkg) {
			return nil
		}
		outside = append(outside, pkg)
		return nil
	})
	return outside, err
}

// guardsItsTestProcess reports whether file declares a TestMain that calls fixtureenv.MustGuard,
// anywhere in its body: a package that has taken up the guard, which guardingPackages is to hold
// and the check above is to read. Why the guard sits where it does is that check's own answer;
// this one asks only whether the package means to guard at all.
func guardsItsTestProcess(file *ast.File) bool {
	fn := testMainIn(file)
	guard := importName(file, fixtureenvPath)
	if fn == nil || guard == "" {
		return false
	}
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && isSelector(call.Fun, guard, "MustGuard") {
			found = true
		}
		return !found
	})
	return found
}

// testMainFault is why file's TestMain does not guard its test process, and "" when it guards.
// found is whether file declares a TestMain at all.
func testMainFault(fset *token.FileSet, file *ast.File) (fault string, found bool) {
	fn := testMainIn(file)
	if fn == nil {
		return "", false
	}
	if line := buildConstraint(file); line != "" {
		return fmt.Sprintf("TestMain (%s) is in a file that carries the build constraint %s, so the build a plain go test of the package makes declares no TestMain and runs every test of it unguarded",
			fset.Position(fn.Pos()).String(), line), true
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

// buildConstraint is the build constraint file carries, as the file writes it, and "" when it
// carries none: a //go:build line (or the older // +build) in a comment group above the package
// clause, which is where the go command reads one. A file under a constraint is left out of the
// package's default build, so a TestMain of its own guards no test that build runs.
func buildConstraint(file *ast.File) string {
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			break
		}
		for _, comment := range group.List {
			if constraint.IsGoBuild(comment.Text) || constraint.IsPlusBuild(comment.Text) {
				return comment.Text
			}
		}
	}
	return ""
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
		if !m.mentionsTests(stmt) {
			continue
		}
		if cond := m.unguardedTestCondition(stmt); cond != token.NoPos {
			return fmt.Sprintf("TestMain (%s) runs the tests at %s under a condition (%s) that does not say a helper-child marker is set, so a run that carries no marker — every ordinary run of the package's tests — takes that branch too and runs them before the fixtureenv.MustGuard at %s",
				m.at(m.fn.Pos()), m.at(stmt.Pos()), m.at(cond), m.at(m.fn.Body.List[guard].Pos()))
		}
		if m.escapes(stmt) {
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
// guard. So does a branch that ends the process on anything other than a helper-child marker
// being set (marksAHelperChild), the condition that tells a child its parent started from one of
// those runs the package's own tests; a branch's else runs where no such marker is set, so a run
// of the tests there is refused as well. The condition of such a branch may not run the tests
// itself, and a block that ends any other way than os.Exit or a return (a panic, a call that
// exits for it) counts as falling through, since the check reads the statement rather than
// guessing what it does.
func (m testMain) escapes(stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case *ast.IfStmt:
		if m.mentionsTests(s.Init) || m.mentionsTests(s.Cond) {
			return false
		}
		if m.mentionsTests(s.Body) && (!m.endsTheProcess(s.Body) || !m.marksAHelperChild(s)) {
			return false
		}
		if block, ok := s.Else.(*ast.BlockStmt); ok {
			return !m.mentionsTests(block)
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

// marksAHelperChild reports whether branch runs under a helper-child marker being set, the one
// condition a TestMain may run the tests before its guard under: a process that holds the marker
// was re-executed by a test of a guarded process, which handed it that variable, so it holds
// nothing of the pod's and the tests it runs are the one helper the marker names. The two shapes
// the module's TestMains write it in, read as written: os.Getenv(NAME) != "" (internal/daemon,
// internal/launcher) and _, ok := os.LookupEnv(NAME); ok. Any other condition — the inverted
// os.Getenv(NAME) == "", one on something that is no environment marker, none of that shape at
// all — leaves the ordinary run of the package's tests taking the branch, where no guard reaches
// it.
func (m testMain) marksAHelperChild(branch *ast.IfStmt) bool {
	if compare, ok := branch.Cond.(*ast.BinaryExpr); ok {
		call, isCall := compare.X.(*ast.CallExpr)
		return branch.Init == nil && compare.Op == token.NEQ && isCall &&
			isSelector(call.Fun, m.os, "Getenv") && len(call.Args) == 1 && isEmptyString(compare.Y)
	}
	set, ok := branch.Cond.(*ast.Ident)
	if !ok || set.Name == "_" {
		return false
	}
	init, ok := branch.Init.(*ast.AssignStmt)
	if !ok || len(init.Lhs) != 2 || len(init.Rhs) != 1 {
		return false
	}
	call, ok := init.Rhs[0].(*ast.CallExpr)
	if !ok || !isSelector(call.Fun, m.os, "LookupEnv") || len(call.Args) != 1 {
		return false
	}
	found, ok := init.Lhs[1].(*ast.Ident)
	return ok && found.Name == set.Name
}

// unguardedTestCondition is where the first branch of stmt that runs the tests and ends the
// process there reads its condition, when that condition does not say a helper-child marker is
// set, and token.NoPos when stmt holds no such branch. This is the one shape that reads as an
// escape and is none: the run it sends into the tests is the ordinary one, so the whole package
// runs unguarded and the guard below it is never reached. The branch is looked for inside stmt as
// well as at it, since a package re-executed as more than one helper writes the branch in a loop
// over its markers (internal/launcher).
func (m testMain) unguardedTestCondition(stmt ast.Stmt) token.Pos {
	found := token.NoPos
	ast.Inspect(stmt, func(node ast.Node) bool {
		branch, ok := node.(*ast.IfStmt)
		if !ok || found != token.NoPos {
			return found == token.NoPos
		}
		if m.mentionsTests(branch.Body) && m.endsTheProcess(branch.Body) && !m.marksAHelperChild(branch) {
			found = branch.Cond.Pos()
			return false
		}
		return true
	})
	return found
}

// isEmptyString reports whether expr is the empty string literal, as a helper-child branch's
// condition compares its marker to.
func isEmptyString(expr ast.Expr) bool {
	literal, ok := expr.(*ast.BasicLit)
	return ok && literal.Kind == token.STRING && literal.Value == `""`
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
