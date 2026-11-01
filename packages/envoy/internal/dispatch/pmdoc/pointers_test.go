package pmdoc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// blockPointerTestSchema is the shipped schema (its callout included) with two test types: a
// requirement, and a claim whose serves attribute is a block[] attribute pointing only at
// requirements, or attributes when given. Call it before useTestBlockSchema swaps the schema.
func blockPointerTestSchema(attributes map[string]BlockAttributeSchema) BlockSchema {
	if attributes == nil {
		attributes = map[string]BlockAttributeSchema{"serves": {Kind: "block[]", Targets: []string{"requirement"}}}
	}
	return BlockSchema{Version: schema.Version, Types: append(slices.Clone(schema.Types),
		BlockTypeSchema{Name: "requirement", Content: BlockContentParagraphs, Render: "host", Attributes: map[string]BlockAttributeSchema{}},
		BlockTypeSchema{Name: "claim", Content: BlockContentParagraphs, Render: "host", Attributes: attributes},
	)}
}

// useTestBlockSchema makes value the block schema for the rest of t, restoring the one before by
// a t.Cleanup (pmdoctest.UseBlockSchema, which this package's tests cannot import).
func useTestBlockSchema(t testing.TB, value BlockSchema) {
	t.Helper()
	restore, err := SetBlockSchemaForTest(value)
	if err != nil {
		t.Fatalf("use test block schema: %v", err)
	}
	t.Cleanup(restore)
}

// A block[] attribute's name is the kind of the links it makes, so it is one `?kind=` can name and
// never a structural edge kind; its targets are types whose blocks the link graph holds; only it
// carries targets; and it starts empty. No shipped type declares one.
func TestABlockPointerSchemaDeclaresANameAndTargets(t *testing.T) {
	if kinds := LinkKinds(); len(kinds) != 0 {
		t.Fatalf("the shipped schema's link kinds = %v, want none", kinds)
	}
	if err := validateBlockSchema(blockPointerTestSchema(nil)); err != nil {
		t.Fatalf("validate the test schema: %v", err)
	}
	pointer := func(targets ...string) BlockAttributeSchema {
		return BlockAttributeSchema{Kind: "block[]", Targets: targets}
	}
	for _, targets := range [][]string{nil, {}, {"requirement", "claim", "callout", "paragraph", "heading", "table", "blockquote", "bullet_list", "code_block"}} {
		if err := validateBlockSchema(blockPointerTestSchema(map[string]BlockAttributeSchema{"serves": pointer(targets...)})); err != nil {
			t.Errorf("targets %v: %v, want valid", targets, err)
		}
	}
	refused := map[string]map[string]BlockAttributeSchema{
		"a name that is not lower case":            {"Serves": pointer("requirement")},
		"a name that is a structural kind":         {"mentions": pointer("requirement")},
		"a name that is the blocks' own kind":      {"part_of": pointer("requirement")},
		"a target the schema does not declare":     {"serves": pointer("nope")},
		"a list item target":                       {"serves": pointer("list_item")},
		"a table cell target":                      {"serves": pointer("table_cell")},
		"targets on a string array":                {"serves": {Kind: "string[]", Targets: []string{"requirement"}}},
		"a default pointing at a block":            {"serves": {Kind: "block[]", Default: []string{"dispatch://CORE-1/spec#b-x"}}},
		"a default that is not a block reference":  {"serves": {Kind: "block[]", Default: []string{"x"}}},
		"a default that is not an array of string": {"serves": {Kind: "block[]", Default: "dispatch://CORE-1/spec#b-x"}},
	}
	for name, attributes := range refused {
		if err := validateBlockSchema(blockPointerTestSchema(attributes)); err == nil {
			t.Errorf("%s: the schema validates, want it refused", name)
		}
	}
	if err := validateBlockSchema(blockPointerTestSchema(map[string]BlockAttributeSchema{"serves": {Kind: "block[]", Default: []string{}}})); err != nil {
		t.Errorf("an empty default: %v, want valid", err)
	}

	twice := blockPointerTestSchema(nil)
	twice.Types = append(twice.Types, BlockTypeSchema{Name: "proof", Content: BlockContentParagraphs, Render: "host", Attributes: map[string]BlockAttributeSchema{
		"serves": pointer(), "note": {Kind: "string"},
	}})
	useTestBlockSchema(t, twice)
	if kinds := LinkKinds(); !slices.Equal(kinds, []string{"serves"}) {
		t.Fatalf("link kinds of two types declaring serves = %v, want [serves]", kinds)
	}
	var served BlockSchema
	if err := json.Unmarshal(SchemaJSON(), &served); err != nil || len(served.Types) != len(twice.Types) {
		t.Fatalf("the served schema = %d types (%v), want the swapped schema's %d", len(served.Types), err, len(twice.Types))
	}
}

