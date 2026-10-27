package ghnotify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheClientTypeHasOneMethod: the poller reads GitHub through one method, get, so no code of
// the package can send GitHub anything but a GET -- it never marks a notification read and never
// changes a subscription (R5, A5's static half; the fake GitHub server, which answers 405 to every
// other method and fails the test when it records one, is the live half).
func TestTheClientTypeHasOneMethod(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package's files: %v", err)
	}
	var methods []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			if receiverName(fn.Recv.List[0].Type) == "client" {
				methods = append(methods, fn.Name.Name)
			}
		}
	}
	if len(methods) != 1 || methods[0] != "get" {
		t.Fatalf("the client type has the methods %v, want get alone", methods)
	}
}

func receiverName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	name, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	return name.Name
}
