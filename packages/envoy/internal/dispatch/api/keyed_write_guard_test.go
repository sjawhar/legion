//go:build !race

// The guard is static, as the copy-lock guard it shares a graph with: the race detector adds
// nothing to it.

package api

import (
	"go/ast"
	"go/types"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// writeStatement is SQL that writes a row: an insert, an update (not a FOR UPDATE lock), a delete,
// a merge or a truncate, anywhere in the statement, a CTE's included.
var writeStatement = regexp.MustCompile(
	`\b(?:insert into|delete from|merge into|truncate)\b|\bupdate [a-z_."]+(?: (?:as )?[a-z_]+)? set\b`)

// sharedPool is the shared pool's type: its transactions take the commit step a keyed request's
// context carries (store.WithBeforeCommit), and its bare statements do not.
const sharedPool = "github.com/sjawhar/envoy/internal/dispatch/store.Pool"

// poolStatementReceivers are the types whose statement methods run SQL outside any transaction of
// the shared pool, so outside the commit step: the shared pool's own bare statements, and every
// pool or connection handed out beside it, whose transactions the step never joins either.
var poolStatementReceivers = map[string]bool{
	sharedPool:                             true,
	"github.com/jackc/pgx/v5/pgxpool.Pool": true,
	"github.com/jackc/pgx/v5/pgxpool.Conn": true,
	"github.com/jackc/pgx/v5.Conn":         true,
}

// keyedWriteGuardStops are the functions the guard does not follow, each with the reason what it
// runs is no write of the request's.
var keyedWriteGuardStops = map[string]string{
	// Resolving the caller writes the credential's and the person's own bookkeeping (a personal
	// token's last use, a sign-in's person and session rows). keyedWrites resolves a keyed
	// request's caller before its key is claimed and hands the handler that resolution
	// (resolvedCallerKey), so none of it runs under the key.
	"api.server.optionalActor": "the caller is resolved before the key is claimed",
	// The pool itself: its statement methods are what the guard looks for at their call sites.
	"store.Pool.Exec": "", "store.Pool.Query": "", "store.Pool.QueryRow": "", "store.Pool.SendBatch": "",
	"store.Pool.CopyFrom": "", "store.Pool.Begin": "", "store.Pool.BeginTx": "", "store.Pool.begin": "",
}

// keyedWriteGuardAllowed are the bare writes a keyed route may make, each with the reason the
// request's key needs no mark from it.
var keyedWriteGuardAllowed = map[string]string{
	// The suggestions a write offered, recorded after that write's own transaction committed and
	// marked the key; a failure is only logged, and the write is never undone for it.
	"api.server.persistSuggestions": "records what a committed write offered, after that write marked its key",
}

// keyedWriteOffence is one statement a keyed route's handler can reach that commits a write outside
// the transactions the commit mark hooks.
type keyedWriteOffence struct {
	at, function, why string
}

// keyedWriteGuard is what the guard reads off the graph: every POST and PATCH route's handler, and
// every function those handlers run on their own goroutine.
type keyedWriteGuard struct {
	graph   *lockGraph
	objects map[*lockFunction]*types.Func
	routes  map[string]*lockFunction
	reached map[*lockFunction]bool
	// poolStatements is every bare pool statement a keyed handler reaches, write or not, by the
	// function that makes it: the guard's proof that it sees them.
	poolStatements map[string]int
	// carriers is every reached function that runs a statement whose SQL is one of its own
	// parameters, by that parameter's index: its callers name the SQL, and are read instead.
	carriers map[*types.Func]int
}

// TestEveryKeyedWriteCommitsInsideATransactionTheMarkHooks enforces what KEY_COMMITTED rests on
// (markRequestKeyCommitted): every write a POST or PATCH route makes commits in a transaction the
// shared pool opens, so the commit step marks the request's key committed with the write itself. A
// write made as a bare pool statement commits on its own and marks nothing; the process dying after
// it, before the answer is kept, leaves a key a repeat takes over, and the write is made twice.
//
// It reads each route's handler from the routes table and follows it through the copy-lock guard's
// graph (TestCopyLockComesBeforeEveryPendingRowAndEvent says how a call is resolved), on the
// handler's own goroutine: a go statement, and work handed to the server's background tracker
// (backgroundWork.goDetached), runs after the handler's commit step has stopped and is no write of
// the request's. In every function it reaches, a write statement - its SQL read as the other static
// guards read it, through the caller that names it when the function takes it as a parameter - run
// on the shared pool itself or on any other pool or connection is refused, and so is a transaction
// begun on one of those others, which the commit step never joins, unless it is read-only. A
// statement whose SQL it cannot read at all is refused too, since it cannot be told from a write.
// keyedWriteGuardStops and keyedWriteGuardAllowed name, with their reasons, what it leaves.
func TestEveryKeyedWriteCommitsInsideATransactionTheMarkHooks(t *testing.T) {
	var offences []string
	for _, offence := range newKeyedWriteGuard(t).offences() {
		offences = append(offences, offence.at+" in "+offence.function+": "+offence.why)
	}
	slices.Sort(offences)
	offences = slices.Compact(offences)
	if len(offences) > 0 {
		t.Fatalf("a POST or PATCH route reaches a write outside the transactions the commit mark hooks:\n\t%s\n"+
			"Make the write inside a transaction of the shared pool (s.begin, store.Pool.Begin): its commit "+
			"step marks the request's Idempotency-Key committed with the write, which is what answers a "+
			"repeat after a crash KEY_COMMITTED rather than making the write a second time.",
			strings.Join(offences, "\n\t"))
	}
}

// TestTheKeyedWriteGuardSeesWhatItGuards holds the guard to what it must see, so it cannot pass by
// seeing nothing: every POST and PATCH route of the table, the writes behind the comment and send
// routes, the bare reads those handlers do make, and the statement a caller names for a function
// that takes its SQL as a parameter.
func TestTheKeyedWriteGuardSeesWhatItGuards(t *testing.T) {
	guard := newKeyedWriteGuard(t)
	guard.offences()
	var want []string
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		if keyedMethod(route.Method) {
			want = append(want, route.Method+" "+route.Pattern)
		}
	}
	for _, route := range want {
		if guard.routes[route] == nil {
			t.Errorf("the guard finds no handler for %s", route)
		}
	}
	if len(guard.routes) != len(want) {
		t.Errorf("the guard reads %d keyed routes, want the table's %d", len(guard.routes), len(want))
	}
	for _, function := range []string{
		"api.server.createCommentFor", "api.server.deliverResolvedCommentMention",
		"api.server.recordCommentDeliveryResolution", "api.server.createStoredMessage",
		"api.server.recordPendingMessageDelivery", "architecture.Importer.touch",
	} {
		if !guard.reaches(function) {
			t.Errorf("the guard does not follow a keyed handler to %s", function)
		}
	}
	if guard.poolStatements["api.server.messageDeliveryRoute"] == 0 {
		t.Errorf("the guard sees no bare pool statement in api.server.messageDeliveryRoute, which reads through one: %v",
			guard.poolStatements)
	}
	if guard.graph.named["docs.Service.documentRooms"] != nil && guard.reaches("docs.Service.documentRooms") {
		object := guard.objects[guard.graph.named["docs.Service.documentRooms"]]
		if _, carries := guard.carriers[object]; !carries {
			t.Error("the guard does not read docs.Service.documentRooms's SQL through its callers")
		}
	}
}

