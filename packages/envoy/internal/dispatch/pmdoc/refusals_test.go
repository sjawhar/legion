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
		{"indented code after a list", "100. a\n\n    x\n    y\n"},
		{"indented code after a quote", ">\n    a\n    b\n"},
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
