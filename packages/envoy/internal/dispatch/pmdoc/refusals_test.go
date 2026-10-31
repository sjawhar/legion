package pmdoc

import (
	"errors"
	"os"
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
// inside the tag and after it - and over a closing tag of a raw text name, which goldmark's
// patterns match nowhere. A declaration opening with a lower-case letter parted them until
// PR #2518, which opens that block in this package's reader too (declarationBlockParser), so such
// a line belongs to no paragraph now and is refused as goldmark's own block of HTML; only a tab
// in its indentation, which that parser alone reads as a block, still parts the two.
func TestParseRefusesALineTheBrowserReadsAsBlockHTML(t *testing.T) {
	for _, test := range []struct{ name, markdown, line string }{
		{"a closing tag a tab ends, opening a quoted paragraph", "> </a\t>\n> text\n", `"> </a\t>"`},
		{"a tab indenting a block-level tag under a paragraph", "> A quote\n> \t<div>\n", `"> \t<div>"`},
		{"a tab indenting a comment under a paragraph", "> A quote\n> \t<!-- c -->\n", `"> \t<!-- c -->"`},
		{"a tab before a tag's own closing slash", "> <br\t/>\n> text\n", `"> <br\t/>"`},
		{"a tab after a tag opening a quoted paragraph", "> <b>\t\n> text\n", `"> <b>\t"`},
		{"a tab indenting a declaration opening with a lower-case letter", "> A quote\n> \t<!x>\n", `"> \t<!x>"`},
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

// The refusal of a line that opens a block of HTML is a document's, not an inline replacement's:
// a textblock's inline markdown lands inside a paragraph the document already holds, where no
// line opens a block, so ParseInline reads such a line as what it is - inline HTML, or the text of
// a tag no `>` closes - where every reader of a document's markdown refuses it
// (dropHTMLFlowLines). Goldmark opens a block of HTML on both lines below when it reads a
// document, so the refusal that still stands on that path is its own (ErrBlockHTML), the one the
// inline reader never reached.
func TestParseInlineReadsHTMLADocumentIsRefusedFor(t *testing.T) {
	for _, test := range []struct{ name, markdown, kind, written string }{
		{"a block-level tag after a hard break", "x  \n<div>y</div>", "html", "<div>"},
		{"an unclosed block-level tag after a hard break", "Title\\\n<div", "text", "<div"},
	} {
		t.Run(test.name, func(t *testing.T) {
			nodes, err := ParseInline(test.markdown, NewWriteBudget())
			if err != nil {
				t.Fatalf("ParseInline(%q) = %v, want it read", test.markdown, err)
			}
			read := false
			for _, node := range nodes {
				written := node.Text
				if value, ok := node.Attrs["value"].(string); ok {
					written = value
				}
				read = read || node.Type == test.kind && strings.Contains(written, test.written)
			}
			if !read {
				t.Errorf("ParseInline(%q) read no %s node holding %q", test.markdown, test.kind, test.written)
			}
			if _, err := ParseForWrite(test.markdown, nil); !errors.Is(err, ErrBlockHTML) {
				t.Errorf("ParseForWrite(%q) = %v, want it refused as block HTML", test.markdown, err)
			}
		})
	}
}

// The corpus-errors documents measured for PR #2507, each refused by every reader of caller
// markdown for the reason named here, so that a reader lifting a refusal for some other reason
// would still be caught. Two reasons stand behind them. A footnote definition inside a typed block
// whose delimiter-like line and narrower setext underline the browser editor's parser reads as a
// table and a heading holds a block other than a paragraph there (typedDefinitionHoldingBlock),
// which the reader before PR #2507 refused already and this change leaves refused. A line whose
// tab or closing tag of a raw text name opens a block of HTML for that parser, where goldmark's
// own patterns read the text of a paragraph, is refused by name (browserBlockHTML, html_flow.go):
// the Proof schema holds no block of HTML, and that parser drops the one it reads together with
// the container around it, so no tree the document could be stored as reads back. A line that
// opens a block of kind 6, a tag of a block-level name however unfinished, is refused with them
// since round 2 of PR #2507, which found the exemption that passed kinds 6 and 7 letting a
// divergent reading of an unfinished tag through.
func TestCorpusErrorsOfDefinitionsAndBrowserBlockHTMLAreRefused(t *testing.T) {
	for name, refusal := range map[string]string{
		"nested-typed-blocks-definition-before-an-indented-underline":                     typedDefinitionHoldingBlock,
		"nested-typed-blocks-definition-ending-in-a-hard-break-before-a-tabbed-underline": typedDefinitionHoldingBlock,
		"typed-block-definition-before-a-tabbed-underline":                                typedDefinitionHoldingBlock,
		"typed-block-definition-before-an-indented-underline":                             typedDefinitionHoldingBlock,
		"typed-block-definition-ending-in-a-hard-break-before-a-tabbed-underline":         typedDefinitionHoldingBlock,
		"typed-block-definition-ending-in-a-hard-break-before-an-indented-underline":      typedDefinitionHoldingBlock,
		"typed-block-definition-of-an-underline-character-before-a-tabbed-underline":      typedDefinitionHoldingBlock,
		"warning-typed-block-definition-before-an-indented-underline":                     typedDefinitionHoldingBlock,
		"warning-typed-block-definition-of-a-bracket-before-a-tabbed-underline":           typedDefinitionHoldingBlock,
		"written-out-typed-block-definition-before-an-indented-underline":                 typedDefinitionHoldingBlock,
		"written-out-typed-block-definition-of-other-text-before-an-indented-underline":   typedDefinitionHoldingBlock,
		"item-text-block-opening-with-a-closing-tag-a-tab-ends":                           browserBlockHTML,
		"quoted-paragraph-opening-with-a-closing-raw-text-tag":                            browserBlockHTML,
		"quoted-paragraph-opening-with-a-closing-tag-a-tab-ends":                          browserBlockHTML,
		"quoted-paragraph-opening-with-a-tab-before-a-tags-slash":                         browserBlockHTML,
		"quoted-paragraph-opening-with-a-tag-a-tab-follows":                               browserBlockHTML,
		"quoted-paragraph-then-a-tab-indented-block-tag":                                  browserBlockHTML,
		"quoted-paragraph-then-a-tab-indented-comment":                                    browserBlockHTML,
		"quoted-setext-heading-opening-with-a-closing-tag-a-tab-ends":                     browserBlockHTML,
		"item-paragraph-then-a-tab-indented-unfinished-block-tag":                         browserBlockHTML,
		"item-paragraph-then-a-tab-indented-closing-block-tag-a-tab-ends":                 browserBlockHTML,
	} {
		t.Run(name, func(t *testing.T) {
			markdown, err := os.ReadFile("testdata/corpus-errors/" + name + ".md")
			if err != nil {
				t.Fatalf("read %s corpus: %v", name, err)
			}
			for _, reader := range blockMarkdownReaders() {
				if _, err := reader.parse(string(markdown)); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
					t.Errorf("%s of %s: %v, want ErrSchema saying %q", reader.name, name, err, refusal)
				}
			}
		})
	}
}
