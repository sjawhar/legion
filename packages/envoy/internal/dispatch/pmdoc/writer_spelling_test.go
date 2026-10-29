package pmdoc

import (
	"testing"
)

// A document Parse reads as the browser editor's engine reads it is stored as its rendering, so
// the writer must spell every such tree in bytes that read back as the tree, in Go and in the
// engine alike (sjawhar/legion#2458). Each row is a document of the
// writer-spells-a-flanked-letter-after-a-backslash corpus document taken alone - a letter right
// after a literal backslash, beside an emphasis run - or the same shape after two backslashes, in a
// heading or in a table cell, or a document of another shape the writer refused to store at some
// revision of this change, kept as a guard of the spelling it now takes, and the bytes the writer
// stores it as. For each row Parse reads it as the engine does, ParseForWrite
// stores it, Render's bytes read back as Parse's tree in both parsers, and those bytes are want.
func TestTheWriterSpellsEveryTreeItReadsAsTheEngine(t *testing.T) {
	rows := []struct {
		name, markdown, want string
	}{
		{"escaped tilde then a backslash before a letter", "p \\~\\a*~* q\n", "p &#126;\\\\&#97;*&#126;* q\n"},
		{"a backslash before a letter then an escaped asterisk", "p \\a*~*\\* q\n", "p \\\\&#97;*&#126;*\\* q\n"},
		{"escaped asterisk then a backslash before a letter", "p \\*\\a*~a* q\n", "p \\*\\\\&#97;*&#126;a* q\n"},
		{"a backslash before a letter beside an underscore", "p \\a*_a* q\n", "p \\\\&#97;*\\_a* q\n"},
		{"a footnote document", "Plain a_~`c`~~www.a.com/~x\\~~\\~ end.\n\n[^a_b]: n\n", "Plain a\\_~~`c`&#126;&#126;~~[~~www.a.com/&#126;x\\\\&#126;&#126;\\\\~~](http://www.a.com/~x\\\\~~\\\\) end.\n\n[^a_b]: n\n"},
		{"a link closer then escaped tildes", "**](u)\\~\\\\~***b***\n", "**]\\(u)\\~\\\\\\~***b***\n"},
		{"a table of www links", "| h |\n| --- |\n| *www.a.com/~x\\~~www.a.com/~xb**~ |\n", "| h |\n| --- |\n| \\*www.a.com/~x\\~~www.a.com/~xb**~ |\n"},
		{"escaped tilde then two backslashes before a letter", "p \\~\\\\a*~* q\n", "p &#126;\\\\&#97;*&#126;* q\n"},
		{"in a heading", "# p \\~\\a*~* q\n", "# p &#126;\\\\&#97;*&#126;* q\n"},
		{"in a table cell", "| h |\n| --- |\n| p \\~\\a*~* q |\n", "| h |\n| --- |\n| p &#126;\\\\&#97;*&#126;* q |\n"},
		{"an escaped bang before a link", "[\\![ ](u)\n", "[\\![ ](u)\n"},
		{"a code block whose language holds a backtick", "- ~~~~`c`\n", "- ~~~`c`\n  \n  ~~~\n"},
		{"an asterisk after italic code", "_`c`_*![\n", "*`c`*\\*![\n"},
		{"an asterisk before italic code in a heading", "# _ \\**`c`*`c`\n", "# \\_ \\**`c`*`c`\n"},
		{"italic around bold code that ends first", "***![\\![`c`**\\**\n", "***![![`c`**\\**\n"},
		{"bold italic code then italic code", "Plain ***`c`**`c`*](u) end.\n\n[^a_b]: n\n", "Plain ***`c`**`c`*]\\(u) end.\n\n[^a_b]: n\n"},
		{"a struck tilde in italic after italic", "p a*a*~*~*~ q\n", "p a*a*~~*~*~~ q\n"},
		{"an italic bare URL before a letter", "*http\\://a.example/a*b\\)**\n", "*http\\://a.example/a*b)**\n"},
	}
	markdowns := make([]string, len(rows))
	docs := make([]*Node, len(rows))
	parseErrs := make([]error, len(rows))
	rendered := make([]string, len(rows))
	renderErrs := make([]error, len(rows))
	for index, row := range rows {
		markdowns[index] = row.markdown
		docs[index], parseErrs[index] = Parse(row.markdown)
		if parseErrs[index] == nil {
			rendered[index], renderErrs[index] = Render(docs[index])
		}
	}
	readings := engineReadings(t, append(append([]string{}, markdowns...), rendered...))
	for index, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			doc, rendering := docs[index], rendered[index]
			if parseErrs[index] != nil {
				t.Fatalf("Parse(%q) = %v", row.markdown, parseErrs[index])
			}
			t.Run("1 Parse reads it as the engine", func(t *testing.T) {
				if readings[index] == nil {
					t.Fatalf("the engine does not read %q", row.markdown)
				}
				if !doc.Equal(readings[index]) {
					t.Errorf("%q: Go reads\n\t%s\nthe engine\n\t%s", row.markdown, readingSummary(doc), readingSummary(readings[index]))
				}
			})
			t.Run("2 ParseForWrite stores it", func(t *testing.T) {
				if _, err := ParseForWrite(row.markdown, nil); err != nil {
					t.Errorf("ParseForWrite(%q) = %v", row.markdown, err)
				}
			})
			t.Run("3 Render reads back in both parsers", func(t *testing.T) {
				if renderErrs[index] != nil {
					t.Fatalf("Render(Parse(%q)) = %v", row.markdown, renderErrs[index])
				}
				if back, err := Parse(rendering); err != nil || !back.Equal(doc) {
					t.Errorf("Go reads the rendering %q of %q back as\n\t%s (%v)\nnot\n\t%s", rendering, row.markdown, readingSummary(back), err, readingSummary(doc))
				}
				if engine := readings[len(rows)+index]; engine == nil || !engine.Equal(doc) {
					t.Errorf("the engine reads the rendering %q of %q back as\n\t%s\nnot\n\t%s", rendering, row.markdown, readingSummary(engine), readingSummary(doc))
				}
			})
			t.Run("4 Render writes want", func(t *testing.T) {
				if renderErrs[index] != nil {
					t.Fatalf("Render(Parse(%q)) = %v", row.markdown, renderErrs[index])
				}
				if rendering != row.want {
					t.Errorf("Render(Parse(%q)) = %q, want %q", row.markdown, rendering, row.want)
				}
			})
		})
	}
}
