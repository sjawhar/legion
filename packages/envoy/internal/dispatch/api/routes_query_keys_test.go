package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The query-parameter structs of routes_shapes.go are declarations alone: no handler reads one,
// because r.URL.Query().Get and .Has differ on an absent key and moving forty handlers' reads
// onto a struct is the one change that could alter what a request means. Nothing else ties the
// two together — the mount guards hold routes to shapes, not query keys to structs — so a
// handler that starts reading a new ?foo= would leave the key out of the emitted document and
// out of every generated client, and nothing would fail. This walk is that tie.
//
// It is file-level, as the plan that asked for it says: a key read in a file is checked against
// the parameters of the routes that file's handlers serve, so it can admit a key the particular
// route does not read. It cannot admit a key no struct names at all, which is the drift that
// reaches a client.

// queryKeyExceptions are the two keys read by a handler and named by no parameters struct, each
// deliberate. session is read by actingPerson on every route that resolves a person, so it
// belongs to the whole API rather than to one route and openapi.go adds it to every operation.
// cursor is read by parseIssuePage only to refuse it (400 INVALID_QUERY), so naming it would
// offer a client a parameter that cannot work.
var queryKeyExceptions = map[string]string{
	"session": "the global parameter openapi.go adds to every operation (actingPerson)",
	"cursor":  "read only to refuse it, 400 INVALID_QUERY (parseIssuePage)",
}

// queryKeysReadThroughAHelper are the reads this walk cannot resolve, because the helper takes
// the key as a parameter and the literal is at the call rather than at the read. The list is
// pinned rather than described: a new unresolvable read changes it and fails, so the hole stays
// named and small instead of growing quietly.
var queryKeysReadThroughAHelper = []string{
	`delivery.go: Get(name)`,
	`delivery.go: Has(name)`,
	`server.go: query[field]`,
}

func TestEveryQueryKeyAHandlerReadsIsNamedByItsParameters(t *testing.T) {
	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	namedByFile, namedAnywhere := queryKeysNamedByShapes(t, routes)

	var unresolved []string
	for _, file := range goFilesOfThisPackage(t) {
		parsed := parseAPIFile(t, file)
		queryKeys, formKeys, dynamic := queryKeysReadByForm(parsed)
		for _, site := range dynamic {
			unresolved = append(unresolved, filepath.Base(file)+": "+site)
		}
		// A file that declares no route handler is a shared helper (events.go, issue_page.go,
		// answers.go and their kind), and its reads belong to whichever routes call it, so they
		// are checked against every parameters struct rather than against one file's.
		allowed, servesRoutes := namedByFile[filepath.Base(file)]
		if !servesRoutes {
			allowed = namedAnywhere
		}
		// A key read as a query parameter must be named by a parameters struct, whatever else
		// the file does with that name. A key the file reads only with FormValue may be named
		// by the multipart body instead, since that is the read the body reaches the handler
		// by; a Get or Has of the same name is a query parameter and nothing else, so a body
		// field does not excuse it.
		for _, key := range slices.Sorted(sortedKeys(queryKeys)) {
			if _, excepted := queryKeyExceptions[key]; excepted || allowed.params[key] {
				continue
			}
			t.Errorf("%s reads the query key %q, which no parameters struct of routes_shapes.go names: "+
				"add it to that route's query struct, or to queryKeyExceptions with the reason it is not "+
				"a parameter of the route", filepath.Base(file), key)
		}
		for _, key := range slices.Sorted(sortedKeys(formKeys)) {
			if _, excepted := queryKeyExceptions[key]; excepted || queryKeys[key] {
				continue
			}
			if !allowed.params[key] && !allowed.bodies[key] {
				t.Errorf("%s reads %q with FormValue, which http.Request fills from the query too, and "+
					"neither a parameters struct nor the multipart body of a route this file serves names "+
					"it: add it to that route's query struct or to its request body", filepath.Base(file), key)
			}
		}
	}

	sort.Strings(unresolved)
	want := slices.Clone(queryKeysReadThroughAHelper)
	sort.Strings(want)
	if !slices.Equal(unresolved, want) {
		t.Errorf("the reads this walk cannot resolve have changed:\n got %v\nwant %v\n"+
			"a read whose key is a parameter of a helper is invisible to a static walk; pin the new one in "+
			"queryKeysReadThroughAHelper, and check by hand that its key is named by a parameters struct",
			unresolved, want)
	}
}

// sortedKeys is iteration over a set in one order, so a failure reads the same way twice.
func sortedKeys(set map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		keys := make([]string, 0, len(set))
		for key := range set {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !yield(key) {
				return
			}
		}
	}
}

// namedKeys is what a set of routes names: params are the keys their parameters structs declare,
// which any read may use, and bodies are the fields a multipart route's body declares, which
// only a FormValue read may use, since that is the one read the body reaches the handler by.
type namedKeys struct{ params, bodies map[string]bool }

