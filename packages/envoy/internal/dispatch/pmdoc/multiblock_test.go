package pmdoc

import (
	"strings"
	"testing"
)

// The browser editor's accept takes block content across exactly two textblocks, reading each
// one position short of its end, and replaces both whole: a one-character textblock at the
// range's edge is not one of them.
func TestMultiblockRangeIsTheTextblocksTheEditorReads(t *testing.T) {
	long := strings.Repeat("L", 250)
	for _, test := range []struct {
		name, markdown, quote string
		// ok is whether the editor reads two textblocks; whole, whether they are the quote's
		// textblocks, from the first one's start to the last one's end.
		ok, whole bool
	}{
		{"two whole paragraphs", "abc\n\nNext\n", "abc Next", true, true},
		{"a whole heading into a table's first cell", "# abc\n\n| Next | b |\n| --- | --- |\n| c | d |\n", "abc Next", true, true},
		{"from part of a paragraph", "Intro abc\n\nNext\n", "abc Next", true, true},
		{"across four textblocks", "abc\n\n| H | I |\n| --- | --- |\n| Next | d |\n", "abc H I Next", false, false},
		{"a one-character paragraph and two cells", "a\n\n| b![i](x.png) |\n| --- |\n| c![j](y.png) |\n", "a b c", true, false},
		{"more than 240 characters", long + "\n\nNext\n", long + " Next", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := Parse(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			quote, err := FindQuote(doc, test.quote, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			first, _ := ContainingTextblock(doc, quote.From)
			last, _ := ContainingTextblock(doc, quote.To)
			got, ok := MultiblockRange(doc, quote)
			if ok != test.ok {
				t.Fatalf("MultiblockRange(%q) reports %v, want %v", test.quote, ok, test.ok)
			}
			if whole := got == (Range{From: first.Content.From, To: last.Content.To}); ok && whole != test.whole {
				t.Fatalf("MultiblockRange(%q) = %v, the quote's textblocks run %v to %v", test.quote, got, first.Content, last.Content)
			}
		})
	}
}
