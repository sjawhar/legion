package docs

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A block pasted in the browser keeps its source's id until a settlement stamps a new one, so the
// live document can hold one id twice. Version 1 holds "Foo." twice, indexed as rows p1 and p2.
// The live document holds p1, a paste of it that kept the id p1, and p2; in the edited case the
// paste's text is "Bar.". refs.IndexBlocks indexes only the first holder of an id, so the
// version's two "Foo." blocks take p1 and p2, each once.
func TestClosedVersionBlocksCountsOnlyTheFirstHolderOfAnID(t *testing.T) {
	paragraph := func(id, body string) *pmdoc.Node {
		return &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: id}, Children: []*pmdoc.Node{{Type: "text", Text: body}}}
	}
	for name, pasted := range map[string]string{"paste": "Foo.", "edited paste": "Bar."} {
		t.Run(name, func(t *testing.T) {
			live, liveBlocks, err := documentBlocks(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{
				paragraph("p1", "Foo."), paragraph("p1", pasted), paragraph("p2", "Foo."),
			}})
			if err != nil {
				t.Fatalf("render the live document: %v", err)
			}
			sum := refs.BlockTextSum(live[liveBlocks[0].From:liveBlocks[0].To])
			indexed := []indexedBlock{{id: "p1", typ: "paragraph", sum: sum}, {id: "p2", typ: "paragraph", sum: sum}}
			markdown, blocks, err := closedVersionBlocks("Foo.\n\nFoo.\n", live, liveBlocks, indexed)
			if err != nil {
				t.Fatalf("match the version's blocks: %v", err)
			}
			var ids []string
			for _, block := range blocks {
				if markdown[block.From:block.To] != live[liveBlocks[0].From:liveBlocks[0].To] {
					t.Fatalf("block %s holds %q, want \"Foo.\"", block.ID, markdown[block.From:block.To])
				}
				ids = append(ids, block.ID)
			}
			if want := []string{"p1", "p2"}; !slices.Equal(ids, want) {
				t.Fatalf("the version's blocks take %v, want %v", ids, want)
			}
		})
	}
}