func (n namedKeys) add(other namedKeys) {
	for key := range other.params {
		n.params[key] = true
	}
	for key := range other.bodies {
		n.bodies[key] = true
	}
}

// queryKeysNamedByShapes answers what each handler file's routes name, by file, and what every
// route names together. A route's handler gives its name at run time; its file comes from the
// AST, because a method value's wrapper reports <autogenerated> and has no source position.
func queryKeysNamedByShapes(t *testing.T, routes []apiRoute) (map[string]namedKeys, namedKeys) {
	t.Helper()
	declaredIn := map[string]string{}
	for _, file := range goFilesOfThisPackage(t) {
		for _, decl := range parseAPIFile(t, file).Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil {
				declaredIn[fn.Name.Name] = filepath.Base(file)
			}
		}
	}
	byFile := map[string]namedKeys{}
	anywhere := namedKeys{params: map[string]bool{}, bodies: map[string]bool{}}
	for _, route := range routes {
		file, known := declaredIn[handlerName(route.Handler)]
		if !known {
			t.Fatalf("%s %s: no file of this package declares its handler %s",
				route.Method, route.Pattern, handlerName(route.Handler))
		}
		if _, started := byFile[file]; !started {
			byFile[file] = namedKeys{params: map[string]bool{}, bodies: map[string]bool{}}
		}
		shape := routeShapes[route.Method+" "+route.Pattern]
		named := namedKeys{params: map[string]bool{}, bodies: map[string]bool{}}
		for _, key := range queryTagsOf(shape.Params) {
			named.params[key] = true
		}
		// A multipart route reads its body's fields with r.FormValue, which also accepts them in
		// the URL's query, so the walk sees them as query keys. They are no drift: the document
		// declares each on that route's own multipart body, which is where a client reads them.
		if shape.Multipart {
			for _, key := range jsonTagsOf(shape.Request) {
				named.bodies[key] = true
			}
		}
		byFile[file].add(named)
		anywhere.add(named)
	}
	return byFile, anywhere
}

// handlerName is the method behind a route's handler value: runtime names a method value
// "<package>.(*server).listIssues-fm".
func handlerName(handler http.HandlerFunc) string {
	full := runtime.FuncForPC(reflect.ValueOf(handler).Pointer()).Name()
	return strings.TrimSuffix(full[strings.LastIndex(full, ".")+1:], "-fm")
}

// queryTagsOf answers the query keys one parameters struct names, and jsonTagsOf the field
// names one request body carries; both are the same read of a struct's tags.
func queryTagsOf(params any) []string { return tagsOf(params, "query") }

func jsonTagsOf(body any) []string { return tagsOf(body, "json") }

// tagsOf answers the name each field of a struct carries under one tag, without the options a
// json tag may add after a comma. A value that is no struct names nothing.
func tagsOf(value any, tag string) []string {
	if value == nil {
		return nil
	}
	structure := reflect.TypeOf(value)
	if structure.Kind() != reflect.Struct {
		return nil
	}
	var keys []string
	for index := range structure.NumField() {
		name, tagged := structure.Field(index).Tag.Lookup(tag)
		if name, _, _ = strings.Cut(name, ","); tagged && name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	return keys
}

func goFilesOfThisPackage(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	return files
}

func parseAPIFile(t *testing.T, path string) *ast.File {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return parsed
}

// isQueryCall answers whether an expression is the r.URL.Query() chain, whatever the request is
// named. Without this the walk would take Header.Get("Authorization") for a query read.
func isQueryCall(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "Query"
}

// queryValueNames are the identifiers of one file that hold url.Values: a helper takes them as a
// parameter (parseIssuePage(query url.Values)) as often as a handler assigns them.
func queryValueNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.FuncDecl:
			if value.Type.Params == nil {
				return true
			}
			for _, field := range value.Type.Params.List {
				selector, ok := field.Type.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Values" {
					continue
				}
				if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "url" {
					for _, name := range field.Names {
						names[name.Name] = true
					}
				}
			}
		case *ast.AssignStmt:
			for index, right := range value.Rhs {
				if index < len(value.Lhs) && isQueryCall(right) {
					if name, ok := value.Lhs[index].(*ast.Ident); ok {
						names[name.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for index, right := range value.Values {
				if index < len(value.Names) && isQueryCall(right) {
					names[value.Names[index].Name] = true
				}
			}
		}
		return true
	})
	return names
}

