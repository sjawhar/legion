package pmdoc

import (
	"slices"
	"testing"
)

// The browser's engine (micromark's code-fenced) takes up to as many columns from each line of
// fenced code as the opening fence stands past its container's content, and a line ending right
// after them leaves the line empty. So a whitespace-only line holding fewer columns past what its
// containers ate than the fence's indentation is an empty line of the code, where goldmark keeps
// what it holds. A line of as many columns or more keeps those past the fence's indentation, and
// a fence at its container's content keeps the whole line. On a blank line a list item eats up to
// its content's offset, a footnote definition eats nothing, and a quote eats its `>` and one
// optional space. Each row is markdown and what the engine reads in it, every code block's text
// and every list's and list item's spread in document order (a nil slice is not checked), asserted
// for every reader a document is read with - so a run without the engine still checks them - and
// then for the engine's own reading.
func TestFencedCodeDropsAWhitespaceLineShortOfTheFenceIndent(t *testing.T) {
	tests := []struct {
		name, markdown string
		codes          []string
		lists, items   []bool
	}{
		// A line short of the fence's indentation is empty.
		{name: "last line, no fence closing", markdown: "  ~~~\n \n", codes: []string{""}},
		{name: "before a line of code", markdown: "  ~~~\n \nx\n", codes: []string{"\nx"}},
		{name: "in a callout", markdown: ":::callout{}\n  ~~~\n \n  x\n:::\n", codes: []string{"\nx"}},
		{name: "in a quote", markdown: ">   ~~~\n>  \n>   x\n", codes: []string{"\nx"}},
		// A line as long as the fence's indentation, or a fence at its container's content, keeps
		// what the line holds past them.
		{name: "fence at the line's start", markdown: "~~~\n \nx\n", codes: []string{" \nx"}},
		{name: "a line past the fence's indentation", markdown: "  ~~~\n   \nx\n", codes: []string{" \nx"}},
		{name: "fence at the quote's content", markdown: "> ~~~\n>  \n> x\n", codes: []string{" \nx"}},
		{name: "fence at the item's content", markdown: "-   ~~~\n   \n    x\n", codes: []string{"\nx"}},
		{name: "fence past a footnote definition's label", markdown: "x[^f]\n\n[^f]:   ~~~\n     \n      x\n", codes: []string{"     \n  x"}},
		// Documents of INFRA-581's that the engine read so and goldmark did not.
		{
			name:     "g22-g4662-round6",
			markdown: "Text[^f] and[^g].\n\n1. >\n   >\n   > x\n\n   > :::callout{}\n   > ~~~\n   code\n   > \n   > ***\n\n   > \n   > :::\n\n[^f]: 1. - >\n         >\n         \n       :::callout{}\n         ~~~\n         code\n    \n         \n         ***\n       \n  :::\n         \n         :::callout{kind=\"note\" title=\"T\"}\n\n         \n         :::\n    [^g]:\n        ===\n        1. 1. ***\n           \n    [^g]:>\n            >\n            > ***\n            > ***\n    > note\n            [^g]:```\n                code\n                ```\n            y\n            ===\n    \n\n    [^f]: [^f]: > ~~~\n        [^f]: > ~~~\n            > code\n            >     indented\n            \n        \n            \n        indented\n\n        :::callout{kind=\"note\" title=\"T\"}\n        - ~~~\n          code\n        > x\n        > ```\n        code\n        > ```\n        > \n        \n        :::\n        \n    \n\nafter\n",
			codes: []string{
				"",
				"code\n\n    \n***\n  ",
				"     :::callout{kind=\"note\" title=\"T\"}\n\n     \n     :::\n[^g]:\n    ===\n    1. 1. ***\n       \n[^g]:>\n        >\n        > ***\n        > ***\n> note\n        [^g]:```\n            code\n            ```\n        y\n        ===\n\n\n[^f]: [^f]: > ~~~\n    [^f]: > ~~~\n        > code\n        >     indented\n        \n    \n        \n    indented\n\n    :::callout{kind=\"note\" title=\"T\"}\n    - ~~~\n      code\n    > x\n    > ```\n    code\n    > ```\n    > \n    \n    :::\n    ",
			},
			lists: []bool{false, false, false},
			items: []bool{true, true, false},
		},
		{
			name:     "g22-g6351-round6",
			markdown: "Text[^f] and[^g].\n\n- ```\n```\n  code\n  ```\n\n  note\n  [^g]:\n  :::callout{kind=\"note\" title=\"T\"}\n  \n      :::\n      [^f]: y\n          ===\n        - ~~~\n        code\n        \n          \n            x\n      \n    :::callout{}\n  [^g]:    indented\n      ~~~\n          code\n          indented\n      > ```\n      > ```\n      > code\n      ```\n      > \n      > Text[^f] and[^g].\n      ```\n      code\n      ```\n      \n      :::\n      \ny\n===\n\n:::callout{}\n[^f]:\n    ===\n1. 1. ```\n       1. ```\n      code\n       ```\n       1. ~~~\n          code\n          \n       x\n          \n\n          \n\n***\n- \n\n\n      indented\n  1. [^g]:y\n       ===\n     \n   \n   \n     note\n     -\n  \n:::\nafter\n",
			codes: []string{
				"",
				"  code",
				":::\n[^f]: y\n    ===\n  - ~~~\n  code\n  \n    \n      x",
				"    code\n    indented\n> ```\n> ```\n> code\n```\n> \n> Text[^f] and[^g].\n```\ncode\n```\n\n:::\n\ny\n===\n\n:::callout{}\n[^f]:\n===\n1. 1. ```\n 1. ```\ncode\n ```\n 1. ~~~\n    code\n    \n x\n    \n\n    \n\n***\n- \n\n\nindented\n1. [^g]:y\n ===\n\n\n\nnote\n-\n",
			},
			lists: []bool{false},
			items: []bool{false},
		},
	}
	readers := []struct {
		name  string
		parse func(string) (*Node, error)
	}{
		{"Parse", Parse},
		{"ParseRendering", ParseRendering},
		{"ParseForWrite", func(markdown string) (*Node, error) { return ParseForWrite(markdown, nil) }},
	}
	// check reports where doc's reading differs from test's, as reader reads it.
	check := func(t *testing.T, reader string, doc *Node, markdown string, codes []string, lists, items []bool) {
		t.Helper()
		got := readCodeAndSpread(doc)
		if codes != nil && !slices.Equal(got.codes, codes) {
			t.Errorf("%s(%q): code %q, want %q", reader, markdown, got.codes, codes)
		}
		if lists != nil && !slices.Equal(got.lists, lists) {
			t.Errorf("%s(%q): list spread %v, want %v", reader, markdown, got.lists, lists)
		}
		if items != nil && !slices.Equal(got.items, items) {
			t.Errorf("%s(%q): item spread %v, want %v", reader, markdown, got.items, items)
		}
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, reader := range readers {
				doc, err := reader.parse(test.markdown)
				if err != nil {
					t.Errorf("%s(%q) = %v", reader.name, test.markdown, err)
					continue
				}
				check(t, reader.name, doc, test.markdown, test.codes, test.lists, test.items)
			}
		})
	}
	t.Run("the engine", func(t *testing.T) {
		markdowns := make([]string, 0, len(tests))
		for _, test := range tests {
			markdowns = append(markdowns, test.markdown)
		}
		readings := engineReadings(t, markdowns)
		for index, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				if readings[index] == nil {
					t.Fatalf("the engine does not read %q", test.markdown)
				}
				check(t, "the engine", readings[index], test.markdown, test.codes, test.lists, test.items)
			})
		}
	})
}
