package main

import (
	"fmt"
	"go/ast"
	"go/printer"
	"strings"
)

// checkHelperRefusals refuses a helper refusal whose code the error page cannot list: every
// Response literal in the helper package that is not OK: true and sets any field must name its
// fields and set Code to one of the documented Code constants in codes. (An OK reply may carry a
// login's confirmation code in Code, and the empty Response is no reply at all.)
func checkHelperRefusals(root string, codes []docConst) error {
	src, err := parseDir(root, helperDir)
	if err != nil {
		return err
	}
	documented := map[string]bool{}
	for _, c := range codes {
		documented[c.name] = true
	}
	var problems []string
	for _, f := range src.files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || len(lit.Elts) == 0 {
				return true
			}
			if t, ok := lit.Type.(*ast.Ident); !ok || t.Name != "Response" {
				return true
			}
			var code ast.Expr
			for _, e := range lit.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					problems = append(problems, src.at(lit)+": a helper Response names its fields")
					return true
				}
				key, _ := kv.Key.(*ast.Ident)
				switch {
				case key == nil:
				case key.Name == "OK":
					if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "true" {
						return true
					}
				case key.Name == "Code":
					code = kv.Value
				}
			}
			switch id, _ := code.(*ast.Ident); {
			case code == nil:
				problems = append(problems, src.at(lit)+": a helper refusal sets no Code")
			case id == nil || !documented[id.Name]:
				problems = append(problems, fmt.Sprintf("%s: a helper refusal's Code is %s, not one of the documented Code constants", src.at(lit), src.text(code)))
			}
			return true
		})
	}
	return refusedAt(problems)
}

// checkExitCodes refuses an agent-secrets exit code the error page cannot list: every function in
// the CLI's package that returns one int, which is what run and each command it dispatches to
// return as the process's exit code, may return only 0, 1, one of the documented exit constants in
// codes, or what another such function returns; so may os.Exit be called only with one of those.
func checkExitCodes(root string, codes []docConst) error {
	src, err := parseDir(root, cliDir)
	if err != nil {
		return err
	}
	documented := map[string]bool{}
	for _, c := range codes {
		documented[c.name] = true
	}
	returnsInt := func(t *ast.FuncType) bool {
		if t.Results == nil || len(t.Results.List) != 1 || len(t.Results.List[0].Names) > 1 {
			return false
		}
		id, ok := t.Results.List[0].Type.(*ast.Ident)
		return ok && id.Name == "int"
	}
	exitFuncs := map[string]bool{}
	for _, f := range src.files {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && returnsInt(fn.Type) {
				exitFuncs[fn.Name.Name] = true
			}
		}
	}
	var problems []string
	allowed := func(e ast.Expr) {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Value == "0" || v.Value == "1" {
				return
			}
		case *ast.Ident:
			if documented[v.Name] {
				return
			}
		case *ast.CallExpr:
			if fun, ok := v.Fun.(*ast.Ident); ok && exitFuncs[fun.Name] {
				return
			}
		}
		problems = append(problems, fmt.Sprintf("%s: exit code %s is not 0, 1 or one of the documented exit constants", src.at(e), src.text(e)))
	}
	// checkBody checks the returns of one int-returning function body, leaving each function
	// literal inside it to its own check.
	var checkBody func(body *ast.BlockStmt)
	checkBody = func(body *ast.BlockStmt) {
		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				if returnsInt(x.Type) {
					checkBody(x.Body)
				}
				return false
			case *ast.ReturnStmt:
				if len(x.Results) != 1 {
					problems = append(problems, src.at(x)+": an exit code is one value")
					return true
				}
				allowed(x.Results[0])
			}
			return true
		})
	}
	for _, f := range src.files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Recv == nil && returnsInt(fn.Type) {
				checkBody(fn.Body)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Exit" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" {
					allowed(call.Args[0])
				}
			}
			return true
		})
	}
	return refusedAt(problems)
}

// refusedAt is one error listing every problem, or nil when there is none.
func refusedAt(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "\n"))
}

// text is the source text of e, as gofmt would print it.
func (s *source) text(e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, s.fset, e); err != nil {
		return fmt.Sprintf("%T", e)
	}
	return b.String()
}
