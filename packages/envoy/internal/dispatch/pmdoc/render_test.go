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
			got, _, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			if plain(got) != plain(fx.RenderedByMilkdown) {
				t.Fatalf("text content differs\n got: %q\nwant: %q\nplain got: %q\nplain want: %q", got, fx.RenderedByMilkdown, plain(got), plain(fx.RenderedByMilkdown))
			}
		})
	}
}

func TestPositionMapRoundTripsTextRuns(t *testing.T) {
	doc, err := FromJSON([]byte(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Some "},{"type":"text","text":"bold","marks":[{"type":"strong"}]},{"type":"text","text":" 😀 end"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	md, pm, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if md != "Some **bold** 😀 end\n" {
		t.Fatalf("md = %q", md)
	}
	if got := pm.ToPM(7); got != 6 {
		t.Fatalf("ToPM(7) = %d, want 6", got)
	}
	if got := pm.ToPM(len16("Some **bold** 😀")); got != 1+len16("Some bold 😀") {
		t.Fatalf("emoji offset mismatch: %d", got)
	}
	if got := pm.ToPM(5); got != 6 {
		t.Fatalf("ToPM(5) = %d, want 6", got)
	}
	if md0, ok := pm.ToMd(6); !ok || md0 != 7 {
		t.Fatalf("ToMd(6) = %d,%v", md0, ok)
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
	markdown, _, err := Render(parsed)
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
	got, _, err := Render(doc)
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
	got, _, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "| head\\|er |\n| :--- |\n| cel\\|l |\n" {
		t.Fatalf("Render(table) = %q", got)
	}
}

func TestRenderTablePositionMapIncludesCellParagraph(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{
		Type: "table",
		Children: []*Node{
			{Type: "table_header_row", Children: []*Node{{Type: "table_header", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "header"}}}}}}},
			{Type: "table_row", Children: []*Node{{Type: "table_cell", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "cell"}}}}}}},
		},
	}}}
	markdown, positions, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "| header |\n| :--- |\n| cell |\n" {
		t.Fatalf("Render(table) = %q", markdown)
	}
	if got := positions.ToPM(len16("| ")); got != 4 {
		t.Fatalf("header PM position = %d, want 4", got)
	}
	if got := positions.ToPM(len16("| header |\n| :--- |\n| ")); got != 16 {
		t.Fatalf("cell PM position = %d, want 16", got)
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
	markdown, positions, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "| a\\|b |\n| :--- |\n| body |\n" {
		t.Fatalf("Render(table) = %q", markdown)
	}
	if got := positions.ToPM(len16("| a\\|")); got != 6 {
		t.Fatalf("ToPM(| a\\|) = %d, want 6", got)
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
	markdown, _, err := Render(doc)
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
	markdown, _, err := Render(doc)
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
	if !back.Equal(doc) {
		t.Fatalf("Parse(Render()) = %#v, want %#v", back, doc)
	}
}

func TestRenderLeavesOrdinaryProseUnescaped(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "text",
		Text: "It's ordinary: prose - with [brackets], | pipes, and < 2.",
	}}}}}
	markdown, _, err := Render(doc)
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
	markdown, _, err := Render(doc)
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
	markdown, _, err := Render(doc)
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
	markdown, _, err := Render(doc)
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
			markdown, _, err := Render(doc)
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
	markdown, _, err := Render(doc)
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
	markdown, _, err := Render(doc)
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
	markdown, _, err := Render(doc)
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
	markdown, _, err := Render(doc)
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
	markdown, _, err := Render(doc)
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

func TestRenderSpanPositionsMatchWalkerTextPositions(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			doc, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}
			_, positions, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			var textRanges []Range
			walk(doc, func(node *Node, _ []int, pos, end int) bool {
				if node.Type == "text" {
					textRanges = append(textRanges, Range{From: pos, To: end})
				}
				return true
			})
			span := 0
			for _, textRange := range textRanges {
				position := textRange.From
				for position < textRange.To {
					if span == len(positions.spans) {
						t.Fatalf("missing span at PM position %d", position)
					}
					got := positions.spans[span]
					if got.PmFrom != position {
						t.Fatalf("span %d starts at PM position %d, want %d", span, got.PmFrom, position)
					}
					position += got.MdTo - got.MdFrom
					if position > textRange.To {
						t.Fatalf("span %d crosses walker text range %v", span, textRange)
					}
					span++
				}
			}
			if span != len(positions.spans) {
				t.Fatalf("got %d unmatched render spans", len(positions.spans)-span)
			}
		})
	}
}
