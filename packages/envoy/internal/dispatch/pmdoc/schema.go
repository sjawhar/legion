package pmdoc

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

//go:embed schema/blocks.json
var blockSchemaJSON []byte

// The schema's shape - BlockSchema, BlockSchemaElement, BlockTypeSchema, BlockAttributeSchema,
// BlockElementVersions, BlockContentRule and the attribute kinds - is declared in
// packages/contracts/schemas/block-schema.schema.json and generated into schema_generated.go,
// which blocks.json above is read into (manifest.go, compileManifest). The manifest names the
// nodes and marks of the Proof schema beside the typed blocks Dispatch adds to it, and every
// element of it - a node, a mark, a typed block, a typed block's attribute, an enum attribute's
// choice - may say from which schema version a build reads it (since) and from which a build may
// create it (creatable_since); both are 1 when it says neither. This build reads every element
// its manifest declares and creates those whose creatable_since is at most Version
// (SchemaVersion).

// LinkKinds is the edge kinds the manifest's `block[]` attributes make, sorted: each such
// attribute's name, once however many types declare it. None is a structural edge kind
// (model.GraphEdgeKinds).
func LinkKinds() []string {
	return slices.Clone(active.linkKinds)
}

// HasLinkKinds reports whether the manifest declares any `block[]` attribute (LinkKinds is not
// empty).
func HasLinkKinds() bool {
	return len(active.linkKinds) > 0
}

// SchemaJSON returns a copy of the canonical server-owned block schema JSON.
func SchemaJSON() []byte {
	return append([]byte(nil), blockSchemaJSON...)
}

// IsTypedBlock reports whether name is a typed block the schema declares.
func IsTypedBlock(name string) bool {
	_, ok := typedBlock(name)
	return ok
}

func typedBlock(name string) (BlockTypeSchema, bool) {
	typ, ok := active.types[name]
	return typ, ok
}

func typedBlockNames() []string {
	names := make([]string, 0, len(active.schema.Types))
	for _, typ := range active.schema.Types {
		names = append(names, typ.Name)
	}
	sort.Strings(names)
	return names
}

func validateBlockSchema(value BlockSchema) error {
	if value.Version <= 0 {
		return fmt.Errorf("version must be positive")
	}
	if len(value.Types) == 0 {
		return fmt.Errorf("must declare at least one type")
	}
	seenTypes := make(map[string]struct{}, len(value.Types))
	for _, typ := range value.Types {
		if !directiveName(typ.Name) {
			return fmt.Errorf("type %q is not a directive name", typ.Name)
		}
		if _, duplicate := seenTypes[typ.Name]; duplicate {
			return fmt.Errorf("type %q is declared twice", typ.Name)
		}
		seenTypes[typ.Name] = struct{}{}
		if !validBlockContentRule(typ.Content) {
			return fmt.Errorf("type %q has unsupported content %q", typ.Name, typ.Content)
		}
		if typ.Render != "host" {
			return fmt.Errorf("type %q has unsupported render %q", typ.Name, typ.Render)
		}
		seenAttributes := make(map[string]struct{}, len(typ.Attributes))
		for _, attr := range typ.Attributes {
			name := attr.Name
			if !directiveName(name) {
				return fmt.Errorf("type %q attribute %q is invalid", typ.Name, name)
			}
			if _, duplicate := seenAttributes[name]; duplicate {
				return fmt.Errorf("type %q attribute %q is declared twice", typ.Name, name)
			}
			seenAttributes[name] = struct{}{}
			// A typed block's markdown reads `id="…"` as its block id (TypedBlockIDAttribute), and
			// its tree holds that id as blockId, so neither can name an attribute.
			if name == "id" || name == BlockIDAttr {
				return fmt.Errorf("type %q attribute %q names the block id, which no attribute may", typ.Name, name)
			}
			if err := validateAttributeSchema(typ.Name, name, attr.BlockAttributeSchema); err != nil {
				return err
			}
		}
	}
	// A target names a type of the whole schema, so it is checked once every name is known.
	for _, typ := range value.Types {
		for _, attr := range typ.Attributes {
			for _, target := range attr.Targets {
				if _, typed := seenTypes[target]; !typed && !builtInBlockNode(value, target) {
					return fmt.Errorf("type %q attribute %q targets %q, which is neither a type the schema declares nor a built-in block the link graph holds", typ.Name, attr.Name, target)
				}
			}
		}
	}
	return nil
}

