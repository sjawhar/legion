package pmdoc

import (
	"testing"
)

// A document Parse reads as the browser editor's engine reads it is stored as its rendering, so
// the writer must spell every such tree in bytes that read back as the tree, in Go and in the
// engine alike (dispatch://INFRA-535). Each row is a document of the
// writer-spells-a-flanked-letter-after-a-backslash corpus document taken alone - a letter right
// after a literal backslash, beside an emphasis run - or the same shape after two backslashes, in a
// heading or in a table cell, and the bytes the writer stores it as. For each row Parse reads it as
// the engine does, ParseForWrite stores it, Render's bytes read back as Parse's tree in both
// parsers, and those bytes are want.
func TestTheWriterSpellsEveryTreeItReadsAsTheEngine(t *testing.T) {
	rows := []struct {
		name, markdown, want string
	}{
		{"escaped tilde then a backslash before a letter", "p \\~\\a*~* q\n", ""},
		{"a backslash before a letter then an escaped asterisk", "p \\a*~*\\* q\n", ""},
		{"escaped asterisk then a backslash before a letter", "p \\*\\a*~a* q\n", ""},
		{"a backslash before a letter beside an underscore", "p \\a*_a* q\n", ""},
		{"a footnote document", "Plain a_~`c`~~www.a.com/~x\\~~\\~ end.\n\n[^a_b]: n\n", ""},
		{"a link closer then escaped tildes", "**](u)\\~\\\\~***b***\n", ""},
		{"a table of www links", "| h |\n| --- |\n| *www.a.com/~x\\~~www.a.com/~xb**~ |\n", ""},
		{"escaped tilde then two backslashes before a letter", "p \\~\\\\a*~* q\n", ""},
		{"in a heading", "# p \\~\\a*~* q\n", ""},
		{"in a table cell", "| h |\n| --- |\n| p \\~\\a*~* q |\n", ""},
	}
	markdowns := make([]string, len(rows))
	for index, row := range rows {
		markdowns[index] = row.markdown
	}
	readings := engineReadings(t, markdowns)
	docs := make([]*Node, len(rows))
	parseErrs := make([]error, len(rows))
	rendered := make([]string, len(rows))
	renderErrs := make([]error, len(rows))
	for index, row := range rows {
		docs[index], parseErrs[index] = Parse(row.markdown)
		if parseErrs[index] == nil {
			rendered[index], renderErrs[index] = Render(docs[index])
		}
	}
	backReadings := engineReadings(t, rendered)
	for index, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			doc, rendering := docs[index], rendered[index]
			if parseErrs[index] != nil {
				t.Fatalf("Parse(%q) = %v", row.markdown, parseErrs[index])
			}
			readsAsEngine := t.Run("1 Parse reads it as the engine", func(t *testing.T) {
				if readings[index] == nil {
					t.Fatalf("the engine does not read %q", row.markdown)
				}
				if !doc.Equal(readings[index]) {
					t.Errorf("%q: Go reads\n\t%s\nthe engine\n\t%s", row.markdown, readingSummary(doc), readingSummary(readings[index]))
				}
			})
			stored := t.Run("2 ParseForWrite stores it", func(t *testing.T) {
				if _, err := ParseForWrite(row.markdown, nil); err != nil {
					t.Errorf("ParseForWrite(%q) = %v", row.markdown, err)
				}
			})
			readsBack := t.Run("3 Render reads back in both parsers", func(t *testing.T) {
				if renderErrs[index] != nil {
					t.Fatalf("Render(Parse(%q)) = %v", row.markdown, renderErrs[index])
				}
				if back, err := Parse(rendering); err != nil || !back.Equal(doc) {
					t.Errorf("Go reads the rendering %q of %q back as\n\t%s (%v)\nnot\n\t%s", rendering, row.markdown, readingSummary(back), err, readingSummary(doc))
				}
				if engine := backReadings[index]; engine == nil || !engine.Equal(doc) {
					t.Errorf("the engine reads the rendering %q of %q back as\n\t%s\nnot\n\t%s", rendering, row.markdown, readingSummary(engine), readingSummary(doc))
				}
			})
			t.Run("4 Render writes want", func(t *testing.T) {
				if renderErrs[index] != nil {
					t.Fatalf("Render(Parse(%q)) = %v", row.markdown, renderErrs[index])
				}
				if row.want == "" {
					if readsAsEngine && stored && readsBack {
						t.Fatalf("want not yet recorded: Render wrote %q", rendering)
					}
					t.Fatalf("want not yet recorded, and Render wrote %q, which steps 1-3 refuse", rendering)
				}
				if rendering != row.want {
					t.Errorf("Render(Parse(%q)) = %q, want %q", row.markdown, rendering, row.want)
				}
			})
		})
	}
}
