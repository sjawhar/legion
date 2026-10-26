package pmdoc

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var orderedMarker = regexp.MustCompile(`(?m)^\d+\.\s*`)
var spanTag = regexp.MustCompile(`</?span(?:\s[^>]*)?>`)
var linkDestination = regexp.MustCompile(`\]\([^)]*\)`)
var setextEquals = regexp.MustCompile(`(?m)^([ >]*)=+[ \t]*$`)
var emptyAttribute = regexp.MustCompile(`(\s[A-Za-z][\w-]*)=""`)

func TestRenderMatchesMilkdownForFixtures(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			doc, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			if plain(got) != plain(fx.RenderedByMilkdown) {
				t.Fatalf("text content differs\n got: %q\nwant: %q\nplain got: %q\nplain want: %q", got, fx.RenderedByMilkdown, plain(got), plain(fx.RenderedByMilkdown))
			}
		})
	}
}

func TestRenderEmphasisAndEmoji(t *testing.T) {
	doc, err := FromJSON([]byte(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Some "},{"type":"text","text":"bold","marks":[{"type":"strong"}]},{"type":"text","text":" 😀 end"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	md, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if md != "Some **bold** 😀 end\n" {
		t.Fatalf("md = %q", md)
	}
}

func TestRenderEmptyDocumentPreservesProofParagraph(t *testing.T) {
	want := &Node{Type: "doc", Children: []*Node{{Type: "paragraph"}}}
	parsed, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Equal(want) {
		t.Fatalf("Parse(\"\") = %#v, want %#v", parsed, want)
	}
	markdown, err := Render(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "" {
		t.Fatalf("Render(Parse(\"\")) = %q, want empty markdown", markdown)
	}
	reparsed, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !reparsed.Equal(want) {
		t.Fatal("Parse(Render(Parse(\"\"))) lost the empty paragraph")
	}
}

func TestRenderHardBreakUsesBackslash(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{
		Type: "paragraph",
		Children: []*Node{
			{Type: "text", Text: "Before"},
			{Type: "hardbreak", Attrs: Attrs{"isInline": false}},
			{Type: "text", Text: "after"},
		},
	}}}
	got, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Before\\\nafter\n" {
		t.Fatalf("Render(hardbreak) = %q", got)
	}
}

func TestRenderTableCellEscapesPipes(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{
		Type: "table",
		Children: []*Node{
			{Type: "table_header_row", Children: []*Node{{Type: "table_header", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "head|er"}}}}}}},
			{Type: "table_row", Children: []*Node{{Type: "table_cell", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "cel|l"}}}}}}},
		},
	}}}
	got, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "| head\\|er |\n| --- |\n| cel\\|l |\n" {
		t.Fatalf("Render(table) = %q", got)
	}
}

func TestRenderTableCellParagraph(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{
		Type: "table",
		Children: []*Node{
			{Type: "table_header_row", Children: []*Node{{Type: "table_header", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "header"}}}}}}},
			{Type: "table_row", Children: []*Node{{Type: "table_cell", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "cell"}}}}}}},
		},
	}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "| header |\n| --- |\n| cell |\n" {
		t.Fatalf("Render(table) = %q", markdown)
	}
}

// typedFenceRun is a typed block's fence, whose length is syntax: the engine writes three colons
// around a callout whose code holds a `:::` line, which its own parser then reads as the callout's
// end, where the renderer writes the longer fence both parsers read.
var typedFenceRun = regexp.MustCompile(`(?m)^(\s*):{3,}`)

func plain(markdown string) string {
	markdown = typedFenceRun.ReplaceAllString(markdown, "$1:::")
	// An attribute holding the empty string reads the same written bare, as the engine writes
	// it (`title`), or quoted, as the renderer does (`title=""`).
	markdown = emptyAttribute.ReplaceAllString(markdown, "$1")
	// A setext underline's length is syntax: the engine writes one `=`, the renderer three.
	markdown = setextEquals.ReplaceAllString(markdown, "$1=")
	markdown = html.UnescapeString(markdown)
	markdown = orderedMarker.ReplaceAllString(markdown, "")
	markdown = spanTag.ReplaceAllString(markdown, "")
	markdown = linkDestination.ReplaceAllString(markdown, "]")
	markdown = strings.NewReplacer("\\", "", "*", "", "_", "", "~", "", "`", "", "#", "", ">", "", "<", "", "-", "", "|", "", "[", "", "]", "", "(", "", ")", "").Replace(markdown)
	return strings.Join(strings.Fields(markdown), " ")
}

func TestRenderTableEscapedPipeMapsToFlattenedText(t *testing.T) {
	doc, err := Parse("| a\\|b |\n| :--- |\n| body |\n")
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "| a\\|b |\n| :--- |\n| body |\n" {
		t.Fatalf("Render(table) = %q", markdown)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(doc) {
		got, _ := back.JSON()
		want, _ := doc.JSON()
		t.Fatalf("Parse(Render(table)) changed escaped-pipe text\n got: %s\nwant: %s", got, want)
	}
	r, err := FindQuote(doc, "a|b", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r != (Range{From: 4, To: 7}) {
		t.Fatalf("FindQuote(a|b) = %v", r)
	}
}

func TestRenderEscapesLiteralMarkdownText(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "text",
		Text: "literal **not bold** and `not code` and [not link](x) < & |",
	}}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := "literal \\*\\*not bold\\*\\* and \\`not code\\` and \\[not link]\\(x) < & |\n"
	if markdown != want {
		t.Fatalf("Render() = %q, want %q", markdown, want)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(doc) {
		t.Fatalf("Parse(Render()) = %#v, want %#v", back, doc)
	}
}

func TestRenderEscapesOrderedListLookingParagraphs(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "text",
		Text: "1. not a list\n2) also not a list",
	}}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "1\\. not a list\\\n2\\) also not a list\n" {
		t.Fatalf("Render() = %q", markdown)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	// The line feed inside the text node is written as the hard break the browser editor draws.
	want := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
		{Type: "text", Text: "1. not a list"},
		{Type: "hardbreak", Attrs: Attrs{"isInline": false}},
		{Type: "text", Text: "2) also not a list"},
	}}}}
	if !back.Equal(want) {
		t.Fatalf("Parse(Render()) = %#v, want %#v", back, want)
	}
}

