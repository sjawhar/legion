package pmdoc

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

var orderedMarker = regexp.MustCompile(`(?m)^\d+\.\s*`)
var spanTag = regexp.MustCompile(`</?span(?:\s[^>]*)?>`)
var linkDestination = regexp.MustCompile(`\]\([^)]*\)`)

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
	if got != "| head\\|er |\n| :--- |\n| cel\\|l |\n" {
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
	if markdown != "| header |\n| :--- |\n| cell |\n" {
		t.Fatalf("Render(table) = %q", markdown)
	}
}

func plain(markdown string) string {
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
	if markdown != "1\\. not a list\n2\\) also not a list\n" {
		t.Fatalf("Render() = %q", markdown)
	}
	back, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	// The newline inside the text node renders as a soft break, which parses back as a space.
	want := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "text",
		Text: "1. not a list 2) also not a list",
	}}}}}
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
			linkHref: "https://example.test/one\\|two",
		},
		{
			name:     "link title",
			markdown: "| header |\n| :--- |\n| [label](https://example.test \"one\\|two\") |\n",
		},
		{
			name:     "autolink-shaped link",
			markdown: "| header |\n| :--- |\n| [https://example.test/one\\|two](https://example.test/one\\|two) |\n",
			linkHref: "https://example.test/one\\|two",
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
			name:          "footnote label",
			markdown:      "| header |\n| :--- |\n| [^one\\|two] |\n\n[^one\\|two]: note\n",
			footnoteLabel: "one\\|two",
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
			name:     "distinct footnote labels",
			markdown: "first[^a&amp;b], second[^a&b]\n\n[^a&amp;b]: one\n\n[^a&b]: two\n",
		},
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
	if got := tableCellLinkHref(doc); got != "https://example.test/one"+strings.Repeat("\\", 3)+"|two" {
		t.Fatalf("table link href = %q, want three source backslashes before the pipe", got)
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
	const want = "| header |\n| :--- |\n| <span data-label=\"one&#124;two\"> |\n"
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
	want := "| header |\n| :--- |\n| [https://example.test/one" + strings.Repeat("\\", 3) + "|two](https://example.test/one\\|two) |\n"
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
