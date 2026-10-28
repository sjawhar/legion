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
