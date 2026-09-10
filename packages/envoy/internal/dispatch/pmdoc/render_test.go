package pmdoc

import (
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
				t.Fatalf("text content differs\n got: %q\nwant: %q", got, fx.RenderedByMilkdown)
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
	markdown = orderedMarker.ReplaceAllString(markdown, "")
	markdown = spanTag.ReplaceAllString(markdown, "")
	markdown = linkDestination.ReplaceAllString(markdown, "]")
	markdown = strings.NewReplacer("*", "", "_", "", "~", "", "`", "", "#", "", ">", "", "<", "", "-", "", "|", "", "[", "", "]", "", "(", "", ")", "").Replace(markdown)
	return strings.Join(strings.Fields(markdown), " ")
}
