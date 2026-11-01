package refs

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A mention starts at the tightest block whose [From, To) holds its start: a child inside its
// parent, the parent between its children, and no block at all between top-level blocks or past
// the last one. A block ending where a mention starts does not hold it.
func TestInnermostBlocksPicksTheTightestRangeHoldingEachStart(t *testing.T) {
	blocks := []model.ArtifactBlock{{ID: "a", From: 0, To: 100}, {ID: "b", From: 10, To: 20}, {ID: "c", From: 30, To: 40}, {ID: "d", From: 120, To: 130}}
	got := innermostBlocks(blocks, []int{5, 15, 20, 25, 35, 50, 110, 125, 200})
	want := []int{0, 1, 0, 0, 2, 0, -1, 3, -1}
	if !slices.Equal(got, want) {
		t.Fatalf("innermostBlocks = %v, want %v", got, want)
	}
}

// A rendered version can repeat an id, and document_blocks holds one row per id: the first holder
// is the node. An id Postgres cannot store in a text[] (empty, holding a NUL, or not UTF-8) is
// no node, nor is one longer than text.MaxBlockIDBytes, which no key of the index can hold; those
// are counted, with the longest one's bytes. An id at the bound is a node.
func TestUniqueBlocksKeepsTheFirstHolderOfEachID(t *testing.T) {
	atBound := strings.Repeat("é", text.MaxBlockIDBytes/2)
	got, long, longest := uniqueBlocks([]model.ArtifactBlock{
		{ID: "a", From: 0, To: 10},
		{ID: "a", From: 20, To: 30},
		{ID: "", From: 40, To: 50},
		{ID: "x\x00", From: 60, To: 70},
		{ID: "\xff", From: 70, To: 75},
		{ID: atBound + "x", From: 75, To: 78},
		{ID: atBound, From: 78, To: 79},
		{ID: strings.Repeat("f", 3200), From: 79, To: 80},
		{ID: "b", From: 80, To: 90},
	})
	type node struct {
		ID       string
		From, To int
	}
	nodes := make([]node, len(got))
	for i, block := range got {
		nodes[i] = node{block.ID, block.From, block.To}
	}
	want := []node{{"a", 0, 10}, {atBound, 78, 79}, {"b", 80, 90}}
	if !slices.Equal(nodes, want) || long != 2 || longest != 3200 {
		t.Fatalf("uniqueBlocks = %#v, %d long, longest %d; want %#v, 2 long, longest 3200", nodes, long, longest, want)
	}
}
