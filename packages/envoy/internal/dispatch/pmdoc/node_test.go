package pmdoc

import (
	"errors"
	"testing"

	"github.com/reearth/ygo/crdt"
)

func TestValidateRejectsUnknownNodeType(t *testing.T) {
	n := &Node{Type: "doc", Children: []*Node{{Type: "callout"}}}
	if err := n.Validate(); err == nil || !errors.Is(err, ErrSchema) {
		t.Fatalf("Validate() = %v, want ErrSchema", err)
	}
}

// Two builds serve one database during a release, so a stored tree can hold an element a newer
// build wrote: a node, mark or typed block this build's manifest does not name, an attribute a
// typed block does not declare, or a value its enum attribute does not list. Its refusal is
// ErrSchema and ErrUnknownElement both. A known element in a broken shape is ErrSchema alone, so a
// reader can tell the two apart.
func TestValidateTellsAnUnknownElementFromABrokenShape(t *testing.T) {
	paragraph := func() *Node {
		return &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: "x"}}}
	}
	callout := func(attrs Attrs, children ...*Node) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "callout", Attrs: attrs, Children: children}}}
	}
	for _, test := range []struct {
		name    string
		tree    *Node
		unknown bool
	}{
		{"an unknown node", &Node{Type: "doc", Children: []*Node{{Type: "future_block", Children: []*Node{paragraph()}}}}, true},
		{"an unknown mark", &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
			{Type: "text", Text: "x", Marks: []Mark{{Type: "futureMark"}}},
		}}}}, true},
		{"an attribute the typed block does not declare", callout(Attrs{"kind": "note", "title": "", "icon": "star"}, paragraph()), true},
		{"a value the enum does not list", callout(Attrs{"kind": "danger", "title": ""}, paragraph()), true},
		{"an unknown node inside a known block", &Node{Type: "doc", Children: []*Node{{Type: "blockquote", Children: []*Node{
			{Type: "future_block", Children: []*Node{paragraph()}},
		}}}}, true},
		{"a typed block with no content", callout(Attrs{"kind": "note", "title": ""}), false},
		{"an enum attribute that is not a string", callout(Attrs{"kind": float64(1), "title": ""}, paragraph()), false},
		{"a paragraph holding a block", &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{paragraph()}}}}, false},
		{"a mark on a block", &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Marks: []Mark{{Type: "strong"}}}}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.tree.Validate()
			if !errors.Is(err, ErrSchema) || errors.Is(err, ErrUnknownElement) != test.unknown {
				t.Fatalf("Validate() = %v, want ErrSchema, and ErrUnknownElement %t", err, test.unknown)
			}
		})
	}
}

// The reader of a stored Yjs tree classifies an element whose type name this build does not know
// as ErrUnknownElement, as Validate does. It does so also where a known element in a broken shape
// comes first, since the repair of that shape replaces the whole document and would delete the
// unknown element. A broken shape alone stays ErrSchema alone.
func TestReadClassifiesAnUnknownYjsElement(t *testing.T) {
	for _, test := range []struct {
		name     string
		elements []string
		unknown  bool
	}{
		{"an unknown element", []string{"future_block"}, true},
		{"an unknown element after a broken known one", []string{"callout", "future_block"}, true},
		{"a broken known element alone", []string{"callout"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := crdt.New()
			fragment := doc.GetXmlFragment("prosemirror")
			doc.Transact(func(txn *crdt.Transaction) {
				for index, name := range test.elements {
					fragment.InsertElement(txn, index, crdt.NewYXmlElement(name))
				}
			})
			if _, err := Read(fragment); !errors.Is(err, ErrSchema) || errors.Is(err, ErrUnknownElement) != test.unknown {
				t.Fatalf("Read() = %v, want ErrSchema, and ErrUnknownElement %t", err, test.unknown)
			}
		})
	}
}