func TestRenderLeavesOrdinaryProseUnescaped(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "text",
		Text: "It's ordinary: prose - with [brackets], | pipes, and < 2.",
	}}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(markdown, "\\") {
		t.Fatalf("Render() over-escaped ordinary prose: %q", markdown)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(doc) {
		t.Fatalf("Parse(Render()) = %#v, want %#v", back, doc)
	}
}

func TestRenderBareURLLinkAsAutolinkLiteral(t *testing.T) {
	const url = "https://example.com/docs"
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
		{Type: "text", Text: "see "},
		{Type: "text", Text: url, Marks: []Mark{{Type: "link", Attrs: Attrs{"href": url, "title": nil}}}},
	}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "see https://example.com/docs\n" {
		t.Fatalf("Render() = %q", markdown)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(doc) {
		t.Fatalf("Parse(Render()) = %#v, want %#v", back, doc)
	}
}

func TestRenderKeepsNonAutolinkURLAsExplicitLink(t *testing.T) {
	const url = "https://example.com/a b"
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "text", Text: url, Marks: []Mark{{Type: "link", Attrs: Attrs{"href": url, "title": nil}}},
	}}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "[https://example.com/a b](<https://example.com/a b>)\n" {
		t.Fatalf("Render() = %q", markdown)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(doc) {
		t.Fatalf("Parse(Render()) = %#v, want %#v", back, doc)
	}
}

func TestRenderDoesNotEscapeExplicitURLLinkLabel(t *testing.T) {
	const url = "https://example.com/x"
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "text", Text: url, Marks: []Mark{{Type: "link", Attrs: Attrs{"href": url, "title": "title"}}},
	}}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "[https://example.com/x](https://example.com/x \"title\")\n" {
		t.Fatalf("Render() = %q", markdown)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(doc) {
		t.Fatalf("Parse(Render()) = %#v, want %#v", back, doc)
	}
}

func TestRenderFencesInlineCodeContainingBackticks(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "internal", text: "a ` b", want: "``a ` b``\n"},
		{name: "edges", text: "`edge`", want: "`` `edge` ``\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
				Type: "text", Text: test.text, Marks: []Mark{{Type: "inlineCode"}},
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
				t.Fatal(err)
			}
			if !back.Equal(doc) {
				t.Fatalf("Parse(Render()) = %#v, want %#v", back, doc)
			}
		})
	}
}

func TestRenderPadsInlineCodeSurroundedBySpaces(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "text", Text: " x ", Marks: []Mark{{Type: "inlineCode"}},
	}}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "`  x  `\n" {
		t.Fatalf("Render() = %q", markdown)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(doc) {
		t.Fatalf("Parse(Render()) = %#v, want %#v", back, doc)
	}
}

func TestRenderEscapesLinkDelimiters(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "text",
		Text: "label",
		Marks: []Mark{{
			Type:  "link",
			Attrs: Attrs{"href": "https://example.com/a(b)", "title": `say "hi"`},
		}},
	}}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := `[label](<https://example.com/a(b)> "say \"hi\"")` + "\n"
	if markdown != want {
		t.Fatalf("Render() = %q, want %q", markdown, want)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(doc) {
		got, _ := back.JSON()
		want, _ := doc.JSON()
		t.Fatalf("Parse(Render())\n got: %s\nwant: %s", got, want)
	}
}

func TestRenderFencesCodeBlockContainingFence(t *testing.T) {
	doc, err := Parse("````markdown\n```\nexample\n```\n````\n")
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "````markdown\n```\nexample\n```\n````\n" {
		t.Fatalf("Render() = %q", markdown)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(doc) {
		t.Fatalf("Parse(Render()) = %#v, want %#v", back, doc)
	}
}

func TestRenderEscapesImageDelimiters(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "image",
		Attrs: Attrs{
			"alt":   "a]b",
			"src":   "https://example.com/a (b)",
			"title": `say "hi"`,
		},
	}}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := `![a\]b](<https://example.com/a (b)> "say \"hi\"")` + "\n"
	if markdown != want {
		t.Fatalf("Render() = %q, want %q", markdown, want)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(doc) {
		got, _ := back.JSON()
		want, _ := doc.JSON()
		t.Fatalf("Parse(Render())\n got: %s\nwant: %s", got, want)
	}
}

