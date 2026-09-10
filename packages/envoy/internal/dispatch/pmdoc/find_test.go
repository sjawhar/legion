package pmdoc

import (
	"errors"
	"testing"

	"github.com/reearth/ygo/crdt"
)

func TestFindQuoteDisambiguatesByOccurrenceAndNearest(t *testing.T) {
	doc, err := Parse("brown X brown Y brown\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FindQuote(doc, "brown", nil, nil); !errors.As(err, new(*ErrTargetAmbiguous)) {
		t.Fatalf("want ambiguity, got %v", err)
	}

	one := 1
	rangeAtOccurrence, err := FindQuote(doc, "brown", &one, nil)
	if err != nil || rangeAtOccurrence.From != 1+len16("brown X ") {
		t.Fatalf("occurrence 1: %v %v", rangeAtOccurrence, err)
	}

	near := 1 + len16("brown X brown Y b")
	rangeNearHint, err := FindQuote(doc, "brown", nil, &near)
	if err != nil || rangeNearHint.From != 1+len16("brown X brown Y ") {
		t.Fatalf("nearest: %v %v", rangeNearHint, err)
	}
	if _, err := FindQuote(doc, "purple", nil, nil); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("want not found, got %v", err)
	}

	normalized, err := Parse("the quick\nbrown fox\n")
	if err != nil {
		t.Fatal(err)
	}
	normalizedRange, err := FindQuote(normalized, "quick brown", nil, nil)
	if err != nil || normalizedRange != (Range{From: 5, To: 16}) {
		t.Fatalf("normalized whitespace range = %v, %v", normalizedRange, err)
	}
}

func TestMarkRangeThenFindMark(t *testing.T) {
	doc := crdt.New()
	frag := doc.GetXmlFragment("prosemirror")
	want, err := Parse("The quick brown fox.\n")
	if err != nil {
		t.Fatal(err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, want)
	})
	if err != nil {
		t.Fatal(err)
	}

	rangeBrown, err := FindQuote(want, "brown", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = MarkRange(txn, frag, rangeBrown, Mark{Type: "dispatchAsk", Attrs: Attrs{"id": "a1", "by": "session:x"}})
	})
	if err != nil {
		t.Fatal(err)
	}

	tree, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	got, quote, ok := FindMark(tree, "dispatchAsk", "a1")
	if !ok || quote != "brown" || got != rangeBrown {
		t.Fatalf("FindMark = %v %q %v", got, quote, ok)
	}

	doc.Transact(func(txn *crdt.Transaction) {
		err = Unmark(txn, frag, "dispatchAsk", "a1")
	})
	if err != nil {
		t.Fatal(err)
	}
	tree, err = Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := FindMark(tree, "dispatchAsk", "a1"); ok {
		t.Fatal("mark still present after Unmark")
	}
	markdown, _, err := Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "The quick brown fox.\n" {
		t.Fatalf("text changed by marking: %q", markdown)
	}
}

func TestSpliceInlineKeepsNeighbourMarks(t *testing.T) {
	doc, err := Parse("Keep **this** and change that.\n")
	if err != nil {
		t.Fatal(err)
	}
	rangeChange, err := FindQuote(doc, "change that", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("*alter* those")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Splice(doc, rangeChange, with)
	if err != nil {
		t.Fatal(err)
	}
	markdown, _, err := Render(out)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "Keep **this** and *alter* those.\n" {
		t.Fatalf("md = %q", markdown)
	}
}

func TestSpliceAcrossBlocksReplacesBlocks(t *testing.T) {
	doc, err := Parse("# Title\n\nOne.\n\nTwo.\n\nThree.\n")
	if err != nil {
		t.Fatal(err)
	}
	from, err := FindQuote(doc, "One.", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	to, err := FindQuote(doc, "Two.", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("- a\n- b\n")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Splice(doc, Range{From: from.From, To: to.To}, with)
	if err != nil {
		t.Fatal(err)
	}
	markdown, _, err := Render(out)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "# Title\n\n- a\n- b\n\nThree.\n" {
		t.Fatalf("md = %q", markdown)
	}
}
