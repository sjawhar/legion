package pmdoc

import (
	"errors"
	"testing"
)

// BlockIndexes finds both endpoints of a range in one walk of a document's blocks, where a write
// looked each one up on its own, each through documentBlock. This test holds the walk to that
// lookup (blockIndex, plain_tree_test.go, which is documentBlock for one position), over every
// document of the corpus and every position in it.
func TestBlockIndexesAnswerTwoBlockIndexLookups(t *testing.T) {
	for _, document := range corpusMarkdown(t) {
		t.Run(document.name, func(t *testing.T) {
			tree, err := Parse(document.markdown)
			if err != nil {
				t.Fatal(err)
			}
			valid := validOf(t, tree)
			size := Size(tree)
			probes := []Range{{From: 0, To: size}, {From: size + 5, To: size + 9}, {From: 4, To: 1}}
			for from := range size + 3 {
				probes = append(probes, Range{From: from, To: from}, Range{From: from, To: from + 3})
			}
			for _, probe := range probes {
				wantFirst, firstErr := blockIndex(tree, Range{From: probe.From, To: probe.From})
				wantLast, lastErr := blockIndex(tree, Range{From: probe.To, To: probe.To})
				first, last, err := valid.BlockIndexes(probe)
				if firstErr != nil || lastErr != nil {
					if err == nil {
						t.Fatalf("BlockIndexes(%v) = %d..%d, want a refusal", probe, first, last)
					}
					if !errors.Is(err, ErrTargetNotFound) {
						t.Fatalf("BlockIndexes(%v) error = %v, want ErrTargetNotFound", probe, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("BlockIndexes(%v) error = %v, want blocks %d..%d", probe, err, wantFirst, wantLast)
				}
				if first != wantFirst || last != wantLast {
					t.Fatalf("BlockIndexes(%v) = %d..%d, want %d..%d", probe, first, last, wantFirst, wantLast)
				}
			}
		})
	}
}
