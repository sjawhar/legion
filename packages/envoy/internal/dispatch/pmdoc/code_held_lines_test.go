package pmdoc

import (
	"testing"
)

// Three readings sjawhar/legion#2481 gave the lines a code block holds, which nothing locked: the
// reader's own suite stayed green with any of them taken out (#2481's review, "Not proven /
// risk"; measured again at #2507's head, where removing each left every test of this package
// passing). Each test below fails when its reading is taken out, so the next change to these
// lines is told.

// A fenced code block whose last line is the document's own, with no line feed and nothing past
// the fence's indentation, holds none of that line: the browser editor's parser takes as many
// columns off each line of the code as the opening fence stands past its container, so the line is
// nothing of the code (fenceEndsEmptyAttr, lines.go). Keeping it would store a trailing line feed
// a person never wrote and render a blank line into the block.
func TestAFencedBlockEndingAtTheDocumentsOwnWhitespaceLineHoldsNoneOfIt(t *testing.T) {
	doc, err := ParseForWrite("  ~~~\nx\n ", nil)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	code := doc.Children[0]
	if code.Type != "code_block" {
		t.Fatalf("first block is %s, want code_block", code.Type)
	}
	if text := code.Children[0].Text; text != "x" {
		t.Errorf("code holds %q, want %q", text, "x")
	}
	rendered, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "```\nx\n```\n"; rendered != want {
		t.Errorf("rendered %q, want %q", rendered, want)
	}
}

// The blank lines a typed block's indentation takes out of the fenced code it holds are the blank
// lines they were before the code held them, and the whitespace lines indented code holds are
// none of the item's either (the keptLast delete in blanksOutsideCode and sourceLines.asHeldBefore
// before sjawhar/legion#2507, sourceLines.blank and blanksOutsideCode now, source_lines.go). A
// blank line inside a typed block in a list item spaces the item to the browser editor's parser,
// so a line read as blank rather than as the code's would store a list a person did not write.
// Each document below holds its line in the code, and the item the typed block stands in is
// tight, as that parser reads it; the fenced form is first, then the indented one.
func TestCodesHeldLinesInATypedBlockSpaceNoItem(t *testing.T) {
	for _, test := range []struct{ name, markdown string }{
		{"a fenced block ending at the document's own whitespace line", "-\n\n- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  ```\n  x\n\t"},
		{"indented code holding a whitespace line", "-\n\n- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n      code\n      \n  :::\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, reader := range blockMarkdownReaders() {
				doc, err := reader.parse(test.markdown)
				if err != nil {
					t.Fatalf("%s: %v, want it read", reader.name, err)
				}
				list := doc.Children[0]
				item := list.Children[1]
				if spread, _ := item.Attrs["spread"].(bool); spread {
					t.Errorf("%s: the item holding the typed block reads spread, want it tight", reader.name)
				}
			}
		})
	}
}
