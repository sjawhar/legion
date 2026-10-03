package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// apiDir is the broker's HTTP package: routes_table.go's routes() lists every route, and each
// handler answers through writeError and writeJSON.
const apiDir = envoyDir + "/internal/broker/api"

// varies stands in for a message refgen cannot spell out, such as a wrapped error's own text.
const varies = "(varies)"

type route struct {
	Method, Pattern string
	Wrapper         string // the adapter that fixes the route's authentication
	Handler         string // the *server method that serves it
	Summary         string // the comment above its row
	At              string
}

// readRoutes reads routes() from the route table, one row per route, refusing a row it cannot
// read or one with no comment above it.
func readRoutes(src *source, funcs map[string]*ast.FuncDecl) ([]route, error) {
	fn, ok := funcs["routes"]
	if !ok {
		return nil, fmt.Errorf("%s declares no routes()", apiDir)
	}
	var table *ast.CompositeLit
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if ret, ok := n.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
			table, _ = ret.Results[0].(*ast.CompositeLit)
		}
		return table == nil
	})
	if table == nil {
		return nil, fmt.Errorf("%s: routes() does not return a composite literal", src.at(fn))
	}
	file := src.fileOf(fn)
	var out []route
	for _, elt := range table.Elts {
		row, ok := elt.(*ast.CompositeLit)
		if !ok || len(row.Elts) != 3 {
			return nil, fmt.Errorf("%s: a route row is {method, pattern, handler}", src.at(elt))
		}
		method, ok := row.Elts[0].(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(method.Sel.Name, "Method") {
			return nil, fmt.Errorf("%s: a route's method is an http.Method constant", src.at(row))
		}
		pattern, ok := strLit(row.Elts[1])
		if !ok {
			return nil, fmt.Errorf("%s: a route's pattern is a string literal", src.at(row))
		}
		call, ok := row.Elts[2].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return nil, fmt.Errorf("%s: a route's handler is adapter((*server).handler)", src.at(row))
		}
		wrapper, ok := call.Fun.(*ast.Ident)
		handler, ok2 := call.Args[0].(*ast.SelectorExpr)
		if !ok || !ok2 {
			return nil, fmt.Errorf("%s: a route's handler is adapter((*server).handler)", src.at(row))
		}
		summary := src.commentAbove(file, src.line(row.Pos()))
		if summary == "" {
			return nil, fmt.Errorf("%s: route %s %s has no comment above it saying what it does", src.at(row), method.Sel.Name, pattern)
		}
		out = append(out, route{
			Method:  strings.ToUpper(strings.TrimPrefix(method.Sel.Name, "Method")),
			Pattern: pattern, Wrapper: wrapper.Name, Handler: handler.Sel.Name,
			Summary: summary, At: src.at(row),
		})
	}
	return out, nil
}

// authClass is one route adapter: the credential its routes need and the refusals
// server.authenticate answers for it.
type authClass struct {
	Wrapper, Label, Doc string
	Refusals            []outcome
}

// credentialLabels names, for a reader, the credential each adapter accepts. refgen refuses an
// adapter missing here, so a new one cannot reach the page under its Go name.
var credentialLabels = map[string]string{
	"public":       "None",
	"sessionAuth":  "Session proof",
	"launcherAuth": "Machine credential",
	"uiAuth":       "Dispatch token",
}

// readAuthClasses reads each adapter a route uses: its doc comment, the routeAuth constant it
// builds its routeHandler with, and what authenticate's case for that constant can refuse.
func (g *graph) readAuthClasses(routes []route) ([]authClass, error) {
	authenticate, ok := g.funcs["authenticate"]
	if !ok {
		return nil, fmt.Errorf("%s declares no authenticate", apiDir)
	}
	cases := map[string]*ast.CaseClause{}
	ast.Inspect(authenticate.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		if tag, ok := sw.Tag.(*ast.Ident); !ok || tag.Name != "auth" {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc := stmt.(*ast.CaseClause)
			for _, e := range cc.List {
				if id, ok := e.(*ast.Ident); ok {
					cases[id.Name] = cc
				}
			}
		}
		return false
	})
	seen := map[string]bool{}
	var out []authClass
	for _, r := range routes {
		if seen[r.Wrapper] {
			continue
		}
		seen[r.Wrapper] = true
		fn, ok := g.funcs[r.Wrapper]
		if !ok {
			return nil, fmt.Errorf("%s: no adapter %s", r.At, r.Wrapper)
		}
		label, ok := credentialLabels[r.Wrapper]
		if !ok {
			return nil, fmt.Errorf("%s: adapter %s has no reader label in refgen's credentialLabels", r.At, r.Wrapper)
		}
		constant := ""
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if kv, ok := n.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "auth" {
					if v, ok := kv.Value.(*ast.Ident); ok {
						constant = v.Name
					}
				}
			}
			return constant == ""
		})
		cc, ok := cases[constant]
		if !ok {
			return nil, fmt.Errorf("%s: authenticate has no case for %s, %s's authentication", g.src.at(authenticate), constant, r.Wrapper)
		}
		info := g.collect(g.src.fileOf(authenticate), &ast.BlockStmt{List: cc.Body}, authenticate.Type.Params)
		refusals, err := g.resolve(info, map[string][]string{}, map[string]bool{"authenticate": true})
		if err != nil {
			return nil, err
		}
		out = append(out, authClass{Wrapper: r.Wrapper, Label: label, Doc: prose(fn.Doc), Refusals: refusals})
	}
	return out, nil
}

