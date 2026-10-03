package main

import (
	"fmt"
	"go/ast"
	"reflect"
	"strconv"
	"strings"
)

// structRef is a named struct type, the declaration holding it, and the package that declares it.
type structRef struct {
	src  *source
	spec *ast.TypeSpec
	decl *ast.GenDecl
}

// doc is the type's doc comment, or its declaration's when the type is declared alone.
func (r structRef) doc() string { return docOf(r.spec.Doc, r.decl.Doc) }

// pkg parses one of the broker's own packages, once.
func (g *graph) pkg(dir string) (*source, error) {
	if src, ok := g.pkgs[dir]; ok {
		return src, nil
	}
	src, err := parseDir(g.src.root, dir)
	if err != nil {
		return nil, err
	}
	g.pkgs[dir] = src
	return src, nil
}

// structOf is the named struct type e names from a file of src: one of src's own types, or
// pkg.Name from one of the broker's packages. ok is false for any other type.
func (g *graph) structOf(src *source, file *ast.File, e ast.Expr) (structRef, bool, error) {
	switch x := e.(type) {
	case *ast.Ident:
		ts, gd := src.typeSpec(x.Name)
		if ts == nil {
			return structRef{}, false, nil
		}
		_, isStruct := ts.Type.(*ast.StructType)
		return structRef{src, ts, gd}, isStruct, nil
	case *ast.SelectorExpr:
		pkgName, ok := x.X.(*ast.Ident)
		if !ok {
			return structRef{}, false, nil
		}
		dir, ok := importDir(file, pkgName.Name)
		if !ok {
			return structRef{}, false, nil
		}
		other, err := g.pkg(dir)
		if err != nil {
			return structRef{}, false, err
		}
		ts, gd := other.typeSpec(x.Sel.Name)
		if ts == nil {
			return structRef{}, false, fmt.Errorf("%s declares no type %s", dir, x.Sel.Name)
		}
		_, isStruct := ts.Type.(*ast.StructType)
		return structRef{other, ts, gd}, isStruct, nil
	}
	return structRef{}, false, nil
}

// jsonField is one JSON field of a struct type, with the object it holds when it holds one.
type jsonField struct {
	Name, Type, Doc string
	Object          *structRef
	Many            bool // the field is an array of Object
}

// fields reads a struct type's JSON fields from their tags, refusing a field with no doc comment
// saying what it is or a type refgen cannot name as JSON.
func (g *graph) fields(ref structRef) ([]jsonField, error) {
	st, ok := ref.spec.Type.(*ast.StructType)
	if !ok {
		return nil, fmt.Errorf("%s: %s is not a struct", ref.src.at(ref.spec), ref.spec.Name.Name)
	}
	file := ref.src.fileOf(ref.spec)
	var out []jsonField
	for _, f := range st.Fields.List {
		if f.Tag == nil || len(f.Names) != 1 {
			return nil, fmt.Errorf("%s: every field of %s is one named field with a json tag", ref.src.at(f), ref.spec.Name.Name)
		}
		tag, err := strconv.Unquote(f.Tag.Value)
		if err != nil {
			return nil, err
		}
		jsonTag, ok := reflect.StructTag(tag).Lookup("json")
		if !ok {
			return nil, fmt.Errorf("%s: field %s of %s has no json tag", ref.src.at(f), f.Names[0].Name, ref.spec.Name.Name)
		}
		if jsonTag == "-" {
			continue
		}
		name, options, _ := strings.Cut(jsonTag, ",")
		doc := docOf(f.Doc, f.Comment)
		if doc == "" {
			return nil, fmt.Errorf("%s: field %s of %s has no doc comment saying what it is", ref.src.at(f), f.Names[0].Name, ref.spec.Name.Name)
		}
		field := jsonField{Name: name, Doc: doc}
		field.Type, err = g.jsonType(ref.src, file, f.Type, &field)
		if err != nil {
			return nil, err
		}
		if options == "omitempty" {
			field.Type += " or absent"
		}
		out = append(out, field)
	}
	return out, nil
}

// jsonType names a Go field type as its JSON appears on the wire: a pointer is the value or null,
// a slice an array, a time.Time an RFC 3339 string, and a named struct an object, which it records
// on field so its own fields can be listed.
func (g *graph) jsonType(src *source, file *ast.File, e ast.Expr, field *jsonField) (string, error) {
	switch x := e.(type) {
	case *ast.StarExpr:
		inner, err := g.jsonType(src, file, x.X, field)
		return inner + " or null", err
	case *ast.ArrayType:
		inner, err := g.jsonType(src, file, x.Elt, field)
		if field.Object != nil {
			field.Many = true
			return "array of objects", err
		}
		return "array of " + inner + "s", err
	case *ast.MapType:
		key, ok := x.Key.(*ast.Ident)
		if !ok || key.Name != "string" {
			break
		}
		inner, err := g.jsonType(src, file, x.Value, field)
		return "object of " + inner + " values", err
	case *ast.Ident:
		switch x.Name {
		case "string":
			return "string", nil
		case "bool":
			return "boolean", nil
		case "int", "int32", "int64", "float64":
			return "number", nil
		}
	case *ast.SelectorExpr:
		if pkgName, ok := x.X.(*ast.Ident); ok && pkgName.Name == "time" && x.Sel.Name == "Time" {
			return "string (RFC 3339 time)", nil
		}
	}
	ref, ok, err := g.structOf(src, file, e)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%s: refgen cannot name this field's JSON type", src.at(e))
	}
	field.Object = &ref
	return "object", nil
}

// writeFields writes a table of ref's JSON fields, then one for each object a field holds, each
// titled with the object's path from the body (path is "" for the body itself).
func (g *graph) writeFields(b *strings.Builder, ref structRef, path string) error {
	fields, err := g.fields(ref)
	if err != nil {
		return err
	}
	b.WriteString("| Field | Type | Description |\n| --- | --- | --- |\n")
	for _, f := range fields {
		fmt.Fprintf(b, "| `%s` | `%s` | %s |\n", f.Name, cellCode(f.Type), cell(capitalize(f.Doc)))
	}
	for _, f := range fields {
		if f.Object == nil {
			continue
		}
		sub := f.Name
		if path != "" {
			sub = path + "." + f.Name
		}
		if f.Many {
			sub += "[]"
		}
		fmt.Fprintf(b, "\nFields of `%s`:\n\n", sub)
		if err := g.writeFields(b, *f.Object, sub); err != nil {
			return err
		}
	}
	return nil
}
