package docs

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A delete op renders the document it leaves and stores that rendering with no read-back, so a
// rendering markdown reads as fewer blocks than it was given stores a document holding less than
// the delete left. The blocks of a typed block in a tight list item are written on consecutive
// lines, which merges three pairs: a paragraph after a quote the quote's lazy line takes, a list
// that interrupts no paragraph read as its text, and an empty item's marker read as a setext
// underline. One blank line keeps each apart (itemBlocks), and the item then reads back spread.
//
// Each row deletes the fenced code block between the pair and reads the version the settlement
// stored, so what is checked is the bytes a person's next read gives back.
func TestDeletingABlockOfATypedBlockInATightItemKeepsTheBlocksApart(t *testing.T) {
	const callout = ":::callout{#c1 kind=\"note\" title=\"T\"}"
	for _, test := range []struct {
		name, markdown, stored string
		blocks                 int
	}{
		{
			name:     "a paragraph after a quote",
			markdown: "- a\n  " + callout + "\n  > q\n  ```\n  x\n  ```\n  r\n  :::\n- b\n",
			stored:   "- a\n  " + callout + "\n  > q\n\n  r\n  :::\n- b\n",
			blocks:   2,
		},
		{
			name:     "an ordered list that interrupts no paragraph",
			markdown: "- a\n  " + callout + "\n  q\n  ```\n  x\n  ```\n  2. r\n  :::\n- b\n",
			stored:   "- a\n  " + callout + "\n  q\n\n  2. r\n  :::\n- b\n",
			blocks:   2,
		},
		{
			name:     "a list whose first item is empty",
			markdown: "- a\n  " + callout + "\n  q\n  ```\n  x\n  ```\n  -\n  :::\n- b\n",
			stored:   "- a\n  " + callout + "\n  q\n\n  - \n  :::\n- b\n",
			blocks:   2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, test.markdown)
			settleCurrentGeneration(t, service, artifactID)

			code := blockIDOfType(t, liveTree(t, service, artifactID), "code_block")
			if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "delete", Block: code}},
				model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
				t.Fatalf("delete the code block: %v", err)
			}
			settleCurrentGeneration(t, service, artifactID)

			if markdown := latestVersionMarkdown(t, service, artifactID); markdown != test.stored {
				t.Fatalf("the stored version is %q, want %q", markdown, test.stored)
			}
			// The blocks the delete left are the blocks the stored version reads back as.
			back := liveTree(t, service, artifactID)
			if held := len(back.Children[0].Children[0].Children[1].Children); held != test.blocks {
				t.Fatalf("the typed block reads back holding %d blocks, want %d", held, test.blocks)
			}
		})
	}
}

// blockIDOfType is the block id of the first block of kind in tree.
func blockIDOfType(t *testing.T, tree *pmdoc.Node, kind string) string {
	t.Helper()
	var found string
	var walk func(node *pmdoc.Node)
	walk = func(node *pmdoc.Node) {
		if found != "" {
			return
		}
		if node.Type == kind {
			found, _ = node.Attrs[pmdoc.BlockIDAttr].(string)
			return
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(tree)
	if found == "" {
		t.Fatalf("no %s block with an id in the document", kind)
	}
	return found
}
