package pmdoc

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A paragraph line the document parser reads as a block is the text the caller loses. #1326
// escaped ATX markers for this reason; these are the rest of the line-start forms: a thematic
// break, a setext underline, and a directive opener, which either silently becomes another node
// or refuses to parse at all.
func TestRenderEscapesBlockMarkerLinesInsideParagraphs(t *testing.T) {
	for _, text := range []string{
		"---",
		"***",
		"___",
		"-  -  -",
		":::callout{kind=\"warning\"}",
		":::",
		"::foo",
		"::::x{a=\"b\"}",
		"~~~",
		"~~~~ swallowed",
		"<div",
		"<!-- note",
		":smile{x}",
		"::1",
	} {
		t.Run(text, func(t *testing.T) {
			doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
				Type: "text",
				Text: text,
			}}}}}
			markdown, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if !back.Equal(doc) {
				t.Fatalf("Parse(Render()) of %q = %q, want the paragraph back", text, mustRender(t, back))
			}
		})
	}
}

// A paragraph line of tildes, written as it is, opens a code fence that swallows every block after
// it, and a `:::` line at a typed block's own prefix closes that block early, leaving its
// remaining lines and its closer to the document around it. Each context renders a document with
// a block after the line and requires every block back, in place.
func TestRenderKeepsTheBlocksAfterALineThatWouldOpenAFenceOrCloseATypedBlock(t *testing.T) {
	text := func(value string) *Node { return &Node{Type: "text", Text: value} }
	hardBreak := &Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}}
	paragraph := func(children ...*Node) *Node { return &Node{Type: "paragraph", Children: children} }
	callout := func(children ...*Node) *Node {
		return &Node{Type: "callout", Attrs: Attrs{BlockIDAttr: "c1", "kind": "note", "title": "T"}, Children: children}
	}
	after := paragraph(text("After."))
	for name, doc := range map[string]*Node{
		"a paragraph of tildes":        {Type: "doc", Children: []*Node{paragraph(text("~~~")), after}},
		"tildes with an info string":   {Type: "doc", Children: []*Node{paragraph(text("~~~ go")), after}},
		"tildes after a hard break":    {Type: "doc", Children: []*Node{paragraph(text("a"), hardBreak, text("~~~")), after}},
		"tildes in a list item":        {Type: "doc", Children: []*Node{{Type: "bullet_list", Children: []*Node{{Type: "list_item", Children: []*Node{paragraph(text("~~~"))}}}}, after}},
		"tildes in a blockquote":       {Type: "doc", Children: []*Node{{Type: "blockquote", Children: []*Node{paragraph(text("~~~"))}}, after}},
		"tildes in a typed block":      {Type: "doc", Children: []*Node{callout(paragraph(text("~~~"))), after}},
		"::: after a hard break in it": {Type: "doc", Children: []*Node{callout(paragraph(text("a"), hardBreak, text(":::")), paragraph(text("Inside."))), after}},
	} {
		t.Run(name, func(t *testing.T) {
			markdown := mustRender(t, doc)
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if len(back.Children) != len(doc.Children) || len(back.Children[0].Children) != len(doc.Children[0].Children) {
				t.Fatalf("Parse(%q) = %q, want every block back", markdown, mustRender(t, back))
			}
			for index, block := range doc.Children {
				if back.Children[index].Type != block.Type {
					t.Fatalf("Parse(%q) block %d is a %s, want a %s", markdown, index, back.Children[index].Type, block.Type)
				}
			}
			if again := mustRender(t, back); again != markdown {
				t.Fatalf("Render(Parse(%q)) = %q", markdown, again)
			}
		})
	}
}