func newKeyedWriteGuard(t *testing.T) *keyedWriteGuard {
	t.Helper()
	graph := copyLockGraph(t)
	guard := &keyedWriteGuard{
		graph: graph, objects: map[*lockFunction]*types.Func{}, routes: map[string]*lockFunction{},
		reached: map[*lockFunction]bool{}, poolStatements: map[string]int{}, carriers: map[*types.Func]int{},
	}
	for object, fn := range graph.declared {
		guard.objects[fn] = object
	}
	table := graph.named["api.server.routes"]
	if table == nil {
		t.Fatal("the guard finds no routes table (api.server.routes)")
	}
	ast.Inspect(table.body, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok || len(literal.Elts) != 5 {
			return true
		}
		method, ok := ast.Unparen(literal.Elts[0]).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		verb := strings.ToUpper(strings.TrimPrefix(method.Sel.Name, "Method"))
		if !keyedMethod(verb) {
			return false
		}
		pattern, _ := literalText(literal.Elts[1], table.pkg, graph.constants)
		for _, handler := range graph.functionValue(literal.Elts[4]) {
			guard.routes[verb+" "+pattern] = handler
			guard.walk(handler)
		}
		return false
	})
	return guard
}

// walk marks fn and everything it runs on its own goroutine reached.
func (g *keyedWriteGuard) walk(fn *lockFunction) {
	if fn == nil || g.reached[fn] {
		return
	}
	if _, stop := keyedWriteGuardStops[fn.name]; stop {
		return
	}
	g.reached[fn] = true
	for _, call := range ownCalls(fn) {
		if g.detached(call) {
			continue
		}
		for _, callee := range g.graph.callees(call) {
			g.walk(callee)
		}
	}
}