// A block[] value is decided by the one table @legion/contracts holds, the same way whether it is
// read from markdown or checked in a tree, and a refused one with the reason its row's problem
// names, where it names one: `testdata/block-pointer-values.json` is generated from
// DISPATCH_BLOCK_POINTER_VALUES, which `packages/contracts/src/dispatch-block-ref.test.ts` holds
// this file to.
func TestABlockPointerAttributeDecidesTheSharedCorpus(t *testing.T) {
	raw, err := os.ReadFile("testdata/block-pointer-values.json")
	if err != nil {
		t.Fatalf("read table: %v", err)
	}
	var rows []struct {
		Value   string `json:"value"`
		OK      bool   `json:"ok"`
		Why     string `json:"why"`
		Problem string `json:"problem"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse table: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the shared block pointer table is empty")
	}
	attr := BlockAttributeSchema{Kind: "block[]", Targets: []string{"requirement"}}
	for _, row := range rows {
		parsed, err := ParseBlockPointerValue(row.Value)
		if (err == nil) != row.OK {
			t.Errorf("ParseBlockPointerValue(%q) (%s) = %v, want accepted %t", row.Value, row.Why, err, row.OK)
		}
		if err != nil {
			// The refusal names what is wrong: the whole value, or the reference in it that is.
			named := strings.Contains(err.Error(), strconv.Quote(row.Value))
			for _, part := range strings.Split(row.Value, " ") {
				named = named || part != "" && strings.Contains(err.Error(), strconv.Quote(part))
			}
			if !named {
				t.Errorf("ParseBlockPointerValue(%q) (%s) = %v, want the refusal to name the value", row.Value, row.Why, err)
			}
			if !strings.Contains(err.Error(), row.Problem) {
				t.Errorf("ParseBlockPointerValue(%q) (%s) = %v, want the refusal to say %q", row.Value, row.Why, err, row.Problem)
			}
		}
		if row.OK && strings.Join(parsed, " ") != row.Value {
			t.Errorf("ParseBlockPointerValue(%q) = %q, want the references as written", row.Value, parsed)
		}
		if _, err := parseTypedAttributeValue(attr, row.Value); (err == nil) != row.OK {
			t.Errorf("parseTypedAttributeValue(%q) (%s) = %v, want accepted %t", row.Value, row.Why, err, row.OK)
		}
		// A tree holds the value as the array Yjs gives back.
		items := []any{}
		if row.Value != "" {
			for _, item := range strings.Split(row.Value, " ") {
				items = append(items, item)
			}
		}
		if err := validateAttributeValue(attr, items); (err == nil) != row.OK {
			t.Errorf("validateAttributeValue(%q) (%s) = %v, want accepted %t", items, row.Why, err, row.OK)
		}
	}
}

// A block[] attribute's targets name built-in blocks the link graph holds: each type the block list
// (RenderWithBlockOffsets, which GET /api/v1/artifacts/{id}/blocks lists) holds for the corpus is
// one, and a list item or a table's row or cell, which it never holds, is not.
func TestBuiltInBlockTargetsAreTheBlocksTheBlockListHolds(t *testing.T) {
	paths, err := filepath.Glob("testdata/corpus/*.md")
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d documents)", err, len(paths))
	}
	listed := map[string]bool{}
	for _, path := range paths {
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		tree, err := Parse(LineFeeds(string(markdown)))
		if err != nil {
			t.Fatalf("%s: Parse: %v", path, err)
		}
		_, offsets, err := RenderWithBlockOffsets(tree)
		if err != nil {
			t.Fatalf("%s: render: %v", path, err)
		}
		for _, offset := range offsets {
			if !IsTypedBlock(offset.Type) {
				listed[offset.Type] = true
			}
		}
	}
	for _, want := range []string{"paragraph", "heading", "table", "bullet_list", "code_block"} {
		if !listed[want] {
			t.Fatalf("the corpus's block lists hold no %s: %v", want, listed)
		}
	}
	for nodeType := range listed {
		if !builtInBlockNode(nodeType) {
			t.Errorf("the block list holds %s, which a block[] attribute cannot target", nodeType)
		}
	}
	for _, nodeType := range []string{"list_item", "table_header_row", "table_row", "table_cell", "table_header", "doc", "text", "nope"} {
		if builtInBlockNode(nodeType) {
			t.Errorf("a block[] attribute can target %s, which no block list holds", nodeType)
		}
	}
}

// A claim's pointers are written in its opening line as markdown reads them, refused by name when
// malformed, and kept through the browser's Yjs document.
func TestABlockPointerAttributeRoundTripsThroughMarkdownAndYjs(t *testing.T) {
	useTestBlockSchema(t, blockPointerTestSchema(nil))
	const (
		first  = "dispatch://CORE-1/spec#b-r1"
		second = "dispatch://CORE/artifact/handbook-md#b-d%C3%A9cision%3A1@v3"
	)
	for _, tc := range []struct {
		markdown string
		want     []string
	}{
		{":::claim{#c1 serves=\"" + first + " " + second + "\"}\nIt holds.\n:::\n", []string{first, second}},
		{":::claim{#c1 serves=\"\"}\nIt holds.\n:::\n", []string{}},
	} {
		tree, err := Parse(tc.markdown)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.markdown, err)
		}
		if got := tree.Children[0].Attrs["serves"]; !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("parsed serves = %#v, want %#v", got, tc.want)
		}
		if rendered, err := Render(tree); err != nil || rendered != tc.markdown {
			t.Fatalf("render = %q, %v; want %q", rendered, err, tc.markdown)
		}
		doc := crdt.New()
		fragment := doc.GetXmlFragment("prosemirror")
		if err := doc.TransactE(func(txn *crdt.Transaction) error {
			return Update(txn, fragment, tree)
		}); err != nil {
			t.Fatalf("write claim to Yjs: %v", err)
		}
		reloaded := crdt.New()
		if err := crdt.ApplyUpdateV1(reloaded, crdt.EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
			t.Fatalf("reload claim update: %v", err)
		}
		read, err := Read(reloaded.GetXmlFragment("prosemirror"))
		if err != nil {
			t.Fatalf("read claim from Yjs: %v", err)
		}
		if err := read.Validate(); err != nil {
			t.Fatalf("the claim read from Yjs is outside the schema: %v", err)
		}
		if rendered, err := Render(read); err != nil || rendered != tc.markdown {
			t.Fatalf("Yjs claim round-trip = %q, %v; want %q", rendered, err, tc.markdown)
		}
	}
	const malformed = "dispatch://CORE-1/artifact/spec#b-r1"
	_, err := Parse(":::claim{#c1 serves=\"" + malformed + "\"}\nIt holds.\n:::\n")
	if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), `"serves"`) || !strings.Contains(err.Error(), malformed) {
		t.Fatalf("parse a malformed pointer = %v, want a schema refusal naming serves and %s", err, malformed)
	}
}

// A pointer a write introduces to a block of a type its attribute does not allow is refused,
// naming what is wrong; one to an allowed or an absent block is accepted, and one the tree before
// the write already held is never refused, whatever its target has become.
func TestCheckBlockPointersRefusesATargetOfADisallowedType(t *testing.T) {
	value := blockPointerTestSchema(nil)
	// A draft holds the same attribute name as text, which a retype into a claim makes a pointer;
	// a loose block holds it as a pointer to any block, which a retype into a claim narrows.
	value.Types = append(value.Types,
		BlockTypeSchema{Name: "draft", Content: BlockContentParagraphs, Render: "host", Attributes: map[string]BlockAttributeSchema{
			"serves": {Kind: "string"},
		}},
		BlockTypeSchema{Name: "loose", Content: BlockContentParagraphs, Render: "host", Attributes: map[string]BlockAttributeSchema{
			"serves": {Kind: "block[]"},
		}},
	)
	anyBlock := blockPointerTestSchema(map[string]BlockAttributeSchema{"serves": {Kind: "block[]"}})
	useTestBlockSchema(t, value)
	const (
		requirement = "dispatch://CORE-1/spec#b-r1"
		paragraph   = "dispatch://CORE-1/spec#b-p1"
		callout     = "dispatch://CORE-1/spec#b-k1"
		absent      = "dispatch://CORE-1/spec#b-gone"
	)
	types := map[string][]string{"CORE-1/spec#r1": {"requirement"}, "CORE-1/spec#p1": {"paragraph"}, "CORE-1/spec#k1": {"callout"}}
	// resolve answers the types of the blocks it is asked for, by node id, and counts its calls:
	// a check looks its targets up at most once.
	var calls int
	resolve := func(targets []text.Ref) (map[string][]string, error) {
		calls++
		found := make(map[string][]string)
		for _, target := range targets {
			if held, ok := types[text.TargetID(target)]; ok {
				found[text.TargetID(target)] = held
			}
		}
		return found, nil
	}
	parse := func(markdown string) *Node {
		t.Helper()
		tree, err := Parse(markdown)
		if err != nil {
			t.Fatalf("parse %q: %v", markdown, err)
		}
		return tree
	}
	claim := func(ref string) *Node {
		return parse(":::claim{#c1 serves=\"" + ref + "\"}\nIt holds.\n:::\n")
	}
	for _, test := range []struct {
		name       string
		tree       *Node
		prior      *Node
		refusedFor string // the target type the refusal names; empty when accepted
	}{
		{name: "a requirement", tree: claim(requirement)},
		{name: "a paragraph", tree: claim(paragraph), refusedFor: "paragraph"},
		{name: "a callout", tree: claim(callout), refusedFor: "callout"},
		{name: "a block no document holds", tree: claim(absent)},
		{name: "a callout the tree before already pointed at", tree: claim(callout), prior: claim(callout)},
		{name: "a callout replacing a requirement", tree: claim(callout), prior: claim(requirement), refusedFor: "callout"},
		{name: "a callout after a pointer of another block", tree: parse(":::claim{#c2 serves=\"" + callout + "\"}\nIt holds.\n:::\n"), prior: claim(callout), refusedFor: "callout"},
		{name: "a callout pointed at by a block retyped from text", tree: claim(callout), prior: parse(":::draft{#c1 serves=\"" + callout + "\"}\nIt holds.\n:::\n"), refusedFor: "callout"},
		{name: "a callout pointed at by a block retyped from a pointer to any block", tree: claim(callout), prior: parse(":::loose{#c1 serves=\"" + callout + "\"}\nIt holds.\n:::\n"), refusedFor: "callout"},
		{name: "a callout second after a requirement", tree: claim(requirement + " " + callout), prior: claim(requirement), refusedFor: "callout"},
		{name: "a callout before a paragraph, both new", tree: claim(callout + " " + paragraph), refusedFor: "callout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls = 0
			err := CheckBlockPointers(test.tree, test.prior, resolve)
			if calls > 1 {
				t.Fatalf("CheckBlockPointers looked its targets up %d times, want at most once", calls)
			}
			if test.refusedFor == "" {
				if err != nil {
					t.Fatalf("CheckBlockPointers = %v, want accepted", err)
				}
				return
			}
			if !errors.Is(err, ErrBlockPointer) || errors.Is(err, ErrSchema) {
				t.Fatalf("CheckBlockPointers = %v, want ErrBlockPointer and no schema refusal", err)
			}
			ref := map[string]string{"paragraph": paragraph, "callout": callout}[test.refusedFor]
			for _, want := range []string{`"serves"`, ref, `"` + test.refusedFor + `"`, "requirement"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("CheckBlockPointers = %v, want it to name %s", err, want)
				}
			}
		})
	}

	// Two blocks share a node id only across a store's rows; any of their types outside the
	// targets refuses.
	shared := func(targets []text.Ref) (map[string][]string, error) {
		return map[string][]string{text.TargetID(targets[0]): {"requirement", "callout"}}, nil
	}
	if err := CheckBlockPointers(claim(requirement), nil, shared); !errors.Is(err, ErrBlockPointer) {
		t.Fatalf("a target holding a requirement and a callout = %v, want refused", err)
	}
	// A pointer with no targets points at any block, so nothing is looked up for it.
	useTestBlockSchema(t, anyBlock)
	if err := CheckBlockPointers(claim(callout), nil, func([]text.Ref) (map[string][]string, error) {
		return nil, errors.New("looked up a pointer with no targets")
	}); err != nil {
		t.Fatalf("a pointer with no targets = %v, want accepted", err)
	}
}
