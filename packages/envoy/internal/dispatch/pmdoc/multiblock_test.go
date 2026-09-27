package pmdoc

import "testing"

// The browser editor's accept takes block content across two textblocks only when the quote lines
// up with them, reading each textblock's text one position short of its end, so a quote ending two
// characters before the last textblock's end is still aligned.
func TestMultiblockAlignedReadsTextblocksAsTheEditorDoes(t *testing.T) {
	for _, test := range []struct {
		name, markdown, quote string
		want                  bool
	}{
		{"two whole paragraphs", "abc\n\nNext\n", "abc Next", true},
		{"a whole heading into a table's first cell", "# abc\n\n| Next | b |\n| --- | --- |\n| c | d |\n", "abc Next", true},
		{"from part of a paragraph", "Intro abc\n\nNext\n", "abc Next", false},
		{"one character into the first textblock", "Xabc\n\nNext\n", "abc Next", true},
		{"two characters into the first textblock", "XYabc\n\nNext\n", "abc Next", false},
		{"two short of the last textblock's end", "| H | I |\n| --- | --- |\n| c | aa |\n| bb e | f |\n", "aa bb", true},
		{"three short of the last textblock's end", "| H | I |\n| --- | --- |\n| c | aa |\n| bb ee | f |\n", "aa bb", false},
		{"across four textblocks", "abc\n\n| H | I |\n| --- | --- |\n| Next | d |\n", "abc H I Next", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := Parse(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			r, err := FindQuote(doc, test.quote, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := MultiblockAligned(doc, r); got != test.want {
				t.Fatalf("MultiblockAligned(%q) = %v, want %v", test.quote, got, test.want)
			}
		})
	}
}
