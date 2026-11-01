package main

import (
	"fmt"
	"go/ast"
	"regexp"
	"strings"
)

const (
	configDir  = envoyDir + "/internal/broker/config"
	brokerMain = envoyDir + "/cmd/broker"
)

// variable is one environment variable the broker reads, or refuses because it was removed.
type variable struct {
	Name, Default, Accepted, Doc string
	Group                        string // the variables Doc documents together, as its comment lists them
	Removed                      bool
	At                           string
}

var brokerVar = regexp.MustCompile(`^BROKER_[A-Z0-9_]+$`)

// documentedVars matches the variables a doc comment opens with: "BROKER_X: …" or
// "BROKER_X, BROKER_X_FILE: …".
var documentedVars = regexp.MustCompile(`^((?:BROKER_[A-Z0-9_]+)(?:, BROKER_[A-Z0-9_]+)*): (.*)$`)

// readConfig reads every BROKER_* variable config.Load and cmd/broker read (any BROKER_* name the
// two spell out as a string is one they read): each one's description from the doc comment that
// opens with its name (a Config field's, or a comment in cmd/broker), its default and accepted
// range from Load's own tables, and the variables Load refuses because they were removed. It
// refuses a variable the code reads that no comment documents, a comment that documents a variable
// nothing reads, and a removed variable with no reason.
func readConfig(root string) ([]variable, error) {
	cfg, err := parseDir(root, configDir)
	if err != nil {
		return nil, err
	}
	main, err := parseDir(root, brokerMain)
	if err != nil {
		return nil, err
	}
	vars := map[string]*variable{}
	// doc is what the comment documenting a variable says, the variables it documents together, and
	// where it is.
	type doc struct{ text, group, at string }
	docs := map[string]doc{}
	addDocs := func(src *source, cg *ast.CommentGroup) {
		m := documentedVars.FindStringSubmatch(prose(cg))
		if m == nil {
			return
		}
		for name := range strings.SplitSeq(m[1], ", ") {
			docs[name] = doc{text: m[2], group: m[1], at: src.at(cg)}
		}
	}

	ts, _ := cfg.typeSpec("Config")
	if ts == nil {
		return nil, fmt.Errorf("%s declares no Config", configDir)
	}
	for _, f := range ts.Type.(*ast.StructType).Fields.List {
		addDocs(cfg, f.Doc)
	}
	for _, f := range main.files {
		for _, cg := range f.Comments {
			addDocs(main, cg)
		}
	}

	read := func(src *source, name string, n ast.Node) {
		if _, ok := vars[name]; !ok {
			vars[name] = &variable{Name: name, At: src.at(n)}
		}
	}
	if vs, _ := cfg.valueSpec("removedVars"); vs != nil {
		for _, elt := range vs.Values[0].(*ast.CompositeLit).Elts {
			row := elt.(*ast.CompositeLit)
			name, _ := strLit(row.Elts[0])
			reason, ok := strLit(row.Elts[1])
			if !ok {
				if id, isIdent := row.Elts[1].(*ast.Ident); isIdent {
					reason, ok = cfg.stringConst(id.Name)
				}
			}
			if name == "" || !ok {
				return nil, fmt.Errorf("%s: a removedVars row is {name, reason}", cfg.at(row))
			}
			if strings.TrimSpace(reason) == "" {
				return nil, fmt.Errorf("%s: removed variable %s has no reason saying why it is gone", cfg.at(row), name)
			}
			vars[name] = &variable{Name: name, Removed: true, Doc: reason, At: cfg.at(row)}
		}
	}
	for _, f := range cfg.files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				fun, _ := x.Fun.(*ast.Ident)
				switch {
				case fun == nil:
				case fun.Name == "secretValue" && len(x.Args) == 2:
					if name, ok := strLit(x.Args[1]); ok {
						read(cfg, name, x)
						read(cfg, name+"_FILE", x)
					}
				case fun.Name == "orDefault" && len(x.Args) == 2:
					inner, ok := x.Args[0].(*ast.CallExpr)
					def, ok2 := strLit(x.Args[1])
					if ok && ok2 && len(inner.Args) == 1 {
						if name, ok := strLit(inner.Args[0]); ok {
							read(cfg, name, x)
							vars[name].Default = def
						}
					}
				}
			case *ast.CompositeLit:
				// Load's integer table: {"BROKER_X", &cfg.Field, default, min, max}.
				if len(x.Elts) != 5 {
					return true
				}
				name, ok := strLit(x.Elts[0])
				def, ok2 := intLit(x.Elts[2])
				lo, ok3 := intLit(x.Elts[3])
				hi, ok4 := intLit(x.Elts[4])
				if ok && ok2 && ok3 && ok4 && brokerVar.MatchString(name) {
					read(cfg, name, x)
					vars[name].Default = fmt.Sprint(def)
					vars[name].Accepted = fmt.Sprintf("whole number from %d to %d", lo, hi)
				}
			case *ast.BasicLit:
				// Every other name the loader spells out, such as the required-variable list and
				// the rules sources oidc.ConfigFromEnv reads.
				if name, ok := strLit(x); ok && brokerVar.MatchString(name) {
					read(cfg, name, x)
				}
			}
			return true
		})
	}
	for _, f := range main.files {
		ast.Inspect(f, func(n ast.Node) bool {
			// Every name cmd/broker spells out is one it reads, whichever call reads it.
			if lit, ok := n.(*ast.BasicLit); ok {
				if name, ok := strLit(lit); ok && brokerVar.MatchString(name) {
					read(main, name, lit)
				}
			}
			return true
		})
	}

	var out []variable
	for _, name := range sortedKeys(vars) {
		v := vars[name]
		if v.Removed {
			out = append(out, *v)
			continue
		}
		d, ok := docs[name]
		if !ok {
			return nil, fmt.Errorf("%s: the broker reads %s, and no comment opens with %q to document it", v.At, name, name+":")
		}
		v.Doc, v.Group = d.text, d.group
		out = append(out, *v)
	}
	for _, name := range sortedKeys(docs) {
		if _, ok := vars[name]; !ok {
			return nil, fmt.Errorf("%s: documents %s, which the broker does not read", docs[name].at, name)
		}
	}
	return out, nil
}