// A linked image is written inside its link's syntax, its title spelled as a text link's and its
// link, held decoded, escaped so it reads back as it is, and reads back with its link and link
// title (INFRA-190).
func TestRenderWritesALinkedImageInsideItsLink(t *testing.T) {
	image := func(title, link, linkTitle any) *Node {
		return &Node{Type: "image", Attrs: Attrs{"src": "i.png", "alt": "x", "title": title, "link": link, "linkTitle": linkTitle}}
	}
	paragraph := func(children ...*Node) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: children}}}
	}
	inCell := func(img *Node) *Node {
		doc, err := Parse("| h |\n| - |\n| ![x](i.png) |\n")
		if err != nil {
			t.Fatal(err)
		}
		doc.Children[0].Children[1].Children[0].Children[0].Children = []*Node{img}
		return doc
	}
	for _, test := range []struct {
		name string
		doc  *Node
		want string
	}{
		{"both titles", paragraph(image("t", "https://u.com", "lt")), `[![x](i.png "t")](https://u.com "lt")` + "\n"},
		{"neither title", paragraph(image(nil, "https://u.com", nil)), "[![x](i.png)](https://u.com)\n"},
		{"a destination needing angle brackets", paragraph(image(nil, "https://u.com/a (b)", nil)), "[![x](i.png)](<https://u.com/a (b)>)\n"},
		{"quotes and backslashes in titles", paragraph(image(`say "hi"`, "https://u.com", `a\b`)), `[![x](i.png "say \"hi\"")](https://u.com "a\\b")` + "\n"},
		{"a table cell with a pipe in the link", inCell(image(nil, "https://u.com/a|b", "c|d")), "| h |\n| --- |\n| [![x](i.png)](https://u.com/a\\|b \"c\\|d\") |\n"},
		{"an ampersand opening a character reference", paragraph(image(nil, "https://u.com/?a=1&amp;b=2", nil)), "[![x](i.png)](https://u.com/?a=1\\&amp;b=2)\n"},
		{"an ampersand opening none", paragraph(image(nil, "https://u.com/?a=1&b=2", nil)), "[![x](i.png)](https://u.com/?a=1&b=2)\n"},
		{"an asterisk", paragraph(image(nil, "https://u.com/a*b", nil)), "[![x](i.png)](https://u.com/a*b)\n"},
		{"an unbalanced closing parenthesis", paragraph(image(nil, "https://u.com/)x", nil)), "[![x](i.png)](<https://u.com/)x>)\n"},
		{"backslashes before punctuation, a letter and the end", paragraph(image(nil, `https://u.com/a\*b\c\`, nil)), "[![x](i.png)](https://u.com/a\\\\*b\\c\\\\)\n"},
		{"angle brackets", paragraph(image(nil, "<https://u.com/<a>>", nil)), "[![x](i.png)](\\<https://u.com/<a>>)\n"},
		{"spaces and angle brackets", paragraph(image(nil, "https://u.com/a b<c>", nil)), "[![x](i.png)](<https://u.com/a b\\<c\\>>)\n"},
		{"control characters", paragraph(image(nil, "https://u.com/a\tb\nc", nil)), "[![x](i.png)](https://u.com/a&#9;b&#10;c)\n"},
		{"a table cell with a pipe, a backslash and a character reference in the link", inCell(image(nil, `https://u.com/a\|b&amp;c`, nil)), "| h |\n| --- |\n| [![x](i.png)](https://u.com/a\\\\\\|b\\&amp;c) |\n"},
		{"after text ending in !", paragraph(&Node{Type: "text", Text: "wow!"}, image(nil, "https://u.com", nil)), "wow\\![![x](i.png)](https://u.com)\n"},
		{"after bold text ending in !", paragraph(&Node{Type: "text", Text: "wow!", Marks: []Mark{{Type: "strong", Attrs: Attrs{"marker": "*"}}}}, image(nil, "https://u.com", nil)), "**wow!**[![x](i.png)](https://u.com)\n"},
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
				t.Fatal(err)
			}
			if !back.Equal(test.doc) {
				got, _ := back.JSON()
				want, _ := test.doc.JSON()
				t.Fatalf("Parse(%q)\n got: %s\nwant: %s", markdown, got, want)
			}
		})
	}
}

