//go:build !race

// The guard is static: it reads the module's source, so the race detector adds nothing to it,
// and leaving it out of the -race run spares that job building every dependency's export data a
// second time.

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// lockEffect is a lock a function's own statements, or the functions it calls, take in the
// transaction they run in: a project's copy lock (docs.lockProjectCopies), a
// doc_settlements_pending row, or the events' commit-order lock an event append takes.
type lockEffect uint8

const (
	takesCopyLock lockEffect = 1 << iota
	takesPendingRow
	appendsEvent
)

var (
	pendingRowWrite = regexp.MustCompile(`\b(?:insert into|delete from|update) doc_settlements_pending\b`)
	eventInsert     = regexp.MustCompile(`\binsert into events\b`)
)

// TestCopyLockComesBeforeEveryPendingRowAndEvent enforces the order the copy lock's deadlock
// freedom rests on (docs.lockProjectCopies): no transaction takes a project's copy lock after it
// has written a doc_settlements_pending row or appended an event. A holder of the copy lock goes
// on to take other documents' pending rows and the events' commit-order lock, so a transaction
// waiting on the copy lock while holding either could close a cycle with it.
//
// It type-checks the Dispatch server's packages of this module from source, the packages they
// import from their export data (go list -export), and finds each lock from the SQL that takes
// it, read as TestNoForUpdateOnOwnerTables reads statements: the copy lock from
// docs.projectCopiesLock, a pending row from an insert into, update of or delete from
// doc_settlements_pending, and an event append from an insert into events. A function takes a
// lock when its own statements do or a function it calls takes it. Each function body, a function
// literal's included, is then read in statement order, and a call that takes the copy lock is
// refused when a call before it, on some path that reaches it, took a pending row or appended an
// event.
//
// A call reaches what the type checker says it names: a function or method, every method of this
// module's types that implements an interface method it calls (s.deps.Docs.SettleCopiesOf reaches
// docs.Service's), and, for a call through a variable or a struct field (transition.Apply,
// options.WriteBlock), every function this module stores in that variable or field. A function
// passed as an argument runs at the call, except to time.AfterFunc or a Go method, which run it on
// a goroutine of its own and so in a transaction of its own; go statements are skipped for the
// same reason, and deferred calls count toward what a function takes but not toward its order. A
// branch that ends in a return or a panic carries nothing past its if. The check does not tell
// transactions apart, so a function that commits one transaction and takes the copy lock in the
// next reads as one; none does. TestCopyLockOrderGuardFollowsTheLockTakers holds the resolution
// to the chains this rule is about, so the check cannot pass by losing them.
func TestCopyLockComesBeforeEveryPendingRowAndEvent(t *testing.T) {
	graph := copyLockGraph(t)
	var offences []string
	for _, fn := range graph.functions {
		offences = append(offences, graph.orderOffences(fn)...)
	}
	slices.Sort(offences)
	offences = slices.Compact(offences)
	if len(offences) > 0 {
		t.Fatalf("the copy lock is taken after a pending row or an event append:\n\t%s\n"+
			"Take the copy lock (lockProjectCopies, through copiedAskSources, owedCopiesOf or "+
			"SettleCopiesOf) before any doc_settlements_pending row and any event append: its holder "+
			"goes on to take both, so a transaction waiting on it while holding either can deadlock.",
			strings.Join(offences, "\n\t"))
	}
}

