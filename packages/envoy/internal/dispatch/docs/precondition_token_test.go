package docs

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A conditional edit encodes its tree once: checkEditPrecondition hands its batch the document
// token it computed, and the batch takes it as the token of the tree it starts from. Between the
// two the edit stamps the block ids the write carries (stampAskBlockIDs, pmdoc.EnsureBlockIDs),
// so the reuse is exact only because the token leaves block identity out. These tests hold both
// halves of that: the stamp changes no token, and the check answers the token the batch would
// have encoded for itself.

func TestStampingBlockIDsChangesNoDocumentToken(t *testing.T) {
	for _, markdown := range []string{
		"Intro.\n\nBody.\n",
		"# Heading\n\n- one\n- two\n\n| h |\n| --- |\n| cell |\n",
		"```go\nfmt.Println(1)\n```\n\nAfter the code.\n",
	} {
		t.Run(markdown, func(t *testing.T) {
			tree, err := parseInput(markdown)
			if err != nil {
				t.Fatal(err)
			}
			// A document a browser wrote can carry a block with no id; this one is made so by
			// taking the ids off, which is the state the stamp exists for.
			pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
				delete(node.Attrs, pmdoc.BlockIDAttr)
				return true
			})
			before, err := nodeToken(tree)
			if err != nil {
				t.Fatal(err)
			}
			if repairs := pmdoc.BlockIDRepairCount(tree); repairs == 0 {
				t.Fatal("the document carries every block id, so the stamp would change nothing by itself")
			}
			pmdoc.EnsureBlockIDs(tree)
			after, err := nodeToken(tree)
			if err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatalf("the stamp changed the document token: %s before, %s after", before, after)
			}
		})
	}
}

// The token a document precondition's check answers is the batch's own `before`: the verdict an
// edit reports for an unchanged document is that same token, so a check answering another one
// would change every outcome the route serves.
func TestCheckEditPreconditionAnswersTheBatchToken(t *testing.T) {
	tree, err := parseInput("Intro.\n\nBody.\n")
	if err != nil {
		t.Fatal(err)
	}
	pmdoc.EnsureBlockIDs(tree)
	token, err := nodeToken(tree)
	if err != nil {
		t.Fatal(err)
	}
	blockID, _ := tree.Children[1].Attrs[pmdoc.BlockIDAttr].(string)
	blockToken, err := nodeToken(tree.Children[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		precondition model.EditPrecondition
	}{
		{"document", model.EditPrecondition{Document: token}},
		{"blocks", model.EditPrecondition{Blocks: []model.EditBlockPrecondition{{ID: blockID, Token: blockToken}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			answered, err := checkEditPrecondition(tree, test.precondition)
			if err != nil {
				t.Fatalf("checkEditPrecondition: %v", err)
			}
			if answered != token {
				t.Fatalf("checkEditPrecondition answered %s, want the document token %s", answered, token)
			}
			// The batch takes that token as its own: an edit that changes nothing reports it.
			batch, err := applyBatch(newBatchTree(tree), []model.EditOp{{Op: "replace", Find: "Body.", With: "Body."}}, nil, answered)
			if err != nil {
				t.Fatal(err)
			}
			outcome, _, err := batch.outcome(1)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Changed || outcome.Token != token {
				t.Fatalf("outcome = {changed %t, token %s}, want {false, %s}", outcome.Changed, outcome.Token, token)
			}
		})
	}
}