// An image's source, title, link and link title are held decoded, as the browser editor's parser
// reads them, and each is written so it reads back as it is held: Go's Parse and ParseForWrite
// take the rendering as the tree, and the editor's engine reads the same image, for references
// with a name an HTML entity has and without one, numeric ones, backslashes, angle brackets,
// parentheses, spaces, quotes, control characters, non-ASCII text, pipes and line breaks whose
// next line could open a block, in every block the writer writes an image in: a paragraph, a level
// 1 and a level 2 heading (written ATX), a table cell, a bullet and an ordered list item, a block
// quote and a list item in a block quote. A title's line feed is written as it is only in a
// top-level paragraph that holds it, so a title the server already stores keeps its bytes, and as
// `&#10;` in every other block.
func TestRenderWritesEachImageFieldSoItReadsBackAsHeld(t *testing.T) {
	common := []string{
		"&amp;", "i&amp;.png", "&copy;", "a&copy;b", "&foo;", "a&foo;b", "&#", "a&#b", "&#65;", "&#x41;", "&#X41;x",
		"&amp", "&copy=1", "&&amp;", `\&amp;`, `\&foo;`, `\`, `a\b`, `a\*b`, `\\`, `a\`, `a\\`, `\|`, "a|b", "|",
		"<", ">", "<a", "a<b>c", "a>", "(", ")", "a(b)c", "a)b", "a b", " a ", "a b<c>", "<a b>", "é ü 日本 \u00a0",
		"a\tb", "a\nb", "a\rb", "a\fb", "a\x01b", "a\x7fb", "a\vb", `a\` + "\tb", `a\` + "\nb", "&#10;", `\&#9;`,
		"https://u.com/?a=1&amp;b=2&c=3", "javascript&#58;x",
		"a\n\nb", "\n", "a\n", "\na", "a\n  b", "a\n\tb", "a \nb", "a\n# b", "a\n===", "a\n---", "a\n- b",
		"a\n1. b", "a\n> b", "a\n```", "a\n|b", "a\n\n\nb",
	}
	titles := []string{`"`, `say "hi"`, `\"`, `a\"b`, "'", `"\`, `a"`}
	image := func(src string, title any, link, linkTitle any) *Node {
		return &Node{Type: "image", Attrs: Attrs{"src": src, "alt": "x", "title": title, "link": link, "linkTitle": linkTitle}}
	}
	// Each context is a source holding `![x](i.png)`, the path of child indexes from the doc to
	// the textblock holding it and the type of each node on that path.
	contexts := []struct {
		name, source string
		path         []int
		types        []string
	}{
		{"a paragraph", "![x](i.png)\n", []int{0}, []string{"paragraph"}},
		{"a level 1 heading", "# ![x](i.png)\n", []int{0}, []string{"heading"}},
		{"a level 2 heading", "## ![x](i.png)\n", []int{0}, []string{"heading"}},
		{"a table cell", "| h |\n| - |\n| ![x](i.png) |\n", []int{0, 1, 0, 0}, []string{"table", "table_row", "table_cell", "paragraph"}},
		{"a bullet list item", "- ![x](i.png)\n", []int{0, 0, 0}, []string{"bullet_list", "list_item", "paragraph"}},
		{"an ordered list item", "1. ![x](i.png)\n", []int{0, 0, 0}, []string{"ordered_list", "list_item", "paragraph"}},
		{"a block quote", "> ![x](i.png)\n", []int{0, 0}, []string{"blockquote", "paragraph"}},
		{"a list item in a block quote", "> - ![x](i.png)\n", []int{0, 0, 0, 0}, []string{"blockquote", "bullet_list", "list_item", "paragraph"}},
	}
	// place parses a context's source and swaps img in for its image, after checking the image
	// stands alone in the textblock the path names.
	place := func(source string, path []int, types []string, img *Node) *Node {
		doc, err := Parse(source)
		if err != nil {
			t.Fatalf("Parse(%q): %v", source, err)
		}
		block := doc
		for depth, index := range path {
			if index >= len(block.Children) || block.Children[index].Type != types[depth] {
				got, _ := doc.JSON()
				t.Fatalf("Parse(%q) = %s, want a %s at depth %d", source, got, types[depth], depth)
			}
			block = block.Children[index]
		}
		if len(block.Children) != 1 || block.Children[0].Type != "image" {
			got, _ := doc.JSON()
			t.Fatalf("Parse(%q) = %s, want one image in its %s", source, got, block.Type)
		}
		block.Children = []*Node{img}
		return doc
	}
	type row struct {
		name  string
		doc   *Node
		image *Node
	}
	var rows []row
	for _, field := range []string{"src", "title", "link", "linkTitle"} {
		values := common
		if field == "title" || field == "linkTitle" {
			values = append(append([]string{}, common...), titles...)
		}
		for _, value := range values {
			var img func() *Node
			switch field {
			case "src":
				img = func() *Node { return image(value, nil, nil, nil) }
			case "title":
				img = func() *Node { return image("i.png", value, nil, nil) }
			case "link":
				img = func() *Node { return image("i.png", nil, value, nil) }
			case "linkTitle":
				img = func() *Node { return image("i.png", nil, "https://u.com", value) }
			}
			for _, context := range contexts {
				placed := img()
				rows = append(rows, row{field + " " + strconv.Quote(value) + " in " + context.name, place(context.source, context.path, context.types, placed), placed})
			}
		}
	}
	rendered := make([]string, len(rows))
	for index, test := range rows {
		markdown, err := Render(test.doc)
		if err != nil {
			t.Fatalf("%s: Render: %v", test.name, err)
		}
		rendered[index] = markdown
	}
	readings := engineReadings(t, rendered)
	for index, test := range rows {
		t.Run(test.name, func(t *testing.T) {
			markdown := rendered[index]
			want, _ := test.doc.JSON()
			if back, err := Parse(markdown); err != nil || !back.Equal(test.doc) {
				got, _ := back.JSON()
				t.Errorf("Parse(%q) = %s (%v)\nwant: %s", markdown, got, err, want)
			}
			if back, err := ParseForWrite(markdown, nil); err != nil || !back.Equal(test.doc) {
				got, _ := back.JSON()
				t.Errorf("ParseForWrite(%q) = %s (%v)\nwant: %s", markdown, got, err, want)
			}
			var engine *Node
			if readings[index] != nil {
				Walk(readings[index], func(node *Node) bool {
					if node.Type == "image" && engine == nil {
						engine = node
					}
					return engine == nil
				})
			}
			if engine == nil || !engine.Equal(test.image) {
				got, _ := readings[index].JSON()
				t.Errorf("the engine reads %q as %s, want its image %v", markdown, got, test.image.Attrs)
			}
		})
	}
	// A title's line feed a top-level paragraph holds is written as it is, as the server stores
	// it; a heading, a table cell and a block quote take it as `&#10;`.
	written := map[string][2]string{
		"a paragraph":       {"", "\n"},
		"a level 1 heading": {"# ", "\n"},
		"a table cell":      {"| h |\n| --- |\n| ", " |\n"},
		"a block quote":     {"> ", "\n"},
	}
	for _, context := range contexts {
		around, ok := written[context.name]
		if !ok {
			continue
		}
		lineFeed := "&#10;"
		if context.name == "a paragraph" {
			lineFeed = "\n"
		}
		for _, test := range []struct {
			field string
			img   *Node
			want  string
		}{
			{"title", image("i.png", "a\nb", nil, nil), `![x](i.png "a` + lineFeed + `b")`},
			{"linkTitle", image("i.png", nil, "https://u.com", "a\nb"), `[![x](i.png)](https://u.com "a` + lineFeed + `b")`},
		} {
			want := around[0] + test.want + around[1]
			if markdown, err := Render(place(context.source, context.path, context.types, test.img)); err != nil || markdown != want {
				t.Errorf("Render(%s \"a\\nb\" in %s) = %q (%v), want %q", test.field, context.name, markdown, err, want)
			}
		}
	}
}

// A `!` ending text right before a `[` the writer puts after it - a linked image's, or a link's the
// next text opens first, a www link's the writer refuses bare after the `!` included - is escaped,
// where it would open an image; a `!` before anything else is written as it is. Go and the browser
// editor's engine both read each rendering back as the tree, its link a link and not an image.
func TestRenderEscapesABangBeforeALinksBracket(t *testing.T) {
	tests := []struct {
		name, markdown, want string
		images               int
	}{
		{"text before a link", "wow\\![a](u)\n", "wow\\![a](u)\n", 0},
		// The link is written outermost, so the bold closes before its `[`.
		{"bold text before a link inside the bold", "**wow\\![a](u)**\n", "**wow!**[**a**](u)\n", 0},
		{"text before a bold link", "wow\\!**[a](u)**\n", "wow\\![**a**](u)\n", 0},
		{"text and a space before a link", "wow! [a](u)\n", "wow! [a](u)\n", 0},
		{"text before a linked image", "wow\\![![x](i.png)](u)\n", "wow\\![![x](i.png)](u)\n", 1},
		{"text ending a paragraph", "wow!\n", "wow!\n", 0},
		// The bare URL after the www link stays bare: the first spelling reads back.
		{"text before a www link", "wow\\![www.a.test](http://www.a.test) see https://x.com/a\n", "wow\\![www.a.test](http://www.a.test) see https://x.com/a\n", 0},
		{"text before a www link in code", "wow\\![`www.a.test`](http://www.a.test) see https://x.com/a\n", "wow\\![`www.a.test`](http://www.a.test) see https://x.com/a\n", 0},
	}
	docs := make([]*Node, len(tests))
	rendered := make([]string, len(tests))
	for index, test := range tests {
		doc, err := Parse(test.markdown)
		if err != nil {
			t.Fatalf("Parse(%q): %v", test.markdown, err)
		}
		images := 0
		Walk(doc, func(node *Node) bool {
			if node.Type == "image" {
				images++
			}
			return true
		})
		if images != test.images {
			t.Fatalf("Parse(%q) reads %d images, want %d", test.markdown, images, test.images)
		}
		docs[index] = doc
		if rendered[index], err = Render(doc); err != nil {
			t.Fatalf("Render(Parse(%q)): %v", test.markdown, err)
		}
	}
	readings := engineReadings(t, rendered)
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if rendered[index] != test.want {
				t.Errorf("Render(Parse(%q)) = %q, want %q", test.markdown, rendered[index], test.want)
			}
			back, err := Parse(rendered[index])
			if err != nil || !back.Equal(docs[index]) {
				got, _ := back.JSON()
				want, _ := docs[index].JSON()
				t.Errorf("Go reads %q back as %s (%v)\nwant: %s", rendered[index], got, err, want)
			}
			if engine := readings[index]; engine == nil || !engine.Equal(docs[index]) {
				got, _ := engine.JSON()
				t.Errorf("the engine reads %q back as %s", rendered[index], got)
			}
		})
	}
}

