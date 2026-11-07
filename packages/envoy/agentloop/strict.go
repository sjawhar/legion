package agentloop

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// strictKeywords is every JSON Schema keyword a Final schema may carry: the keywords both
// providers' documented structured-output subsets enforce and this package's SDK sends on, and
// the two annotations that constrain nothing. Anything else is refused by name — minLength,
// maximum, multipleOf, pattern, minItems, uniqueItems, const, a keyword a later draft adds —
// because a constraint that does not reach the provider's grammar is a constraint the caller
// believes in and the model never sees. The SDK's Anthropic provider is the clearest case: it
// strips every one of those keywords out of the schema and writes it into the node's
// description, which is prose to the model and binds nothing.
//
//	https://platform.claude.com/docs/en/build-with-claude/structured-outputs#json-schema-limitations
//	https://developers.openai.com/api/docs/guides/structured-outputs#supported-schemas
var strictKeywords = []string{
	"$defs", "$ref", "additionalProperties", "definitions", "description", "enum", "format",
	"items", "properties", "required", "title", "type",
}

// strictUnions is every keyword that joins subschemas. A Final schema takes none of them:
// OpenAI's strict mode refuses allOf, not, if, then and else outright and refuses an anyOf at
// the root, Anthropic refuses allOf with $ref and counts every anyOf and every type array
// against a per-request cap of sixteen union parameters, and one Request.Final rides to
// whichever provider the request names.
var strictUnions = []string{"allOf", "anyOf", "else", "if", "not", "oneOf", "then"}

// strictTypes is every type a Final schema's node may be: the basic types, each on its own.
var strictTypes = []string{"array", "boolean", "integer", "null", "number", "object", "string"}

// strictFormats is every string format both providers enforce: Anthropic's list without uri,
// which OpenAI's does not carry.
var strictFormats = []string{
	"date", "date-time", "duration", "email", "hostname", "ipv4", "ipv6", "time", "uuid",
}

// strictFinal is Request.Final held to the structured-output subset both providers enforce,
// which is how a run finds out that a schema would ride to the model and bind nothing. The
// error names the JSON path of the node at fault and what is wrong with it, so the caller fixes
// the schema rather than the symptom; the caller of this function gives it its prefix.
func strictFinal(final json.RawMessage) error {
	var doc any
	if err := json.Unmarshal(final, &doc); err != nil {
		return err
	}
	s := &strictSchema{doc: doc}
	return s.node(doc, "")
}

// strictSchema is one walk of a Final schema: the whole document, which a $ref points into, and
// the $ref targets the walk is inside, which is how a recursive schema is caught.
type strictSchema struct {
	doc  any
	refs []string
}

// node is one subschema checked and walked: its keywords, its type, then what that type brings
// with it. Every walk of a node that holds other nodes goes through here, so no part of a
// schema is left unread.
func (s *strictSchema) node(value any, path string) error {
	m, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: not a JSON object", at(path))
	}
	for _, key := range slices.Sorted(maps.Keys(m)) {
		switch {
		case slices.Contains(strictUnions, key):
			return fmt.Errorf("%s: an %s union (a node of a Final schema takes one type)", at(path), key)
		case !slices.Contains(strictKeywords, key):
			return fmt.Errorf("%s: unsupported keyword %q", at(path), key)
		}
	}
	if _, ok := m["$ref"]; ok {
		// The shape a schema generator writes when the type it reflects is named:
		// a root that is one $ref into $defs. Neither provider takes it.
		if path == "" {
			return fmt.Errorf("%s: the schema itself is a $ref, not an object", at(path))
		}
		return s.ref(m, path)
	}

	kind := ""
	if raw, ok := m["type"]; ok {
		switch t := raw.(type) {
		case string:
			kind = t
		case []any:
			return fmt.Errorf("%s: a type union (type is an array)", at(path))
		default:
			return fmt.Errorf("%s: type is not a string", at(path))
		}
		if !slices.Contains(strictTypes, kind) {
			return fmt.Errorf("%s: unsupported type %q", at(path), kind)
		}
	}
	if path == "" && kind != "object" {
		return fmt.Errorf("%s: the schema itself is not an object", at(path))
	}
	if err := s.scalar(m, path); err != nil {
		return err
	}

	_, properties := m["properties"]
	_, required := m["required"]
	_, additional := m["additionalProperties"]
	if kind == "object" || properties || required || additional {
		if err := s.object(m, path); err != nil {
			return err
		}
	}
	if _, items := m["items"]; kind == "array" || items {
		if err := s.array(m, path); err != nil {
			return err
		}
	}
	return s.defs(m, path)
}

// ref is a $ref node: it names a place in this schema and nothing else, the walk goes on at what
// it names, and a $ref the walk is already inside is a recursive schema, which Anthropic does
// not serve.
func (s *strictSchema) ref(m map[string]any, path string) error {
	pointer, ok := m["$ref"].(string)
	if !ok {
		return fmt.Errorf("%s: $ref is not a string", at(path))
	}
	for _, key := range slices.Sorted(maps.Keys(m)) {
		if key != "$ref" && key != "description" && key != "title" {
			return fmt.Errorf("%s: a $ref that carries %q as well", at(path), key)
		}
	}
	if slices.Contains(s.refs, pointer) {
		return fmt.Errorf("%s: a recursive $ref, %q", at(path), pointer)
	}
	target, err := s.resolve(pointer, path)
	if err != nil {
		return err
	}
	s.refs = append(s.refs, pointer)
	defer func() { s.refs = s.refs[:len(s.refs)-1] }()
	return s.node(target, path)
}