// builtInBlockNode reports whether name is a built-in block type of value, the manifest being
// validated, whose blocks are nodes of the link graph, which a `block[]` attribute's targets may
// name: a type the renderer records a block offset for (renderer.block: any node but the document
// and inline nodes), so one GET /api/v1/artifacts/{id}/blocks lists, less the types the renderer
// writes without recording one - a list item and a table's rows and cells - which no block list
// holds. It reads value's own nodes rather than the installed manifest's, since the manifest
// being validated is not installed yet (compileManifest).
func builtInBlockNode(value BlockSchema, name string) bool {
	switch name {
	case "doc", "list_item", "table_header_row", "table_row", "table_header", "table_cell":
		return false
	}
	if isInlineNodeType(name) {
		return false
	}
	return slices.ContainsFunc(value.Nodes, func(node BlockSchemaElement) bool { return node.Name == name })
}

func validBlockContentRule(rule BlockContentRule) bool {
	switch rule {
	case BlockContentParagraphs, BlockContentBlocks, BlockContentParagraphsOptionalBulletList:
		return true
	default:
		return false
	}
}

func normalizeSchemaDefaults(value *BlockSchema) error {
	for typeIndex := range value.Types {
		attributes := value.Types[typeIndex].Attributes
		for attrIndex := range attributes {
			attr := &attributes[attrIndex]
			if (attr.Kind != BlockAttributeStringList && attr.Kind != BlockAttributeBlockList) || attr.Default == nil {
				continue
			}
			items, ok := attr.Default.([]any)
			if !ok {
				continue
			}
			normalized := make([]string, len(items))
			for index, item := range items {
				text, ok := item.(string)
				if !ok {
					return fmt.Errorf("type %q attribute %q default contains %T", value.Types[typeIndex].Name, attr.Name, item)
				}
				normalized[index] = text
			}
			attr.Default = normalized
		}
	}
	return nil
}

// linkKindPattern is a `block[]` attribute's name, which is the kind of the edges it makes: what
// every structural edge kind matches, and nothing a `?kind=` comma list would split.
var linkKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func validateAttributeSchema(typeName, name string, attr BlockAttributeSchema) error {
	switch attr.Kind {
	case BlockAttributeString, BlockAttributeBool, BlockAttributeEnum, BlockAttributeStringList,
		BlockAttributeBlockList, BlockAttributeActor, BlockAttributeTimestamp:
	default:
		return fmt.Errorf("type %q attribute %q has unknown kind %q", typeName, name, attr.Kind)
	}
	if attr.Kind == BlockAttributeBlockList {
		if !linkKindPattern.MatchString(name) {
			return fmt.Errorf("type %q attribute %q is a block[] attribute, whose name is the kind of the links it makes and must match %s", typeName, name, linkKindPattern)
		}
		if slices.Contains(model.GraphEdgeKinds, name) {
			return fmt.Errorf("type %q attribute %q is a block[] attribute named after a structural edge kind (%s)", typeName, name, strings.Join(model.GraphEdgeKinds, ", "))
		}
	} else if attr.Targets != nil {
		return fmt.Errorf("type %q attribute %q has targets, which only a block[] attribute may carry", typeName, name)
	}
	if attr.Kind == BlockAttributeEnum {
		if len(attr.Choices) == 0 {
			return fmt.Errorf("type %q attribute %q enum has no choices", typeName, name)
		}
		seen := make(map[string]struct{}, len(attr.Choices))
		for _, choice := range attr.Choices {
			if choice == "" {
				return fmt.Errorf("type %q attribute %q enum has an empty choice", typeName, name)
			}
			if _, duplicate := seen[choice]; duplicate {
				return fmt.Errorf("type %q attribute %q enum repeats %q", typeName, name, choice)
			}
			seen[choice] = struct{}{}
		}
	}
	if attr.Default != nil {
		if err := validateAttributeValue(attr, attr.Default); err != nil {
			return fmt.Errorf("type %q attribute %q default: %w", typeName, name, err)
		}
		if items, _ := stringSlice(attr.Default); attr.Kind == BlockAttributeBlockList && len(items) > 0 {
			return fmt.Errorf("type %q attribute %q default must be absent or [], since a block[] default would point every new block at the same blocks", typeName, name)
		}
	}
	return nil
}