// The first spelling of a run escapes a `!` before the `[` it writes next, inside marks it keeps
// open as well, and before a www link it writes as an explicit link because a `!` stands before it,
// and leaves one alone where a closer or a delimiter opener stands between.
func TestTheFirstSpellingEscapesABangBeforeALinksBracket(t *testing.T) {
	for _, test := range []struct {
		markdown string
		spelling runSpelling
		want     string
	}{
		{"**wow\\![a](u)**\n", runSpelling{keepOpen: true}, "**wow\\![a](u)**"},
		{"**wow\\![a](u)**\n", runSpelling{}, "**wow!**[**a**](u)"},
		{"wow\\![www.a.test](http://www.a.test)\n", runSpelling{}, "wow\\![www.a.test](http://www.a.test)"},
		{"wow\\![`www.a.test`](http://www.a.test)\n", runSpelling{}, "wow\\![`www.a.test`](http://www.a.test)"},
		{"wow!*[www.a.test](http://www.a.test)*\n", runSpelling{}, "wow!*www.a.test*"},
	} {
		doc, err := Parse(test.markdown)
		if err != nil {
			t.Fatalf("Parse(%q): %v", test.markdown, err)
		}
		r := &renderer{}
		r.writeInlineRun(doc.Children[0].Children, "", inlineContext{startsLine: true}, test.spelling)
		if got := r.b.String(); got != test.want {
			t.Errorf("writeInlineRun(Parse(%q), %+v) = %q, want %q", test.markdown, test.spelling, got, test.want)
		}
	}
}

func TestRenderPrefixesNestedCodeBlockLines(t *testing.T) {
	doc, err := Parse("> - item\n>\n>       code\n")
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(doc) {
		got, _ := back.JSON()
		want, _ := doc.JSON()
		t.Fatalf("Parse(Render()) differs\n got: %s\nwant: %s\nmarkdown:\n%s", got, want, markdown)
	}
}