// stringSlices are the string slices of one file by name, so a key read over one can be checked
// element by element: delivery.go reads its nine facets as query[facet] over deliveryFacets, and
// a tenth added there with no struct entry is exactly the drift this guard exists to catch.
// Inline literals count too (block_links.go ranges over [2]string{"from", "to"}), under a name
// no source can collide with.
func stringSlices(file *ast.File) (byName map[string][]string, rangedOver map[string][]string) {
	byName = map[string][]string{}
	rangedOver = map[string][]string{}
	elementsOf := func(literal *ast.CompositeLit) []string {
		var elements []string
		for _, element := range literal.Elts {
			basic, ok := element.(*ast.BasicLit)
			if !ok || basic.Kind != token.STRING {
				return nil
			}
			text, err := strconv.Unquote(basic.Value)
			if err != nil {
				return nil
			}
			elements = append(elements, text)
		}
		return elements
	}
	ast.Inspect(file, func(node ast.Node) bool {
		if spec, ok := node.(*ast.ValueSpec); ok {
			for index, value := range spec.Values {
				literal, ok := value.(*ast.CompositeLit)
				if !ok || index >= len(spec.Names) {
					continue
				}
				if elements := elementsOf(literal); len(elements) > 0 {
					byName[spec.Names[index].Name] = elements
				}
			}
		}
		// A loop variable may range over several slices in one file — facet ranges over both
		// deliveryFacets and deliveryColorFacets — so it collects every one of them. Keeping
		// only the last silently dropped two facets while this guard was being written.
		loop, ok := node.(*ast.RangeStmt)
		if !ok {
			return true
		}
		variable, named := loop.Value.(*ast.Ident)
		if !named {
			return true
		}
		switch over := loop.X.(type) {
		case *ast.Ident:
			rangedOver[variable.Name] = append(rangedOver[variable.Name], over.Name)
		case *ast.CompositeLit:
			if elements := elementsOf(over); len(elements) > 0 {
				inline := "\x00inline " + variable.Name
				byName[inline] = elements
				rangedOver[variable.Name] = append(rangedOver[variable.Name], inline)
			}
		}
		return true
	})
	return byName, rangedOver
}

// queryKeysRead answers the query keys one file reads, however it reads them, and the read sites
// whose key is a parameter of the enclosing helper rather than a literal, which a static walk
// cannot follow.
func queryKeysRead(file *ast.File) (keys map[string]bool, dynamic []string) {
	keys, fromForm, dynamic := queryKeysReadByForm(file)
	for key := range fromForm {
		keys[key] = true
	}
	return keys, dynamic
}

// queryKeysReadByForm answers the keys a file reads as a query parameter — Get, Has, or an index
// of a url.Values — apart from those it reads with FormValue or PostFormValue, which is how a
// multipart route reads a field of its own body. The two differ in what may name them: a body
// field names only the form read, since a Get of that name is a query parameter and nothing
// else.
func queryKeysReadByForm(file *ast.File) (fromQuery, fromForm map[string]bool, dynamic []string) {
	keys := map[string]bool{}
	fromForm = map[string]bool{}
	values := queryValueNames(file)
	slicesByName, rangedOver := stringSlices(file)
	fromSlices := func(variable string) bool {
		names, ranged := rangedOver[variable]
		for _, name := range names {
			for _, key := range slicesByName[name] {
				keys[key] = true
			}
		}
		return ranged
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch read := node.(type) {
		case *ast.CallExpr:
			selector, ok := read.Fun.(*ast.SelectorExpr)
			if !ok || len(read.Args) != 1 {
				return true
			}
			// FormValue and PostFormValue take the request itself, not a url.Values, and
			// http.Request fills r.Form from the URL's query as well as the body, so a key read
			// with either is a key a caller may send as ?key= (artifacts.go). They are read on
			// whatever the request is named, as Get and Has are read on whatever holds the
			// values, and they are recorded apart because a body field names only them.
			into := keys
			onQuery := false
			switch selector.Sel.Name {
			case "Get", "Has":
				onQuery = isQueryCall(selector.X)
				if name, ok := selector.X.(*ast.Ident); ok && values[name.Name] {
					onQuery = true
				}
			case "FormValue", "PostFormValue":
				onQuery, into = true, fromForm
			}
			if !onQuery {
				return true
			}
			switch argument := read.Args[0].(type) {
			case *ast.BasicLit:
				if argument.Kind == token.STRING {
					if text, err := strconv.Unquote(argument.Value); err == nil {
						into[text] = true
					}
				}
			case *ast.Ident:
				if !fromSlices(argument.Name) {
					dynamic = append(dynamic, selector.Sel.Name+"("+argument.Name+")")
				}
			}
		case *ast.IndexExpr:
			// The receiver is a name holding url.Values, or the r.URL.Query() call itself:
			// r.URL.Query()["since"] is as live in this package as query["since"] is
			// (asks.go, message_delivery.go, references.go).
			if name, ok := read.X.(*ast.Ident); !ok || !values[name.Name] {
				if !isQueryCall(read.X) {
					return true
				}
			}
			switch index := read.Index.(type) {
			case *ast.BasicLit:
				if index.Kind == token.STRING {
					if text, err := strconv.Unquote(index.Value); err == nil {
						keys[text] = true
					}
				}
			case *ast.Ident:
				if !fromSlices(index.Name) {
					dynamic = append(dynamic, "query["+index.Name+"]")
				}
			}
		}
		return true
	})
	return keys, fromForm, dynamic
}
