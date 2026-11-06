package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// openAPIDocumentForTest is as much of the document as these tests read: the operations by path
// and lower-cased method, and the schemas it declares.
type openAPIDocumentForTest struct {
	Paths      map[string]map[string]openAPIOperationForTest `json:"paths"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

type openAPIOperationForTest struct {
	ID       string `json:"operationId"`
	TestHook bool   `json:"x-test-hook"`
}

// renderOpenAPI writes the document and hands back its bytes.
func renderOpenAPI(t testing.TB) []byte {
	t.Helper()
	var document bytes.Buffer
	if err := WriteOpenAPI(&document); err != nil {
		t.Fatalf("WriteOpenAPI: %v", err)
	}
	return document.Bytes()
}

// readOpenAPI writes the document and reads back what these tests assert on.
func readOpenAPI(t testing.TB) ([]byte, openAPIDocumentForTest) {
	t.Helper()
	raw := renderOpenAPI(t)
	var document openAPIDocumentForTest
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("read the emitted document: %v", err)
	}
	return raw, document
}

// The document is a committed artifact and a generator's input, so two runs on one tree must write
// the same bytes: a map walked in its own order would make every run a diff, and no reader could
// tell a real change from the walk.
func TestOpenAPIDocumentIsTheSameBytesEveryRun(t *testing.T) {
	first, second := renderOpenAPI(t), renderOpenAPI(t)
	if !bytes.Equal(first, second) {
		t.Fatalf("two runs wrote different bytes: %d and %d bytes", len(first), len(second))
	}
	if !bytes.HasSuffix(first, []byte("\n")) {
		t.Error("the document does not end with a newline")
	}
}

// The document describes the API, so it holds the table and the table holds it: a row with no
// operation is a route a client is given no type for, and an operation that is no row is a route
// nothing serves.
func TestOpenAPIDocumentHasOneOperationPerMountedRoute(t *testing.T) {
	_, document := readOpenAPI(t)
	described := map[string]bool{}
	for pattern, item := range document.Paths {
		for method := range item {
			described[strings.ToUpper(method)+" "+pattern] = true
		}
	}
	mounted := map[string]bool{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		mounted[route.Method+" "+route.Pattern] = true
	}
	for name := range mounted {
		if !described[name] {
			t.Errorf("%s is mounted and the document describes no operation for it", name)
		}
	}
	for name := range described {
		if !mounted[name] {
			t.Errorf("the document describes %s, which the table does not mount", name)
		}
	}
	if len(described) != len(mounted) {
		t.Fatalf("the document holds %d operations and the table %d rows", len(described), len(mounted))
	}
}

// Every reference resolves to a schema the document declares. An unresolved one is a type a
// generator renders as `unknown` or refuses outright, and nothing else in the pipeline would say
// which reference it was.
func TestOpenAPIDocumentDeclaresEverySchemaItReferences(t *testing.T) {
	raw, document := readOpenAPI(t)
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("read the emitted document: %v", err)
	}
	references := map[string]bool{}
	collectOpenAPIReferences(tree, references)
	if len(references) == 0 {
		t.Fatal("the document holds no reference at all, so this test would pass on an empty document")
	}
	var unresolved []string
	for reference := range references {
		name, inComponents := strings.CutPrefix(reference, "#/components/schemas/")
		if !inComponents {
			unresolved = append(unresolved, reference+": not a component schema reference")
			continue
		}
		if _, declared := document.Components.Schemas[name]; !declared {
			unresolved = append(unresolved, reference+": no such schema")
		}
	}
	sort.Strings(unresolved)
	if len(unresolved) > 0 {
		t.Fatalf("references the document does not declare:\n\t%s", strings.Join(unresolved, "\n\t"))
	}
}

// collectOpenAPIReferences gathers every $ref the document holds, wherever it holds it.
func collectOpenAPIReferences(node any, into map[string]bool) {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if reference, isString := child.(string); key == "$ref" && isString {
				into[reference] = true
				continue
			}
			collectOpenAPIReferences(child, into)
		}
	case []any:
		for _, child := range value {
			collectOpenAPIReferences(child, into)
		}
	}
}

// A test hook is a row a deployment may leave out, so the document marks each one and nothing
// else: a client generated from it keeps the hooks out of the API it offers, and a hook that lost
// its mark would be offered as an ordinary route.
func TestOpenAPIDocumentMarksEveryTestHookAndNothingElse(t *testing.T) {
	_, document := readOpenAPI(t)
	marked := map[string]bool{}
	for pattern, item := range document.Paths {
		for method, operation := range item {
			if operation.TestHook {
				marked[strings.ToUpper(method)+" "+pattern] = true
			}
		}
	}
	hooks := openAPIHookRoutes()
	if len(hooks) != 12 {
		t.Fatalf("the table mounts %d test hooks with Deps.TestHooksEnabled, want 12", len(hooks))
	}
	for name := range hooks {
		if !marked[name] {
			t.Errorf("%s is a test hook and its operation carries no x-test-hook", name)
		}
	}
	for name := range marked {
		if !hooks[name] {
			t.Errorf("%s carries x-test-hook and is no test hook", name)
		}
	}
}

// The committed artifact is what the clients' types are rendered from, so a tree whose document
// has moved on fails `go test` as well as `envoy-dispatch openapi --check`: nobody has to remember
// to run the command.
func TestTheCommittedOpenAPIDocumentIsWhatTheEmitterWrites(t *testing.T) {
	// From this package to the repository root, which OpenAPIDocumentPath is written from.
	path := filepath.Join("..", "..", "..", "..", "..", filepath.FromSlash(OpenAPIDocumentPath))
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed document: %v", err)
	}
	if !bytes.Equal(committed, renderOpenAPI(t)) {
		t.Fatalf("%s is stale: run `go run ./cmd/dispatch openapi --out ../../%s` in packages/envoy and commit the result",
			OpenAPIDocumentPath, OpenAPIDocumentPath)
	}
}

// A request body's required set is what its own Go fields tag `required:"true"`, and nothing
// else. The "no omitempty" rule the other schemas take is Go's marshalling, which says what a
// reader of a response always gets and nothing at all about what a caller must send: held to it,
// createIssueInput.Parent would render `parent: string | null` where TypeScript has
// `parent?: string`, and every call site that leaves it out would stop compiling. So the two
// sets are read apart here - one from the document the emitter writes, one from the Go fields by
// reflection - and held equal, and neither can drift from the other without this failing.
func TestOpenAPIRequestBodySchemasRequireWhatTheirFieldsTag(t *testing.T) {
	_, document := readOpenAPI(t)
	bodies, bothDirections := openAPIRequestBodyTypes()
	if len(bodies) == 0 {
		t.Fatal("the table reflects no request body at all, so this test would pass on anything")
	}
	held := 0
	for _, body := range sortedOpenAPITypes(bodies) {
		name := openAPISchemaName(body)
		if name == "" {
			// An unnamed struct is written where it stands, and is held against its tags by
			// the walk of the named body that holds it.
			continue
		}
		declared, inDocument := document.Components.Schemas[name]
		if !inDocument {
			t.Errorf("%s is a request body and the document declares no schema %s for it",
				openAPIGoTypeName(body), name)
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(declared, &schema); err != nil {
			t.Errorf("read schema %s: %v", name, err)
			continue
		}
		held += holdOpenAPIRequiredToTags(t, body, schema)
	}
	if held == 0 {
		t.Fatal("no request body was held to its tags, so this test proves nothing")
	}
	for _, body := range bothDirections {
		t.Errorf("%s is reflected both as a request body and as a response, and one schema "+
			"cannot answer for both directions", openAPIGoTypeName(body))
	}
}

// holdOpenAPIRequiredToTags holds one request body's schema to its Go fields' tags, and every
// unnamed struct it holds to theirs: those are written where they stand rather than declared, so
// nothing else reaches them. It answers how many schemas it read, so a walk that silently found
// nothing cannot pass for a walk that found everything right.
func holdOpenAPIRequiredToTags(t *testing.T, body reflect.Type, schema map[string]any) int {
	t.Helper()
	name := openAPIGoTypeName(body)
	got, want := openAPIRequiredOf(t, name, schema), openAPITaggedRequired(body)
	if !slices.Equal(got, want) {
		t.Errorf("%s requires %v and its fields tag %v: what a caller must send is the "+
			"handler's own refusals, which only `required:\"true\"` states", name, got, want)
	}
	read := 1
	properties, _ := schema["properties"].(map[string]any)
	for index := range body.NumField() {
		field := body.Field(index)
		held := openAPIStructOf(field.Type)
		if held == nil || held.Name() != "" {
			continue
		}
		property, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if property == "" || property == "-" {
			continue
		}
		inner, written := properties[property].(map[string]any)
		if !written {
			t.Errorf("%s holds an unnamed struct in %s and the schema writes no such property",
				name, property)
			continue
		}
		// A slice of unnamed structs is an array of them, and the struct is under `items`.
		for element := field.Type; written && element.Kind() != reflect.Struct; element = element.Elem() {
			if element.Kind() != reflect.Slice && element.Kind() != reflect.Array {
				continue
			}
			if inner, written = inner["items"].(map[string]any); !written {
				t.Errorf("%s writes %s as no array of objects", name, property)
			}
		}
		if written {
			read += holdOpenAPIRequiredToTags(t, held, inner)
		}
	}
	return read
}

