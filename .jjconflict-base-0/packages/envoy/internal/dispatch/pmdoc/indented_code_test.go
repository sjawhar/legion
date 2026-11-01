package pmdoc

import (
	"slices"
	"testing"
)

// The browser's engine (micromark's code-indented and mdast-util-from-markdown) ends indented
// code at the last line that is not code: a line whose indentation past what its containers ate
// is four columns or more is code whatever it holds, whitespace only included, and a shorter blank
// line between two such lines is passed over. So the code keeps every trailing whitespace-only
// line of four columns or more and the shorter blank lines between them, less one final line
// ending. On a blank line a list item eats up to its content's offset, a footnote definition eats
// nothing, and a quote eats its `>` and one optional space. Code right after a list or a quote,
// outside it and with no blank line between, keeps none: the engine holds that list or quote open
// there. Those whitespace-only lines are code, so they spread no list. Each row is what the engine
// reads in its markdown (checkCodeAndSpread).
func TestIndentedCodeKeepsTheWhitespaceLinesTheEngineKeeps(t *testing.T) {
	checkCodeAndSpread(t, []codeAndSpreadRow{
		// Code text.
		{name: "top level", markdown: "          x\n        \n       \nx\n", codes: []string{"      x\n    \n   "}},
		{name: "four columns then a blank line", markdown: "    code\n    \n\nafter\n", codes: []string{"code"}},
		{name: "two lines of four columns", markdown: "    code\n    \n    \nafter\n", codes: []string{"code\n"}},
		{name: "a tab past the code's columns", markdown: "    code\n\t\t\n", codes: []string{"code\n\t"}},
		{name: "in a quote", markdown: ">           x\n>        \n>       \n> x\n", codes: []string{"      x\n   \n  "}},
		{name: "in a list item", markdown: "- a\n\n          x\n        \n       \n  y\n", codes: []string{"    x\n  \n "}},
		{name: "in a footnote definition", markdown: "x[^f]\n\n[^f]: a\n\n          x\n        \n       \n    y\n", codes: []string{"  x\n    \n   "}},
		{name: "in a callout", markdown: ":::callout{kind=\"note\" title=\"T\"}\n    code\n      \n:::\n", codes: []string{"code\n  "}},
		// A document ending in a whitespace-only line with no line feed: that line is the code's
		// last, empty once its four columns are eaten, and only one final line ending is dropped.
		{name: "at the document's end with no final line feed", markdown: "    code\n    \n    ", codes: []string{"code\n"}},
		{name: "after a shorter blank line, with no final line feed", markdown: "        code\n     code\n      deeper\n\n\t", codes: []string{"    code\n code\n  deeper\n"}},
		{name: "in a list item, with no final line feed", markdown: "*     code\n     \n   \t  ", codes: []string{"code\n"}},
		// A typed block indented in its container takes its indentation from that last line too.
		{name: "in a quoted typed block, with no final line feed", markdown: ">   :::callout{}\n>       code\n>         ", codes: []string{"code\n  "}},
		{name: "in a typed block in an item, with no final line feed", markdown: "- a\n\n    :::callout{}\n          code\n            ", codes: []string{"  code\n    "}},
		{name: "in an item in a quoted typed block, with no final line feed", markdown: ">\t:::callout{}\n>\t-     x\n>            ", codes: []string{"x\n   "}},
		// Code the engine holds a list or a quote open over keeps none.
		{name: "right after a list", markdown: "100. a\n\n    x\n          \nb\n", codes: []string{"x"}},
		{name: "right after a quote", markdown: "> # h\n    x\n      \nb\n", codes: []string{"x"}},
		{name: "right after a quoted rule", markdown: "> ***\n    x\n        \n", codes: []string{"x"}},
		// Whitespace-only lines kept as code spread no list.
		{name: "quoted item holding only code", markdown: "> -\n>       code\n>       \n>       note\n>       \n>   \n\nafter\n", codes: []string{"code\n\nnote"}, lists: []bool{false}, items: []bool{false}},
		{name: "item ending in code before the next item", markdown: "- a\n\n          x\n        \n- b\n", codes: []string{"    x\n  "}, lists: []bool{false}, items: []bool{true, false}},
		{name: "quoted item ending in code before the next item", markdown: "> - a\n>\n>       code\n>       \n> - b\n", lists: []bool{false}, items: []bool{true, false}},
		{name: "quoted item opening with code", markdown: "> -       x\n>       \n>   b\n", codes: []string{"  x"}, items: []bool{false}},
		{name: "a blank line after the code spreads the list", markdown: "- a\n\n          x\n        \n\n- b\n", lists: []bool{true}},
		// Whitespace-only lines inside the code are kept as they were.
		{name: "internal line", markdown: "- a\n\n      code\n          \n      more\n", codes: []string{"code\n    \nmore"}},
	})
}

// codeAndSpread is what an indented code block's whitespace-only lines decide in a document: every
// code block's text and every list's and list item's spread, in document order.
type codeAndSpread struct {
	codes        []string
	lists, items []bool
}

func readCodeAndSpread(doc *Node) (reading codeAndSpread) {
	Walk(doc, func(node *Node) bool {
		switch node.Type {
		case "code_block":
			reading.codes = append(reading.codes, TextContent(node))
		case "bullet_list", "ordered_list":
			reading.lists = append(reading.lists, node.Attrs["spread"] == true)
		case "list_item":
			reading.items = append(reading.items, node.Attrs["spread"] == true)
		}
		return true
	})
	return reading
}

// codeAndSpreadRow is markdown and what the engine reads in it, every code block's text and every
// list's and list item's spread in document order; a nil slice is not checked.
type codeAndSpreadRow struct {
	name, markdown string
	codes          []string
	lists, items   []bool
}

// checkCodeAndSpread asserts each row for every reader a document is read with - so a run without
// the engine still checks them - and then for the engine's own reading.
func checkCodeAndSpread(t *testing.T, rows []codeAndSpreadRow) {
	t.Helper()
	readers := []struct {
		name  string
		parse func(string) (*Node, error)
	}{
		{"Parse", Parse},
		{"ParseRendering", ParseRendering},
		{"ParseForWrite", func(markdown string) (*Node, error) { return ParseForWrite(markdown, nil) }},
	}
	// check reports where doc's reading differs from the row's, as reader reads it.
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
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			for _, reader := range readers {
				doc, err := reader.parse(row.markdown)
				if err != nil {
					t.Errorf("%s(%q) = %v", reader.name, row.markdown, err)
					continue
				}
				check(t, reader.name, doc, row.markdown, row.codes, row.lists, row.items)
			}
		})
	}
	t.Run("the engine", func(t *testing.T) {
		markdowns := make([]string, 0, len(rows))
		for _, row := range rows {
			markdowns = append(markdowns, row.markdown)
		}
		readings := engineReadings(t, markdowns)
		for index, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				if readings[index] == nil {
					t.Fatalf("the engine does not read %q", row.markdown)
				}
				check(t, "the engine", readings[index], row.markdown, row.codes, row.lists, row.items)
			})
		}
	})
}
