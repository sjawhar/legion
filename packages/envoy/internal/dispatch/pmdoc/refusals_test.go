package pmdoc

import (
	"errors"
	"strings"
	"testing"
)

// TestParseNamesTheFirstRefusedBlock pins the order refuseBlocks gives refusals: a document holding
// two refused blocks is refused with the reason of the one written first, whichever kinds they are,
// since the walk, not conversion, refuses every block.
func TestParseNamesTheFirstRefusedBlock(t *testing.T) {
	blocks := []struct{ name, markdown string }{
		{"a link reference definition", "[a]: http://x\n"},
		{"block HTML", "<div>\nx\n</div>\n"},
		{"a quoted line the browser editor reads as block HTML", "> </a\t>\n> text\n"},
		{"an unknown typed block", ":::unknown{#b1}\nBody.\n:::\n"},
		{"an undeclared attribute", ":::callout{#b2 kind=\"note\" title=\"T\" priority=\"high\"}\nBody.\n:::\n"},
		{"an invalid attribute value", ":::callout{#b3 kind=\"urgent\" title=\"T\"}\nBody.\n:::\n"},
		{"a footnote definition inside another", "x[^n]\n\n[^n]: a\n\n    [^m]: b\n"},
		{"a footnote definition whose label holds whitespace", "[^x y]: note\n"},
		{"a lazy line after a table in a quote", "> | a |\n> | - |\n> | b |\ntail\n"},
		{"an ordered item numbered 2 after a table", "| a | b |\n| - | - |\n| 1 | 2 |\n2. a\n"},
		{"a lone dash under a table after text", "text\n| a | b |\n| - | - |\n| 1 | 2 |\n-\n"},
		{"a table row holding more cells than its table", "| a | b |\n| - | - |\n| `x | y` | z |\n"},
		{"indented code after a list", "100. a\n\n    x\n    y\n"},
		{"indented code after a quote", ">\n    a\n    b\n"},
		{"a code block whose language holds an escape", "```a\\*b\nx\n```\n"},
		{"a Pandoc fenced div", "::: {.callout}\nBody.\n:::\n"},
		{"a leaf directive", "::callout{#b4}\n"},
	}
	reasons := make([]string, len(blocks))
	seen := map[string]string{}
	for i, block := range blocks {
		_, err := Parse(block.markdown)
		if !errors.Is(err, ErrSchema) {
			t.Fatalf("%s alone: Parse(%q) = %v, want a schema refusal", block.name, block.markdown, err)
		}
		reasons[i] = err.Error()
		if other, ok := seen[reasons[i]]; ok {
			t.Fatalf("%s and %s are refused with one reason, %q, so their order cannot be told apart", other, block.name, reasons[i])
		}
		seen[reasons[i]] = block.name
	}
	for i, first := range blocks {
		for j, second := range blocks {
			if i == j {
				continue
			}
			markdown := first.markdown + "\nNext.\n\n" + second.markdown
			if _, err := Parse(markdown); err == nil || err.Error() != reasons[i] {
				t.Errorf("%s, then %s: Parse(%q) = %v, want %q", first.name, second.name, markdown, err, reasons[i])
			}
		}
	}
}

// A code block's language is refused where an escape or a character reference in it decodes: the
// browser editor's parser decodes both in the info string's first word, and goldmark keeps them as
// written. What that parser leaves as written, and the rest of the info string, which both drop,
// is read as before.
func TestParseRefusesACodeLanguageTheBrowserDecodes(t *testing.T) {
	for _, markdown := range []string{
		"```a\\*b\nx\n```\n",
		"```a&amp;b\nx\n```\n",
		"```a&#42;b\nx\n```\n",
		"~~~a\\~b\nx\n~~~\n",
		"- ```q=1\\_x\n  y\n  ```\n",
	} {
		if _, err := Parse(markdown); !errors.Is(err, ErrSchema) {
			t.Errorf("Parse(%q) = %v, want a schema refusal", markdown, err)
		}
	}
	for markdown, language := range map[string]string{
		"```a\\qb\nx\n```\n":     `a\qb`,
		"```a&bogus;b\nx\n```\n": "a&bogus;b",
		"```a b\\*c\nx\n```\n":   "a",
		"```c++\nx\n```\n":       "c++",
	} {
		doc, err := Parse(markdown)
		if err != nil {
			t.Errorf("Parse(%q) = %v, want it read", markdown, err)
			continue
		}
		if got := doc.Children[0].Attrs["language"]; got != language {
			t.Errorf("Parse(%q) language = %v, want %q", markdown, got, language)
		}
	}
}

