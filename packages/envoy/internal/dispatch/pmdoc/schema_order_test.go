package pmdoc

import (
	"slices"
	"testing"
)

// A typed block's attributes are written in the order its type declares them in
// schema/blocks.json, never sorted, so one order decides what the server writes, what the browser
// editor reads back and what a stored document holds. The shipped types' order is pinned by
// testdata/fixtures.json; this holds a type whose declared order no sort would produce, and reads
// the markdown back into the tree it came from.
func TestTypedAttributesRenderInDeclarationOrder(t *testing.T) {
	declared := BlockAttributes{
		{Name: "zeta", BlockAttributeSchema: BlockAttributeSchema{Kind: BlockAttributeString, Default: "z"}},
		{Name: "alpha", BlockAttributeSchema: BlockAttributeSchema{Kind: BlockAttributeString, Default: "a"}},
		{Name: "mid", BlockAttributeSchema: BlockAttributeSchema{
			Kind: BlockAttributeEnum, Choices: []string{"one", "two"}, Default: "one",
		}},
	}
	useTestBlockSchema(t, BlockSchema{Version: schema.Version, Types: append(slices.Clone(schema.Types),
		BlockTypeSchema{Name: "ordered", Content: BlockContentParagraphs, Render: "host", Attributes: declared},
	)})

	// The paragraph's id is not written in markdown, so the reader mints the one it holds here.
	SetBlockIDGenerator(func() string { return "b-2" })
	t.Cleanup(func() { SetBlockIDGenerator(nil) })

	doc := &Node{Type: "doc", Children: []*Node{{
		Type:  "ordered",
		Attrs: Attrs{BlockIDAttr: "b-1", "zeta": "z", "alpha": "a", "mid": "two"},
		Children: []*Node{{
			Type:     "paragraph",
			Attrs:    Attrs{BlockIDAttr: "b-2"},
			Children: []*Node{{Type: "text", Text: "Body text."}},
		}},
	}}}
	rendered, err := Render(doc)
	if err != nil {
		t.Fatalf("render the ordered block: %v", err)
	}
	const want = ":::ordered{#b-1 zeta=\"z\" alpha=\"a\" mid=\"two\"}\nBody text.\n:::\n"
	if rendered != want {
		t.Fatalf("Render() = %q, want %q (the order the type declares)", rendered, want)
	}

	read, err := Parse(rendered)
	if err != nil {
		t.Fatalf("parse the rendered block: %v", err)
	}
	if !read.EqualWithBlockIDs(doc) {
		readJSON, _ := read.JSON()
		docJSON, _ := doc.JSON()
		t.Fatalf("round-tripped tree = %s, want %s", readJSON, docJSON)
	}
}
