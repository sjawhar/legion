package pmdoc

import "testing"

// A typed block no fence closes, in a list item of a quote, whose last lines hold the quote's
// marker, a space and a tab: whether the browser editor's parser spreads the inner list turns on
// where that tab's column lands, so it answers tight for two blank-looking lines with the item's
// content four spaces in and spread for the same two lines with the content two spaces in. This
// reader must answer what that parser answers for each; it may not answer one thing for the family.
//
// Every expectation is measured against that parser (packages/proof-editor's createHeadlessProof
// with schema/blocks.json). Before this pull request this reader answered tight for the first two
// and so stores the third as a document the browser draws otherwise; a reader that answers spread
// for the first stores one the browser draws otherwise in its place (#2507).
func TestATabFillInsideAQuotedItemFollowsTheBrowser(t *testing.T) {
	const callout = ":::callout{#c1 kind=\"note\" title=\"T\"}"
	for _, test := range []struct {
		name     string
		markdown string
		spread   bool
	}{
		{
			name:     "the marker tight against the item, its content four spaces in",
			markdown: ">- a\n>    " + callout + "\n>    - x\n> \t\n> \t\n",
			spread:   false,
		},
		{
			name:     "the same, a space after the marker",
			markdown: "> - a\n>     " + callout + "\n>     - x\n> \t\n> \t\n",
			spread:   false,
		},
		{
			name:     "the same, the item's content two spaces in",
			markdown: ">- a\n>  " + callout + "\n>  - x\n> \t\n> \t\n",
			spread:   true,
		},
		{
			name:     "the first, with one such line rather than two",
			markdown: ">- a\n>    " + callout + "\n>    - x\n> \t\n",
			spread:   false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := ParseForWrite(test.markdown, nil)
			if err != nil {
				t.Fatalf("ParseForWrite(%q): %v", test.markdown, err)
			}
			list := innerTypedListOf(doc)
			if list == nil {
				t.Fatalf("ParseForWrite(%q) holds no list inside its typed block", test.markdown)
			}
			if spread, _ := list.Attrs["spread"].(bool); spread != test.spread {
				t.Errorf("ParseForWrite(%q) reads the typed block's list spread=%t, want %t, as the browser editor's parser reads it",
					test.markdown, spread, test.spread)
			}
		})
	}
}