func TestRenderParseRoundTripPreservesTableCellPipes(t *testing.T) {
	cases := []struct {
		name          string
		markdown      string
		htmlValue     string
		footnoteLabel string
		linkHref      string
	}{
		{
			name:     "text",
			markdown: "| header |\n| :--- |\n| one\\|two |\n",
		},
		{
			name:     "inline code",
			markdown: "| header |\n| :--- |\n| `one\\|two` |\n",
		},
		{
			name:     "emphasis",
			markdown: "| header |\n| :--- |\n| **one\\|two** |\n",
		},
		{
			name:     "link text",
			markdown: "| header |\n| :--- |\n| [one\\|two](https://example.test) |\n",
		},
		{
			name:     "link destination",
			markdown: "| header |\n| :--- |\n| [label](https://example.test/one\\|two) |\n",
			linkHref: "https://example.test/one|two",
		},
		{
			name:     "link title",
			markdown: "| header |\n| :--- |\n| [label](https://example.test \"one\\|two\") |\n",
		},
		{
			name:     "autolink-shaped link",
			markdown: "| header |\n| :--- |\n| [https://example.test/one\\|two](https://example.test/one\\|two) |\n",
			linkHref: "https://example.test/one|two",
		},
		{
			name:     "image alt",
			markdown: "| header |\n| :--- |\n| ![one\\|two](https://example.test/image.png) |\n",
		},
		{
			name:     "image destination",
			markdown: "| header |\n| :--- |\n| ![image](https://example.test/one\\|two.png) |\n",
		},
		{
			name:     "image title",
			markdown: "| header |\n| :--- |\n| ![image](https://example.test/image.png \"one\\|two\") |\n",
		},
		{
			name:      "raw HTML attribute",
			markdown:  "| header |\n| :--- |\n| <span data-label=\"one&#124;two\">text</span> |\n",
			htmlValue: "<span data-label=\"one&#124;two\">",
		},
		{
			// The browser editor's parser decodes the label's escaped pipe, in the cell and out.
			name:          "footnote label",
			markdown:      "| header |\n| :--- |\n| [^one\\|two] |\n\n[^one\\|two]: note\n",
			footnoteLabel: "one|two",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := Parse(tc.markdown)
			if err != nil {
				t.Fatalf("parse table: %v", err)
			}
			if tc.htmlValue != "" {
				if got := tableCellHTMLValue(doc); got != tc.htmlValue {
					t.Fatalf("table HTML value = %q, want %q", got, tc.htmlValue)
				}
			}
			if tc.linkHref != "" {
				if got := tableCellLinkHref(doc); got != tc.linkHref {
					t.Fatalf("table link href = %q, want %q", got, tc.linkHref)
				}
			}
			if tc.footnoteLabel != "" {
				if got := footnoteLabelsInDocument(doc); len(got) != 2 || got[0] != tc.footnoteLabel || got[1] != tc.footnoteLabel {
					t.Fatalf("footnote labels = %q, want two %q labels", got, tc.footnoteLabel)
				}
			}
			rendered, err := Render(doc)
			if err != nil {
				t.Fatalf("render table: %v", err)
			}
			if rendered != tc.markdown {
				t.Fatalf("rendered table = %q, want %q", rendered, tc.markdown)
			}
			reparsed, err := Parse(rendered)
			if err != nil {
				t.Fatalf("reparse rendered table: %v", err)
			}
			if !doc.Equal(reparsed) {
				got, _ := reparsed.JSON()
				want, _ := doc.JSON()
				t.Fatalf("Parse(Render(Parse(table))) changed the tree\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func tableCellHTMLValue(doc *Node) string {
	return doc.Children[0].Children[1].Children[0].Children[0].Children[0].Attrs["value"].(string)
}

func tableCellLinkHref(doc *Node) string {
	return doc.Children[0].Children[1].Children[0].Children[0].Children[0].Marks[0].Attrs["href"].(string)
}

func footnoteLabelsInDocument(doc *Node) []string {
	var labels []string
	walk(doc, func(node *Node, _ []int, _, _ int) bool {
		if node.Type == "footnote_reference" || node.Type == "footnote_definition" {
			labels = append(labels, node.Attrs["label"].(string))
		}
		return true
	})
	return labels
}

func TestRenderParseRoundTripPreservesNonTableEntities(t *testing.T) {
	cases := []struct {
		name     string
		markdown string
	}{
		{
			name:     "entity-bearing link destination",
			markdown: "[x](https://example.test/one&amp;amp;two)\n",
		},
		{
			name:     "raw HTML attribute",
			markdown: "<span data-label=\"one&#124;two\">text</span>\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := Parse(tc.markdown)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			rendered, err := Render(doc)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if rendered != tc.markdown {
				t.Fatalf("rendered = %q, want %q", rendered, tc.markdown)
			}
			reparsed, err := Parse(rendered)
			if err != nil {
				t.Fatalf("reparse: %v", err)
			}
			if !doc.Equal(reparsed) {
				got, _ := reparsed.JSON()
				want, _ := doc.JSON()
				t.Fatalf("Parse(Render(Parse())) changed the tree\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestRenderTableLinkDestinationPreservesLiteralBackslashBeforePipe(t *testing.T) {
	markdown := "| header |\n| :--- |\n| [label](https://example.test/one" + strings.Repeat("\\", 3) + "|two) |\n"
	doc, err := Parse(markdown)
	if err != nil {
		t.Fatalf("parse table: %v", err)
	}
	if got := tableCellLinkHref(doc); got != "https://example.test/one\\|two" {
		t.Fatalf("table link href = %q, want a literal backslash before the pipe", got)
	}
	rendered, err := Render(doc)
	if err != nil {
		t.Fatalf("render table: %v", err)
	}
	if rendered != markdown {
		t.Fatalf("rendered table = %q, want %q", rendered, markdown)
	}
	reparsed, err := Parse(rendered)
	if err != nil {
		t.Fatalf("reparse table: %v", err)
	}
	if !doc.Equal(reparsed) {
		t.Fatal("Parse(Render(Parse(table))) lost the literal backslash")
	}
}

func TestRenderTableHTMLPipeCanonicalizesToEntity(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{
		Type: "table",
		Children: []*Node{
			{Type: "table_header_row", Children: []*Node{{Type: "table_header", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "header"}}}}}}},
			{Type: "table_row", Children: []*Node{{Type: "table_cell", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "html", Attrs: Attrs{"value": `<span data-label="one|two">`}}}}}}}},
		},
	}}}
	rendered, err := Render(doc)
	if err != nil {
		t.Fatalf("render table: %v", err)
	}
	const want = "| header |\n| --- |\n| <span data-label=\"one&#124;two\"> |\n"
	if rendered != want {
		t.Fatalf("rendered table = %q, want %q", rendered, want)
	}
	parsed, err := Parse(rendered)
	if err != nil {
		t.Fatalf("parse canonical table: %v", err)
	}
	if got := tableCellHTMLValue(parsed); got != `<span data-label="one&#124;two">` {
		t.Fatalf("canonical HTML value = %q", got)
	}
	rerendered, err := Render(parsed)
	if err != nil {
		t.Fatalf("rerender canonical table: %v", err)
	}
	if rerendered != want {
		t.Fatalf("rerendered table = %q, want %q", rerendered, want)
	}
}

