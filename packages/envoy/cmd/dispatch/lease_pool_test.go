package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A leased maintenance loop holds a connection of the lease pool for the whole of each pass it
// makes, and takes one even for a pass it skips because another process holds the lease
// (duty.Exclusive), so the pool needs one connection per caller that can be inside Exclusive at
// once. Its size says so in words (store.LeasePoolSize) and the wiring below is what has to match
// it: a caller added here without a connection added there waits for one, for as long as the
// longest pass then holding one, and nothing says so. The test counts this package's wiring out
// of its own source and compares the count with the pool's size, so the next caller fails here
// instead.

// leaseCallers is how many callers each call site of the lease pool starts, where it is not one.
// A site the map does not name counts as one, so a new leased loop needs no entry.
var leaseCallers = map[string]int{
	// joinRegistry takes the pool as a parameter and passes it on to processes.RunPrune, the
	// caller, which is counted at that call in registry.go.
	"joinRegistry": 0,
}

// selfOpeningLeaseSites are the call sites that start leased callers without taking this
// package's lease value, because they open the lease pool themselves. delivery.StartIntakes does:
// it opens the pool and starts one intake per durable, each reconciling its own durable under a
// lease of that durable's name (delivery.consumerLease), so its count is read out of the delivery
// package rather than written here and a third intake fails this test.
var selfOpeningLeaseSites = map[string]string{
	"delivery.StartIntakes": "StartIntakes",
}

// deliveryPackage is where the intakes' own wiring is read from.
const deliveryPackage = "../../internal/dispatch/delivery"

// Every caller of the lease pool has a connection of it: the wiring of this package, plus the
// intakes' own, counts exactly store.LeasePoolSize callers.
func TestTheLeasePoolHoldsAConnectionForEveryLeasedCaller(t *testing.T) {
	intakes := leaseSitesIn(t, deliveryPackage, selfOpeningLeaseSites["delivery.StartIntakes"])
	if len(intakes) == 0 {
		t.Fatal("delivery.StartIntakes passes its lease pool to nothing, so this test counts no intake")
	}
	callers, named := 0, []string{}
	for _, site := range leaseSitesIn(t, ".", "") {
		count, ok := leaseCallers[site.callee]
		if !ok {
			count = 1
		}
		if count > 0 {
			named = append(named, site.String())
		}
		callers += count
	}
	for _, site := range intakes {
		named = append(named, site.String())
		callers++
	}
	if callers != store.LeasePoolSize {
		sort.Strings(named)
		t.Fatalf("the wiring starts %d callers of the lease pool, which holds %d connections (store.LeasePoolSize); the callers are:\n\t%s",
			callers, store.LeasePoolSize, strings.Join(named, "\n\t"))
	}
}

// Every lease pool in the server is opened in one of these two places, so the count above is the
// whole of the wiring: a third opener would be leased callers nothing here counts.
func TestTheLeasePoolIsOpenedOnlyWhereTheCountReadsIt(t *testing.T) {
	want := []string{
		"cmd/dispatch/main.go: main",
		"internal/dispatch/delivery/intakes.go: StartIntakes",
	}
	got := leasePoolOpeners(t, ".", "../../internal")
	if !slices.Equal(got, want) {
		t.Fatalf("the lease pool is opened in:\n\t%s\nwant:\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
}

// leaseSite is one call expression that is handed a lease pool: the function called and where.
type leaseSite struct {
	callee string
	where  string
}

func (s leaseSite) String() string { return s.callee + " (" + s.where + ")" }

// leaseSitesIn is every call expression in dir's non-test Go files that is handed a lease pool -
// a value a `Leases()` call answered, or a parameter of type duty.Leases - as an argument. within
// bounds it to one function by name, "" to the whole package. The value is followed by the name
// it is bound to, which is how the wiring passes it on: one name per function, and every caller
// of this package's pool is `leases`.
func leaseSitesIn(t *testing.T, dir, within string) []leaseSite {
	t.Helper()
	var sites []leaseSite
	fset := token.NewFileSet()
	for _, path := range goFilesIn(t, dir) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || (within != "" && fn.Name.Name != within) {
				continue
			}
			held := leaseNames(fn)
			if len(held) == 0 {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !passesLease(call, held) {
					return true
				}
				sites = append(sites, leaseSite{callee: calleeName(call), where: fset.Position(call.Pos()).String()})
				return true
			})
		}
	}
	return sites
}

// leaseNames are the names fn binds a lease pool to: its parameters of type duty.Leases, and the
// variables it assigns a Leases() call to.
func leaseNames(fn *ast.FuncDecl) map[string]bool {
	held := make(map[string]bool)
	for _, param := range fn.Type.Params.List {
		if selector, ok := param.Type.(*ast.SelectorExpr); ok && selector.Sel.Name == "Leases" {
			for _, name := range param.Names {
				held[name.Name] = true
			}
		}
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, value := range assign.Rhs {
			if call, ok := value.(*ast.CallExpr); ok && calleeFunction(call) == "Leases" && len(assign.Lhs) > 0 {
				if name, ok := assign.Lhs[0].(*ast.Ident); ok {
					held[name.Name] = true
				}
			}
		}
		return true
	})
	return held
}

// passesLease reports whether one of held's names is an argument of call.
func passesLease(call *ast.CallExpr, held map[string]bool) bool {
	for _, arg := range call.Args {
		if name, ok := arg.(*ast.Ident); ok && held[name.Name] {
			return true
		}
	}
	return false
}

// calleeName is how the source names the function a call calls, with its receiver or package
// where it has one, which is the key leaseCallers and selfOpeningLeaseSites use.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		if receiver, ok := fn.X.(*ast.Ident); ok {
			return receiver.Name + "." + fn.Sel.Name
		}
		return fn.Sel.Name
	}
	return "an unnamed call"
}

// calleeFunction is the called function's own name, without its receiver.
func calleeFunction(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// leasePoolOpeners names, sorted, every function under roots that opens a lease pool
// (Pool.Leases), as "<path relative to packages/envoy>: <function>".
func leasePoolOpeners(t *testing.T, roots ...string) []string {
	t.Helper()
	var openers []string
	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				t.Fatalf("parse %s: %v", path, parseErr)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok || calleeFunction(call) != "Leases" || len(call.Args) != 0 {
						return true
					}
					openers = append(openers, relativeToEnvoy(path)+": "+fn.Name.Name)
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	sort.Strings(openers)
	return slices.Compact(openers)
}

// relativeToEnvoy is path as it reads from packages/envoy, whatever directory the test runs in.
func relativeToEnvoy(path string) string {
	cleaned := filepath.ToSlash(filepath.Clean(path))
	cleaned = strings.TrimPrefix(cleaned, "../../")
	if strings.HasPrefix(cleaned, "internal/") {
		return cleaned
	}
	return "cmd/dispatch/" + strings.TrimPrefix(cleaned, "./")
}

// goFilesIn is dir's non-test Go files, sorted.
func goFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)
	return files
}
