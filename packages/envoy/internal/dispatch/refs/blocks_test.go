package refs

import (
	"slices"
	"testing"
)

// A mention starts at the tightest block whose [From, To) holds its start: a child inside its
// parent, the parent between its children, and no block at all between top-level blocks or past
// the last one. A block ending where a mention starts does not hold it.
func TestInnermostBlocksPicksTheTightestRangeHoldingEachStart(t *testing.T) {
	blocks := []Block{{ID: "a", From: 0, To: 100}, {ID: "b", From: 10, To: 20}, {ID: "c", From: 30, To: 40}, {ID: "d", From: 120, To: 130}}
	got := innermostBlocks(blocks, []int{5, 15, 20, 25, 35, 50, 110, 125, 200})
	want := []int{0, 1, 0, 0, 2, 0, -1, 3, -1}
	if !slices.Equal(got, want) {
		t.Fatalf("innermostBlocks = %v, want %v", got, want)
	}
}

// A rendered version can repeat an id, and document_blocks holds one row per id: the first holder
// is the node. An id Postgres cannot store in a text[] (empty, holding a NUL, or not UTF-8) is
// no node.
func TestUniqueBlocksKeepsTheFirstHolderOfEachID(t *testing.T) {
	got := uniqueBlocks([]Block{
		{ID: "a", From: 0, To: 10},
		{ID: "a", From: 20, To: 30},
		{ID: "", From: 40, To: 50},
		{ID: "x\x00", From: 60, To: 70},
		{ID: "\xff", From: 70, To: 75},
		{ID: "b", From: 80, To: 90},
	})
	want := []Block{{ID: "a", From: 0, To: 10}, {ID: "b", From: 80, To: 90}}
	if !slices.Equal(got, want) {
		t.Fatalf("uniqueBlocks = %#v, want %#v", got, want)
	}
}