// A paragraph line the browser editor's parser opens a block of HTML on, where goldmark's own
// patterns open none and read the line as text, is refused: the Proof schema holds no block of
// HTML, and that parser drops the one it reads with the container around it, so nothing the
// document could be stored as reads back as written. The two grammars part where micromark takes
// a tab for the whitespace goldmark's patterns spell as a space - in the line's indentation,
// inside the tag and after it - and over a declaration opening with a lower-case letter and a
// closing tag of a raw text name, which goldmark's patterns match nowhere.
func TestParseRefusesALineTheBrowserReadsAsBlockHTML(t *testing.T) {
	for _, test := range []struct{ name, markdown, line string }{
		{"a closing tag a tab ends, opening a quoted paragraph", "> </a\t>\n> text\n", `"> </a\t>"`},
		{"a tab indenting a block-level tag under a paragraph", "> A quote\n> \t<div>\n", `"> \t<div>"`},
		{"a tab indenting a comment under a paragraph", "> A quote\n> \t<!-- c -->\n", `"> \t<!-- c -->"`},
		{"a tab before a tag's own closing slash", "> <br\t/>\n> text\n", `"> <br\t/>"`},
		{"a tab after a tag opening a quoted paragraph", "> <b>\t\n> text\n", `"> <b>\t"`},
		{"a declaration opening with a lower-case letter", "> <!x>\n> text\n", `"> <!x>"`},
		{"a closing tag of a raw text name", "> </script>\n> text\n", `"> </script>"`},
		{"such a line in a tight item's text block", "- </a\t>\n  text\n", `"- </a\t>"`},
		{"such a line in the paragraph a setext underline ends", "> </a\t>\n> text\n> ===\n", `"> </a\t>"`},
	} {
		for _, reader := range []struct {
			name string
			read func(string) (*Node, error)
		}{
			{name: "Parse", read: Parse},
			{name: "ParseForWrite", read: func(markdown string) (*Node, error) { return ParseForWrite(markdown, nil) }},
		} {
			t.Run(test.name+"/"+reader.name, func(t *testing.T) {
				_, err := reader.read(test.markdown)
				if !errors.Is(err, ErrBlockHTML) {
					t.Fatalf("read(%q) = %v, want it refused as block HTML", test.markdown, err)
				}
				if !strings.Contains(err.Error(), test.line) {
					t.Errorf("read(%q) = %v, want the refusal to name the line %s", test.markdown, err, test.line)
				}
			})
		}
	}
}

// A line both parsers read on the same terms is read, not refused. A lone tag continuing a
// paragraph opens a block of HTML in neither - CommonMark forbids a block of kind 7 from
// interrupting a paragraph, and the browser editor's copy of the construct obeys that on a lazy
// line as on any other (packages/proof-editor/src/html-flow.ts) - so the tab inside such a line
// is no reason to refuse it either.
func TestParseReadsALineBothParsersReadAsText(t *testing.T) {
	for _, test := range []struct{ name, markdown string }{
		{"a lone tag on a lazy line", "> A quote\n<b>\n"},
		{"a lone tag carrying the quote's prefix", "> A quote\n> <b>\n"},
		{"a closing tag a tab ends, continuing the paragraph", "> A quote\n> </a\t>\n"},
		{"a tag a tab ends on a lazy line", "> A quote\n</a\t>\n"},
		{"a lone closing tag on an item's lazy line", "- An item\n</b>\n"},
		{"a tag beside text", "a <b>x</b>\n"},
		{"four columns of indentation before the tag", "> A quote\n>     <div>\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse(test.markdown); err != nil {
				t.Errorf("Parse(%q) = %v, want it read", test.markdown, err)
			}
			if _, err := ParseForWrite(test.markdown, nil); err != nil {
				t.Errorf("ParseForWrite(%q) = %v, want it read", test.markdown, err)
			}
		})
	}
}
