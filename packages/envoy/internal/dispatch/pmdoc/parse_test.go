package pmdoc

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
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

// The browser editor's parser reads a line of `=` or `-` continuing the paragraph of a footnote
// definition inside another as a setext underline, where CommonMark reads it as the paragraph's
// text, and resolves a reference to a definition inside a typed block only from inside a typed
// block or after it, so a definition inside either is refused.
func TestParseRefusesAFootnoteDefinitionInsideAnotherOrATypedBlock(t *testing.T) {
	for _, test := range []struct{ markdown, reason string }{
		{"Ref[^n].\n\n[^n]: a\n\n    [^1]: x\n", "a footnote definition inside another"},
		{"Ref[^n].\n\n[^n]: a\n\n    [^1]: x\n    =\n", "a footnote definition inside another"},
		{"Ref[^n].\n\n[^n]: > a\n    >\n    > [^1]: x\n", "a footnote definition inside another"},
		{"Ref[^1].\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n[^1]: x\n:::\n", "a footnote definition inside a typed block"},
		{":::callout{#c1 kind=\"note\" title=\"T\"}\n> [^1]: x\n:::\n\nRef[^1].\n", "a footnote definition inside a typed block"},
	} {
		_, err := Parse(test.markdown)
		if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), test.reason) {
			t.Errorf("Parse(%q) error = %v, want a schema refusal naming %s", test.markdown, err, test.reason)
		}
	}
}

// A linked image is read as the browser editor stores it: the image without the link, which the
// editor's store keeps on text alone (LEGION-365). A refusal would leave a document the editor
// holds unsavable, so the link's text around the image keeps its link and the write is taken.
func TestParseReadsALinkedImageAsTheEditorStoresIt(t *testing.T) {
	for _, test := range []struct{ linked, stored string }{
		{"[![x](i.png)](https://u.com)\n", "![x](i.png)\n"},
		{"see [a ![x](i.png) b](https://u.com \"t\") now\n", "see [a ](https://u.com \"t\")![x](i.png)[ b](https://u.com \"t\") now\n"},
		{"- **[![x](i.png)](https://u.com)**\n", "- ![x](i.png)\n"},
	} {
		got, err := ParseForWrite(test.linked, nil)
		if err != nil {
			t.Errorf("ParseForWrite(%q) = %v, want the write taken", test.linked, err)
			continue
		}
		want, err := Parse(test.stored)
		if err != nil {
			t.Fatalf("Parse(%q): %v", test.stored, err)
		}
		if !got.Equal(want) {
			markdown, _ := Render(got)
			t.Errorf("ParseForWrite(%q) reads as %q, want what %q reads as", test.linked, markdown, test.stored)
		}
	}
}

// The browser editor's parser matches a reference to its definition by the labels as written,
// before their character references are decoded, and a label is stored and written decoded, so a
// reference whose label matches its definition's only as written would lose it: `[^&AUML;]` finds
// `[^&auml;]: `, but written `[^\&AUML;]` beside `[^ä]: ` it is text. One whose decoded label still
// matches the definition's reads.
func TestParseRefusesAReferenceMatchingItsDefinitionOnlyAsWritten(t *testing.T) {
	for _, markdown := range []string{
		"x[^&auml;] and [^&AUML;]\n\n[^&auml;]: d\n",
		"x[^&auml;]\n\n[^&AUML;]: d\n",
	} {
		if _, err := Parse(markdown); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "only as written") {
			t.Errorf("Parse(%q) error = %v, want a schema refusal of a reference matching only as written", markdown, err)
		}
	}
	readable := "x[^&auml;] and [^&Auml;]\n\n[^&auml;]: d\n"
	if _, err := Parse(readable); err != nil {
		t.Errorf("Parse(%q): %v", readable, err)
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

// A reference goldmark's exact match leaves unresolved is looked up among the definitions by its
// label's key once, not compared with every definition: with 2,000 definitions and 2,000 such
// references a document parses in about the time it takes with the definitions written as plain
// paragraphs, where comparing each pair took over a hundred times as long. The two timings are
// taken in the same run, so a slow machine slows both.
func TestParseResolvesUnmatchedReferencesWithoutComparingEveryDefinition(t *testing.T) {
	document := func(definitions bool) string {
		var markdown strings.Builder
		for range 2000 {
			markdown.WriteString("Unmatched [^QQ].\n\n")
		}
		for index := range 2000 {
			if definitions {
				fmt.Fprintf(&markdown, "[^d%d]: x\n\n", index)
			} else {
				fmt.Fprintf(&markdown, "d%d: x\n\n", index)
			}
		}
		return markdown.String()
	}
	fastest := func(markdown string) time.Duration {
		best := time.Duration(1<<63 - 1)
		for range 3 {
			start := time.Now()
			if _, err := Parse(markdown); err != nil {
				t.Fatal(err)
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	plain, withDefinitions := fastest(document(false)), fastest(document(true))
	if withDefinitions > 10*plain {
		t.Fatalf("parsing took %v with 2,000 definitions and %v with them as paragraphs, want under ten times as long", withDefinitions, plain)
	}
}

// A panic in this package's reader or renderer is recovered at the entry point, so it never
// crashes the caller, and is this package's bug: an ErrPanic, which callers answer as an internal
// error, never an ErrSchema refusal of the caller's markdown.
func TestAPanicWhileReadingIsAnInternalErrorNotARefusal(t *testing.T) {
	read := func() (doc *Node, err error) {
		defer recoverPanic(&doc, &err, "reading markdown")
		panic("can not call with inline nodes.")
	}
	doc, err := read()
	if doc != nil || !errors.Is(err, ErrPanic) || errors.Is(err, ErrSchema) || !strings.HasPrefix(err.Error(), "panic: ") {
		t.Fatalf("read() = %v, %v; want nil and an ErrPanic that is not an ErrSchema, reading \"panic: ...\"", doc, err)
	}
}

// A run that can both open and close pairs by the lengths it has left once a pair used part of
// it, as the browser editor's parser pairs it: after the strong pair, `***Note:****&#32;see
// below*` leaves the closer's other `**` as text, and the opener's last `*` pairs with the final
// one. Goldmark judged the lengths the runs were written with and paired that `*` with the
// closer, reading " see below" as emphasis alone.
func TestParsePairsARunByTheLengthsItHasLeft(t *testing.T) {
	nodes, err := ParseInline("***Note:****&#32;see below*")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, node := range nodes {
		var marks []string
		for _, mark := range node.Marks {
			marks = append(marks, mark.Type)
		}
		got = append(got, fmt.Sprintf("%q %v", node.Text, marks))
	}
	want := []string{`"Note:" [emphasis strong]`, `"** see below" [emphasis]`}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("ParseInline = %v, want %v", got, want)
	}
	for _, line := range got {
		if line == `" see below" [emphasis]` {
			t.Errorf("ParseInline still reads %s, goldmark's pairing of the closer's last delimiter", line)
		}
	}
}
