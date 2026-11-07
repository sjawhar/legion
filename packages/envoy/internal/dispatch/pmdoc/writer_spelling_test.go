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
// stores it as. Later rows are the stored spellings the browser suite's parity rows asserted
// before they were deleted (INFRA-1404), each marked by the spec it came from, so a reader knows
// why a browser claim is made here. For each row Parse reads it as the engine does, ParseForWrite
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
		// The rows below are the stored spellings five rows of the browser suite, in the four
		// specs named under them, used to assert of a document written through the API, moved
		// here when those rows were deleted (INFRA-1404): the fixture table cannot carry them,
		// since fixtures.json's rendered_by_milkdown is the engine's own re-serialization and
		// render_test.go compares it only through plain(), which strips a character reference
		// and a backslash alike. Each want below was measured here, and carries what its browser
		// row asserted of the stored document: the whole of it, or the strings that row named.
		// From strikethrough.e2e.ts: a tilde run strikes only beside a run of its own length,
		// and an unpaired one is stored as a character reference so it reads back as text.
		{"an unpaired tilde after a letter", "Plain ~a~~ text.\n", "Plain &#126;a&#126;&#126; text.\n"},
		{"two unpaired tildes before a letter", "Plain ~~a~ text.\n", "Plain &#126;&#126;a&#126; text.\n"},
		{"a struck escaped tilde", "Plain ~\\~~ end.\n", "Plain ~~&#126;~~ end.\n"},
		{"a paired tilde run", "Plain ~~b~~ text.\n", "Plain ~~b~~ text.\n"},
		// From emphasis-beside-a-tilde.e2e.ts: a tilde is punctuation, so an asterisk beside one
		// opens emphasis in the first row and pairs nothing in the other two.
		{"an asterisk pair around a tilde", "Plain b*~* text.\n", "Plain &#98;*~* text.\n"},
		{"a tilde before asterisks that pair nothing", "Plain ~**b<* end.\n", "Plain ~\\*\\*b<* end.\n"},
		{"asterisks that pair nothing before two tildes", "Plain *b**~~ end.\n", "Plain \\*b\\*\\*~~ end.\n"},
		// From leaf-marks.e2e.ts, over blocks of testdata/corpus/leaf-marks-in-one-document.md: a
		// declaration is raw HTML to both readers, and an autolink in a link label leaves the
		// label's own link on the text around it.
		{"an inline HTML declaration", "p <!a> q\n", "p <!a> q\n"},
		{"a label that is nothing but an autolink", "[<https://u.test>](u)\n", "https://u.test\n"},
		{"an autolink between a label's words", "[a <https://u.test> b](u)\n", "[a ](u)<https://u.test>[ b](u)\n"},
		// From indented-code-whitespace-lines.e2e.ts: an indented code block is stored fenced,
		// holding the whitespace-only lines the engine keeps in it - a line with spaces past the
		// block's indent stays, one without them drops off its end - and the fencing spreads
		// neither the list nor the item around the block (sjawhar/legion#2481). The second row
		// ends in such a line with no line feed after it, which an API write can carry.
		{"indented code blocks whose whitespace lines the engine keeps", "> -\n>       code\n>       \n>       note\n>       \n>   \n\nBetween.\n\n- a\n\n          x\n        \n- b\n\nAfter.\n\n          y\n        \n       \nEnd.\n", "> - ```\n>   code\n>   \n>   note\n>   ```\n\nBetween.\n\n- a\n\n  ```\n      x\n    \n  ```\n- b\n\nAfter.\n\n```\n      y\n    \n   \n```\n\nEnd.\n"},
		{"an indented code block ending in a whitespace line with no final line feed", "Before.\n\n    code\n    \n    ", "Before.\n\n```\ncode\n\n```\n"},
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