// TestCopyLockOrderGuardFollowsTheLockTakers holds the guard's resolution to the chains its rule
// is about: if a rename or a refactor hid one of these from it, the guard would pass by seeing
// nothing.
func TestCopyLockOrderGuardFollowsTheLockTakers(t *testing.T) {
	graph := copyLockGraph(t)
	for _, want := range []struct {
		function string
		effect   lockEffect
	}{
		{"docs.lockProjectCopies", takesCopyLock},
		{"docs.Service.SettleCopiesOf", takesCopyLock},
		{"docs.Service.reconcileAskBlocks", takesCopyLock},
		{"docs.Service.settleRoomWithin", takesCopyLock | takesPendingRow | appendsEvent},
		{"api.server.transitionAskTx", takesCopyLock | takesPendingRow},
		{"api.server.closeAskTx", takesCopyLock | takesPendingRow | appendsEvent},
		{"docs.PgVersioned.appendUpdateTxClass", takesPendingRow},
		{"docs.Ledger.Commit", takesPendingRow},
		{"api.server.appendEvent", appendsEvent},
		{"api.server.storeArtifact", takesPendingRow | appendsEvent},
		{"api.server.createIssue", takesPendingRow | appendsEvent},
	} {
		fn := graph.named[want.function]
		if fn == nil {
			t.Errorf("the guard finds no function %s", want.function)
			continue
		}
		if fn.reach&want.effect != want.effect {
			t.Errorf("the guard sees %s take %s, want %s", want.function, fn.reach, want.effect)
		}
	}
}

func (effect lockEffect) String() string {
	var names []string
	if effect&takesCopyLock != 0 {
		names = append(names, "the copy lock")
	}
	if effect&takesPendingRow != 0 {
		names = append(names, "a pending row")
	}
	if effect&appendsEvent != 0 {
		names = append(names, "an event append")
	}
	if len(names) == 0 {
		return "nothing"
	}
	return strings.Join(names, ", ")
}

// lockFunction is one function body the guard reads: a declared function or method, or a
// function literal.
type lockFunction struct {
	name   string
	pkg    string
	body   *ast.BlockStmt
	source moduleSource
	seeds  lockEffect
	reach  lockEffect
}

type lockGraph struct {
	root      string
	info      *types.Info
	functions []*lockFunction
	named     map[string]*lockFunction
	declared  map[*types.Func]*lockFunction
	literals  map[*ast.FuncLit]*lockFunction
	// stored are the functions this module assigns to each variable or struct field, which a
	// call through it runs.
	stored map[*types.Var][]*lockFunction
	// concrete are this module's named types that are not interfaces, the candidates for an
	// interface method call.
	concrete        []types.Type
	implementations map[*types.Func][]*lockFunction
}

// listedPackage is the part of `go list -json` the guard reads.
type listedPackage struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
	Module     *struct{ Main bool }
}

// moduleImporter type-checks this module's packages from source, recording their syntax and type
// information, and imports every other package from its export data.
type moduleImporter struct {
	fset     *token.FileSet
	listed   map[string]listedPackage
	exported types.Importer
	checked  map[string]*types.Package
	info     *types.Info
	sources  []moduleSource
	problems []error
}