// A rule that opens the document is written `---`, which the front-matter reader takes as an
// opener when any later line is also `---`: a second rule or a code line that is just `---`
// closes it, and everything up to there - the rule, the text, a fence - is read as front matter.
// A document with no such line keeps the `---` it has always been written with.
func TestRenderKeepsALeadingRuleFromReadingAsFrontMatter(t *testing.T) {
	text := func(value string) *Node { return &Node{Type: "text", Text: value} }
	paragraph := func(value string) *Node { return &Node{Type: "paragraph", Children: []*Node{text(value)}} }
	rule := func() *Node { return &Node{Type: "hr"} }
	code := &Node{Type: "code_block", Attrs: Attrs{"language": ""}, Children: []*Node{text("---")}}
	for name, doc := range map[string]*Node{
		"a second rule":     {Type: "doc", Children: []*Node{rule(), paragraph("x"), rule(), paragraph("y")}},
		"a code line ---":   {Type: "doc", Children: []*Node{rule(), paragraph("x"), code}},
		"no later --- line": {Type: "doc", Children: []*Node{rule(), paragraph("x")}},
	} {
		t.Run(name, func(t *testing.T) {
			markdown := mustRender(t, doc)
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if len(back.Children) != len(doc.Children) {
				t.Fatalf("Parse(%q) = %q, want every block back", markdown, mustRender(t, back))
			}
			for index, block := range doc.Children {
				if back.Children[index].Type != block.Type {
					t.Fatalf("Parse(%q) block %d is a %s, want a %s", markdown, index, back.Children[index].Type, block.Type)
				}
			}
			if again := mustRender(t, back); again != markdown {
				t.Fatalf("Render(Parse(%q)) = %q", markdown, again)
			}
		})
	}
	if got := mustRender(t, &Node{Type: "doc", Children: []*Node{rule(), paragraph("x")}}); got != "---\n\nx\n" {
		t.Fatalf("a leading rule with no later --- line = %q, want main's %q", got, "---\n\nx\n")
	}
}

// A list nested inside a blockquote writes its lines behind the quote's `>` and the list's
// indentation. Read without the list indentation they share, but with the quote marker left on,
// a second-level item's `<div` or `~~~` would look like indented code, and be left to open an
// HTML block or a fence in place.
func TestRenderEscapesBlockFormsInAListNestedInABlockquote(t *testing.T) {
	for _, marker := range []string{"<div", "~~~"} {
		t.Run(marker, func(t *testing.T) {
			tree, err := Parse("> - a\n>   - b\n>\n>     x\n\nAfter.\n")
			if err != nil {
				t.Fatal(err)
			}
			item := tree.Children[0].Children[0].Children[0].Children[1].Children[0]
			item.Children[1].Children[0].Text = marker
			markdown := mustRender(t, tree)
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if len(back.Children) != 2 || back.Children[1].Type != "paragraph" {
				t.Fatalf("Parse(%q) = %q, want the blockquote and the paragraph after it", markdown, mustRender(t, back))
			}
			if again := mustRender(t, back); again != markdown {
				t.Fatalf("Render(Parse(%q)) = %q", markdown, again)
			}
		})
	}
}

// GFM reads a single tilde as strikethrough too, and refuses a delimiter run of three: text
// `~a~` came back struck, and struck text ending or starting with a tilde lost its strike. Such a
// paragraph writes its tildes as references; a paragraph whose tildes pair with nothing - "about
// ~200ms" - and an ordinary strikethrough keep the bytes they have always had.
func TestRenderKeepsTildesAsTheyWereWritten(t *testing.T) {
	text := func(value string, marks ...Mark) *Node { return &Node{Type: "text", Text: value, Marks: marks} }
	struck := Mark{Type: "strike_through"}
	paragraph := func(children ...*Node) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: children}}}
	}
	for name, doc := range map[string]*Node{
		"a single-tilde pair":       paragraph(text("~a~")),
		"a pair across words":       paragraph(text("keep ~this~ as typed")),
		"struck text ending in ~":   paragraph(text("x~", struck), text(" tail")),
		"struck text starting in ~": paragraph(text("x "), text("~a", struck)),
		"a struck tilde alone":      paragraph(text("a "), text("~", struck), text(" b")),
	} {
		t.Run(name, func(t *testing.T) {
			markdown := mustRender(t, doc)
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if !back.Equal(doc) {
				t.Fatalf("Parse(%q) = %q, want the text and its marks back", markdown, mustRender(t, back))
			}
		})
	}
	for _, test := range []struct {
		doc  *Node
		want string
	}{
		{paragraph(text("about ~200ms")), "about ~200ms\n"},
		{paragraph(text("done", struck), text(" tail")), "~~done~~ tail\n"},
	} {
		if got := mustRender(t, test.doc); got != test.want {
			t.Fatalf("Render() = %q, want the bytes main writes, %q", got, test.want)
		}
	}
}