func TestRenderTableAutolinkPreservesHref(t *testing.T) {
	markdown := "| header |\n| :--- |\n| <https://example.test/one\\|two> |\n"
	doc, err := Parse(markdown)
	if err != nil {
		t.Fatalf("parse table: %v", err)
	}
	if got := tableCellLinkHref(doc); got != "https://example.test/one\\|two" {
		t.Fatalf("autolink href = %q, want a literal backslash before the pipe", got)
	}
	want := "| header |\n| :--- |\n| [https://example.test/one" + strings.Repeat("\\", 3) + "|two](https://example.test/one" + strings.Repeat("\\", 3) + "|two) |\n"
	rendered, err := Render(doc)
	if err != nil {
		t.Fatalf("render table: %v", err)
	}
	if rendered != want {
		t.Fatalf("rendered table = %q, want %q", rendered, want)
	}
	reparsed, err := Parse(rendered)
	if err != nil {
		t.Fatalf("reparse table: %v", err)
	}
	if !doc.Equal(reparsed) {
		t.Fatal("Parse(Render(Parse(table))) changed the autolink href")
	}
}

// Every mark the schema allows that the rendering does not write is an anchor, and no anchor may
// change the markdown, whichever one a later schema adds: the loop reads markTypes, so a new
// invisible mark is covered here the day it exists.
func TestNoUnrenderedSchemaMarkChangesTheMarkdown(t *testing.T) {
	const head, marked, tail = "Use snake_", "case here. see [x]", "(y)"
	whole := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
		{Type: "text", Text: head + marked + tail},
	}}}}
	want, err := Render(whole)
	if err != nil {
		t.Fatal(err)
	}
	for markType := range markTypes {
		if renderedMarkTypes[markType] {
			continue
		}
		t.Run(markType, func(t *testing.T) {
			marks := []Mark{{Type: markType, Attrs: Attrs{"id": "m1", "by": "user:alice"}}}
			split := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
				{Type: "text", Text: head},
				{Type: "text", Text: marked, Marks: marks},
				{Type: "text", Text: tail},
			}}}}
			markdown, err := Render(split)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != want {
				t.Fatalf("Render() with a %s mark = %q, want the unmarked %q", markType, markdown, want)
			}
		})
	}
}

// An anchor mark renders nothing, but one that starts or ends inside a word splits the word's
// text into two runs. The rendering is the same as if the mark were not there: an escape is
// decided over the whole run, so `snake_case` is never written `snake\_case`.
func TestRenderIsTheSameWhereverAnAnchorMarkSplitsText(t *testing.T) {
	comment := []Mark{{Type: "proofComment", Attrs: Attrs{"id": "c1", "by": "user:alice"}}}
	ask := []Mark{{Type: "dispatchAsk", Attrs: Attrs{"id": "a1", "by": "user:alice"}}}
	for _, test := range []struct {
		name string
		runs []*Node
		want string
	}{
		{
			name: "a comment on the end of a word",
			runs: []*Node{{Type: "text", Text: "Use snake_"}, {Type: "text", Text: "case", Marks: comment}, {Type: "text", Text: " here."}},
			want: "Use snake_case here.\n",
		},
		{
			name: "a comment ending between a link's brackets and its target",
			runs: []*Node{{Type: "text", Text: "see [x]", Marks: comment}, {Type: "text", Text: "(y) here."}},
			want: "see \\[x]\\(y) here.\n",
		},
		{
			name: "an ask on the start of a word",
			runs: []*Node{{Type: "text", Text: "Rename the "}, {Type: "text", Text: "user", Marks: ask}, {Type: "text", Text: "_id column."}},
			want: "Rename the user_id column.\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: test.runs}}}
			markdown, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("Render() = %q, want %q", markdown, test.want)
			}
		})
	}
}

// A tight list item writes its blocks on consecutive lines, but a paragraph followed by another
// paragraph, or by a rule written `---`, would read back as one paragraph or a setext heading. The
// browser editor's writer puts a blank line between two paragraphs, which the item then reads back
// as spread, and writes a rule `***`; so does this renderer, and every block reads back.
func TestRenderKeepsTheBlocksOfATightListItem(t *testing.T) {
	paragraph := func(value string) *Node {
		return &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: value}}}
	}
	item := func(children ...*Node) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "bullet_list", Attrs: Attrs{"spread": false}, Children: []*Node{
			{Type: "list_item", Attrs: Attrs{"spread": false}, Children: children},
			{Type: "list_item", Attrs: Attrs{"spread": false}, Children: []*Node{paragraph("next")}},
		}}}}
	}
	for name, test := range map[string]struct {
		doc  *Node
		want []string
	}{
		"two paragraphs":      {item(paragraph("a"), paragraph("b")), []string{"paragraph", "paragraph"}},
		"three paragraphs":    {item(paragraph("a"), paragraph("b"), paragraph("c")), []string{"paragraph", "paragraph", "paragraph"}},
		"a paragraph, a rule": {item(paragraph("a"), &Node{Type: "hr"}), []string{"paragraph", "hr"}},
	} {
		t.Run(name, func(t *testing.T) {
			markdown := mustRender(t, test.doc)
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			list := back.Children[0]
			if len(back.Children) != 1 || list.Type != "bullet_list" || len(list.Children) != 2 {
				t.Fatalf("Render() = %q, which reads back as %q", markdown, mustRender(t, back))
			}
			var got []string
			for _, child := range list.Children[0].Children {
				got = append(got, child.Type)
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("Render() = %q, whose first item reads back holding %v, want %v", markdown, got, test.want)
			}
		})
	}
}