func (m *moduleImporter) Import(path string) (*types.Package, error) {
	if pkg, ok := m.checked[path]; ok {
		return pkg, nil
	}
	listed, ok := m.listed[path]
	if !ok || listed.Module == nil || !listed.Module.Main {
		return m.exported.Import(path)
	}
	var files []*ast.File
	for _, name := range listed.GoFiles {
		file, err := parser.ParseFile(m.fset, filepath.Join(listed.Dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
		m.sources = append(m.sources, moduleSource{path: filepath.Join(listed.Dir, name), file: file, positions: m.fset})
	}
	config := types.Config{Importer: m, Error: func(err error) { m.problems = append(m.problems, err) }}
	pkg, _ := config.Check(path, m.fset, files, m.info)
	m.checked[path] = pkg
	return pkg, nil
}

func copyLockGraph(t *testing.T) *lockGraph {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Dir,GoFiles,Export,Module", "./cmd/dispatch")
	command.Dir = root
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("list the Dispatch server's packages: %v\n%s", err, stderr.String())
	}
	listed := map[string]listedPackage{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		listed[pkg.ImportPath] = pkg
	}
	fset := token.NewFileSet()
	modules := &moduleImporter{
		fset:   fset,
		listed: listed,
		exported: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
			pkg, ok := listed[path]
			if !ok || pkg.Export == "" {
				return nil, fmt.Errorf("no export data for %s", path)
			}
			return os.Open(pkg.Export)
		}),
		checked: map[string]*types.Package{},
		info: &types.Info{
			Defs:       map[*ast.Ident]types.Object{},
			Uses:       map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		},
	}
	if _, err := modules.Import("github.com/sjawhar/envoy/cmd/dispatch"); err != nil {
		t.Fatalf("type-check the Dispatch server: %v", err)
	}
	if len(modules.problems) > 0 {
		t.Fatalf("type-check the Dispatch server: %v", errors.Join(modules.problems...))
	}

	constants := stringConstants(t, modules.sources)
	copyLock, ok := constants["docs.projectCopiesLock"]
	if !ok {
		t.Fatal("docs.projectCopiesLock, the statement the copy lock is taken with, is gone: point the guard at its replacement")
	}
	copyLock = sqlWhitespace.ReplaceAllString(strings.ToLower(copyLock), " ")
	graph := &lockGraph{
		root:            root,
		info:            modules.info,
		named:           map[string]*lockFunction{},
		declared:        map[*types.Func]*lockFunction{},
		literals:        map[*ast.FuncLit]*lockFunction{},
		stored:          map[*types.Var][]*lockFunction{},
		implementations: map[*types.Func][]*lockFunction{},
	}
	for _, pkg := range modules.checked {
		for _, name := range pkg.Scope().Names() {
			typeName, ok := pkg.Scope().Lookup(name).(*types.TypeName)
			if !ok || typeName.IsAlias() || types.IsInterface(typeName.Type()) {
				continue
			}
			if named, ok := typeName.Type().(*types.Named); ok && named.TypeParams().Len() == 0 {
				graph.concrete = append(graph.concrete, named, types.NewPointer(named))
			}
		}
	}
	for _, source := range modules.sources {
		graph.addFile(source)
	}
	for _, source := range modules.sources {
		graph.addStored(source)
	}
	for _, fn := range graph.functions {
		fn.seeds = statementLocks(fn, constants, copyLock)
		fn.reach = fn.seeds
	}
	for changed := true; changed; {
		changed = false
		for _, fn := range graph.functions {
			reach := fn.reach
			for _, callees := range graph.calls(fn) {
				for _, callee := range callees {
					reach |= callee.reach
				}
			}
			if reach != fn.reach {
				fn.reach, changed = reach, true
			}
		}
	}
	return graph
}

// addFile registers every function, method and function literal source declares.
func (g *lockGraph) addFile(source moduleSource) {
	pkg := source.file.Name.Name
	for _, declared := range source.file.Decls {
		decl, ok := declared.(*ast.FuncDecl)
		if !ok || decl.Body == nil {
			continue
		}
		object, ok := g.info.Defs[decl.Name].(*types.Func)
		if !ok {
			continue
		}
		fn := &lockFunction{name: functionName(object), pkg: pkg, body: decl.Body, source: source}
		g.add(fn)
		g.named[fn.name] = fn
		g.declared[object] = fn
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			if lit, ok := node.(*ast.FuncLit); ok {
				g.add(&lockFunction{
					name: fmt.Sprintf("the function literal at %s in %s", g.line(source, lit), fn.name),
					pkg:  pkg, body: lit.Body, source: source,
				})
				g.literals[lit] = g.functions[len(g.functions)-1]
			}
			return true
		})
	}
}

func (g *lockGraph) add(fn *lockFunction) {
	g.functions = append(g.functions, fn)
}

func (g *lockGraph) line(source moduleSource, node ast.Node) string {
	path, err := filepath.Rel(g.root, source.path)
	if err != nil {
		path = source.path
	}
	return filepath.ToSlash(path) + ":" + strconv.Itoa(source.positions.Position(node.Pos()).Line)
}

// functionName is a declared function's name as the guard reports it: docs.lockProjectCopies,
// docs.Service.SettleCopiesOf.
func functionName(fn *types.Func) string {
	name := fn.Pkg().Name() + "."
	if receiver := fn.Signature().Recv(); receiver != nil {
		recv := receiver.Type()
		if pointer, ok := recv.(*types.Pointer); ok {
			recv = pointer.Elem()
		}
		if named, ok := recv.(*types.Named); ok {
			name += named.Obj().Name() + "."
		}
	}
	return name + fn.Name()
}