// A heading's text that ends in a run of `#` after a space reads back as the heading's closing
// sequence, and the text loses it; a list item's text that begins `[ ]` or `[x]` reads back as a
// task checkbox. Both are written so they come back as the text they are, while `C#`, a `#` in
// the middle and `[ ]` outside a list item keep the bytes main writes.
func TestRenderKeepsHeadingAndTaskTextThatReadsAsSyntax(t *testing.T) {
	text := func(value string) *Node { return &Node{Type: "text", Text: value} }
	heading := func(value string) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "heading", Attrs: Attrs{"level": 2}, Children: []*Node{text(value)}}}}
	}
	item := func(value string) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "bullet_list", Children: []*Node{{Type: "list_item", Children: []*Node{
			{Type: "paragraph", Children: []*Node{text(value)}},
		}}}}}}
	}
	for name, doc := range map[string]*Node{
		"a heading ending in #":   heading("a #"),
		"a heading ending in ###": heading("Step 3 ###"),
		"a heading of # alone":    heading("# #"),
		"an item beginning [ ]":   item("[ ]"),
		"an item beginning [x]":   item("[x] done"),
	} {
		t.Run(name, func(t *testing.T) {
			markdown := mustRender(t, doc)
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if got, want := mustRender(t, back), markdown; got != want || flatText(back) != flatText(doc) {
				t.Fatalf("Parse(%q) = %q, want the text back", markdown, got)
			}
		})
	}
	for _, test := range []struct {
		doc  *Node
		want string
	}{
		{heading("C#"), "## C#\n"},
		{heading("a # b"), "## a # b\n"},
		{&Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{text("[ ] later")}}}}, "[ ] later\n"},
	} {
		if got := mustRender(t, test.doc); got != test.want {
			t.Fatalf("Render() = %q, want the bytes main writes, %q", got, test.want)
		}
	}
}

// flatText is every text node's text in document order.
func flatText(node *Node) string {
	if node.Type == "text" {
		return node.Text
	}
	var out strings.Builder
	for _, child := range node.Children {
		out.WriteString(flatText(child))
	}
	return out.String()
}

// The parser trims whitespace at the start of a textblock's line and at the end of the
// textblock, so text that begins or ends with it lost it. The first leading and the last trailing
// whitespace character are written as references; whitespace inside a mark or a link keeps the
// bytes main writes, since the parser keeps it there.
func TestRenderKeepsWhitespaceAtATextblocksEdges(t *testing.T) {
	text := func(value string, marks ...Mark) *Node { return &Node{Type: "text", Text: value, Marks: marks} }
	hardBreak := &Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}}
	doc := func(block *Node) *Node { return &Node{Type: "doc", Children: []*Node{block}} }
	paragraph := func(children ...*Node) *Node { return &Node{Type: "paragraph", Children: children} }
	cell := func(kind, value string) *Node { return &Node{Type: kind, Children: []*Node{paragraph(text(value))}} }
	for name, tree := range map[string]*Node{
		"a paragraph's leading space":   doc(paragraph(text(" x"))),
		"three leading spaces":          doc(paragraph(text("   x"))),
		"a paragraph's trailing spaces": doc(paragraph(text("x  "))),
		"a leading tab":                 doc(paragraph(text("\tx"))),
		"a backslash before the last":   doc(paragraph(text("a\\ "))),
		"after a hard break":            doc(paragraph(text("a"), hardBreak, text(" b"))),
		"a heading's edges":             doc(&Node{Type: "heading", Attrs: Attrs{"level": 2}, Children: []*Node{text(" x ")}}),
		"a table cell's edges": doc(&Node{Type: "table", Children: []*Node{
			{Type: "table_header_row", Children: []*Node{cell("table_header", "h")}},
			{Type: "table_row", Children: []*Node{cell("table_cell", " x ")}},
		}}),
	} {
		t.Run(name, func(t *testing.T) {
			markdown := mustRender(t, tree)
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if flatText(back) != flatText(tree) {
				t.Fatalf("Parse(%q) has text %q, want %q", markdown, flatText(back), flatText(tree))
			}
			if again := mustRender(t, back); again != markdown {
				t.Fatalf("Render(Parse(%q)) = %q", markdown, again)
			}
		})
	}
	link := Mark{Type: "link", Attrs: Attrs{"href": "https://x.test"}}
	for _, test := range []struct {
		tree *Node
		want string
	}{
		{doc(paragraph(text(" x", link))), "[ x](https://x.test)\n"},
		{doc(paragraph(text("a  b"))), "a  b\n"},
	} {
		if got := mustRender(t, test.tree); got != test.want {
			t.Fatalf("Render() = %q, want the bytes main writes, %q", got, test.want)
		}
	}
}

