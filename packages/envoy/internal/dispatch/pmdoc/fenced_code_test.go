package pmdoc

import "testing"

// The browser's engine (micromark's code-fenced) takes up to as many columns from each line of
// fenced code as the opening fence stands past its container's content, and a line ending right
// after them leaves the line empty. So a whitespace-only line holding fewer columns past what its
// containers ate than the fence's indentation is an empty line of the code, where goldmark keeps
// what it holds. A line of as many columns or more keeps those past the fence's indentation, and
// a fence at its container's content keeps the whole line. On a blank line a list item eats up to
// its content's offset, a footnote definition eats nothing, and a quote eats its `>` and one
// optional space. Each row is what the engine reads in its markdown (checkCodeAndSpread).
func TestFencedCodeDropsAWhitespaceLineShortOfTheFenceIndent(t *testing.T) {
	checkCodeAndSpread(t, []codeAndSpreadRow{
		// A line short of the fence's indentation is empty.
		{name: "last line, no fence closing", markdown: "  ~~~\n \n", codes: []string{""}},
		{name: "before a line of code", markdown: "  ~~~\n \nx\n", codes: []string{"\nx"}},
		{name: "in a callout", markdown: ":::callout{}\n  ~~~\n \n  x\n:::\n", codes: []string{"\nx"}},
		{name: "in a quote", markdown: ">   ~~~\n>  \n>   x\n", codes: []string{"\nx"}},
		// A document ending, with no line feed, in a line no fence closes that holds no more than
		// the fence's indentation: the engine ends the code before that empty line.
		{name: "short last line with no final line feed", markdown: "  ~~~\nx\n ", codes: []string{"x"}},
		{name: "last line of the fence's indentation with no final line feed", markdown: "  ~~~\n  x\n  ", codes: []string{"x"}},
		{name: "only such a line, with no final line feed", markdown: " ```js\n\n ", codes: []string{""}},
		// In a quote, after a line of the quote's markers alone, the quote takes that blank line
		// from the code and the document's last line, with no line feed, ends the code empty: one
		// line ending stays, which main read so.
		{name: "quoted, after a blank quote line, short last line with no final line feed", markdown: ">  ~~~\n>  x\n>\n>  ", codes: []string{"x\n"}},
		{name: "quoted in an item, after a blank quote line, short last line with no final line feed", markdown: "- >  ```\n  >  x\n  >\n  >  ", codes: []string{"x\n"}},
		// A last line with no line feed holding a tab its container eats part of keeps the
		// columns the container leaves, as spaces, as it does with a line feed after it.
		{name: "quoted tab-only last line with no final line feed", markdown: "> ```\n> x\n>\t", codes: []string{"x\n  "}},
		{name: "tab-only last line of an item with no final line feed", markdown: "- ```\n  x\n\t", codes: []string{"x\n  "}},
		// A container inside the one that took part of the tab takes its own columns from the rest,
		// and an item whose content stands past them takes them all.
		{name: "tab-only last line of a quoted item, with no final line feed", markdown: "> - ```\n>   x\n>\t", codes: []string{"x"}},
		{name: "tab-only last line short of a quoted item's content, with no final line feed", markdown: "> -  ```\n>    x\n>\t", codes: []string{"x"}},
		{name: "tab-only last line past the fence's indent in a quote, with no final line feed", markdown: ">  ```\n>  x\n>\t", codes: []string{"x\n "}},
		// A typed block indented in its container takes its indentation from that last line too.
		{name: "in a quoted typed block, empty last line with no final line feed", markdown: ">   :::callout{}\n>   ~~~\n>   x\n>   ", codes: []string{"x"}},
		{name: "in a typed block in an item, empty last line with no final line feed", markdown: "- a\n\n    :::callout{}\n    ~~~\n    x\n    ", codes: []string{"x"}},
		// A line as long as the fence's indentation, or a fence at its container's content, keeps
		// what the line holds past them.
		{name: "fence at the line's start", markdown: "~~~\n \nx\n", codes: []string{" \nx"}},
		{name: "a line past the fence's indentation", markdown: "  ~~~\n   \nx\n", codes: []string{" \nx"}},
		{name: "fence at the quote's content", markdown: "> ~~~\n>  \n> x\n", codes: []string{" \nx"}},
		{name: "fence at the item's content", markdown: "-   ~~~\n   \n    x\n", codes: []string{"\nx"}},
		{name: "fence past a footnote definition's label", markdown: "x[^f]\n\n[^f]:   ~~~\n     \n      x\n", codes: []string{"     \n  x"}},
		// Documents of sjawhar/legion#2403's generator that the engine read so and goldmark did not.
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
	})
}
