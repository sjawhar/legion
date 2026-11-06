package refs

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An anchor artifact_id that spells the spec's id in upper case, or inside braces, still casts to
// the spec's uuid, so the ask node keeps the spec as its parent, as it did before the guard: only
// an artifact_id that cannot cast falls through to the ask's block document.
func TestAskNodeKeepsTheDocumentAnUpperCaseOrBracedAnchorNames(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	specID, specRefKey := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
	blockID, _ := createReferenceArtifact(t, tx, "", "CORE", "handbook-md")
	spellings := map[string]string{
		"upper case": insertAnchoredReferenceAsk(t, tx, "00000000-0000-4000-8000-0000000000b1", "CORE-1", strings.ToUpper(specID), &blockID),
		"braces":     insertAnchoredReferenceAsk(t, tx, "00000000-0000-4000-8000-0000000000b2", "CORE-1", "{"+specID+"}", &blockID),
	}
	wantParent := model.NodeParent{Kind: "artifact", ID: specID, Ref: "dispatch://" + specRefKey}
	for spelling, askID := range spellings {
		node, found, err := Node(ctx, tx, "ask", askID, false)
		if err != nil || !found {
			t.Fatalf("read the %s ask node: found=%t err=%v", spelling, found, err)
		}
		if node.Parent == nil || *node.Parent != wantParent {
			t.Errorf("the %s anchor's ask parent = %#v, want the spec %#v", spelling, node.Parent, wantParent)
		}
	}
}