// addStored records the functions source stores in a variable or a struct field: assigned,
// declared with one, or written into a struct literal.
func (g *lockGraph) addStored(source moduleSource) {
	store := func(target types.Object, value ast.Expr) {
		variable, ok := target.(*types.Var)
		if !ok {
			return
		}
		if functions := g.functionValue(value); len(functions) > 0 {
			variable = variable.Origin()
			g.stored[variable] = append(g.stored[variable], functions...)
		}
	}
	ast.Inspect(source.file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) != len(n.Rhs) {
				break
			}
			for index, value := range n.Rhs {
				switch target := ast.Unparen(n.Lhs[index]).(type) {
				case *ast.Ident:
					if object := g.info.Defs[target]; object != nil {
						store(object, value)
					} else {
						store(g.info.Uses[target], value)
					}
				case *ast.SelectorExpr:
					if selection, ok := g.info.Selections[target]; ok {
						store(selection.Obj(), value)
					} else {
						store(g.info.Uses[target.Sel], value)
					}
				}
			}
		case *ast.ValueSpec:
			for index, value := range n.Values {
				if index < len(n.Names) {
					store(g.info.Defs[n.Names[index]], value)
				}
			}
		case *ast.KeyValueExpr:
			if key, ok := n.Key.(*ast.Ident); ok {
				store(g.info.Uses[key], n.Value)
			}
		}
		return true
	})
}

// functionValue is what calling value would run: a function literal, or a function or method
// named as a value.
func (g *lockGraph) functionValue(value ast.Expr) []*lockFunction {
	switch v := ast.Unparen(value).(type) {
	case *ast.FuncLit:
		return []*lockFunction{g.literals[v]}
	case *ast.Ident:
		if fn, ok := g.info.Uses[v].(*types.Func); ok {
			return g.funcTargets(fn)
		}
	case *ast.SelectorExpr:
		if selection, ok := g.info.Selections[v]; ok {
			if fn, ok := selection.Obj().(*types.Func); ok && selection.Kind() != types.FieldVal {
				return g.funcTargets(fn)
			}
		} else if fn, ok := g.info.Uses[v.Sel].(*types.Func); ok {
			return g.funcTargets(fn)
		}
	}
	return nil
}

// funcTargets is the module's bodies a call of fn runs: its own, or, for an interface method,
// every method of this module's types that implements it.
func (g *lockGraph) funcTargets(fn *types.Func) []*lockFunction {
	fn = fn.Origin()
	receiver := fn.Signature().Recv()
	if receiver == nil || !types.IsInterface(receiver.Type()) {
		if body, ok := g.declared[fn]; ok {
			return []*lockFunction{body}
		}
		return nil
	}
	if implementations, ok := g.implementations[fn]; ok {
		return implementations
	}
	var implementations []*lockFunction
	iface, _ := receiver.Type().Underlying().(*types.Interface)
	for _, candidate := range g.concrete {
		if iface == nil || !types.Implements(candidate, iface) {
			continue
		}
		object, _, _ := types.LookupFieldOrMethod(candidate, true, fn.Pkg(), fn.Name())
		method, ok := object.(*types.Func)
		if !ok {
			continue
		}
		if body, ok := g.declared[method.Origin()]; ok && !slices.Contains(implementations, body) {
			implementations = append(implementations, body)
		}
	}
	g.implementations[fn] = implementations
	return implementations
}

// statementLocks is what fn's own statements take, read from the SQL they run, not counting the
// function literals inside it, which are functions of their own.
func statementLocks(fn *lockFunction, constants map[string]string, copyLock string) lockEffect {
	var effect lockEffect
	ast.Inspect(fn.body, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		expression, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		text, ok := literalText(expression, fn.pkg, constants)
		if !ok {
			return true
		}
		statement := sqlWhitespace.ReplaceAllString(strings.ToLower(text), " ")
		if strings.Contains(statement, copyLock) {
			effect |= takesCopyLock
		}
		if pendingRowWrite.MatchString(statement) {
			effect |= takesPendingRow
		}
		if eventInsert.MatchString(statement) {
			effect |= appendsEvent
		}
		return false
	})
	return effect
}

