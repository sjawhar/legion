package pmdoc

import (
	"errors"
	"slices"
	"testing"
)

// The flattened text a quote is matched against is built from a count of what it will hold
// (flattenedTextSize) and then filled. The count sets capacity alone, so these cases hold the
// fill: a U+0000, which stands for three bytes of U+FFFD but one UTF-16 unit; a character above
// U+FFFF, which is one rune but two units and so two positions; two text nodes that touch, between
// which no gap is written, against two a block boundary separates, between which one is; and a
// text carrying marks, which the markdown matcher reads through. Each pins the range the text
// answers with through FindQuote, so they hold whatever the flattening does inside.
func TestFlattenedTextAnswersQuoteRanges(t *testing.T) {
	text := func(value string, marks ...Mark) *Node { return &Node{Type: "text", Text: value, Marks: marks} }
	paragraph := func(children ...*Node) *Node { return &Node{Type: "paragraph", Children: children} }
	document := func(blocks ...*Node) *Node { return &Node{Type: "doc", Children: blocks} }

	for _, test := range []struct {
		name           string
		doc            *Node
		quote          string
		want           Range
		wantCandidates []Range
		wantErr        error
	}{
		{
			name:  "nul reads as the replacement character",
			doc:   document(paragraph(text("a\x00b"))),
			quote: "a\uFFFDb",
			want:  Range{From: 1, To: 4},
		},
		{
			name:  "character above U+FFFF is two positions",
			doc:   document(paragraph(text("hi \U0001F600 there"))),
			quote: "\U0001F600 there",
			want:  Range{From: 4, To: 12},
		},
		{
			name:  "adjacent text nodes have no gap between them",
			doc:   document(paragraph(text("plain"), text("bold", Mark{Type: "strong"}))),
			quote: "inbo",
			want:  Range{From: 4, To: 8},
		},
		{
			name:  "a block boundary is one space",
			doc:   document(paragraph(text("alpha")), paragraph(text("beta"))),
			quote: "alpha beta",
			want:  Range{From: 1, To: 12},
		},
		{
			name:  "inline code is matched through its markdown",
			doc:   document(paragraph(text("Use "), text("config", Mark{Type: "inlineCode"}), text(" now"))),
			quote: "Use `config`",
			want:  Range{From: 1, To: 11},
		},
		{
			name:  "strong is matched through its markdown",
			doc:   document(paragraph(text("A "), text("bold", Mark{Type: "strong"}), text(" claim"))),
			quote: "**bold**",
			want:  Range{From: 3, To: 7},
		},
		{
			name:    "a document holding no text matches nothing",
			doc:     document(paragraph()),
			quote:   "anything",
			wantErr: ErrTargetNotFound,
		},
		{
			name:           "a repeated quote names every place it matched",
			doc:            document(paragraph(text("repeat one")), paragraph(text("repeat two"))),
			quote:          "repeat",
			wantCandidates: []Range{{From: 1, To: 7}, {From: 13, To: 19}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := FindQuote(test.doc, test.quote, nil, nil)
			switch {
			case test.wantErr != nil:
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("FindQuote(%q) = %v, %v, want %v", test.quote, got, err, test.wantErr)
				}
			case test.wantCandidates != nil:
				var ambiguous *ErrTargetAmbiguous
				if !errors.As(err, &ambiguous) {
					t.Fatalf("FindQuote(%q) = %v, %v, want an ambiguity", test.quote, got, err)
				}
				ranges := make([]Range, 0, len(ambiguous.Candidates))
				for _, candidate := range ambiguous.Candidates {
					ranges = append(ranges, candidate.Range)
				}
				if !slices.Equal(ranges, test.wantCandidates) {
					t.Fatalf("FindQuote(%q) candidates = %v, want %v", test.quote, ranges, test.wantCandidates)
				}
			default:
				if err != nil {
					t.Fatalf("FindQuote(%q) = %v", test.quote, err)
				}
				if got != test.want {
					t.Fatalf("FindQuote(%q) = %v, want %v", test.quote, got, test.want)
				}
			}
		})
	}
}