// A footnote reference reads as one only when its label is defined, so text shaped like one - `[^1]`
// where the document defines `1` - is written with its bracket escaped, and a run beside a
// reference is read back with its definition, so delimiters that pair around the reference are
// escaped as they are anywhere else.
func TestRenderKeepsTextAroundFootnoteReferences(t *testing.T) {
	definition := func(label string) *Node {
		return &Node{Type: "footnote_definition", Attrs: Attrs{"label": label}, Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "x"}}}}}
	}
	text := func(value string) *Node { return &Node{Type: "text", Text: value} }
	reference := func(label string) *Node { return &Node{Type: "footnote_reference", Attrs: Attrs{"label": label}} }
	paragraph := func(children ...*Node) *Node { return &Node{Type: "paragraph", Children: children} }
	for name, doc := range map[string]*Node{
		"reference-shaped text":                    {Type: "doc", Children: []*Node{paragraph(text("see [^1] here"), reference("1")), definition("1")}},
		"reference-shaped text, other case":        {Type: "doc", Children: []*Node{paragraph(text("[^Note]"), reference("note")), definition("note")}},
		"reference-shaped text, full case mapping": {Type: "doc", Children: []*Node{paragraph(text("[^ß]"), reference("SS")), definition("SS")}},
		"reference-shaped text in its definition": {Type: "doc", Children: []*Node{paragraph(text("a"), reference("1")),
			{Type: "footnote_definition", Attrs: Attrs{"label": "1"}, Children: []*Node{paragraph(text("[^1]"))}}}},
		"asterisks before a reference":   {Type: "doc", Children: []*Node{paragraph(text("*-*"), reference("1")), definition("1")}},
		"underscores around a reference": {Type: "doc", Children: []*Node{paragraph(text("_a"), reference("1"), text("b_")), definition("1")}},
	} {
		t.Run(name, func(t *testing.T) {
			markdown := mustRender(t, doc)
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if !back.Equal(doc) {
				t.Fatalf("Render() = %q, which reads back as %q", markdown, mustRender(t, back))
			}
		})
	}
	undefined := &Node{Type: "doc", Children: []*Node{paragraph(text("[^2] stays"), reference("1")), definition("1")}}
	if got := mustRender(t, undefined); !strings.HasPrefix(got, "[^2] stays") {
		t.Fatalf("Render() = %q, want text naming an undefined label written as main writes it", got)
	}
}

// Both parsers keep the whitespace of a blank line in code inside a footnote definition, whose
// indentation a blank line does not take, so a blank code line there is written without the
// definition's indentation; elsewhere it keeps the prefix it has always been written with.
func TestRenderKeepsBlankCodeLinesInFootnoteDefinitions(t *testing.T) {
	paragraph := func(value string) *Node {
		return &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: value}}}
	}
	code := func(value string) *Node {
		return &Node{Type: "code_block", Attrs: Attrs{"language": nil}, Children: []*Node{{Type: "text", Text: value}}}
	}
	for name, block := range map[string]*Node{
		"code":           code("a\n\nb"),
		"a quote's code": {Type: "blockquote", Children: []*Node{code("a\n\nb")}},
	} {
		t.Run(name, func(t *testing.T) {
			doc := &Node{Type: "doc", Children: []*Node{
				{Type: "paragraph", Children: []*Node{{Type: "text", Text: "Ref"}, {Type: "footnote_reference", Attrs: Attrs{"label": "1"}}}},
				{Type: "footnote_definition", Attrs: Attrs{"label": "1"}, Children: []*Node{paragraph("x"), block}},
			}}
			markdown := mustRender(t, doc)
			back, err := Parse(markdown)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", markdown, err)
			}
			if !back.Equal(doc) {
				t.Fatalf("Render() = %q, which reads back as %q", markdown, mustRender(t, back))
			}
		})
	}
	quoted := &Node{Type: "doc", Children: []*Node{{Type: "blockquote", Children: []*Node{code("a\n\nb")}}}}
	if got, want := mustRender(t, quoted), "> ```\n> a\n> \n> b\n> ```\n"; got != want {
		t.Fatalf("Render() = %q, want the bytes main writes, %q", got, want)
	}
}

// An empty task item is written as an empty item, as the browser editor writes it: no form of
// its marker alone reads back as a task, and `- [ ]` reads back as an item holding the text `[ ]`.
func TestRenderWritesAnEmptyTaskItemAsAnEmptyItem(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{Type: "bullet_list", Attrs: Attrs{"spread": false}, Children: []*Node{
		{Type: "list_item", Attrs: Attrs{"label": "•", "listType": "bullet", "checked": false, "spread": false}, Children: []*Node{{Type: "paragraph"}}},
		{Type: "list_item", Attrs: Attrs{"label": "•", "listType": "bullet", "checked": true, "spread": false}, Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "b"}}}}},
	}}}}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "- \n- [x] b\n" {
		t.Fatalf("Render() = %q, want %q", markdown, "- \n- [x] b\n")
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if first := back.Children[0].Children[0]; len(first.Children[0].Children) != 0 {
		t.Fatalf("%q reads back with the first item holding %q", markdown, first.Children[0].Children[0].Text)
	}
}

// An image whose title holds a line feed, in a list item or a block quote, is stored today with
// the line feed as it is (the next line is the paragraph's lazy continuation), and reads back as
// held, so no write after INFRA-190 changes those bytes (R3): the title's line feed stays raw
// wherever the block holds it, not only in a top-level paragraph.
func TestRenderKeepsAStoredImageTitlesLineFeedInAListItemOrBlockQuote(t *testing.T) {
	for _, markdown := range []string{
		"- ![x](i.png \"a\nb\")\n",
		"1. ![x](i.png \"a\nb\")\n",
		"> ![x](i.png \"a\nb\")\n",
		"- > ![x](i.png \"a\nb\")\n",
	} {
		doc, err := ParseForWrite(markdown, nil)
		if err != nil {
			t.Errorf("ParseForWrite(%q) = %v, want the write taken", markdown, err)
			continue
		}
		got, err := Render(doc)
		if err != nil {
			t.Errorf("Render(ParseForWrite(%q)) = %v", markdown, err)
			continue
		}
		if got != markdown {
			t.Errorf("Render(ParseForWrite(%q)) = %q, want the stored bytes kept", markdown, got)
		}
	}
}
