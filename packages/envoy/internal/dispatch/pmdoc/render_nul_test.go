package pmdoc

import (
	"strings"
	"testing"
)

// A U+0000 a browser edit leaves in a live document is written as U+FFFD, which is what CommonMark
// reads one as, wherever the rendering writes the character as it is: a node's text, a link's href
// and title, an image's source, alt and title, a code block's language, a typed block's id. The
// markdown a version stores then holds none, which PostgreSQL's text cannot store. A typed block's
// other attributes are written quoted, U+0000 as the escape \x00, and read back as U+0000.
func TestRenderWritesNoNul(t *testing.T) {
	text := func(value string, marks ...Mark) *Node { return &Node{Type: "text", Text: value, Marks: marks} }
	paragraph := func(children ...*Node) *Node { return &Node{Type: "paragraph", Children: children} }
	for name, block := range map[string]*Node{
		"text":        paragraph(text("a\x00b")),
		"heading":     {Type: "heading", Attrs: Attrs{"level": 2}, Children: []*Node{text("a\x00b")}},
		"inline code": paragraph(text("a\x00b", Mark{Type: "inlineCode"})),
		"code block":  {Type: "code_block", Attrs: Attrs{"language": "g\x00o"}, Children: []*Node{text("a\x00b")}},
		"link":        paragraph(text("label", Mark{Type: "link", Attrs: Attrs{"href": "https://example.com/a\x00b", "title": "t\x00t"}})),
		"image":       paragraph(&Node{Type: "image", Attrs: Attrs{"src": "https://example.com/a\x00b", "alt": "a\x00b", "title": "t\x00t"}}),
		"typed id":    {Type: "callout", Attrs: Attrs{BlockIDAttr: "c\x00d", "kind": "note", "title": ""}, Children: []*Node{paragraph(text("inside"))}},
	} {
		t.Run(name, func(t *testing.T) {
			markdown := mustRender(t, &Node{Type: "doc", Children: []*Node{block}})
			if strings.IndexByte(markdown, 0) >= 0 || !strings.Contains(markdown, "\uFFFD") {
				t.Fatalf("Render() = %q, want each U+0000 written as U+FFFD", markdown)
			}
		})
	}

	callout := &Node{Type: "doc", Children: []*Node{{
		Type:     "callout",
		Attrs:    Attrs{BlockIDAttr: "c1", "kind": "note", "title": "t\x00t"},
		Children: []*Node{paragraph(text("inside"))},
	}}}
	markdown := mustRender(t, callout)
	if strings.IndexByte(markdown, 0) >= 0 || !strings.Contains(markdown, `title="t\x00t"`) {
		t.Fatalf("Render() = %q, want the callout's title written as the escape \\x00", markdown)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if title := back.Children[0].Attrs["title"]; title != "t\x00t" {
		t.Fatalf("%q reads back with title %q, want %q", markdown, title, "t\x00t")
	}
}
