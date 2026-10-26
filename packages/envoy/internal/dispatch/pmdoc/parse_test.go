package pmdoc

import (
	"errors"
	"fmt"
	"runtime"
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

// A link around one whole image is kept as the image's link and link title, as the browser editor
// stores it (INFRA-190). A link around an image and text keeps its link on the text alone, since
// the editor's store keeps a mark on text only, and a mark around the linked image is lost; each
// write is taken, since a refusal would leave a document the editor holds unsavable.
func TestParseKeepsALinkedImagesLink(t *testing.T) {
	for _, test := range []struct{ linked, stored string }{
		{"[![x](i.png)](https://u.com)\n", "[![x](i.png)](https://u.com)\n"},
		{"[![x](i.png)](https://u.com \"t\")\n", "[![x](i.png)](https://u.com \"t\")\n"},
		{"[a ![x](i.png) b](https://u.com)\n", "[a ](https://u.com)![x](i.png)[ b](https://u.com)\n"},
		{"see [a ![x](i.png) b](https://u.com \"t\") now\n", "see [a ](https://u.com \"t\")![x](i.png)[ b](https://u.com \"t\") now\n"},
		{"- **[![x](i.png)](https://u.com)**\n", "- [![x](i.png)](https://u.com)\n"},
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
		markdown, err := Render(got)
		if err != nil {
			t.Fatalf("Render(ParseForWrite(%q)): %v", test.linked, err)
		}
		if !got.Equal(want) || markdown != test.stored {
			t.Errorf("ParseForWrite(%q) reads as %q, want %q", test.linked, markdown, test.stored)
		}
	}
}

// A linked image's link is its destination as CommonMark reads it, character references and
// backslash escapes decoded, as the browser editor stores it and as the image's own src is read:
// a link written through the documents API must open the address the live editor's view, a past
// version and a comment all draw for the same markdown, and must read back so (INFRA-190).
func TestParseDecodesALinkedImagesLinkAsTheEditorReadsIt(t *testing.T) {
	for _, test := range []struct{ markdown, link string }{
		{"[![x](i.png)](https://u.com/?a=1&amp;b=2)\n", "https://u.com/?a=1&b=2"},
		{"[![x](i.png)](https://u.com/a\\*b)\n", "https://u.com/a*b"},
		{"[![x](i.png)](https://u.com/\\)x)\n", "https://u.com/)x"},
	} {
		got, err := ParseForWrite(test.markdown, nil)
		if err != nil {
			t.Errorf("ParseForWrite(%q) = %v, want the write taken", test.markdown, err)
			continue
		}
		image := got.Children[0].Children[0]
		if link, _ := image.Attrs["link"].(string); image.Type != "image" || link != test.link {
			t.Errorf("ParseForWrite(%q) image link = %q, want %q", test.markdown, link, test.link)
			continue
		}
		markdown, err := Render(got)
		if err != nil {
			t.Fatalf("Render(ParseForWrite(%q)): %v", test.markdown, err)
		}
		again, err := Parse(markdown)
		if err != nil || !again.Equal(got) {
			t.Errorf("Render wrote %q, which does not read back as the stored tree (err %v)", markdown, err)
		}
	}
}

// A destination's escapes and character references are decoded in one pass, as the browser
// editor's parser decodes them: an escaped `&` opens no reference, a reference wants its `;`, and
// a number naming a control character is U+FFFD. An image's source is read the same way.
func TestParseDecodesALinkedImagesLinkInOnePassAsTheEditorReadsIt(t *testing.T) {
	for _, test := range []struct{ markdown, src, link string }{
		{"[![x](i.png)](https://u.com/?a=1\\&amp;b=2)\n", "i.png", "https://u.com/?a=1&amp;b=2"},
		{"[![x](i.png)](https://u.com/?a=1&copy=2&amp;b=3)\n", "i.png", "https://u.com/?a=1&copy=2&b=3"},
		{"[![x](i.png)](https://u.com/&ampx;&#1;&#X41;)\n", "i.png", "https://u.com/&ampx;\uFFFDA"},
		{"[![x](i\\&amp;&copy=.png)](u)\n", "i&amp;&copy=.png", "u"},
		{"| h |\n| - |\n| [![x](i.png)](https://u.com/a\\|b\\\\\\|c&amp;d) |\n", "i.png", "https://u.com/a|b\\|c&d"},
	} {
		got, err := Parse(test.markdown)
		if err != nil {
			t.Errorf("Parse(%q): %v", test.markdown, err)
			continue
		}
		var image *Node
		Walk(got, func(node *Node) bool {
			if node.Type == "image" {
				image = node
			}
			return image == nil
		})
		if image == nil || image.Attrs["src"] != test.src || image.Attrs["link"] != test.link {
			t.Errorf("Parse(%q) image = %v, want src %q and link %q", test.markdown, image, test.src, test.link)
		}
	}
}

// A link to a `javascript:` address spelled with a character reference, an escape or mixed case
// is stored as the address it names, so every view checks the scheme the browser would follow and
// draws no link to it (linked-image.ts in @legion/proof-editor).
func TestParseStoresAnEncodedJavascriptLinkAsTheAddressItNames(t *testing.T) {
	for _, test := range []struct{ markdown, link string }{
		{"[![x](i.png)](javascript&#58;window.__pwned=1)\n", "javascript:window.__pwned=1"},
		{"[![x](i.png)](javascript\\:window.__pwned=1)\n", "javascript:window.__pwned=1"},
		{"[![x](i.png)](JaVaScRiPt:window.__pwned=1)\n", "JaVaScRiPt:window.__pwned=1"},
		{"[![x](i.png)](&#x6A;avascript:window.__pwned=1)\n", "javascript:window.__pwned=1"},
	} {
		got, err := ParseForWrite(test.markdown, nil)
		if err != nil {
			t.Errorf("ParseForWrite(%q) = %v, want the write taken", test.markdown, err)
			continue
		}
		if image := got.Children[0].Children[0]; image.Type != "image" || image.Attrs["link"] != test.link {
			t.Errorf("ParseForWrite(%q) = %v, want an image linked to %q", test.markdown, image, test.link)
		}
	}
}

// An image whose source escapes the `&` of a character reference (`i\&amp;.png`) is a write the
// server took before INFRA-190, and the change adds no refusal (R2): the source is held as the
// editor reads it, `i&amp;.png`, so it must be written back so that it reads as that again, with
// the `&` escaped, for a plain image and for a linked one alike.
func TestParseTakesAnImageWhoseSourceEscapesAReferencesAmpersand(t *testing.T) {
	for _, test := range []struct{ markdown, src string }{
		{"![x](i\\&amp;.png)\n", "i&amp;.png"},
		{"[![x](i\\&amp;.png)](https://u.com)\n", "i&amp;.png"},
		{"![x](i.png?a=1\\&copy;)\n", "i.png?a=1&copy;"},
	} {
		got, err := ParseForWrite(test.markdown, nil)
		if err != nil {
			t.Errorf("ParseForWrite(%q) = %v, want the write taken, as before INFRA-190", test.markdown, err)
			continue
		}
		image := got.Children[0].Children[0]
		if src, _ := image.Attrs["src"].(string); image.Type != "image" || src != test.src {
			t.Errorf("ParseForWrite(%q) image src = %q, want %q", test.markdown, src, test.src)
		}
	}
}

// A linked image whose link title holds a line break the context cannot hold as written - a blank
// line, or any line break in a heading or a table cell - is a write the server took before
// INFRA-190 (it dropped the link), and the change adds no refusal (R2): the link title is held
// decoded, so it must be written back so that it reads as that again, and the link kept (R1).
func TestParseTakesALinkedImageWhoseLinkTitleHoldsALineBreak(t *testing.T) {
	for _, test := range []struct{ markdown, linkTitle string }{
		{"[![x](i.png)](https://u.com \"a&#10;&#10;b\")\n", "a\n\nb"},
		{"# [![x](i.png)](https://u.com \"a&#10;b\")\n", "a\nb"},
		{"| a |\n| - |\n| [![x](i.png)](https://u.com \"a&#10;b\") |\n", "a\nb"},
		{"Head [![x](i.png)](https://u.com \"a\nb\")\n===\n", "a\nb"},
	} {
		got, err := ParseForWrite(test.markdown, nil)
		if err != nil {
			t.Errorf("ParseForWrite(%q) = %v, want the write taken, as before INFRA-190", test.markdown, err)
			continue
		}
		var image *Node
		Walk(got, func(node *Node) bool {
			if node.Type == "image" {
				image = node
			}
			return image == nil
		})
		if image == nil || image.Attrs["link"] != "https://u.com" || image.Attrs["linkTitle"] != test.linkTitle {
			t.Errorf("ParseForWrite(%q) image = %v, want link %q and link title %q", test.markdown, image, "https://u.com", test.linkTitle)
		}
	}
}

// A title line that Go's reader (goldmark) opens an HTML block at, and the title-line allow-list
// admits as text, must not be written raw: `<meta` (a block tag in goldmark's list, not in
// CommonMark's or the allow-list's), `</ div>` (goldmark reads a space after `</` and still takes
// the block tag) and `<pre/>` or `<pre` and a form feed (goldmark's raw-element opening). Written
// raw, the stored markdown reads back otherwise in Go and the write is refused, where the base
// took a linked image's write (it dropped the link): R2. An image's own title holding such a line
// is refused at the base too; it is the same writer's defect, so it is taken here as well.
func TestParseTakesAnImageTitleLineGoReadsAsAnHTMLBlock(t *testing.T) {
	for _, prefix := range []string{"", "x | ", "- ", "> "} {
		for _, line := range []string{"<meta", `<META x=\"1\">`, "</ div>", "</ p", "<pre/>", "<script/>x", "<pre\fx"} {
			title := "a&#10;" + line + "&#10;z"
			for _, markdown := range []string{
				prefix + "[![x](i.png)](https://u.com \"" + title + "\")\n",
				prefix + "![x](i.png \"" + title + "\")\n",
			} {
				got, err := ParseForWrite(markdown, nil)
				if err != nil {
					t.Errorf("ParseForWrite(%q) = %v, want the write taken", markdown, err)
					continue
				}
				var image *Node
				Walk(got, func(node *Node) bool {
					if node.Type == "image" {
						image = node
					}
					return image == nil
				})
				if image == nil || strings.HasPrefix(markdown, prefix+"[") && image.Attrs["link"] != "https://u.com" {
					t.Errorf("ParseForWrite(%q) image = %v, want the image and its link kept", markdown, image)
				}
			}
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

// Reading an unclosed `---` as a rule must not cost a closed front-matter block, which stays the
// document's own first node.
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
// paragraphs, where comparing each pair would take over a hundred times as long. The two timings
// are taken in the same run, so a slow machine slows both.
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
// one. Goldmark judges the lengths the runs were written with and pairs that `*` with the
// closer, reading " see below" as emphasis alone.
func TestParsePairsARunByTheLengthsItHasLeft(t *testing.T) {
	nodes, err := ParseInline("***Note:****&#32;see below*", NewWriteBudget())
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

func TestParseRejectsExcessiveTablePaddingBeforeAllocation(t *testing.T) {
	const maximumAllocation = 8 << 20
	for _, width := range []int{500, 1000, 1500} {
		markdown := tablePaddingBomb(width)
		for _, parser := range []struct {
			name string
			read func(string) (*Node, error)
		}{
			{name: "Parse", read: Parse},
			{name: "ParseForWrite", read: func(markdown string) (*Node, error) {
				return ParseForWrite(markdown, nil)
			}},
		} {
			t.Run(fmt.Sprintf("%s/n=%d", parser.name, width), func(t *testing.T) {
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				started := time.Now()
				_, err := parser.read(markdown)
				elapsed := time.Since(started)
				runtime.ReadMemStats(&after)
				allocated := after.TotalAlloc - before.TotalAlloc
				t.Logf("n=%d elapsed=%s allocated=%d", width, elapsed, allocated)
				if !errors.Is(err, ErrSchema) {
					t.Errorf("parse error = %v, want ErrSchema", err)
				} else {
					for _, want := range []string{"table 1", "writes", "the table's width", "limit 10000"} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("parse error = %q, want it to name %q", err, want)
						}
					}
				}
				if allocated > maximumAllocation {
					t.Errorf("allocated %d bytes, want at most %d", allocated, maximumAllocation)
				}
			})
		}
	}
}

func TestParseRejectsTablePaddingBudgetAcrossTables(t *testing.T) {
	markdown := tablePaddingBomb(70) + "\n" + tablePaddingBomb(70) + "\n" + tablePaddingBomb(70)
	_, err := Parse(markdown)
	if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "table 3") {
		t.Fatalf("Parse() error = %v, want ErrSchema naming table 3", err)
	}
}

// A paragraph's text is its lines and the soft breaks between them, and each run under one set of
// marks is joined once: a 40,000-byte paragraph of 20,000 lines reads as one text node of its
// lines joined by spaces, at about a kibibyte a line, where joining each line to the text before
// it would copy that text again for every line.
func TestParseJoinsAParagraphsLinesOnce(t *testing.T) {
	const lines = 20_000
	markdown := strings.Repeat("x\n", lines)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	doc, err := Parse(markdown)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("allocated=%d", allocated)
	if err != nil {
		t.Fatal(err)
	}
	paragraph := doc.Children[0]
	if want := strings.Repeat("x ", lines-1) + "x"; len(paragraph.Children) != 1 || paragraph.Children[0].Text != want {
		t.Errorf("paragraph children = %d, first text %d bytes, want one text node of %d bytes", len(paragraph.Children), len(paragraph.Children[0].Text), len(want))
	}
	if allocated > 64<<20 {
		t.Errorf("allocated %d bytes, want at most %d", allocated, 64<<20)
	}
}

func tablePaddingBomb(width int) string {
	return strings.Repeat("| header ", width) + "|\n" +
		strings.Repeat("| --- ", width) + "|\n" +
		strings.Repeat("| body |\n", width)
}
