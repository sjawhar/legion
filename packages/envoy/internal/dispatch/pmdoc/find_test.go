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

func TestMarkRangeSupportsCodeBlocksAndMultipleBlocks(t *testing.T) {
	doc, err := Parse("```\ncode\n```\n\none\n\ntwo\n")
	if err != nil {
		t.Fatal(err)
	}

	state := crdt.New()
	frag := state.GetXmlFragment("prosemirror")
	state.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	code, err := FindQuote(doc, "code", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	state.Transact(func(txn *crdt.Transaction) {
		err = MarkRange(txn, frag, code, Mark{Type: "proofComment", Attrs: Attrs{"id": "code", "by": "session:x"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	one, err := FindQuote(doc, "one", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := FindQuote(doc, "two", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	state.Transact(func(txn *crdt.Transaction) {
		err = MarkRange(txn, frag, Range{From: one.From, To: two.To}, Mark{Type: "dispatchAsk", Attrs: Attrs{"id": "both", "by": "session:x"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMark(got, "proofComment") || !hasMark(got, "dispatchAsk") {
		t.Fatal("marks were not applied across every selected text span")
	}
}
