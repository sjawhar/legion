package pmdoc

import (
	"strings"
	"testing"
)

// nulPlaces is one document for each place a caller's markdown can carry a U+0000: text, a code
// span, a heading, a fence's info string and its body, a link's href, a typed block's title
// attribute, the character alone, indented code and a table cell. Each holds exactly one.
var nulPlaces = map[string]string{
	"text":                  "a\x00b\n",
	"a code span":           "`a\x00b`\n",
	"a heading":             "# a\x00b\n",
	"a fence's info string": "```g\x00o\nx\n```\n",
	"a fence's body":        "```go\na\x00b\n```\n",
	"a link's href":         "[l](https://example.com/a\x00b)\n",
	"a typed block's title": ":::callout{#c1 kind=\"note\" title=\"t\x00t\"}\ninside\n:::\n",
	"the character alone":   "\x00",
	"indented code":         "    a\x00b\n",
	"a table cell":          "| a\x00b |\n| --- |\n| c |\n",
}

// Every reader of a caller's markdown reads a U+0000 as U+FFFD, as CommonMark requires of a reader
// and the browser editor's parser does, wherever the character stands (nulPlaces): goldmark keeps
// it as it is, so before this a write holding one was accepted into a tree holding a character the
// markdown it is stored as does not carry, since the rendering writes U+FFFD (withNulsWritten).
// The tree a read served and the bytes the version carried disagreed, and the write that left them
// so was refused nowhere. The readers below share one entry (parseUnstamped), which ParseRendering
// reaches as well, and each tree's own rendering is written back with the replacement in it.
func TestEveryReaderReadsNulAsReplacement(t *testing.T) {
	for place, markdown := range nulPlaces {
		for _, reader := range blockMarkdownReaders() {
			t.Run(place+"/"+reader.name, func(t *testing.T) {
				doc, err := reader.parse(markdown)
				if err != nil {
					t.Fatalf("%s of %q: %v", reader.name, markdown, err)
				}
				assertNulRead(t, doc, markdown)
			})
		}
	}
}

// ParseInline reads the character the same way, for the places a textblock's inline markdown can
// hold it: a leading heading marker, a fence or a table row is text there, and no block opens, so
// the other places of nulPlaces are no inline markdown.
func TestParseInlineReadsNulAsReplacement(t *testing.T) {
	for _, place := range []string{"text", "a code span", "a link's href", "the character alone"} {
		markdown := nulPlaces[place]
		t.Run(place, func(t *testing.T) {
			inline, err := ParseInline(markdown, NewWriteBudget())
			if err != nil {
				t.Fatalf("ParseInline(%q): %v", markdown, err)
			}
			assertNulRead(t, &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: inline}}}, markdown)
		})
	}
}

// assertNulRead reports whether doc, read from markdown holding one U+0000, holds the U+FFFD it is
// read as and no U+0000 of its own, in its text or in an attribute, and whether its rendering
// carries the replacement too, so that the tree and the markdown it is stored as agree.
func assertNulRead(t *testing.T, doc *Node, markdown string) {
	t.Helper()
	data, err := doc.JSON()
	if err != nil {
		t.Fatalf("JSON of the tree read from %q: %v", markdown, err)
	}
	tree := string(data)
	if strings.Contains(tree, `\u0000`) {
		t.Errorf("the tree read from %q is %s, want its U+0000 read as U+FFFD", markdown, tree)
	}
	if count := strings.Count(tree, "\uFFFD"); count != 1 {
		t.Errorf("the tree read from %q holds %d U+FFFD, want the one its U+0000 is read as: %s", markdown, count, tree)
	}
	rendering := mustRender(t, doc)
	if strings.IndexByte(rendering, 0) >= 0 || !strings.Contains(rendering, "\uFFFD") {
		t.Errorf("Render of the tree read from %q = %q, want the U+FFFD written as it is", markdown, rendering)
	}
}
