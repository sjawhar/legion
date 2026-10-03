package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// envoyModule is the broker's Go module and envoyDir where it lives in the repository, so an
// import of one of its packages maps to a directory refgen can parse.
const (
	envoyModule = "github.com/sjawhar/envoy"
	envoyDir    = "packages/envoy"
)

// source is one Go package's non-test files, parsed with their comments.
type source struct {
	root  string // the repository root
	dir   string // the package's directory, relative to root
	fset  *token.FileSet
	files []*ast.File
}

func parseDir(root, dir string) (*source, error) {
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		return nil, err
	}
	s := &source{root: root, dir: dir, fset: token.NewFileSet()}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(s.fset, filepath.Join(root, dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		s.files = append(s.files, f)
	}
	if len(s.files) == 0 {
		return nil, fmt.Errorf("%s has no Go files", dir)
	}
	return s, nil
}

// at is n's position as "dir/file.go:line", relative to the repository root.
func (s *source) at(n ast.Node) string {
	p := s.fset.Position(n.Pos())
	rel, err := filepath.Rel(s.root, p.Filename)
	if err != nil {
		rel = p.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), p.Line)
}

func (s *source) line(pos token.Pos) int { return s.fset.Position(pos).Line }

// fileOf is the file that holds n.
func (s *source) fileOf(n ast.Node) *ast.File {
	for _, f := range s.files {
		if f.Pos() <= n.Pos() && n.End() <= f.End() {
			return f
		}
	}
	return nil
}

// funcs is every top-level function and method, keyed by name; a method is keyed by its name
// alone, and two declarations sharing a name are refused, so a call by name is never ambiguous.
func (s *source) funcs() (map[string]*ast.FuncDecl, error) {
	out := map[string]*ast.FuncDecl{}
	for _, f := range s.files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if prev, dup := out[fn.Name.Name]; dup {
				return nil, fmt.Errorf("%s and %s both declare %s; refgen resolves calls by name", s.at(prev), s.at(fn), fn.Name.Name)
			}
			out[fn.Name.Name] = fn
		}
	}
	return out, nil
}

// typeSpec finds a named type declared in the package.
func (s *source) typeSpec(name string) *ast.TypeSpec {
	for _, f := range s.files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				if ts := spec.(*ast.TypeSpec); ts.Name.Name == name {
					return ts
				}
			}
		}
	}
	return nil
}

// valueSpec finds a package-level var or const and the declaration holding it.
func (s *source) valueSpec(name string) (*ast.ValueSpec, *ast.GenDecl, int) {
	for _, f := range s.files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || (gd.Tok != token.VAR && gd.Tok != token.CONST) {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if n.Name == name {
						return vs, gd, i
					}
				}
			}
		}
	}
	return nil, nil, 0
}

// stringConst is the string a package-level const or var is set to: a literal, an
// errors.New(literal), or an fmt.Errorf(literal) with its verbs shown as "…".
func (s *source) stringConst(name string) (string, bool) {
	vs, _, i := s.valueSpec(name)
	if vs == nil || i >= len(vs.Values) {
		return "", false
	}
	switch v := vs.Values[i].(type) {
	case *ast.BasicLit:
		return strLit(v)
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		if !ok || len(v.Args) == 0 {
			return "", false
		}
		pkg, _ := sel.X.(*ast.Ident)
		if pkg == nil {
			return "", false
		}
		text, ok := strLit(v.Args[0])
		switch {
		case !ok:
			return "", false
		case pkg.Name == "errors" && sel.Sel.Name == "New":
			return text, true
		case pkg.Name == "fmt" && sel.Sel.Name == "Errorf":
			return formatVerbs.ReplaceAllString(text, "…"), true
		}
	}
	return "", false
}

// formatVerbs matches the fmt verbs a rendered message shows as "…".
var formatVerbs = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

func strLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func intLit(e ast.Expr) (int, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.Atoi(lit.Value)
	return n, err == nil
}

// prose is a doc comment as one paragraph.
func prose(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return strings.Join(strings.Fields(cg.Text()), " ")
}

// lineComment is the text of a // comment that sits alone on the line just above line.
func (s *source) commentAbove(f *ast.File, line int) string {
	for _, cg := range f.Comments {
		if s.line(cg.End()) == line-1 {
			return prose(cg)
		}
	}
	return ""
}

// importDir maps the package an identifier names in file f to the repository directory it
// lives in, when it is one of the broker's own packages.
func importDir(f *ast.File, ident string) (string, bool) {
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name != ident {
			continue
		}
		rest, ok := strings.CutPrefix(path, envoyModule+"/")
		if !ok {
			return "", false
		}
		return envoyDir + "/" + rest, true
	}
	return "", false
}

// httpStatus maps a net/http status constant's name to its code, from the names http.StatusText
// gives each code.
var httpStatus = func() map[string]int {
	out := map[string]int{}
	for code := 100; code < 600; code++ {
		text := http.StatusText(code)
		if text == "" {
			continue
		}
		name := "Status" + strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, text)
		out[name] = code
	}
	return out
}()

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