// resolve is the node a pointer names. An external $ref is a document neither provider loads,
// and a pointer that names nothing is a schema that cannot be compiled at all.
func (s *strictSchema) resolve(pointer, path string) (any, error) {
	if pointer == "#" {
		return s.doc, nil
	}
	if !strings.HasPrefix(pointer, "#/") {
		return nil, fmt.Errorf("%s: $ref %q points outside this schema", at(path), pointer)
	}
	node := s.doc
	for _, segment := range strings.Split(strings.TrimPrefix(pointer, "#/"), "/") {
		segment = strings.ReplaceAll(segment, "~1", "/")
		segment = strings.ReplaceAll(segment, "~0", "~")
		m, ok := node.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: $ref %q names nothing in this schema", at(path), pointer)
		}
		if node, ok = m[segment]; !ok {
			return nil, fmt.Errorf("%s: $ref %q names nothing in this schema", at(path), pointer)
		}
	}
	return node, nil
}

// object is an object node: every object a provider generates holds the properties the schema
// names and no others, so additionalProperties is false and every property is required. A
// property left out of required is the defect this check exists for: the schema rides to the
// provider, the provider does not hold the model to it, and the reply is whatever the model
// writes.
func (s *strictSchema) object(m map[string]any, path string) error {
	additional, ok := m["additionalProperties"]
	switch {
	case !ok:
		return fmt.Errorf("%s: no additionalProperties: false", at(path))
	case additional != false:
		return fmt.Errorf("%s: additionalProperties is %v, want false", at(path), additional)
	}
	properties := map[string]any{}
	if raw, ok := m["properties"]; ok {
		if properties, ok = raw.(map[string]any); !ok {
			return fmt.Errorf("%s: properties is not an object", at(path))
		}
	}
	required := map[string]bool{}
	if raw, ok := m["required"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s: required is not an array", at(path))
		}
		for _, value := range list {
			name, ok := value.(string)
			if !ok {
				return fmt.Errorf("%s: required holds %v, which is no property name", at(path), value)
			}
			if _, ok := properties[name]; !ok {
				return fmt.Errorf("%s: required names %q, which is no property of this object", at(path), name)
			}
			required[name] = true
		}
	}
	names := slices.Sorted(maps.Keys(properties))
	for _, name := range names {
		if !required[name] {
			return fmt.Errorf("%s: optional property (not in required)", at(under(path, "properties", name)))
		}
	}
	for _, name := range names {
		if err := s.node(properties[name], under(path, "properties", name)); err != nil {
			return err
		}
	}
	return nil
}

// array is an array node: it says what its items are, since a provider constrains what it
// generates and can constrain no item it has no schema for. How many items there are is not a
// Final schema's to say: Anthropic enforces no array constraint but a minItems of 0 or 1, and
// the SDK takes minItems out of the schema before the call either way.
func (s *strictSchema) array(m map[string]any, path string) error {
	items, ok := m["items"]
	if !ok {
		return fmt.Errorf("%s: an array with no items schema", at(path))
	}
	return s.node(items, under(path, "items"))
}

// scalar is what a node says about the value itself: an enum of plain values, since neither
// provider takes an object or an array in one, and a format both providers know.
func (s *strictSchema) scalar(m map[string]any, path string) error {
	if raw, ok := m["enum"]; ok {
		values, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s: enum is not an array", at(path))
		}
		for _, value := range values {
			switch value.(type) {
			case nil, bool, float64, string:
			default:
				return fmt.Errorf("%s: enum holds %v, which is no plain value", at(path), value)
			}
		}
	}
	if raw, ok := m["format"]; ok {
		format, ok := raw.(string)
		if !ok || !slices.Contains(strictFormats, format) {
			return fmt.Errorf("%s: unsupported format %v", at(path), raw)
		}
	}
	return nil
}

// defs is the schema's own definitions walked, each under the pointer a $ref would name it by,
// so a definition the schema never uses is held to the same subset and a definition already
// being walked through a $ref is not walked twice.
func (s *strictSchema) defs(m map[string]any, path string) error {
	for _, keyword := range []string{"$defs", "definitions"} {
		raw, ok := m[keyword]
		if !ok {
			continue
		}
		defs, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %s is not an object", at(path), keyword)
		}
		for _, name := range slices.Sorted(maps.Keys(defs)) {
			escaped := strings.ReplaceAll(name, "~", "~0")
			escaped = strings.ReplaceAll(escaped, "/", "~1")
			pointer := "#/" + keyword + "/" + escaped
			if slices.Contains(s.refs, pointer) {
				continue
			}
			s.refs = append(s.refs, pointer)
			err := s.node(defs[name], under(path, keyword, name))
			s.refs = s.refs[:len(s.refs)-1]
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// under is the path of a node inside path, which is empty at the schema's root.
func under(path string, segments ...string) string {
	if path != "" {
		segments = append([]string{path}, segments...)
	}
	return strings.Join(segments, ".")
}

// at is a path as an error names it.
func at(path string) string {
	if path == "" {
		return "the root"
	}
	return path
}