// graph is the broker API package's functions, read for what each one can answer.
type graph struct {
	src   *source
	funcs map[string]*ast.FuncDecl
	pkgs  map[string]*source // the broker's other packages, parsed on first use, by directory
}

func newGraph(root string) (*graph, error) {
	src, err := parseDir(root, apiDir)
	if err != nil {
		return nil, err
	}
	funcs, err := src.funcs()
	if err != nil {
		return nil, err
	}
	return &graph{src: src, funcs: funcs, pkgs: map[string]*source{}}, nil
}

type siteKind int

const (
	siteError   siteKind = iota // writeError(w, status, code, message)
	siteSuccess                 // writeJSON(w, status, v) or w.WriteHeader(status)
	siteCall                    // a call to another function or method of the package
)

// site is one call that answers the request, or that leads to code that does.
type site struct {
	kind      siteKind
	call      *ast.CallExpr
	callee    string
	sentinels []string // the messages of the errors.Is targets of the case clause around it
	bodyType  string   // readJSON's: the request body's type
	respType  string   // writeJSON's: the response body's type, "" when refgen cannot name it
	query     string   // r.URL.Query().Get's: the query parameter
	at        string
}

type funcInfo struct {
	params []string
	sites  []site
	// assigned is every value each local variable is assigned, so a status chosen at run time
	// (status := http.StatusCreated; if … { status = http.StatusOK }) reads as both.
	assigned map[string][]ast.Expr
}

// collect lists the sites in body, a function whose parameters are params.
func (g *graph) collect(file *ast.File, body ast.Node, params *ast.FieldList) funcInfo {
	info := funcInfo{assigned: map[string][]ast.Expr{}}
	if params != nil {
		for _, field := range params.List {
			for _, n := range field.Names {
				info.params = append(info.params, n.Name)
			}
		}
	}
	locals := map[string]string{} // var name -> declared type name
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ValueSpec:
			if t, ok := x.Type.(*ast.Ident); ok {
				for _, name := range x.Names {
					locals[name.Name] = t.Name
				}
			}
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						info.assigned[id.Name] = append(info.assigned[id.Name], x.Rhs[i])
					}
				}
			}
		}
		return true
	})
	var stack []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		s := site{call: call, at: g.src.at(call)}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			switch {
			case fun.Name == "writeError":
				s.kind = siteError
			case fun.Name == "writeJSON":
				s.kind = siteSuccess
				if len(call.Args) == 3 {
					s.respType = g.typeName(call.Args[2], info.assigned, locals)
				}
			case g.funcs[fun.Name] != nil:
				s.kind, s.callee = siteCall, fun.Name
				if fun.Name == "readJSON" && len(call.Args) > 2 {
					if u, ok := call.Args[2].(*ast.UnaryExpr); ok {
						if id, ok := u.X.(*ast.Ident); ok {
							s.bodyType = locals[id.Name]
						}
					}
				}
			default:
				return true
			}
		case *ast.SelectorExpr:
			switch {
			case fun.Sel.Name == "WriteHeader":
				s.kind = siteSuccess
			case fun.Sel.Name == "Get" && isQueryCall(fun.X) && len(call.Args) == 1:
				name, ok := strLit(call.Args[0])
				if !ok {
					return true
				}
				s.kind, s.callee, s.query = siteCall, "", name
			case g.funcs[fun.Sel.Name] != nil && g.funcs[fun.Sel.Name].Recv != nil:
				s.kind, s.callee = siteCall, fun.Sel.Name
			default:
				return true
			}
		default:
			return true
		}
		s.sentinels = g.caseSentinels(file, stack)
		info.sites = append(info.sites, s)
		return true
	})
	return info
}

