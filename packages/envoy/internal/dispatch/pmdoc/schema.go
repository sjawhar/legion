package pmdoc

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed schema/blocks.json
var blockSchemaJSON []byte

// BlockSchema is the document schema manifest, schema/blocks.json, which this build embeds and GET
// /api/v1/schema/blocks serves byte for byte: the nodes and marks of the Proof schema and the typed
// blocks Dispatch adds to it. Every element of it - a node, a mark, a typed block, a typed block's
// attribute, an enum attribute's choice - may say from which schema version a build reads it
// (since) and from which a build may create it (creatable_since); both are 1 when it says neither
// (ElementVersions). This build reads every element its manifest declares and creates those whose
// creatable_since is at most Version (SchemaVersion).
type BlockSchema struct {
	Version int               `json:"version"`
	Nodes   []ElementSchema   `json:"nodes"`
	Marks   []ElementSchema   `json:"marks"`
	Types   []BlockTypeSchema `json:"types"`
}

// ElementSchema is one node or mark of the Proof schema.
type ElementSchema struct {
	Name string `json:"name"`
	ElementVersions
}

// BlockContentRule describes the block children a typed node may contain.
type BlockContentRule string

const (
	BlockContentParagraphs                   BlockContentRule = "paragraph+"
	BlockContentBlocks                       BlockContentRule = "block+"
	BlockContentParagraphsOptionalBulletList BlockContentRule = "paragraph+ bullet_list?"
)

// BlockTypeSchema describes one typed ProseMirror block.
type BlockTypeSchema struct {
	Name       string                          `json:"name"`
	Content    BlockContentRule                `json:"content"`
	Render     string                          `json:"render"`
	Attributes map[string]BlockAttributeSchema `json:"attributes"`
	ElementVersions
}

// BlockAttributeSchema describes a typed block attribute. An enum's choices stay plain strings,
// since the editors of earlier builds compare a value against them; ChoiceVersions holds a choice's
// since and creatable_since, by choice, for each choice that has numbers other than 1.
type BlockAttributeSchema struct {
	Kind           string                     `json:"kind"`
	Choices        []string                   `json:"choices,omitempty"`
	Default        any                        `json:"default,omitempty"`
	Server         bool                       `json:"server,omitempty"`
	ChoiceVersions map[string]ElementVersions `json:"choice_versions,omitempty"`
	ElementVersions
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
	typ, ok := schemaTypes[name]
	return typ, ok
}

func typedBlockNames() []string {
	names := make([]string, 0, len(schema.Types))
	for _, typ := range schema.Types {
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
		for name, attr := range typ.Attributes {
			if !directiveName(name) {
				return fmt.Errorf("type %q attribute %q is invalid", typ.Name, name)
			}
			if err := validateAttributeSchema(typ.Name, name, attr); err != nil {
				return err
			}
		}
	}
	return nil
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
		for name, attr := range value.Types[typeIndex].Attributes {
			if attr.Kind != "string[]" || attr.Default == nil {
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
					return fmt.Errorf("type %q attribute %q default contains %T", value.Types[typeIndex].Name, name, item)
				}
				normalized[index] = text
			}
			attr.Default = normalized
			value.Types[typeIndex].Attributes[name] = attr
		}
	}
	return nil
}

func validateAttributeSchema(typeName, name string, attr BlockAttributeSchema) error {
	switch attr.Kind {
	case "string", "bool", "enum", "string[]", "actor", "timestamp":
	default:
		return fmt.Errorf("type %q attribute %q has unknown kind %q", typeName, name, attr.Kind)
	}
	if attr.Kind == "enum" {
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
	}
	return nil
}

func validateAttributeValue(attr BlockAttributeSchema, value any) error {
	switch attr.Kind {
	case "string", "actor":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("must be a string, got %T", value)
		}
	case "timestamp":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be an RFC 3339 timestamp string, got %T", value)
		}
		if _, err := time.Parse(time.RFC3339, text); err != nil {
			return fmt.Errorf("must be an RFC 3339 timestamp: %w", err)
		}
	case "bool":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("must be a bool, got %T", value)
		}
	case "enum":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be one of %s, got %T", strings.Join(attr.Choices, ", "), value)
		}
		for _, choice := range attr.Choices {
			if text == choice {
				return nil
			}
		}
		return fmt.Errorf("must be one of %s, got %q", strings.Join(attr.Choices, ", "), text)
	case "string[]":
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
	}
	return nil
}

