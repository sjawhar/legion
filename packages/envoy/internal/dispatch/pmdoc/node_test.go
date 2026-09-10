package pmdoc

import (
	"errors"
	"testing"
)

func TestValidateRejectsUnknownNodeType(t *testing.T) {
	n := &Node{Type: "doc", Children: []*Node{{Type: "callout"}}}
	if err := n.Validate(); err == nil || !errors.Is(err, ErrSchema) {
		t.Fatalf("Validate() = %v, want ErrSchema", err)
	}
}

func TestEqualIgnoresAttrKeyOrder(t *testing.T) {
	a := &Node{Type: "text", Text: "x", Marks: []Mark{{Type: "link", Attrs: Attrs{"href": "h", "title": nil}}}}
	b := &Node{Type: "text", Text: "x", Marks: []Mark{{Type: "link", Attrs: Attrs{"title": nil, "href": "h"}}}}
	if !a.Equal(b) {
		t.Fatal("Equal() = false for attr-order-only difference")
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