// calls lists, for each call fn makes on its own goroutine, the bodies it may run.
func (g *lockGraph) calls(fn *lockFunction) [][]*lockFunction {
	var calls [][]*lockFunction
	ast.Inspect(fn.body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit, *ast.GoStmt:
			return false
		case *ast.CallExpr:
			calls = append(calls, g.callees(n))
		}
		return true
	})
	return calls
}

// callees is what call runs on its caller's goroutine (see
// TestCopyLockComesBeforeEveryPendingRowAndEvent): what it names, and the functions it is passed
// unless it runs them on a goroutine of its own.
func (g *lockGraph) callees(call *ast.CallExpr) []*lockFunction {
	var callees []*lockFunction
	var named *types.Func
	fun := ast.Unparen(call.Fun)
	switch generic := fun.(type) {
	case *ast.IndexExpr:
		fun = generic.X
	case *ast.IndexListExpr:
		fun = generic.X
	}
	switch f := fun.(type) {
	case *ast.FuncLit:
		callees = append(callees, g.literals[f])
	case *ast.Ident:
		switch object := g.info.Uses[f].(type) {
		case *types.Func:
			named = object
		case *types.Var:
			callees = append(callees, g.stored[object.Origin()]...)
		}
	case *ast.SelectorExpr:
		if selection, ok := g.info.Selections[f]; ok {
			switch object := selection.Obj().(type) {
			case *types.Func:
				named = object
			case *types.Var:
				callees = append(callees, g.stored[object.Origin()]...)
			}
		} else {
			switch object := g.info.Uses[f.Sel].(type) {
			case *types.Func:
				named = object
			case *types.Var:
				callees = append(callees, g.stored[object.Origin()]...)
			}
		}
	}
	async := false
	if named != nil {
		callees = append(callees, g.funcTargets(named)...)
		async = named.Name() == "Go" || (named.Pkg() != nil && named.Pkg().Path() == "time" && named.Name() == "AfterFunc")
	}
	if !async {
		for _, argument := range call.Args {
			callees = append(callees, g.functionValue(argument)...)
		}
	}
	return callees
}

// taken is what a path through a function has taken so far, and where it first took it.
type taken struct {
	effect lockEffect
	first  string
}

func (a taken) union(b taken) taken {
	if a.first == "" {
		a.first = b.first
	}
	a.effect |= b.effect
	return a
}

// orderCheck reads one function body in statement order (see
// TestCopyLockComesBeforeEveryPendingRowAndEvent).
type orderCheck struct {
	graph    *lockGraph
	fn       *lockFunction
	now      taken
	offences []string
}

func (g *lockGraph) orderOffences(fn *lockFunction) []string {
	check := &orderCheck{graph: g, fn: fn}
	check.statements(fn.body.List, taken{})
	return check.offences
}

// statements reads list from what was taken before it, returning what is taken after it and
// whether every path through it ends.
func (c *orderCheck) statements(list []ast.Stmt, before taken) (taken, bool) {
	for _, statement := range list {
		var ended bool
		before, ended = c.statement(statement, before)
		if ended {
			return before, true
		}
	}
	return before, false
}