// ownCalls is every call fn's own body makes, not inside a function literal (a function of its own)
// or a go statement.
func ownCalls(fn *lockFunction) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(fn.body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit, *ast.GoStmt:
			return false
		case *ast.CallExpr:
			calls = append(calls, n)
		}
		return true
	})
	return calls
}

// detached reports whether call hands its work to the server's background tracker, which runs it
// on a goroutine of its own after the handler's commit step has ended.
func (g *keyedWriteGuard) detached(call *ast.CallExpr) bool {
	selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	selection, ok := g.graph.info.Selections[selector]
	return ok && selection.Obj().Name() == "goDetached"
}

func (g *keyedWriteGuard) reaches(name string) bool {
	fn := g.graph.named[name]
	return fn != nil && g.reached[fn]
}

// offences reads every reached function's own bare pool statements, then the SQL each caller of a
// carrier names.
func (g *keyedWriteGuard) offences() []keyedWriteOffence {
	var offences []keyedWriteOffence
	refuse := func(fn *lockFunction, call ast.Node, why string) {
		if _, allowed := keyedWriteGuardAllowed[fn.name]; !allowed {
			offences = append(offences, keyedWriteOffence{g.graph.line(fn.source, call), fn.name, why})
		}
	}
	judge := func(fn *lockFunction, call ast.Node, sql ast.Expr, what string) {
		text, known := literalText(sql, fn.pkg, g.graph.constants)
		statement := sqlWhitespace.ReplaceAllString(strings.ToLower(text), " ")
		switch {
		case !known:
			refuse(fn, call, "a bare "+what+" whose SQL the guard cannot read, so cannot tell from a write: name it with a constant")
		case writeStatement.MatchString(statement):
			refuse(fn, call, "a bare write: "+statement)
		}
	}
	for fn := range g.reached {
		for _, call := range ownCalls(fn) {
			receiver, method := g.poolMethod(call)
			if receiver == "" {
				continue
			}
			what := receiver + "." + method
			switch method {
			case "Begin", "BeginTx", "BeginFunc":
				if receiver != sharedPool && !readOnly(call) {
					refuse(fn, call, "a transaction on "+receiver+", which the commit step never joins")
				}
			case "CopyFrom", "SendBatch":
				refuse(fn, call, what+" writes outside a transaction")
			case "Exec", "Query", "QueryRow":
				if len(call.Args) < 2 {
					continue
				}
				g.poolStatements[fn.name]++
				if index, carried := g.parameter(fn, call.Args[1]); carried {
					g.carriers[g.objects[fn]] = index
					continue
				}
				judge(fn, call, call.Args[1], what)
			}
		}
	}
	for fn := range g.reached {
		for _, call := range ownCalls(fn) {
			for _, callee := range g.graph.callees(call) {
				index, carries := g.carriers[g.objects[callee]]
				if carries && index < len(call.Args) {
					judge(fn, call, call.Args[index], "statement for "+callee.name)
				}
			}
		}
	}
	return offences
}

// parameter reports whether sql is one of fn's own parameters, and which.
func (g *keyedWriteGuard) parameter(fn *lockFunction, sql ast.Expr) (int, bool) {
	ident, ok := ast.Unparen(sql).(*ast.Ident)
	object := g.objects[fn]
	if !ok || object == nil {
		return 0, false
	}
	parameters := object.Signature().Params()
	for index := range parameters.Len() {
		if g.graph.info.Uses[ident] == parameters.At(index) {
			return index, true
		}
	}
	return 0, false
}

// readOnly reports whether a BeginTx call opens a read-only transaction, which Postgres refuses
// every write in.
func readOnly(call *ast.CallExpr) bool {
	read := false
	for _, argument := range call.Args {
		ast.Inspect(argument, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "ReadOnly" {
				read = true
			}
			return !read
		})
	}
	return read
}

// poolMethod is the receiver type and the method call names, when it is a method of one of
// poolStatementReceivers.
func (g *keyedWriteGuard) poolMethod(call *ast.CallExpr) (receiver, method string) {
	selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	selection, ok := g.graph.info.Selections[selector]
	if !ok {
		return "", ""
	}
	fn, ok := selection.Obj().(*types.Func)
	if !ok || fn.Signature().Recv() == nil {
		return "", ""
	}
	recv := fn.Signature().Recv().Type()
	if pointer, isPointer := recv.(*types.Pointer); isPointer {
		recv = pointer.Elem()
	}
	named, ok := recv.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return "", ""
	}
	name := named.Obj().Pkg().Path() + "." + named.Obj().Name()
	if !poolStatementReceivers[name] {
		return "", ""
	}
	return name, fn.Name()
}
