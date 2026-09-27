package pmdoc

import (
	"errors"
	"strings"
	"testing"
)

func TestParseMatchesMilkdownForFixtures(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			installCounterBlockIDs(t)
			got, err := Parse(fx.Markdown)
			if err != nil {
				t.Fatal(err)
			}
			want, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}
			if !got.EqualWithBlockIDs(want) {
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
			md, err := Render(doc)
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

func TestParsePreservesTaskListCheckboxState(t *testing.T) {
	doc, err := Parse("- [x] checked\n- [ ] unchecked\n")
	if err != nil {
		t.Fatalf("parse task list: %v", err)
	}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatalf("render task list: %v", err)
	}
	const want = "- [x] checked\n- [ ] unchecked\n"
	if markdown != want {
		t.Fatalf("task list round trip = %q, want %q", markdown, want)
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

// A document opening with `---` and no closing line is ordinary markdown, so its first line is
// a thematic break and the text after it a paragraph, as the browser editor's parser reads it.
// Goldmark's front-matter extension instead consumed an unclosed opener to the end of the input,
// which lost every block after it: an `insert` of "---\n\nTwo." wrote nothing and reported
// itself unchanged. (That parser, having tried the front matter to the end, then reads no list,
// quote or footnote definition in the rest; the renderer never writes such a document, since a
// rule opening one is written `***`.)
func TestParseUnclosedFrontmatterOpenerIsAThematicBreak(t *testing.T) {
	for _, test := range []struct {
		markdown string
		kinds    []string
	}{
		{markdown: "---", kinds: []string{"hr"}},
		{markdown: "---\n\nTwo.", kinds: []string{"hr", "paragraph"}},
		{markdown: "---\ntitle: x\n\nBody.\n", kinds: []string{"hr", "paragraph", "paragraph"}},
		{markdown: "---\ntext\n", kinds: []string{"hr", "paragraph"}},
		{markdown: "---\ntext\n\nMore.\n", kinds: []string{"hr", "paragraph", "paragraph"}},
	} {
		t.Run(test.markdown, func(t *testing.T) {
			tree, err := Parse(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			var kinds []string
			for _, child := range tree.Children {
				kinds = append(kinds, child.Type)
			}
			if strings.Join(kinds, ",") != strings.Join(test.kinds, ",") {
				t.Fatalf("Parse(%q) = %v, want %v", test.markdown, kinds, test.kinds)
			}
		})
	}
}

// The fix must not cost a closed front-matter block, which stays the document's own first node.
func TestParseKeepsClosedFrontmatter(t *testing.T) {
	const markdown = "---\ntitle: x\n---\n\nBody.\n"
	tree, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Children) != 2 || tree.Children[0].Type != "frontmatter" {
		t.Fatalf("Parse(%q) children = %#v", markdown, tree.Children)
	}
	back, err := Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if back != markdown {
		t.Fatalf("Render(Parse()) = %q, want %q", back, markdown)
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

func TestParseJoinsSoftLineBreaksWithSpaces(t *testing.T) {
	got, err := Parse("First soft line\ncontinues in the same paragraph.\n")
	if err != nil {
		t.Fatal(err)
	}
	want := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "text",
		Text: "First soft line continues in the same paragraph.",
	}}}}}
	if !got.Equal(want) {
		t.Fatalf("Parse(soft break) = %#v, want %#v", got, want)
	}
	md, err := Render(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.TrimSuffix(md, "\n"), "\n") {
		t.Fatalf("Render(soft break) re-wrapped the paragraph: %q", md)
	}
}

// Goldmark appends a footnote's backlink to its last paragraph, or, when the definition ends in
// another block, as a block after it. The backlink is the HTML renderer's decoration, not document
// content, so a definition ending in code, a list, a quote, a rule or a typed block reads as the
// blocks it holds and writes back the same.
func TestParseReadsAFootnoteDefinitionEndingInABlock(t *testing.T) {
	for _, test := range []struct{ name, markdown, last string }{
		{"code", "x[^1]\n\n[^1]: Note.\n\n    ```\n    code\n    ```\n", "code_block"},
		{"a list", "x[^1]\n\n[^1]: Note.\n\n    - item\n", "bullet_list"},
		{"a quote", "x[^1]\n\n[^1]: Note.\n\n    > quoted\n", "blockquote"},
		{"a rule", "x[^1]\n\n[^1]: Note.\n\n    ***\n", "hr"},
		{"a callout", "x[^1]\n\n[^1]: Note.\n\n    :::callout{#c1 kind=\"note\" title=\"T\"}\n    Body.\n    :::\n", "callout"},
		{"only a callout", "x[^1]\n\n[^1]: :::callout{#c1 kind=\"note\" title=\"T\"}\n    Body.\n    :::\n", "callout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := Parse(test.markdown)
			if err != nil {
				t.Fatalf("Parse(%q): %v", test.markdown, err)
			}
			definition := doc.Children[len(doc.Children)-1]
			if definition.Type != "footnote_definition" || definition.Children[len(definition.Children)-1].Type != test.last {
				json, _ := doc.JSON()
				t.Fatalf("Parse(%q) = %s, want a footnote definition ending in a %s", test.markdown, json, test.last)
			}
			markdown, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			back, err := Parse(markdown)
			if err != nil || !back.Equal(doc) {
				t.Fatalf("Render(Parse(%q)) = %q, which does not read back the same (%v)", test.markdown, markdown, err)
			}
		})
	}
}