// typeName is the package type e is a value of, when refgen can read it: a composite literal
// (or its address), a call to one of the package's functions with one named result, or a local
// variable declared with, or assigned, one of those. "" means it cannot.
func (g *graph) typeName(e ast.Expr, assigned map[string][]ast.Expr, locals map[string]string) string {
	switch x := e.(type) {
	case *ast.CompositeLit:
		if id, ok := x.Type.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return g.typeName(x.X, assigned, locals)
		}
	case *ast.CallExpr:
		id, ok := x.Fun.(*ast.Ident)
		if !ok {
			return ""
		}
		fn := g.funcs[id.Name]
		if fn == nil || fn.Recv != nil || fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
			return ""
		}
		if result, ok := fn.Type.Results.List[0].Type.(*ast.Ident); ok {
			return result.Name
		}
	case *ast.Ident:
		if t, ok := locals[x.Name]; ok {
			return t
		}
		if values := assigned[x.Name]; len(values) > 0 {
			return g.typeName(values[0], assigned, locals)
		}
	}
	return ""
}

// isQueryCall reports whether e is r.URL.Query().
func isQueryCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Query"
}

// caseSentinels is the message of each errors.Is(err, X) target guarding the innermost case
// clause or if body in stack, where refgen can read X's declaration.
func (g *graph) caseSentinels(file *ast.File, stack []ast.Node) []string {
	for i := len(stack) - 1; i >= 0; i-- {
		var guards []ast.Expr
		switch x := stack[i].(type) {
		case *ast.CaseClause:
			guards = x.List
		case *ast.IfStmt:
			if i+1 >= len(stack) || stack[i+1] != x.Body {
				continue
			}
			guards = []ast.Expr{x.Cond}
		default:
			continue
		}
		var out []string
		for _, guard := range guards {
			ast.Inspect(guard, func(n ast.Node) bool {
				if not, ok := n.(*ast.UnaryExpr); ok && not.Op == token.NOT {
					return false // a negated errors.Is guards the other errors
				}
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Is" {
					return true
				}
				if text, ok := g.sentinel(file, call.Args[1]); ok {
					out = append(out, text)
				}
				return false
			})
		}
		return out
	}
	return nil
}

// sentinel reads the message of an error variable: one of this package's, or pkg.Name from one
// of the broker's own packages.
func (g *graph) sentinel(file *ast.File, e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.Ident:
		return g.src.stringConst(x.Name)
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		dir, ok := importDir(file, pkg.Name)
		if !ok {
			return "", false
		}
		src, err := g.pkg(dir)
		if err != nil {
			return "", false
		}
		return src.stringConst(x.Sel.Name)
	}
	return "", false
}

// outcome is one answer a route can give.
type outcome struct {
	Status   int
	Code     string // "" for a success
	Messages []string
	Query    string // a query parameter the route reads
	BodyType string // the type its JSON request body decodes into
	RespType string // a success's: the type its JSON response body encodes, "" for no body
	At       string
}

// resolve lists every outcome of info, whose parameters env binds to the values a caller passed
// (each a set of candidates), following calls into the package's other functions.
func (g *graph) resolve(info funcInfo, params map[string][]string, visiting map[string]bool) ([]outcome, error) {
	env := map[string][]string{}
	for name, values := range params {
		env[name] = values
	}
	for name, exprs := range info.assigned {
		if _, isParam := params[name]; isParam {
			continue
		}
		for _, e := range exprs {
			env[name] = union(env[name], g.eval(e, params, nil))
		}
	}
	var out []outcome
	for _, s := range info.sites {
		switch s.kind {
		case siteError:
			if len(s.call.Args) != 4 {
				return nil, fmt.Errorf("%s: writeError takes (w, status, code, message)", s.at)
			}
			statuses := g.eval(s.call.Args[1], env, nil)
			codes := g.eval(s.call.Args[2], env, nil)
			if len(statuses) == 0 || len(codes) == 0 {
				return nil, fmt.Errorf("%s: cannot read this refusal's status and code", s.at)
			}
			messages := g.eval(s.call.Args[3], env, s.sentinels)
			if len(messages) == 0 {
				messages = []string{varies}
			}
			for _, st := range statuses {
				status, err := strconv.Atoi(st)
				if err != nil {
					return nil, fmt.Errorf("%s: status %q is not a number", s.at, st)
				}
				for _, code := range codes {
					out = append(out, outcome{Status: status, Code: code, Messages: messages, At: s.at})
				}
			}
		case siteSuccess:
			arg := s.call.Args[0]
			if len(s.call.Args) == 3 {
				arg = s.call.Args[1]
			}
			statuses := g.eval(arg, env, nil)
			if len(statuses) == 0 {
				if _, isParam := paramIndex(info.params, arg); isParam {
					continue // a status passed in, as writeError's own WriteHeader takes it
				}
				return nil, fmt.Errorf("%s: cannot read this answer's status", s.at)
			}
			if fun, ok := s.call.Fun.(*ast.Ident); ok && fun.Name == "writeJSON" && s.respType == "" {
				return nil, fmt.Errorf("%s: cannot name this answer's body type; answer with one of the package's named response types", s.at)
			}
			for _, st := range statuses {
				status, err := strconv.Atoi(st)
				if err != nil {
					return nil, fmt.Errorf("%s: status %q is not a number", s.at, st)
				}
				out = append(out, outcome{Status: status, RespType: s.respType, At: s.at})
			}
		case siteCall:
			if s.query != "" {
				out = append(out, outcome{Query: s.query, At: s.at})
				continue
			}
			if s.bodyType != "" {
				out = append(out, outcome{BodyType: s.bodyType, At: s.at})
			}
			if visiting[s.callee] {
				continue
			}
			callee := g.funcs[s.callee]
			calleeInfo := g.collect(g.src.fileOf(callee), callee.Body, callee.Type.Params)
			calleeEnv := map[string][]string{}
			for i, name := range calleeInfo.params {
				if i < len(s.call.Args) {
					calleeEnv[name] = g.eval(s.call.Args[i], env, s.sentinels)
				}
			}
			visiting[s.callee] = true
			got, err := g.resolve(calleeInfo, calleeEnv, visiting)
			delete(visiting, s.callee)
			if err != nil {
				return nil, err
			}
			out = append(out, got...)
		}
	}
	return out, nil
}

