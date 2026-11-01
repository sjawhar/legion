package refs

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A mention in a comment or message starts at the block holding it: one pair per (block, target),
// so a target cited twice in one block is one pair and cited from two blocks is two, and an
// external link writes none.
func TestItemBlockPairsStartAtTheBlockHoldingEachMention(t *testing.T) {
	body := "Intro cites dispatch://CORE-1 and dispatch://CORE-1 again.\n\n" +
		"See https://example.com.\n\n" +
		"- dispatch://CORE-2\n- dispatch://CORE-1"
	next := itemblocks.Split(body)
	if len(next) != 3 {
		t.Fatalf("Split = %+v, want three blocks", next)
	}
	ids := []string{"b1", "b4", "b2"}
	pairs := itemBlockPairs(next, ids, text.ExtractAt(body, "https://dispatch.example"))
	got := make([]string, len(pairs))
	for i, pair := range pairs {
		got[i] = pair.blockID + "->" + pair.target.Kind + ":" + pair.target.ID
	}
	want := []string{"b1->issue:CORE-1", "b2->issue:CORE-2", "b2->issue:CORE-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("itemBlockPairs = %v, want %v", got, want)
	}
	if pairs := itemBlockPairs(next, ids, nil); len(pairs) != 0 {
		t.Fatalf("itemBlockPairs with no mentions = %v, want none", pairs)
	}
}

func TestBlockNumberReadsAMintedID(t *testing.T) {
	for id, want := range map[string]int{"b1": 1, "b12": 12} {
		if got, ok := blockNumber(id); !ok || got != want {
			t.Errorf("blockNumber(%q) = %d, %v, want %d", id, got, ok, want)
		}
	}
	for _, id := range []string{"", "b", "x1", "b1a"} {
		if _, ok := blockNumber(id); ok {
			t.Errorf("blockNumber(%q) read a number", id)
		}
	}
}
