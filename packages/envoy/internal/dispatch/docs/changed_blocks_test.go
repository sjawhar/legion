package docs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A write's refusals all read the document-level blocks it changed, which the write now finds in
// one walk and passes on (matchedBlocks). This test holds that walk to pmdoc.BlockIndex, the
// lookup it replaced.

// TestMatchedBlocksAnswersTwoBlockIndexLookups holds the one walk to pmdoc.BlockIndex, the lookup
// it replaced, for every document of the corpus at every position in it: the first block holding
// the range's start, the last holding its end, and a refusal when either endpoint lies in none.
func TestMatchedBlocksAnswersTwoBlockIndexLookups(t *testing.T) {
	for _, document := range corpusTrees(t) {
		t.Run(document.name, func(t *testing.T) {
			tree := document.tree
			valid, err := pmdoc.ValidateDocument(tree, "BlockIndex")
			if err != nil {
				t.Fatal(err)
			}
			size := pmdoc.Size(tree)
			probes := []pmdoc.Range{{From: 0, To: size}, {From: size + 5, To: size + 9}}
			for from := range size + 3 {
				probes = append(probes, pmdoc.Range{From: from, To: from}, pmdoc.Range{From: from, To: from + 3})
			}
			for _, probe := range probes {
				wantFirst, firstErr := pmdoc.BlockIndex(tree, pmdoc.Range{From: probe.From, To: probe.From})
				wantLast, lastErr := pmdoc.BlockIndex(tree, pmdoc.Range{From: probe.To, To: probe.To})
				found, err := matchedBlocks(valid, probe)
				if firstErr != nil || lastErr != nil {
					if err == nil {
						t.Fatalf("matchedBlocks(%v) = %d..%d, want a refusal", probe, found.first, found.last)
					}
					if !errors.Is(err, pmdoc.ErrTargetNotFound) {
						t.Fatalf("matchedBlocks(%v) error = %v, want ErrTargetNotFound", probe, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("matchedBlocks(%v) error = %v, want blocks %d..%d", probe, err, wantFirst, wantLast)
				}
				if found.first != wantFirst || found.last != wantLast {
					t.Fatalf("matchedBlocks(%v) = %d..%d, want %d..%d", probe, found.first, found.last, wantFirst, wantLast)
				}
				// lastAfter counts the blocks the write added, as it did when every refusal
				// computed it from the tree it held.
				for _, gained := range []int{-2, -1, 0, 1, 5} {
					blocks := len(tree.Children) + gained
					if blocks < 0 {
						continue
					}
					after := &pmdoc.Node{Type: "doc", Children: make([]*pmdoc.Node, blocks)}
					want := max(found.first, found.last+blocks-len(tree.Children))
					if got := found.span(after).lastAfter; got != want {
						t.Fatalf("span of %d blocks has lastAfter %d, want %d", blocks, got, want)
					}
				}
			}
		})
	}
}

// namedTree is one corpus document parsed, under the name its failures give it.
type namedTree struct {
	name string
	tree *pmdoc.Node
}

// corpusTrees is pmdoc's own corpus parsed, the documents the block walk is held over.
func corpusTrees(t *testing.T) []namedTree {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "pmdoc", "testdata", "corpus", "*.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d documents)", err, len(paths))
	}
	trees := make([]namedTree, 0, len(paths))
	for _, path := range paths {
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		tree, err := parseInput(string(markdown))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		trees = append(trees, namedTree{name: filepath.Base(path), tree: tree})
	}
	return trees
}
