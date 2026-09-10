package pmdoc

import (
	"errors"
	"testing"
)

func TestParseMatchesMilkdownForFixtures(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			got, err := Parse(fx.Markdown)
			if err != nil {
				t.Fatal(err)
			}
			want, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(want) {
				g, _ := got.JSON()
				t.Fatalf("Parse() differs from Milkdown\n got: %s\nwant: %s", g, fx.PMJSON)
			}
		})
	}
}

func TestRenderParseRoundTrip(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			doc, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}
			md, _, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			back, err := Parse(md)
			if err != nil {
				t.Fatalf("re-parse canonical markdown: %v\n%s", err, md)
			}
			if !StripAnchorMarks(doc).Equal(back) {
				g, _ := back.JSON()
				w, _ := StripAnchorMarks(doc).JSON()
				t.Fatalf("round trip differs\n got: %s\nwant: %s\nmd:\n%s", g, w, md)
			}
		})
	}
}

func TestParseRejectsBlockHTML(t *testing.T) {
	_, err := Parse("<div>\nblock HTML\n</div>\n")
	if err == nil || !errors.Is(err, ErrSchema) {
		t.Fatalf("Parse(block HTML) = %v, want ErrSchema", err)
	}
}

func TestParsePreservesInlineHTMLAtom(t *testing.T) {
	got, err := Parse("Before <span class=\"note\">inside</span> after.\n")
	if err != nil {
		t.Fatal(err)
	}
	want := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
		{Type: "text", Text: "Before "},
		{Type: "html", Attrs: Attrs{"value": "<span class=\"note\">"}},
		{Type: "text", Text: "inside"},
		{Type: "html", Attrs: Attrs{"value": "</span>"}},
		{Type: "text", Text: " after."},
	}}}}
	if !got.Equal(want) {
		t.Fatalf("Parse(inline HTML) = %#v, want %#v", got, want)
	}
}

func TestParsePreservesLiteralEscapesInsideCodeSpan(t *testing.T) {
	got, err := Parse("`\\*literal\\* &amp;`\n")
	if err != nil {
		t.Fatal(err)
	}
	want := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type:  "text",
		Text:  "\\*literal\\* &amp;",
		Marks: []Mark{{Type: "inlineCode"}},
	}}}}}
	if !got.Equal(want) {
		t.Fatalf("Parse(code span) = %#v, want %#v", got, want)
	}
}
