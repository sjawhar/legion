package pmdoc

import (
	"errors"
	"strings"
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
// lines they were before the code held them, so a document holding one in a list item beside an
// empty list item is refused as it was before sjawhar/legion#2481 (the keptLast delete in
// blanksOutsideCode, source_lines.go): the browser editor spaces the item by that line and this
// reader does not, so storing it would draw a list a person did not write. Every reader of caller
// markdown refuses it.
func TestATypedBlocksBlankLineInAnItemIsRefusedBesideAnEmptyItem(t *testing.T) {
	markdown := "-\n\n- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  ```\n  x\n\t"
	const refusal = "a typed block holding a blank line inside a list item"
	for _, reader := range blockMarkdownReaders() {
		if _, err := reader.parse(markdown); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
			t.Errorf("%s: %v, want ErrSchema saying %q", reader.name, err, refusal)
		}
	}
}

// The whitespace lines indented code holds are the blank lines they were before it held them, so
// a typed block holding one in a list item beside an empty list item is refused as it was before
// sjawhar/legion#2481 (sourceLines.asHeldBefore, source_lines.go). Those lines are none of the
// code's to blanksOutsideCode, which reads a code block only up to the line it began holding at,
// but sourceLines.blank passes over them for every other reader of the source; asHeldBefore is
// what puts them back for this one refusal. Without it the reader stores the document, and the
// browser editor spaces the list by that line where the reader does not, so the person sees a
// list they did not write. The fenced form of the same shape is the test above; this is the
// indented form, which only asHeldBefore decides.
func TestIndentedCodesHeldLineInATypedBlockIsRefusedBesideAnEmptyItem(t *testing.T) {
	markdown := "-\n\n- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n      code\n      \n  :::\n"
	const refusal = "a typed block holding a blank line inside a list item"
	for _, reader := range blockMarkdownReaders() {
		if _, err := reader.parse(markdown); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
			t.Errorf("%s: %v, want ErrSchema saying %q", reader.name, err, refusal)
		}
	}
}