func parseTypedAttributeValue(attr BlockAttributeSchema, raw any) (any, error) {
	text, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("must be encoded as a string, got %T", raw)
	}
	switch attr.Kind {
	case "string", "enum", "actor", "timestamp":
		if err := validateAttributeValue(attr, text); err != nil {
			return nil, err
		}
		return text, nil
	case "bool":
		value, err := strconv.ParseBool(text)
		if err != nil {
			return nil, fmt.Errorf("must be true or false")
		}
		return value, nil
	case "string[]":
		var value []string
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			return nil, fmt.Errorf("must be a JSON string array: %w", err)
		}
		if err := validateAttributeValue(attr, value); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return nil, fmt.Errorf("has unknown kind %q", attr.Kind)
	}
}

// defaultAttributes is the attributes a block of typ holds before it names any of its own: each
// attribute's written default (writtenDefault).
func defaultAttributes(typ BlockTypeSchema) Attrs {
	attrs := make(Attrs, len(typ.Attributes))
	for name, attr := range typ.Attributes {
		if value, ok := writtenDefault(typ, name, attr); ok {
			attrs[name] = value
		}
	}
	return attrs
}

// writtenDefault is the value of typ's attribute name, attr, for a block that does not hold it: its
// default, and false when it has none or when this build may not create the attribute yet. A
// block an earlier build wrote, before the attribute existed, reads with that default (readElement,
// renderTypedAttributes), as the browser editor reads it. An attribute this build cannot create is
// left out, so that no write of this build adds it.
func writtenDefault(typ BlockTypeSchema, name string, attr BlockAttributeSchema) (any, bool) {
	if attr.Default == nil || !creatable(Element{Kind: AttributeElement, Block: typ.Name, Name: name}) {
		return nil, false
	}
	return cloneSchemaValue(attr.Default), true
}

// withWrittenDefaults is attrs, a typed block's attributes as a stored tree holds them, with the
// written default of each attribute of typ that it does not hold. It adds to attrs in place.
func withWrittenDefaults(typ BlockTypeSchema, attrs Attrs) Attrs {
	for name, attr := range typ.Attributes {
		if _, present := attrs[name]; present {
			continue
		}
		if value, ok := writtenDefault(typ, name, attr); ok {
			if attrs == nil {
				attrs = make(Attrs, len(typ.Attributes))
			}
			attrs[name] = value
		}
	}
	return attrs
}

func renderTypedAttributes(n *Node, typ BlockTypeSchema) (string, error) {
	id, ok := n.Attrs[BlockIDAttr].(string)
	if !ok || id == "" {
		return "", fmt.Errorf("%w: typed block %q is missing blockId", ErrSchema, n.Type)
	}
	parts := []string{"#" + id}
	names := make([]string, 0, len(typ.Attributes))
	for name := range typ.Attributes {
		names = append(names, name)
	}
	sort.SliceStable(names, func(left, right int) bool {
		if n.Type != "ask" {
			return names[left] < names[right]
		}
		return askAttributeRank(names[left]) < askAttributeRank(names[right])
	})
	for _, name := range names {
		definition := typ.Attributes[name]
		if name == "invalid" && definition.Server {
			continue
		}
		value, present := n.Attrs[name]
		if !present {
			value, present = writtenDefault(typ, name, definition)
		}
		if !present || (value == nil && definition.Default == nil) {
			continue
		}
		var encoded string
		switch definition.Kind {
		case "string", "enum", "actor", "timestamp":
			text, ok := value.(string)
			if !ok {
				return "", fmt.Errorf("%w: typed block %q attribute %q must be a string", ErrSchema, n.Type, name)
			}
			encoded = text
		case "bool":
			flag, ok := value.(bool)
			if !ok {
				return "", fmt.Errorf("%w: typed block %q attribute %q must be a bool", ErrSchema, n.Type, name)
			}
			encoded = strconv.FormatBool(flag)
		case "string[]":
			items, ok := stringSlice(value)
			if !ok {
				return "", fmt.Errorf("%w: typed block %q attribute %q must be a string array", ErrSchema, n.Type, name)
			}
			jsonValue, err := json.Marshal(items)
			if err != nil {
				return "", fmt.Errorf("%w: encode typed block %q attribute %q: %v", ErrSchema, n.Type, name, err)
			}
			encoded = string(jsonValue)
		default:
			return "", fmt.Errorf("%w: typed block %q attribute %q has unknown kind %q", ErrSchema, n.Type, name, definition.Kind)
		}
		parts = append(parts, name+"="+strconv.Quote(escapeDirectiveAttribute(encoded)))
	}
	return strings.Join(parts, " "), nil
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

func askAttributeRank(name string) int {
	switch name {
	case "urgency":
		return 0
	case "multiple":
		return 1
	case "state":
		return 2
	case "answered_by":
		return 3
	case "answered_at":
		return 4
	case "selected":
		return 5
	case "answer":
		return 6
	case "copied_from":
		return 7
	case "copied_from_document":
		return 8
	default:
		return 9
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
