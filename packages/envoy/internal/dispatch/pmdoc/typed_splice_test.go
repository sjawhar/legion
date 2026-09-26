package pmdoc

import (
	"slices"
	"testing"
)

// A replacement spliced into a typed block is fitted inside it, as ProseMirror fits one into a
// callout (the engine oracle's callout-paragraph-and-code case). The fit never goes on to the
// block's parent, where the only fit replaces the block and everything it holds: an ask given a
// code block keeps its id and its place, holding what it cannot parse, for the document service's
// ask-block check to refuse. ProseMirror would instead split the ask, closing it after the paragraph
// and putting the code block after it, which no oracle case pins because Dispatch refuses the write.
func TestSpliceKeepsAnAskItsReplacementDoesNotFit(t *testing.T) {
	doc, err := Parse("Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	r, err := FindQuote(doc, "Which one?", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("Which?\n\n```\ncode\n```\n")
	if err != nil {
		t.Fatal(err)
	}

	got, err := Splice(doc, r, with)

	if err != nil {
		t.Fatalf("Splice() error = %v", err)
	}
	ask := got.Children[1]
	var children []string
	for _, child := range ask.Children {
		children = append(children, child.Type)
	}
	if ask.Type != "ask" || ask.Attrs[BlockIDAttr] != "a1" || !slices.Equal(children, []string{"paragraph", "code_block"}) {
		t.Fatalf("Splice() second block = %s %v holding %v, want ask a1 holding the paragraph and the code block",
			ask.Type, ask.Attrs, children)
	}
}
