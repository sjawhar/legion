package pmdoc

import (
	"errors"
	"testing"
)

// TestParseNamesTheFirstRefusedBlock pins the order refuseBlocks gives refusals: a document holding
// two refused blocks is refused with the reason of the one written first, whichever kinds they are,
// since the walk, not conversion, refuses every block.
func TestParseNamesTheFirstRefusedBlock(t *testing.T) {
	blocks := []struct{ name, markdown string }{
		{"a link reference definition", "[a]: http://x\n"},
		{"block HTML", "<div>\nx\n</div>\n"},
		{"an unknown typed block", ":::unknown{#b1}\nBody.\n:::\n"},
		{"an undeclared attribute", ":::callout{#b2 kind=\"note\" title=\"T\" priority=\"high\"}\nBody.\n:::\n"},
		{"an invalid attribute value", ":::callout{#b3 kind=\"urgent\" title=\"T\"}\nBody.\n:::\n"},
		{"a footnote definition inside another", "x[^n]\n\n[^n]: a\n\n    [^m]: b\n"},
		{"a footnote definition whose label holds whitespace", "[^x y]: note\n"},
		{"a lazy line after a table in a quote", "> | a |\n> | - |\n> | b |\ntail\n"},
		{"an ordered item numbered 2 after a table", "| a | b |\n| - | - |\n| 1 | 2 |\n2. a\n"},
		{"a lone dash under a table after text", "text\n| a | b |\n| - | - |\n| 1 | 2 |\n-\n"},
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