func (c *orderCheck) statement(statement ast.Stmt, before taken) (taken, bool) {
	c.now = before
	switch s := statement.(type) {
	case *ast.ReturnStmt:
		for _, result := range s.Results {
			c.evaluate(result)
		}
		return c.now, true
	case *ast.BlockStmt:
		return c.statements(s.List, before)
	case *ast.LabeledStmt:
		return c.statement(s.Stmt, before)
	case *ast.GoStmt, *ast.DeferStmt, *ast.EmptyStmt:
		return before, false
	case *ast.ExprStmt:
		c.evaluate(s.X)
		if call, ok := s.X.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "panic" {
				return c.now, true
			}
		}
		return c.now, false
	case *ast.IfStmt:
		if s.Init != nil {
			before, _ = c.statement(s.Init, before)
		}
		c.now = before
		c.evaluate(s.Cond)
		before = c.now
		thenAfter, thenEnded := c.statements(s.Body.List, before)
		elseAfter, elseEnded := before, false
		if s.Else != nil {
			elseAfter, elseEnded = c.statement(s.Else, before)
		}
		return merged([]taken{thenAfter, elseAfter}, []bool{thenEnded, elseEnded})
	case *ast.ForStmt:
		if s.Init != nil {
			before, _ = c.statement(s.Init, before)
		}
		c.now = before
		c.evaluate(s.Cond)
		before = c.now
		once, _ := c.statements(s.Body.List, before)
		if s.Post != nil {
			once, _ = c.statement(s.Post, once)
		}
		again, _ := c.statements(s.Body.List, before.union(once))
		return before.union(once).union(again), false
	case *ast.RangeStmt:
		c.evaluate(s.X)
		before = c.now
		once, _ := c.statements(s.Body.List, before)
		again, _ := c.statements(s.Body.List, before.union(once))
		return before.union(once).union(again), false
	case *ast.SwitchStmt:
		if s.Init != nil {
			before, _ = c.statement(s.Init, before)
		}
		c.now = before
		c.evaluate(s.Tag)
		return c.clauses(s.Body, c.now)
	case *ast.TypeSwitchStmt:
		if s.Init != nil {
			before, _ = c.statement(s.Init, before)
		}
		before, _ = c.statement(s.Assign, before)
		return c.clauses(s.Body, before)
	case *ast.SelectStmt:
		return c.clauses(s.Body, before)
	default:
		c.evaluate(statement)
		return c.now, false
	}
}

// clauses reads a switch's or select's clauses, each from before; a switch with no default can
// also run none of them.
func (c *orderCheck) clauses(body *ast.BlockStmt, before taken) (taken, bool) {
	var afters []taken
	var ended []bool
	exhaustive := false
	for _, clause := range body.List {
		start := before
		var list []ast.Stmt
		switch typed := clause.(type) {
		case *ast.CaseClause:
			exhaustive = exhaustive || typed.List == nil
			c.now = start
			for _, expression := range typed.List {
				c.evaluate(expression)
			}
			start, list = c.now, typed.Body
		case *ast.CommClause:
			exhaustive = exhaustive || typed.Comm == nil
			if typed.Comm != nil {
				start, _ = c.statement(typed.Comm, start)
			}
			list = typed.Body
		}
		after, end := c.statements(list, start)
		afters, ended = append(afters, after), append(ended, end)
	}
	if !exhaustive {
		afters, ended = append(afters, before), append(ended, false)
	}
	return merged(afters, ended)
}

func merged(afters []taken, ended []bool) (taken, bool) {
	var result taken
	all := true
	for index, after := range afters {
		if ended[index] {
			continue
		}
		all = false
		result = result.union(after)
	}
	return result, all
}

// evaluate reads the calls in node in the order they run: a call's receiver and arguments before
// the call. A function literal runs only where it is called or passed.
func (c *orderCheck) evaluate(node ast.Node) {
	if node == nil {
		return
	}
	ast.Inspect(node, func(child ast.Node) bool {
		switch n := child.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			switch fun := ast.Unparen(n.Fun).(type) {
			case *ast.SelectorExpr:
				c.evaluate(fun.X)
			case *ast.FuncLit:
			default:
				c.evaluate(fun)
			}
			for _, argument := range n.Args {
				c.evaluate(argument)
			}
			c.call(n)
			return false
		}
		return true
	})
}

func (c *orderCheck) call(call *ast.CallExpr) {
	var effect lockEffect
	for _, callee := range c.graph.callees(call) {
		effect |= callee.reach
	}
	callee := types.ExprString(call.Fun)
	if effect&takesCopyLock != 0 && c.now.effect&(takesPendingRow|appendsEvent) != 0 {
		c.offences = append(c.offences, fmt.Sprintf("%s: %s calls %s, which takes the copy lock, after %s",
			c.graph.line(c.fn.source, call), c.fn.name, callee, c.now.first))
	}
	if held := effect &^ takesCopyLock; held != 0 {
		if c.now.first == "" {
			c.now.first = fmt.Sprintf("%s, which takes %s (%s)", callee, held, c.graph.line(c.fn.source, call))
		}
		c.now.effect |= held
	}
}