// Stored markdown can hold an element a newer build wrote too: a stored version's markdown is that
// build's rendering. Its read-back classifies a typed block this build's manifest does not name, an
// attribute a typed block does not declare, and a value its enum attribute does not list as
// ErrUnknownElement, as the reader of a stored tree does. A known typed block in a broken shape
// stays ErrSchema alone.
func TestParseRenderingClassifiesAnUnknownTypedBlockElement(t *testing.T) {
	for _, test := range []struct {
		name     string
		markdown string
		unknown  bool
	}{
		{"an unknown typed block", ":::future_block{#b1}\nBody.\n:::\n", true},
		{"an attribute the typed block does not declare", ":::callout{#b2 kind=\"note\" title=\"T\" icon=\"star\"}\nBody.\n:::\n", true},
		{"a value the enum does not list", ":::callout{#b3 kind=\"danger\" title=\"T\"}\nBody.\n:::\n", true},
		{"an unknown typed block inside a known one", "::::callout{#b4 kind=\"note\" title=\"T\"}\n:::future_block{#b5}\nBody.\n:::\n::::\n", true},
		{"a bool attribute that is not true or false", ":::ask{#b6 urgency=\"med\" multiple=\"maybe\"}\nWhich one?\n:::\n", false},
		{"an unclosed typed block", ":::callout{#b7 kind=\"note\" title=\"T\"}\nBody.\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseRendering(test.markdown); !errors.Is(err, ErrSchema) || errors.Is(err, ErrUnknownElement) != test.unknown {
				t.Fatalf("ParseRendering() = %v, want ErrSchema, and ErrUnknownElement %t", err, test.unknown)
			}
		})
	}
}

func TestEqualIgnoresAttrKeyOrder(t *testing.T) {
	a := &Node{Type: "text", Text: "x", Marks: []Mark{{Type: "link", Attrs: Attrs{"href": "h", "title": nil}}}}
	b := &Node{Type: "text", Text: "x", Marks: []Mark{{Type: "link", Attrs: Attrs{"title": nil, "href": "h"}}}}
	if !a.Equal(b) {
		t.Fatal("Equal() = false for attr-order-only difference")
	}
}

// A run carrying two comments may list them in either order (Y attribute map iteration, the
// engine's mark set): sorting gives one order, so the two compare equal.
func TestSortMarksOrdersTwoMarksOfOneType(t *testing.T) {
	bob := Mark{Type: "proofComment", Attrs: Attrs{"id": "c1", "by": "user:bob"}}
	alice := Mark{Type: "proofComment", Attrs: Attrs{"id": "c2", "by": "user:alice"}}
	strong := Mark{Type: "strong"}
	left := []Mark{alice, strong, bob}
	right := []Mark{bob, alice, strong}
	sortMarks(left)
	sortMarks(right)
	if !marksEqual(left, right) {
		t.Fatalf("sorted %v and %v differ", left, right)
	}
	if left[0].Attrs["id"] != "c1" || left[1].Attrs["id"] != "c2" || left[2].Type != "strong" {
		t.Fatalf("sorted = %v, want by type, then id", left)
	}
}

func TestValidateRejectsInlineAtomAsDocumentChild(t *testing.T) {
	n := &Node{Type: "doc", Children: []*Node{{Type: "html", Attrs: Attrs{"value": "<i>"}}}}
	if err := n.Validate(); err == nil || !errors.Is(err, ErrSchema) {
		t.Fatalf("Validate() = %v, want ErrSchema", err)
	}
}

func TestValidateRequiresParagraphInTableCell(t *testing.T) {
	n := &Node{Type: "doc", Children: []*Node{{
		Type: "table",
		Children: []*Node{
			{Type: "table_header_row", Children: []*Node{{Type: "table_header", Children: []*Node{{Type: "text", Text: "header"}}}}},
			{Type: "table_row", Children: []*Node{{Type: "table_cell", Children: []*Node{{Type: "text", Text: "cell"}}}}},
		},
	}}}
	if err := n.Validate(); err == nil || !errors.Is(err, ErrSchema) {
		t.Fatalf("Validate() = %v, want ErrSchema", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	n := &Node{Type: "doc", Children: []*Node{{Type: "heading", Attrs: Attrs{"level": float64(2), "id": ""}, Children: []*Node{{Type: "text", Text: "Hi"}}}}}
	b, err := n.JSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if !n.Equal(back) {
		t.Fatalf("round trip differs: %s", b)
	}
}