func validateAttributeValue(attr BlockAttributeSchema, value any) error {
	switch attr.Kind {
	case BlockAttributeString, BlockAttributeActor:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("must be a string, got %T", value)
		}
	case BlockAttributeTimestamp:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be an RFC 3339 timestamp string, got %T", value)
		}
		if _, err := time.Parse(time.RFC3339, text); err != nil {
			return fmt.Errorf("must be an RFC 3339 timestamp: %w", err)
		}
	case BlockAttributeBool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("must be a bool, got %T", value)
		}
	case BlockAttributeEnum:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be one of %s, got %T", strings.Join(attr.Choices, ", "), value)
		}
		for _, choice := range attr.Choices {
			if text == choice {
				return nil
			}
		}
		return unknownElement(fmt.Errorf("must be one of %s, got %q", strings.Join(attr.Choices, ", "), text))
	case BlockAttributeStringList:
		switch items := value.(type) {
		case []string:
			for _, item := range items {
				if item == "" {
					return fmt.Errorf("must not contain an empty string")
				}
			}
		case []any:
			for _, item := range items {
				text, ok := item.(string)
				if !ok || text == "" {
					return fmt.Errorf("must be a string array")
				}
			}
		default:
			return fmt.Errorf("must be a string array, got %T", value)
		}
	case BlockAttributeBlockList:
		items, ok := stringSlice(value)
		if !ok {
			return fmt.Errorf("must be an array of block references, got %T", value)
		}
		return validateBlockPointerValues(items)
	}
	return nil
}

// ParseBlockPointerValue reads a `block[]` attribute as markdown writes it: the empty string for
// no pointers, else canonical block references (text.CanonicalBlockRef) joined by exactly one
// space, with none leading or trailing and none repeated. Anything else is refused, never
// normalised, so the server and the editor never disagree on a value. Its twin is
// parseBlockPointerValue in @legion/contracts, whose blockPointerValueProblem gives its refusal's
// reason in this function's wording, quoting a value as JSON.stringify does where this uses %q;
// both are tested against `testdata/block-pointer-values.json`.
func ParseBlockPointerValue(raw string) ([]string, error) {
	if raw == "" {
		return []string{}, nil
	}
	if strings.HasPrefix(raw, " ") || strings.HasSuffix(raw, " ") || strings.Contains(raw, "  ") {
		return nil, fmt.Errorf("%q must be block references separated by exactly one space, with none leading or trailing", raw)
	}
	values := strings.Split(raw, " ")
	if err := validateBlockPointerValues(values); err != nil {
		return nil, err
	}
	return values, nil
}

