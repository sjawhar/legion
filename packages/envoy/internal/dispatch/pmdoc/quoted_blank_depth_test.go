package pmdoc

import (
	"strings"
	"testing"
)

// A typed block no fence closes, in a list item, whose last two lines hold the quote's markers and
// a tab: the browser editor's parser spreads the inner list at one quote and at three, and leaves
// it tight at two, because at two the line's tab lands inside the item's own content and holds
// nothing of the list. This reader must answer what that parser answers at each depth; it may not
// answer one thing for every depth.
//
// Every expectation below is measured against that parser (packages/proof-editor's
// createHeadlessProof with schema/blocks.json), and this reader answered tight at all three depths before this pull request - right at two quotes and wrong at one and three, which is the
// divergence this change closes. A reader that answers spread at every depth closes one and three
// and breaks two, which is what this test is here to catch.
func TestAnUnclosedTypedBlocksTabBlankLinesFollowTheBrowserAtEveryQuoteDepth(t *testing.T) {
	const callout = ":::callout{#c1 kind=\"note\" title=\"T\"}"
	for _, test := range []struct {
		name   string
		quotes int
		spread bool
	}{
		{name: "one quote", quotes: 1, spread: true},
		{name: "two quotes", quotes: 2, spread: false},
		{name: "three quotes", quotes: 3, spread: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			markers := strings.Repeat("> ", test.quotes)
			blank := strings.TrimRight(markers, " ") + "\t\n"
			markdown := markers + "- a\n" +
				markers + "  " + callout + "\n" +
				markers + "  - x\n" +
				blank + blank
			doc, err := ParseForWrite(markdown, nil)
			if err != nil {
				t.Fatalf("ParseForWrite(%q): %v", markdown, err)
			}
			list := innerTypedListOf(doc)
			if list == nil {
				t.Fatalf("ParseForWrite(%q) holds no list inside its typed block", markdown)
			}
			if spread, _ := list.Attrs["spread"].(bool); spread != test.spread {
				t.Errorf("ParseForWrite(%q) reads the typed block's list spread=%t, want %t, as the browser editor's parser reads it at this depth",
					markdown, spread, test.spread)
			}
		})
	}
}

// innerTypedListOf is the first list inside the first typed block of tree.
func innerTypedListOf(tree *Node) *Node {
	var found *Node
	var walk func(node *Node, inTyped bool)
	walk = func(node *Node, inTyped bool) {
		if found != nil {
			return
		}
		typed := inTyped || node.Type == "callout"
		if typed && (node.Type == "bullet_list" || node.Type == "ordered_list") {
			found = node
			return
		}
		for _, child := range node.Children {
			walk(child, typed)
		}
	}
	walk(tree, false)
	return found
}