// sortedOpenAPITypes orders a set of Go types by the name their package writes, so a failure
// names the same type whichever run reads it.
func sortedOpenAPITypes(types map[reflect.Type]bool) []reflect.Type {
	ordered := make([]reflect.Type, 0, len(types))
	for goType := range types {
		ordered = append(ordered, goType)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return openAPIGoTypeName(ordered[i]) < openAPIGoTypeName(ordered[j])
	})
	return ordered
}

// openAPIRequiredOf is one schema's `required`, sorted.
func openAPIRequiredOf(t *testing.T, name string, schema map[string]any) []string {
	t.Helper()
	held, carried := schema["required"].([]any)
	if !carried {
		return nil
	}
	required := make([]string, 0, len(held))
	for _, property := range held {
		text, isText := property.(string)
		if !isText {
			t.Errorf("%s requires %v, which is no property name", name, property)
			continue
		}
		required = append(required, text)
	}
	sort.Strings(required)
	return required
}

// openAPITaggedRequired is the json names of one Go type's fields tagged `required:"true"`, the
// tag jsonschema-go reads (reflect.go, ReadBoolTag), sorted. An embedded struct carries its own
// fields into the object, so its tags are read as the outer type's.
func openAPITaggedRequired(goType reflect.Type) []string {
	var required []string
	for index := range goType.NumField() {
		field := goType.Field(index)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" && field.Anonymous && field.Type.Kind() == reflect.Struct {
			required = append(required, openAPITaggedRequired(field.Type)...)
			continue
		}
		if name == "" {
			name = field.Name
		}
		if field.Tag.Get("required") == "true" {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	return required
}
