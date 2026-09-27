package docs

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// An accept pads block content over a table only when the match covers exactly the textblocks the
// browser editor's accept replaces. The editor reads each textblock one position short of its end,
// so it skips a one-character paragraph and replaces the two cells after it, keeping the paragraph
// that Splice would replace.
func TestPadsLikeTheBrowserOnlyOverTheTextblocksTheEditorReplaces(t *testing.T) {
	for _, test := range []struct {
		name, markdown, quote string
		want                  bool
	}{
		{"a whole paragraph into a table's first cell", "abc\n\n| Next | b |\n| --- | --- |\n| c | d |\n", "abc Next", true},
		{"a whole heading into a table's first cell", "# abc\n\n| Next | b |\n| --- | --- |\n| c | d |\n", "abc Next", true},
		{"a whole list item into a table's first cell", "- abc\n\n| Next | b |\n| --- | --- |\n| c | d |\n", "abc Next", false},
		{"three whole textblocks", "abc\n\nMid\n\n| Next | b |\n| --- | --- |\n| c | d |\n", "abc Mid Next", false},
		{"a one-character paragraph through two cells ending in images", "a\n\n| b![i](x.png) |\n| --- |\n| c![j](y.png) |\n| ee |\n", "a b c", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := pmdoc.Parse(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			quote, err := pmdoc.FindQuote(doc, test.quote, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			at, _ := pmdoc.ContainingTextblock(doc, quote.From)
			last, _ := pmdoc.ContainingTextblock(doc, quote.To)
			match := pmdoc.Range{From: at.Content.From, To: last.Content.To}
			if got := padsLikeTheBrowser(doc, match, at, false); got != test.want {
				t.Fatalf("padsLikeTheBrowser over %v = %v, want %v", match, got, test.want)
			}
		})
	}
}