// A delimiter beside whitespace cannot open or close emphasis, so bold, italic or struck text
// that began or ended with a space read back as literal asterisks or tildes. That whitespace is
// written as a reference, which is not whitespace to the delimiter rule.
func TestRenderKeepsWhitespaceAtAMarksEdges(t *testing.T) {
	text := func(value string, marks ...Mark) *Node { return &Node{Type: "text", Text: value, Marks: marks} }
	strong, emphasis, strike := Mark{Type: "strong"}, Mark{Type: "emphasis"}, Mark{Type: "strike_through"}
	link := Mark{Type: "link", Attrs: Attrs{"href": "https://x.test"}}
	paragraph := func(children ...*Node) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: children}}}
	}
	for name, tree := range map[string]*Node{
		"bold text opening with a space":    paragraph(text(" x", strong), text(" tail")),
		"italic text closing with a space":  paragraph(text("a "), text("x ", emphasis), text(" b")),
		"struck text with both":             paragraph(text(" x ", strike), text(" tail")),
		"after a hard break":                paragraph(text("a"), &Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}}, text(" x", strong)),
		"nested marks":                      paragraph(text("a "), text(" x", strong, emphasis), text(" b")),
		"asterisks beside bold's delimiter": paragraph(text("***", strong), text(" tail")),
		"a backslash and an asterisk":       paragraph(text("a\\*", emphasis), text(" tail")),
		"bold inside a link":                paragraph(text("a "), text(" x", strong, link), text(" b")),
	} {
		t.Run(name, func(t *testing.T) {
			markdown := mustRender(t, tree)
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if got, want := inlineSignature(back.Children[0].Children), inlineSignature(tree.Children[0].Children); !slices.Equal(got, want) {
				t.Fatalf("Parse(%q) = %q, want %q", markdown, got, want)
			}
		})
	}
	for _, test := range []struct {
		tree *Node
		want string
	}{
		{paragraph(text("a "), text(" x ", link), text(" b")), "a [ x ](https://x.test) b\n"},
		{paragraph(text("a "), text("x y", strong), text(" b")), "a **x y** b\n"},
		{paragraph(text("*.", strike), text(" b")), "~~*.~~ b\n"},
	} {
		if got := mustRender(t, test.tree); got != test.want {
			t.Fatalf("Render() = %q, want the bytes main writes, %q", got, test.want)
		}
	}
}

// Asterisks and underscores the writer's rules leave unescaped can still pair into emphasis the
// text never had: across a hard break, around a reference, inside a link label, or with a mark's
// own delimiter. A run that does not read back as written is written again with them escaped.
func TestRenderKeepsAsterisksAndUnderscoresThatWouldPair(t *testing.T) {
	text := func(value string, marks ...Mark) *Node { return &Node{Type: "text", Text: value, Marks: marks} }
	hardBreak := &Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}}
	link := Mark{Type: "link", Attrs: Attrs{"href": "https://x.test"}}
	paragraph := func(children ...*Node) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: children}}}
	}
	for name, tree := range map[string]*Node{
		"underscores across a hard break": paragraph(text(" _"), hardBreak, text(" _")),
		"asterisks across a hard break":   paragraph(text("-*\\"), hardBreak, text("-*\\")),
		"inside italic text":              paragraph(text(":*\\", Mark{Type: "emphasis"}), text(" after")),
		"inside a link label":             paragraph(text("*[*", link), text(" after")),
	} {
		t.Run(name, func(t *testing.T) {
			markdown := mustRender(t, tree)
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if got, want := inlineSignature(back.Children[0].Children), inlineSignature(tree.Children[0].Children); !slices.Equal(got, want) {
				t.Fatalf("Parse(%q) = %q, want %q", markdown, got, want)
			}
		})
	}
	// A run that reads back as written keeps main's bytes, snake_case and a lone asterisk included.
	for _, test := range []struct {
		tree *Node
		want string
	}{
		{paragraph(text("user_id and 2 * 3")), "user_id and 2 * 3\n"},
		{paragraph(text("a * b"), hardBreak, text("c * d")), "a * b\\\nc * d\n"},
	} {
		if got := mustRender(t, test.tree); got != test.want {
			t.Fatalf("Render() = %q, want the bytes main writes, %q", got, test.want)
		}
	}
}