func paramIndex(params []string, e ast.Expr) (int, bool) {
	id, ok := e.(*ast.Ident)
	if !ok {
		return 0, false
	}
	for i, p := range params {
		if p == id.Name {
			return i, true
		}
	}
	return 0, false
}

// eval is every string e can be: a literal, a parameter's candidates, an http.Status constant's
// code, a concatenation of those, err.Error() (the case clause's sentinel messages, or varies),
// or an fmt.Sprintf with its verbs shown as "…". Nil means e cannot be read.
func (g *graph) eval(e ast.Expr, env map[string][]string, sentinels []string) []string {
	switch x := e.(type) {
	case *ast.BasicLit:
		if s, ok := strLit(x); ok {
			return []string{s}
		}
		if x.Kind == token.INT {
			return []string{x.Value}
		}
	case *ast.Ident:
		return env[x.Name]
	case *ast.ParenExpr:
		return g.eval(x.X, env, sentinels)
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "http" {
			if code, ok := httpStatus[x.Sel.Name]; ok {
				return []string{strconv.Itoa(code)}
			}
		}
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return nil
		}
		left, right := g.eval(x.X, env, sentinels), g.eval(x.Y, env, sentinels)
		if len(left) == 0 {
			left = []string{"…"}
		}
		if len(right) == 0 {
			right = []string{"…"}
		}
		var out []string
		for _, l := range left {
			for _, r := range right {
				out = append(out, l+r)
			}
		}
		return out
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok {
			return nil
		}
		if sel.Sel.Name == "Error" && len(x.Args) == 0 {
			if len(sentinels) > 0 {
				return sentinels
			}
			return []string{varies}
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" && sel.Sel.Name == "Sprintf" && len(x.Args) > 0 {
			if format, ok := strLit(x.Args[0]); ok {
				return []string{formatVerbs.ReplaceAllString(format, "…")}
			}
		}
	}
	return nil
}

// handlerOutcomes is everything a route's handler can answer, with its authentication's
// refusals left to its auth class.
func (g *graph) handlerOutcomes(r route) ([]outcome, error) {
	fn, ok := g.funcs[r.Handler]
	if !ok {
		return nil, fmt.Errorf("%s: no handler %s", r.At, r.Handler)
	}
	info := g.collect(g.src.fileOf(fn), fn.Body, fn.Type.Params)
	return g.resolve(info, map[string][]string{}, map[string]bool{r.Handler: true})
}

// refusals is the distinct error outcomes in outs, by code and then status.
func refusals(outs []outcome) []outcome {
	byKey := map[string]*outcome{}
	for _, o := range outs {
		if o.Code == "" {
			continue
		}
		key := o.Code + "\x00" + strconv.Itoa(o.Status)
		if prev, ok := byKey[key]; ok {
			prev.Messages = union(prev.Messages, o.Messages)
			continue
		}
		copied := o
		copied.Messages = union(nil, o.Messages)
		byKey[key] = &copied
	}
	out := make([]outcome, 0, len(byKey))
	for _, k := range sortedKeys(byKey) {
		out = append(out, *byKey[k])
	}
	return out
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