// validateBlockPointerValues refuses a list of block pointers holding a part of an ask or a block
// of a comment or a message (text.Ref.Item), which a pointer never names: a `block[]` attribute
// points at blocks of documents, whose types its targets limit, and an item's block has no type.
// It refuses one that is otherwise not a canonical block reference (text.CanonicalBlockRef), and
// one named twice. Its twin is blockPointersProblem in @legion/contracts, which follows its
// wording, quoting a value as JSON.stringify does.
func validateBlockPointerValues(values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if ref, ok := text.ParseRef(value, ""); ok && ref.Kind == "block" && ref.Item != "" {
			// The kind takes the article its noun does: an ask, a comment, a message.
			article := "a"
			if ref.Item == "ask" {
				article = "an"
			}
			return fmt.Errorf("%q is a block of %s %s: a block[] attribute points only at a block of a document", value, article, ref.Item)
		}
		if _, ok := text.CanonicalBlockRef(value); !ok {
			return fmt.Errorf("%q is not a block reference as Dispatch writes one: dispatch://<ISSUE>/spec#b-<id>, or dispatch://<ISSUE or PROJECT>/artifact/<slug>#b-<id>, the id percent-encoded in upper-case hex except ASCII letters, digits and -, optionally pinned @v<N>", value)
		}
		if _, repeated := seen[value]; repeated {
			return fmt.Errorf("names %q twice", value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func parseTypedAttributeValue(attr BlockAttributeSchema, raw any) (any, error) {
	text, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("must be encoded as a string, got %T", raw)
	}
	switch attr.Kind {
	case BlockAttributeString, BlockAttributeEnum, BlockAttributeActor, BlockAttributeTimestamp:
		if err := validateAttributeValue(attr, text); err != nil {
			return nil, err
		}
		return text, nil
	case BlockAttributeBool:
		value, err := strconv.ParseBool(text)
		if err != nil {
			return nil, fmt.Errorf("must be true or false")
		}
		return value, nil
	case BlockAttributeStringList:
		var value []string
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			return nil, fmt.Errorf("must be a JSON string array: %w", err)
		}
		if err := validateAttributeValue(attr, value); err != nil {
			return nil, err
		}
		return value, nil
	case BlockAttributeBlockList:
		return ParseBlockPointerValue(text)
	default:
		return nil, fmt.Errorf("has unknown kind %q", attr.Kind)
	}
}

// defaultAttributes is the attributes a block of typ holds before it names any of its own: each
// attribute's written default (writtenDefault).
func defaultAttributes(typ BlockTypeSchema) Attrs {
	attrs := make(Attrs, len(typ.Attributes))
	for _, attr := range typ.Attributes {
		if value, ok := writtenDefault(typ, attr); ok {
			attrs[attr.Name] = value
		}
	}
	return attrs
}

// writtenDefault is the value of typ's attribute attr, for a block that does not hold it: its
// default, and false when it has none or when this build may not create the attribute yet. A
// block an earlier build wrote, before the attribute existed, reads with that default (readElement,
// renderTypedAttributes), as the browser editor reads it. An attribute this build cannot create is
// left out, so that no write of this build adds it.
func writtenDefault(typ BlockTypeSchema, attr BlockAttribute) (any, bool) {
	if attr.Default == nil || !creatable(Element{Kind: AttributeElement, Block: typ.Name, Name: attr.Name}) {
		return nil, false
	}
	return cloneSchemaValue(attr.Default), true
}

// withWrittenDefaults is attrs, a typed block's attributes as a stored tree holds them, with the
// written default of each attribute of typ that it does not hold. It adds to attrs in place.
func withWrittenDefaults(typ BlockTypeSchema, attrs Attrs) Attrs {
	for _, attr := range typ.Attributes {
		if _, present := attrs[attr.Name]; present {
			continue
		}
		if value, ok := writtenDefault(typ, attr); ok {
			if attrs == nil {
				attrs = make(Attrs, len(typ.Attributes))
			}
			attrs[attr.Name] = value
		}
	}
	return attrs
}

// TypedBlockAttributes is each attribute node carries, in the order its type declares them
// (schema/blocks.json), which is the order its opening line writes them: the values a reader of
// that line reads back, with no escape and no quotes. A block of no type the schema declares
// carries none, and so does one whose attributes the renderer would refuse.
func TypedBlockAttributes(node *Node) []model.BlockAttributeValue {
	typ, ok := typedBlock(node.Type)
	if !ok {
		return nil
	}
	values, err := typedAttributeValues(node, typ)
	if err != nil {
		return nil
	}
	return values
}

// typedAttributeValues is a typed block's declared attributes, their written values and the kind
// its type declares each under, in declaration order, leaving out one the block does not hold and
// one the markdown never carries.
func typedAttributeValues(n *Node, typ BlockTypeSchema) ([]model.BlockAttributeValue, error) {
	values := make([]model.BlockAttributeValue, 0, len(typ.Attributes))
	for _, attribute := range typ.Attributes {
		name, definition := attribute.Name, attribute.BlockAttributeSchema
		if name == "invalid" && definition.Server {
			continue
		}
		value, present := n.Attrs[name]
		if !present {
			value, present = writtenDefault(typ, attribute)
		}
		if !present || (value == nil && definition.Default == nil) {
			continue
		}
		encoded, err := encodeTypedAttribute(n.Type, name, definition, value)
		if err != nil {
			return nil, err
		}
		values = append(values, model.BlockAttributeValue{
			Name: name, Value: encoded, Kind: string(definition.Kind),
		})
	}
	if len(values) == 0 {
		return nil, nil
	}
	return values, nil
}

// renderTypedAttributes writes a typed block's directive attributes in the order its type
// declares them (schema/blocks.json), which is the order every reader of the markdown sees. It
// answers the values it wrote beside the line, so the one caller that wants them both - a
// version write, which lists every typed block's attributes beside the markdown it renders -
// encodes each value once rather than walking the tree again for them.
func renderTypedAttributes(n *Node, typ BlockTypeSchema) (string, []model.BlockAttributeValue, error) {
	id, ok := n.Attrs[BlockIDAttr].(string)
	if !ok || id == "" {
		return "", nil, fmt.Errorf("%w: typed block %q is missing blockId", ErrSchema, n.Type)
	}
	values, err := typedAttributeValues(n, typ)
	if err != nil {
		return "", nil, err
	}
	parts := make([]string, 0, len(values)+1)
	parts = append(parts, TypedBlockIDAttribute(id))
	for _, value := range values {
		parts = append(parts, value.Name+"="+strconv.Quote(escapeDirectiveAttribute(value.Value)))
	}
	return strings.Join(parts, " "), values, nil
}

// encodeTypedAttribute is the text one attribute value is written as, before escaping and
// quoting; it refuses a value whose kind is not the one the type declares.
func encodeTypedAttribute(blockType, name string, definition BlockAttributeSchema, value any) (string, error) {
	switch definition.Kind {
	case BlockAttributeString, BlockAttributeEnum, BlockAttributeActor, BlockAttributeTimestamp:
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("%w: typed block %q attribute %q must be a string", ErrSchema, blockType, name)
		}
		return text, nil
	case BlockAttributeBool:
		flag, ok := value.(bool)
		if !ok {
			return "", fmt.Errorf("%w: typed block %q attribute %q must be a bool", ErrSchema, blockType, name)
		}
		return strconv.FormatBool(flag), nil
	case BlockAttributeStringList:
		items, ok := stringSlice(value)
		if !ok {
			return "", fmt.Errorf("%w: typed block %q attribute %q must be a string array", ErrSchema, blockType, name)
		}
		jsonValue, err := json.Marshal(items)
		if err != nil {
			return "", fmt.Errorf("%w: encode typed block %q attribute %q: %v", ErrSchema, blockType, name, err)
		}
		return string(jsonValue), nil
	case BlockAttributeBlockList:
		// Joined by one space, as ParseBlockPointerValue reads them. The joined value still
		// passes through escapeDirectiveAttribute and strconv.Quote in the renderer, which leave
		// a canonical reference unchanged: it holds none of the characters they escape.
		items, ok := stringSlice(value)
		if !ok {
			return "", fmt.Errorf("%w: typed block %q attribute %q must be an array of block references", ErrSchema, blockType, name)
		}
		return strings.Join(items, " "), nil
	default:
		return "", fmt.Errorf("%w: typed block %q attribute %q has unknown kind %q", ErrSchema, blockType, name, definition.Kind)
	}
}

func escapeDirectiveAttribute(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "\"", "&#x22;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	return strings.ReplaceAll(value, ">", "&gt;")
}

func stringSlice(value any) ([]string, bool) {
	switch items := value.(type) {
	case []string:
		return items, true
	case []any:
		out := make([]string, len(items))
		for index, item := range items {
			var ok bool
			out[index], ok = item.(string)
			if !ok {
				return nil, false
			}
		}
		return out, true
	default:
		return nil, false
	}
}

func cloneSchemaValue(value any) any {
	switch current := value.(type) {
	case []string:
		return append([]string(nil), current...)
	default:
		return current
	}
}
