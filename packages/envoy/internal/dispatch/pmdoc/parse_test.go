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

// An image's alt text keeps the characters of the raw HTML in its label, as the browser editor's
// reader keeps them: `mdast-util-to-string`, which that reader builds an image's `alt` with,
// writes a literal node's own value, and an mdast `html` node is a literal node. parseImage
// writes each `html` node's value into the alt beside the label's text and its nested images, so
// every kind of raw HTML a label can hold reaches the alt. An anchor span is one of them: inside
// a label parseInlineMarks reads a `<span data-proof=…>` as the raw HTML it is rather than as
// the mark it is elsewhere, since the browser editor's reader builds the alt as the label closes,
// before it turns such a span into a mark.
//
// Measured with gen/differential.ts against the browser editor's engine: `![p <!a> q](i.png)` reads
// with alt `p <!a> q`, `![r <b> s](i.png)` with alt `r <b> s`, the same for a comment
// (`<!-- c -->`), a processing instruction and a CDATA section, and an anchored label with the
// span's own characters. An alt holding less would have a comment drawing the markdown and the
// document editor drawing the stored document show different alt text, the divergence this
// package exists to close.
func TestParseKeepsRawHTMLInAnImageAltText(t *testing.T) {
	for _, row := range []struct{ markdown, alt string }{
		// A declaration at a lowercase letter: inline raw HTML to both readers
		// (inlineRawHTMLParser), so its characters are the alt's.
		{"![p <!a> q](i.png)\n", "p <!a> q"},
		{"![<!a>](i.png)\n", "<!a>"},
		{"![p <!a> q](i.png \"t\")\n", "p <!a> q"},
		// Every other kind of raw HTML in a label, which both readers read as raw HTML.
		{"![r <b> s](i.png)\n", "r <b> s"},
		{"![r <!A> s](i.png)\n", "r <!A> s"},
		{"![r <!-- c --> s](i.png)\n", "r <!-- c --> s"},
		{"![r <?x?> s](i.png)\n", "r <?x?> s"},
		{"![r <![CDATA[x]]> s](i.png)\n", "r <![CDATA[x]]> s"},
		// An anchor span, which parseInlineMarks reads as a mark everywhere but in a label. The
		// mark would reach nothing here, an attribute holding none, and the engine's alt carries
		// the span's characters.
		{"![a <span data-proof=\"comment\" data-id=\"c1\" data-by=\"user:x\">b</span> c](i.png)\n", "a <span data-proof=\"comment\" data-id=\"c1\" data-by=\"user:x\">b</span> c"},
	} {
		t.Run(row.markdown, func(t *testing.T) {
			doc, err := Parse(row.markdown)
			if err != nil {
				t.Fatalf("parse %q: %v", row.markdown, err)
			}
			var images []*Node
			Walk(doc, func(node *Node) bool {
				if node.Type == "image" {
					images = append(images, node)
				}
				return true
			})
			if len(images) != 1 {
				t.Fatalf("parse %q: %d images, want 1", row.markdown, len(images))
			}
			if alt, _ := images[0].Attrs["alt"].(string); alt != row.alt {
				t.Fatalf("alt of %q = %q, want %q", row.markdown, alt, row.alt)
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

// The browser editor's parser reads a line of `=` or `-` continuing the paragraph of a footnote
// definition inside another as a setext underline, where CommonMark reads it as the paragraph's
// text, so a definition inside another is refused.
func TestParseRefusesAFootnoteDefinitionInsideAnother(t *testing.T) {
	for _, test := range []struct{ markdown, reason string }{
		{"Ref[^n].\n\n[^n]: a\n\n    [^1]: x\n", "a footnote definition inside another"},
		{"Ref[^n].\n\n[^n]: a\n\n    [^1]: x\n    =\n", "a footnote definition inside another"},
		{"Ref[^n].\n\n[^n]: > a\n    >\n    > [^1]: x\n", "a footnote definition inside another"},
	} {
		_, err := Parse(test.markdown)
		if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), test.reason) {
			t.Errorf("Parse(%q) error = %v, want a schema refusal naming %s", test.markdown, err, test.reason)
		}
	}
}

// Since the browser editor reads a `:::` block as a container like a quote (INFRA-581), a footnote
// definition inside a typed block defines its footnote for a call anywhere in the text: before the
// block, inside it and after it, as a definition inside a quote does. A document write reads such
// a definition as the browser editor does and keeps it where it was written; refusing it, or
// giving as its reason that the editor refers to it only from inside the block or after it, would
// tell the writer something the editor does not do.
func TestParseReadsAFootnoteDefinitionInsideATypedBlockAsTheBrowserDoes(t *testing.T) {
	for _, markdown := range []string{
		"Ref[^1].\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n[^1]: x\n:::\n",
		":::callout{#c1 kind=\"note\" title=\"T\"}\n> [^1]: x\n:::\n\nRef[^1].\n",
	} {
		doc, err := Parse(markdown)
		if err != nil {
			t.Errorf("Parse(%q) = %v, want it read as the browser editor reads it", markdown, err)
			continue
		}
		if firstNodeOfType(doc, "footnote_reference") == nil {
			t.Errorf("Parse(%q) read no footnote reference; the browser editor reads Ref[^1] as one", markdown)
		}
		if callout := firstNodeOfType(doc, "callout"); callout == nil || firstNodeOfType(callout, "footnote_definition") == nil {
			t.Errorf("Parse(%q) holds no footnote definition inside its callout", markdown)
		}
		if rendered, err := Render(doc); err != nil || rendered != markdown {
			t.Errorf("Render(Parse(%q)) = %q, %v, want the markdown as written", markdown, rendered, err)
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

// A list whose first item could not interrupt a paragraph - an empty item, a bare marker, an
// ordered marker that is not a lone `1` - opens after indented code only where the code is no
// longer the open construct, as the browser editor's parser reads it (interruptsOpenBlock). That
// parser keeps the code open while the line opening the item opens the container it stands in, so
// `    code\n> -` is a quote holding the paragraph `-`, and a blank line between them opens
// nothing and leaves it open; but a line carrying the container's markers opened that container a
// line earlier, and in its flow nothing is open by the item's line, so `    code\n>\n> -` is a
// quote holding a list with one empty item. Where the code is the container's own last block
// instead, lines of markers and whitespace inside it leave the code open, so `>     code\n>  \n> -`
// is a quoted paragraph. Before this, every line of markers and whitespace left the code open, so
// the five rows below whose quote opened a line earlier read as that quote's paragraph.
func TestParseOpensAnItemAfterIndentedCodeWhereTheContainerOpenedEarlier(t *testing.T) {
	text := func(value string) *Node { return &Node{Type: "text", Text: value} }
	paragraph := func(children ...*Node) *Node { return &Node{Type: "paragraph", Children: children} }
	code := func(value string) *Node {
		return &Node{Type: "code_block", Children: []*Node{text(value)}}
	}
	quote := func(children ...*Node) *Node { return &Node{Type: "blockquote", Children: children} }
	emptyItem := &Node{Type: "list_item", Attrs: Attrs{"label": "•", "listType": "bullet", "spread": false}, Children: []*Node{paragraph()}}
	bullets := func(items ...*Node) *Node {
		return &Node{Type: "bullet_list", Attrs: Attrs{"spread": false}, Children: items}
	}
	ordered := func(order int, items ...*Node) *Node {
		return &Node{Type: "ordered_list", Attrs: Attrs{"order": order, "spread": false}, Children: items}
	}
	item := func(children ...*Node) *Node {
		return &Node{Type: "list_item", Attrs: Attrs{"label": "•", "listType": "bullet", "spread": false}, Children: children}
	}
	for _, test := range []struct {
		name     string
		markdown string
		want     *Node
	}{
		{
			name:     "a quote opening on the item's own line holds the marker as a paragraph",
			markdown: "    code\n> -\n",
			want:     &Node{Type: "doc", Children: []*Node{code("code"), quote(paragraph(text("-")))}},
		},
		{
			name:     "a blank line between the code and that quote opens nothing",
			markdown: "    code\n\n> -\n",
			want:     &Node{Type: "doc", Children: []*Node{code("code"), quote(paragraph(text("-")))}},
		},
		{
			name:     "an ordered marker on the quote's own opening line is that paragraph too",
			markdown: "    code\n> 2. b\n",
			want:     &Node{Type: "doc", Children: []*Node{code("code"), quote(paragraph(text("2. b")))}},
		},
		{
			name:     "a quote opened a line earlier holds a list with one empty item",
			markdown: "    code\n>\n> -\n",
			want:     &Node{Type: "doc", Children: []*Node{code("code"), quote(bullets(emptyItem))}},
		},
		{
			name:     "the quote's earlier line holding spaces opens it just as well",
			markdown: "    code\n>  \n> -\n",
			want:     &Node{Type: "doc", Children: []*Node{code("code"), quote(bullets(emptyItem))}},
		},
		{
			name:     "spaces after the marker leave the item empty",
			markdown: "    code\n>       \n> -    \n",
			want:     &Node{Type: "doc", Children: []*Node{code("code"), quote(bullets(emptyItem))}},
		},
		{
			name:     "a paragraph before the code changes none of it",
			markdown: "a\n\n    code\n>   \n> -\n",
			want:     &Node{Type: "doc", Children: []*Node{paragraph(text("a")), code("code"), quote(bullets(emptyItem))}},
		},
		{
			name:     "an ordered marker in a quote opened a line earlier opens its list",
			markdown: "    code\n>\n> 2. b\n",
			want:     &Node{Type: "doc", Children: []*Node{code("code"), quote(ordered(2, item(paragraph(text("b")))))}},
		},
		{
			name:     "code the quote itself holds stays open across the quote's whitespace line",
			markdown: ">     code\n>  \n> -\n",
			want:     &Node{Type: "doc", Children: []*Node{quote(code("code"), paragraph(text("-")))}},
		},
		{
			name:     "a quote with no code before it holds the list",
			markdown: ">   \n> -\n",
			want:     &Node{Type: "doc", Children: []*Node{quote(bullets(emptyItem))}},
		},
		{
			name:     "code, a rule and code again, then a quote opened a line earlier",
			markdown: "\t ```js\n---\n         \n  \n    >    \n>       \n> -    \n",
			want: &Node{Type: "doc", Children: []*Node{
				code(" ```js"), {Type: "hr"}, code(">    "), quote(bullets(emptyItem)),
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseForWrite(test.markdown, nil)
			if err != nil {
				t.Fatalf("ParseForWrite(%q): %v", test.markdown, err)
			}
			if !got.Equal(test.want) {
				g, _ := got.JSON()
				w, _ := test.want.JSON()
				t.Fatalf("ParseForWrite(%q)\n got: %s\nwant: %s", test.markdown, g, w)
			}
		})
	}
}

// The browser editor's parser takes a fenced code block's own indentation off every line it
// holds, and takes a line holding fewer columns of whitespace than that whole, so such a line is
// an empty line of code. Goldmark fell back there to the line's first non-space position, which a
// line of whitespace has none of, less the padding left of a tab a container's marker took part
// of, so it started the line that many bytes before its text, inside the markers: `>    ```` over
// `>` and a tab read `>\t` as the code.
func TestParseReadsAWhitespaceLineUnderAWiderFenceAsEmptyCode(t *testing.T) {
	code := func(value string) *Node {
		node := &Node{Type: "code_block", Attrs: Attrs{"language": nil}}
		if value != "" {
			node.Children = []*Node{{Type: "text", Text: value}}
		}
		return node
	}
	quote := func(children ...*Node) *Node { return &Node{Type: "blockquote", Children: children} }
	for _, test := range []struct {
		name, markdown string
		want           *Node
	}{
		{
			name:     "a quote's tab line under a fence indented four columns",
			markdown: ">    ```\n>\t\n",
			want:     &Node{Type: "doc", Children: []*Node{quote(code(""))}},
		},
		{
			name:     "the same with code and a closing fence after it",
			markdown: ">    ```\n>\t\n>    x\n>    ```\n",
			want:     &Node{Type: "doc", Children: []*Node{quote(code("\nx"))}},
		},
		{
			name:     "a tilde fence over the same line",
			markdown: ">    ~~~\n>\t\n>    ~~~\n",
			want:     &Node{Type: "doc", Children: []*Node{quote(code(""))}},
		},
		{
			name:     "spaces narrower than the quote's fence",
			markdown: ">    ```\n>  \n",
			want:     &Node{Type: "doc", Children: []*Node{quote(code(""))}},
		},
		{
			name:     "whitespace reaching the fence keeps what stands past it",
			markdown: ">    ```\n>     \n",
			want:     &Node{Type: "doc", Children: []*Node{quote(code(" "))}},
		},
		{
			name:     "a document-level fence over a narrower line",
			markdown: "   ```\n  \n```\n",
			want:     &Node{Type: "doc", Children: []*Node{code("")}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseForWrite(test.markdown, nil)
			if err != nil {
				t.Fatalf("ParseForWrite(%q): %v", test.markdown, err)
			}
			if !got.Equal(test.want) {
				g, _ := got.JSON()
				w, _ := test.want.JSON()
				t.Fatalf("ParseForWrite(%q)\n got: %s\nwant: %s", test.markdown, g, w)
			}
		})
	}
}

// Each top-level block starts where its own markdown does: front matter at 0, a heading at its
// marker, a paragraph at its first line, a fenced code block at its opening fence, a table at its
// header row and a callout at its opener. The tree is the one Parse reads, and the empty paragraph
// of a document with no blocks starts nowhere.
func TestParseWithSourceStartsPlacesEveryTopLevelBlock(t *testing.T) {
	openings := []string{
		"---\ntitle: x\n---",
		"# Heading",
		"A paragraph\nsecond line.",
		"```go\ncode\n```",
		"| a | b |\n|---|---|\n| 1 | 2 |",
		":::callout{kind=\"note\"}\nInside.\n:::",
	}
	markdown := strings.Join(openings, "\n\n") + "\n"
	doc, starts, err := ParseWithSourceStarts(markdown)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Equal(parsed) {
		got, _ := doc.JSON()
		want, _ := parsed.JSON()
		t.Fatalf("tree differs from Parse\n got: %s\nwant: %s", got, want)
	}
	types := []string{"frontmatter", "heading", "paragraph", "code_block", "table", "callout"}
	if len(doc.Children) != len(types) || len(starts) != len(types) {
		t.Fatalf("children = %d, starts = %v, want %d of each", len(doc.Children), starts, len(types))
	}
	for index, opening := range openings {
		want := strings.Index(markdown, opening)
		if doc.Children[index].Type != types[index] || starts[index] != want {
			t.Errorf("child %d = %s at %d, want %s at %d", index, doc.Children[index].Type, starts[index], types[index], want)
		}
	}

	_, starts, err = ParseWithSourceStarts(" \n\n\t")
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 1 || starts[0] != -1 {
		t.Errorf("starts of a body of whitespace = %v, want [-1]", starts)
	}
}

func tablePaddingBomb(width int) string {
	return strings.Repeat("| header ", width) + "|\n" +
		strings.Repeat("| --- ", width) + "|\n" +
		strings.Repeat("| body |\n", width)
}
