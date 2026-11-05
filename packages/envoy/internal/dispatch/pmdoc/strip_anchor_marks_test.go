package pmdoc

import (
	"reflect"
	"testing"
)

// StripAnchorMarks is the rendering's own strip and merge (writtenTree), copied (cloneNode). The
// copy is what its callers own and change (docs.asUploaded, the paragraph a docs edit builds,
// spreadDifferences), so it must share nothing with the document it was given, nor with the tree a
// later rendering of that document reads, which shares the document's nodes (asWritten).

// stripSampleDoc holds every shape the copy and the merge must carry: anchor marks a strip
// removes (proofComment, dispatchAsk), runs the strip makes mergeable, a link mark with
// attributes, a U+0000 in text and in a link href, which the strip leaves as it is, the two line
// breaks a rendering writes and a strip does not - a hard break the editor kept from a paste
// (isInline) and a line feed in text outside code - and a paragraph the rendering writes as it is,
// which writtenTree gives back unchanged and only cloneNode copies.
func stripSampleDoc() *Node {
	text := func(value string, marks ...Mark) *Node { return &Node{Type: "text", Text: value, Marks: marks} }
	strong := Mark{Type: "strong"}
	link := Mark{Type: "link", Attrs: Attrs{"href": "https://example.com/a\x00b", "title": "t"}}
	comment, ask := writtenCommentMark("c1"), writtenAskMark("a1")
	return &Node{Type: "doc", Children: []*Node{
		{Type: "paragraph", Children: []*Node{text("kept as it is")}},
		{Type: "paragraph", Children: []*Node{
			text("an", strong, comment),
			text("chor", strong),
			text(" and a nul\x00 here", ask),
			text("link", link, comment),
			text("ed", link),
		}},
		{Type: "paragraph", Children: []*Node{
			text("before a soft break"),
			{Type: "hardbreak", Attrs: Attrs{"isInline": true}},
			text("after it, and a line feed\nin text"),
		}},
	}}
}

// The copy holds exactly the written marks, in their order, the merged runs' first attributes and
// children, nil in place of every empty slice and map (cloneNode, cloneAttrs, cloneMarks), and
// every line break as the document holds it: the strip copies what writtenTree gives with
// lineBreaks false, which rewrites none of them.
func TestStripAnchorMarksIsItsDocumentStripped(t *testing.T) {
	text := func(value string, marks ...Mark) *Node { return &Node{Type: "text", Text: value, Marks: marks} }
	comment := writtenCommentMark("c1")
	for name, test := range map[string]struct{ doc, want *Node }{
		"anchors, a merge, a link and nuls": {
			doc: stripSampleDoc(),
			want: &Node{Type: "doc", Children: []*Node{
				{Type: "paragraph", Children: []*Node{text("kept as it is")}},
				{Type: "paragraph", Children: []*Node{
					text("anchor", Mark{Type: "strong"}),
					text(" and a nul\x00 here"),
					text("linked", Mark{Type: "link", Attrs: Attrs{"href": "https://example.com/a\x00b", "title": "t"}}),
				}},
				{Type: "paragraph", Children: []*Node{
					text("before a soft break"),
					{Type: "hardbreak", Attrs: Attrs{"isInline": true}},
					text("after it, and a line feed\nin text"),
				}},
			}},
		},
		"empty attributes and marks, and a merged run's attributes": {
			doc: &Node{Type: "doc", Attrs: Attrs{}, Children: []*Node{
				{Type: "paragraph", Marks: []Mark{}, Children: []*Node{
					{Type: "text", Text: "a", Attrs: Attrs{"note": "first"}, Marks: []Mark{comment}},
					{Type: "text", Text: "b", Attrs: Attrs{"note": "second"}, Marks: []Mark{}},
				}},
			}},
			want: &Node{Type: "doc", Children: []*Node{
				{Type: "paragraph", Children: []*Node{
					{Type: "text", Text: "ab", Attrs: Attrs{"note": "first"}},
				}},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := StripAnchorMarks(test.doc); !reflect.DeepEqual(got, test.want) {
				gotJSON, _ := got.JSON()
				wantJSON, _ := test.want.JSON()
				t.Fatalf("StripAnchorMarks gave %#v (%s), want %#v (%s)", got, gotJSON, test.want, wantJSON)
			}
		})
	}
	if got := StripAnchorMarks(nil); got != nil {
		t.Fatalf("StripAnchorMarks(nil) = %#v, want nil", got)
	}
}

// Nothing the copy holds is the document's, or the rendering's: every node, attribute map and mark
// slice is its own, and writing all of them leaves the document rendering as it did.
func TestStripAnchorMarksSharesNothingWithItsDocument(t *testing.T) {
	doc := stripSampleDoc()
	before, err := Render(doc)
	if err != nil {
		t.Fatalf("Render of the sample document: %v", err)
	}
	stripped := StripAnchorMarks(doc)
	written := asWritten(doc)
	for _, tree := range []struct {
		name string
		tree *Node
	}{{"the document", doc}, {"the tree its rendering reads", written}} {
		if shared := sharedWith(stripped, tree.tree); shared != "" {
			t.Errorf("StripAnchorMarks' tree shares %s with %s", shared, tree.name)
		}
	}
	writeOverTree(stripped)
	after, err := Render(doc)
	if err != nil {
		t.Fatalf("Render of the sample document after writing over the copy: %v", err)
	}
	if after != before {
		t.Fatalf("writing over StripAnchorMarks' tree changed the rendering of its document to %q, want %q", after, before)
	}
}

// sharedWith names the first node, attribute map or mark slice of tree that other also holds, or
// "" when they share none.
func sharedWith(tree, other *Node) string {
	held := map[uintptr]bool{}
	walkAddresses(other, func(address uintptr, _ string) { held[address] = true })
	shared := ""
	walkAddresses(tree, func(address uintptr, name string) {
		if shared == "" && held[address] {
			shared = name
		}
	})
	return shared
}

// walkAddresses gives visit the address and the name of every node, non-empty attribute map,
// mark slice and children slice under node.
func walkAddresses(node *Node, visit func(address uintptr, name string)) {
	visit(reflect.ValueOf(node).Pointer(), "the node "+node.Type)
	if len(node.Attrs) > 0 {
		visit(reflect.ValueOf(node.Attrs).Pointer(), "the attributes of "+node.Type)
	}
	if cap(node.Marks) > 0 {
		visit(reflect.ValueOf(node.Marks).Pointer(), "the marks of "+node.Type)
	}
	for _, mark := range node.Marks {
		if len(mark.Attrs) > 0 {
			visit(reflect.ValueOf(mark.Attrs).Pointer(), "the attributes of a "+mark.Type+" mark")
		}
	}
	if cap(node.Children) > 0 {
		visit(reflect.ValueOf(node.Children).Pointer(), "the children of "+node.Type)
	}
	for _, child := range node.Children {
		walkAddresses(child, visit)
	}
}

// writeOverTree writes over every text, attribute and mark under node.
func writeOverTree(node *Node) {
	node.Text = "written over"
	for name := range node.Attrs {
		node.Attrs[name] = "written over"
	}
	for _, mark := range node.Marks {
		for name := range mark.Attrs {
			mark.Attrs[name] = "written over"
		}
	}
	node.Marks = append(node.Marks, Mark{Type: "emphasis"})
	for _, child := range node.Children {
		writeOverTree(child)
	}
}