// A paragraph's second line of dashes or equals signs underlines its first into a heading, so
// the escape applies to a marker line anywhere in the paragraph, not only its first.
func TestRenderEscapesSetextUnderlinesInsideParagraphs(t *testing.T) {
	for _, test := range []struct {
		text string
		want string
	}{
		{text: "Title\n===", want: "Title ==="},
		{text: "Title\n--", want: "Title --"},
		{text: "a | b\n--- | ---", want: "a | b --- | ---"},
		{text: "Title\n:--", want: "Title :--"},
		{text: "Title\n-:", want: "Title -:"},
	} {
		t.Run(test.text, func(t *testing.T) {
			doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
				Type: "text",
				Text: test.text,
			}}}}}
			markdown, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			// The newline renders as a soft break, which parses back as a space.
			want := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
				Type: "text",
				Text: test.want,
			}}}}}
			if !back.Equal(want) {
				t.Fatalf("Parse(Render()) of %q = %q, want %q", test.text, mustRender(t, back), test.want)
			}
		})
	}
}

// A backslash with nothing after it in its block escapes nothing, and reads back as itself, so
// it is written as it was. Escaping it would rewrite the canonical markdown of every stored
// document that carries one - a new version on the next edit, and an approved spec waiting on a
// human to approve the same words again.
func TestRenderLeavesABlockFinalBackslashAlone(t *testing.T) {
	for _, test := range []struct {
		name string
		doc  *Node
		want string
	}{
		{
			name: "paragraph",
			doc:  &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: `C:\`}}}}},
			want: "C:\\\n",
		},
		{
			name: "heading",
			doc: &Node{Type: "doc", Children: []*Node{{
				Type:     "heading",
				Attrs:    Attrs{"level": 2},
				Children: []*Node{{Type: "text", Text: `C:\`}},
			}}},
			want: "## C:\\\n",
		},
		{
			name: "list item",
			doc: &Node{Type: "doc", Children: []*Node{{
				Type:  "bullet_list",
				Attrs: Attrs{"marker": "-"},
				Children: []*Node{{
					Type:     "list_item",
					Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: `C:\`}}}},
				}},
			}}},
			want: "- C:\\\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			markdown, err := Render(test.doc)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("Render() = %q, want %q", markdown, test.want)
			}
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if again := mustRender(t, back); again != markdown {
				t.Fatalf("Render(Parse()) = %q, want %q", again, markdown)
			}
		})
	}
}

// A `]` inside link text closes the link's label early: `[a]b](url)` reads back as plain text
// with the link gone, and text like `[x]: /u` makes the line a link reference definition the
// Proof schema refuses. The image case already escapes `]` in alt text for the same reason.
func TestRenderEscapesBracketsInLinkText(t *testing.T) {
	for _, text := range []string{
		`a]b`, `a]`, `]x`, `a](b)`, `[x]: /u`, `[^1]: n`,
	} {
		t.Run(text, func(t *testing.T) {
			link := []Mark{{Type: "link", Attrs: Attrs{"href": "https://x.test"}}}
			doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
				{Type: "text", Text: text, Marks: link},
			}}}}
			markdown, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			first := back.Children[0].Children[0]
			if first.Text != text || len(first.Marks) != 1 || first.Marks[0].Type != "link" {
				t.Fatalf("Parse(%q) = %#v, want %q under a link", markdown, first, text)
			}
		})
	}
}

// Brackets that pair on their own are written as they are: they read back as the link already,
// and escaping them would change the markdown of every stored document carrying a citation-style
// link, minting a version on its next anchor or edit.
func TestRenderLeavesPairedBracketsInLinkTextAlone(t *testing.T) {
	for _, test := range []struct {
		text string
		want string
	}{
		{text: `Issue [#42]`, want: "[Issue [#42]](https://x.test)\n"},
		{text: `[1]`, want: "[[1]](https://x.test)\n"},
		{text: `see [docs] here`, want: "[see [docs] here](https://x.test)\n"},
		{text: `a[b[c]d]e`, want: "[a[b[c]d]e](https://x.test)\n"},
	} {
		t.Run(test.text, func(t *testing.T) {
			link := []Mark{{Type: "link", Attrs: Attrs{"href": "https://x.test"}}}
			doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
				{Type: "text", Text: test.text, Marks: link},
			}}}}
			markdown, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("Render() = %q, want %q", markdown, test.want)
			}
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			first := back.Children[0].Children[0]
			if first.Text != test.text || len(first.Marks) != 1 || first.Marks[0].Type != "link" {
				t.Fatalf("Parse(%q) = %#v, want %q under a link", markdown, first, test.text)
			}
		})
	}
}

// A bare URL is written as itself so the parser linkifies it again, and its brackets are part of
// the address: escaping them stops linkify at the backslash and truncates the href.
func TestRenderKeepsBracketsInABareURL(t *testing.T) {
	for _, url := range []string{
		"https://x.test/p?q=[1]",
		"https://x.test/[a]",
		"https://x.test/a]b",
	} {
		t.Run(url, func(t *testing.T) {
			doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
				{Type: "text", Text: url, Marks: []Mark{{Type: "link", Attrs: Attrs{"href": url}}}},
			}}}}
			markdown, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != url+"\n" {
				t.Fatalf("Render() = %q, want the bare URL %q", markdown, url)
			}
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			first := back.Children[0].Children[0]
			href, _ := first.Marks[0].Attrs["href"].(string)
			if first.Text != url || href != url {
				t.Fatalf("Parse(%q) = %q linking %q, want the whole address", markdown, first.Text, href)
			}
		})
	}
}

// A backslash that ends marked text eats the marker that closes it: `**a\**` reads back as an
// escaped asterisk and the bold is gone, and a link's text does the same to its `]`.
func TestRenderKeepsMarksAroundTextEndingInABackslash(t *testing.T) {
	for _, test := range []struct {
		name  string
		marks []Mark
	}{
		{name: "strong", marks: []Mark{{Type: "strong"}}},
		{name: "emphasis", marks: []Mark{{Type: "emphasis"}}},
		{name: "inlineCode", marks: []Mark{{Type: "inlineCode"}}},
		{name: "link", marks: []Mark{{Type: "link", Attrs: Attrs{"href": "https://x.test"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
				{Type: "text", Text: `a\`, Marks: test.marks},
				{Type: "text", Text: " tail"},
			}}}}
			markdown, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			// The parser stamps a mark with the marker it read (`**` or `_`), so the round trip
			// is compared by the text and mark that came back, and by rendering it again.
			first := back.Children[0].Children[0]
			if first.Text != `a\` || len(first.Marks) != 1 || first.Marks[0].Type != test.name {
				t.Fatalf("Parse(%q) = %#v, want %q under a %s mark", markdown, first, `a\`, test.name)
			}
			if again := mustRender(t, back); again != markdown {
				t.Fatalf("Render(Parse(%q)) = %q", markdown, again)
			}
		})
	}
}

// A backslash at the end of a text node eats the backslash form of the hard break that follows
// it, turning two lines into one, so the break is written as two trailing spaces instead.
func TestRenderKeepsAHardBreakAfterATrailingBackslash(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
		{Type: "text", Text: "Ends in a backslash \\"},
		{Type: "hardbreak"},
		{Type: "text", Text: "next line"},
	}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatalf("Parse(%q) = %v", markdown, err)
	}
	var kinds []string
	for _, child := range back.Children[0].Children {
		kinds = append(kinds, child.Type)
	}
	if strings.Join(kinds, ",") != "text,hardbreak,text" {
		t.Fatalf("Parse(Render()) inline = %v, rendered %q", kinds, markdown)
	}
	if again := mustRender(t, back); again != markdown {
		t.Fatalf("Render(Parse(Render())) = %q, want %q", again, markdown)
	}
}

// Four spaces or a tab at the start of a paragraph's first line open an indented code block, so
// the paragraph comes back as one. That whitespace takes no backslash, so it is written as the
// numeric reference the parser decodes back to it.
func TestRenderKeepsLeadingWhitespaceOnAParagraph(t *testing.T) {
	for _, test := range []struct {
		text string
		want string
	}{
		{text: "    indented code", want: "&#32;   indented code\n"},
		{text: "\tindented tab", want: "&#9;indented tab\n"},
	} {
		t.Run(test.text, func(t *testing.T) {
			doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
				Type: "text",
				Text: test.text,
			}}}}}
			markdown, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("Render() = %q, want %q", markdown, test.want)
			}
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if !back.Equal(doc) {
				t.Fatalf("Parse(Render()) = %q, want the paragraph back", mustRender(t, back))
			}
		})
	}
}

// The line a setext underline underlines can end at a hard break, where the underline opens a
// fresh text node whose own offsets show no predecessor.
func TestRenderEscapesASetextUnderlineAfterAHardBreak(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
		{Type: "text", Text: "Title"},
		{Type: "hardbreak"},
		{Type: "text", Text: "=="},
	}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "Title\\\n\\==\n" {
		t.Fatalf("Render() = %q", markdown)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	// The parser stamps a hard break with its own `isInline` attribute, so the tree is compared
	// by what it renders and by the block it is: one paragraph, not a heading with an underline.
	if len(back.Children) != 1 || back.Children[0].Type != "paragraph" {
		t.Fatalf("Parse(Render()) children = %#v", back.Children)
	}
	if again := mustRender(t, back); again != markdown {
		t.Fatalf("Render(Parse(Render())) = %q, want %q", again, markdown)
	}
}

// Stored markdown that main already renders byte for byte must come back byte for byte: a changed
// rendering mints a new version of the document on its next anchor or edit, and on an approved
// spec a stale approval. Each row is main's own rendering of text that reads back correctly, in
// a context an escape rule could misjudge - a label split by formatting or a code span, a label
// beside a different link, a heading, a table cell, the inside of a mark, a paragraph's later
// line, a typed block.
func TestRenderKeepsStoredMarkdownThatReadsBack(t *testing.T) {
	for _, markdown := range []string{
		"See [`pmdoc.Parse` [source]](https://x.test) for details.\n",
		"See [**RFC** [7231]](https://x.test) for details.\n",
		"As shown [[1]](https://a.test)[[2]](https://b.test).\n",
		"See [Issue [#42]](https://x.test) for details.\n",
		"[::1](https://x.test) is the loopback.\n",
		"**::1**\n",
		"*___*\n",
		"~~---~~\n",
		"## ~~~ tildes\n",
		"| a | b |\n| :--- | :--- |\n| ::1 | --- |\n",
		"<br is an element.\n",
		"<span class is not closed.\n",
		"a | b | c\\\n--- | ---\n",
		":::\n",
		":: a note\n",
		"The path is C:\\\n",
		// A hard break ends the line with a backslash, so none of these is a block.
		"---\\\nnext\n",
		"***\\\nnext\n",
		"___\\\nnext\n",
		"<div\\\nnext\n",
		// A list item's first line follows the previous item's last line, whose rule or HTML is
		// that item's own.
		"- Step one\n\n  ---\n- --dry-run skips the push\n",
		"1. Step one\n\n   ---\n2. -1 means unlimited\n",
		"> - a\n>\n>   ---\n> - -b\n",
		"- a\\\n  <br>\n- <- x\n",
		// A lone `:::` inside a blockquote or list within a typed block cannot close it.
		":::callout{#c1 kind=\"note\" title=\"T\"}\n> :::\n:::\n",
		":::callout{#c1 kind=\"note\" title=\"T\"}\n- :::\n:::\n",
		":::callout{#c1 kind=\"note\" title=\"T\"}\n- a\\\n  :::\n:::\n",
		":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich?\n\n- first\n- :::\n:::\n",
		":::callout{#c1 kind=\"note\" title=\"T\"}\n1. :::\n:::\n",
		// A task item's checkbox is already on the line, so its text opens no block.
		"- [ ] ::1 answers on port 80\n",
		"- [x] ---\n",
		"- [ ] ~~~ tildes\n",
		// Whitespace just inside a mark's marker is the mark's text, not a paragraph's indentation.
		"[    x](https://x.test)\n",
	} {
		t.Run(markdown, func(t *testing.T) {
			tree, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if got := mustRender(t, tree); got != markdown {
				t.Fatalf("Render(Parse(%q)) = %q, want the stored bytes", markdown, got)
			}
		})
	}
}

// The forms those contexts leave alone are still escaped where they do open a block: a lone
// `:::` inside a typed block closes it, a delimiter row under a header with as many cells makes a
// table, and `--` after a list item's `- ` finishes a thematic break.
func TestRenderEscapesBlockFormsWhereTheyOpenABlock(t *testing.T) {
	callout := &Node{Type: "callout", Attrs: Attrs{
		BlockIDAttr: "callout-1", "kind": "note", "title": "Read this",
	}, Children: []*Node{
		{Type: "paragraph", Children: []*Node{{Type: "text", Text: "inside"}}},
		{Type: "paragraph", Children: []*Node{{Type: "text", Text: ":::"}}},
	}}
	table := &Node{Type: "paragraph", Children: []*Node{
		{Type: "text", Text: "a | b"},
		{Type: "hardbreak"},
		{Type: "text", Text: "--- | ---"},
	}}
	list, err := Parse("- x\n")
	if err != nil {
		t.Fatal(err)
	}
	list.Children[0].Children[0].Children[0].Children[0].Text = "--"
	// After a code fence in a list item, the next item's `--` completes a rule and `<div` opens an
	// HTML block, whatever the fence's closing line reads as on its own.
	afterFence := func(text string) *Node {
		tree, err := Parse("- a\n  ```\n  x\n  ```\n- x\n")
		if err != nil {
			t.Fatal(err)
		}
		tree.Children[0].Children[1].Children[0].Children[0].Text = text
		return tree
	}
	// A footnote definition's first line and a third-level item's line after a hard break are read
	// as the parser reads them in place, where `---` is a rule and `<div` an HTML block.
	footnote := func(text string) *Node {
		tree, err := Parse("x[^1]\n\n[^1]: x\n")
		if err != nil {
			t.Fatal(err)
		}
		tree.Children[1].Children[0].Children[0].Text = text
		return tree
	}
	deepAfterBreak := func(text string) *Node {
		tree, err := Parse("- a\n  - b\n    - c\\\n      x\n")
		if err != nil {
			t.Fatal(err)
		}
		paragraph := tree.Children[0].Children[0].Children[1].Children[0].Children[1].Children[0].Children[0]
		paragraph.Children[len(paragraph.Children)-1].Text = text
		return tree
	}
	// A hard break's backslash makes `:::` the refused directive `:::\`.
	directive := &Node{Type: "paragraph", Children: []*Node{
		{Type: "text", Text: ":::"},
		{Type: "hardbreak"},
		{Type: "text", Text: "next"},
	}}
	for name, doc := range map[string]*Node{
		"a lone ::: in a typed block":           {Type: "doc", Children: []*Node{callout}},
		"a delimiter row that fits":             {Type: "doc", Children: []*Node{table}},
		"a rule its list marker would complete": list,
		"::: before a hard break":               {Type: "doc", Children: []*Node{directive}},
		"a rule after a code fence":             afterFence("--"),
		"an HTML block after a code fence":      afterFence("<div"),
		"a rule opening a footnote":             footnote("---"),
		"an HTML block deep in a list":          deepAfterBreak("<div"),
	} {
		t.Run(name, func(t *testing.T) {
			markdown, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if len(back.Children) != len(doc.Children) || back.Children[0].Type != doc.Children[0].Type {
				t.Fatalf("Parse(%q) = %v blocks, want the %s back", markdown, len(back.Children), doc.Children[0].Type)
			}
			if again := mustRender(t, back); again != markdown {
				t.Fatalf("Render(Parse()) = %q, want %q", again, markdown)
			}
		})
	}
}

// A strikethrough opener at a line's start is `~~`, so struck text that begins with `~` would
// write `~~~200ms~~`: a tilde fence that swallows every block after it.
func TestRenderKeepsStruckTextBeginningWithATilde(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{
		{Type: "paragraph", Children: []*Node{
			{Type: "text", Text: "~200ms", Marks: []Mark{{Type: "strike_through"}}},
			{Type: "text", Text: " at p99"},
		}},
		{Type: "paragraph", Children: []*Node{{Type: "text", Text: "After."}}},
	}}
	markdown := mustRender(t, doc)
	back, err := Parse(markdown)
	if err != nil {
		t.Fatalf("Parse(%q) = %v", markdown, err)
	}
	if len(back.Children) != 2 || back.Children[0].Type != "paragraph" {
		t.Fatalf("Parse(%q) = %d blocks, want both paragraphs", markdown, len(back.Children))
	}
	first := back.Children[0].Children
	if len(first) != 2 || first[0].Text != "~200ms" || !nodeHasMark(first[0], "strike_through") || first[1].Text != " at p99" {
		t.Fatalf("Parse(%q) lost the struck text", markdown)
	}
	if again := mustRender(t, back); again != markdown {
		t.Fatalf("Render(Parse()) = %q, want %q", again, markdown)
	}
}

func mustRender(t *testing.T, tree *Node) string {
	t.Helper()
	markdown, err := Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	return markdown
}

// The writer escapes a line's first character after the line is written, which must cost the line,
// not the document: rendering stays linear in the number of escaped lines.
func BenchmarkRenderEscapedLineStarts(b *testing.B) {
	doc := &Node{Type: "doc"}
	for index := 0; index < 16000; index++ {
		doc.Children = append(doc.Children, &Node{Type: "paragraph", Children: []*Node{
			{Type: "text", Text: fmt.Sprintf("<div %d", index)},
		}})
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := Render(doc); err != nil {
			b.Fatal(err)
		}
	}
}
