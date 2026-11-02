package refs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// insertAnchoredReferenceAsk records an ask anchored on anchorArtifactID, whatever that text is,
// optionally naming an ask block on blockArtifactID. The anchor is written as the server writes
// one, so the only difference between the rows a test seeds is the artifact_id it holds.
func insertAnchoredReferenceAsk(t *testing.T, tx pgx.Tx, id, issueKey, anchorArtifactID string, blockArtifactID *string) string {
	t.Helper()
	anchor, err := json.Marshal(model.Anchor{ArtifactID: anchorArtifactID, MarkID: id, Version: 1, Quote: "brown"})
	if err != nil {
		t.Fatalf("encode the ask's anchor: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
		insert into asks (id, issue_key, author, question, anchor, block_artifact_id)
		values ($1, $2, '{"kind":"user","id":"alice"}', 'Anchored ask', $3, $4)
	`, id, issueKey, anchor, blockArtifactID); err != nil {
		t.Fatalf("insert the ask anchored on %q: %v", anchorArtifactID, err)
	}
	return id
}

// An ask an earlier release could store holds an anchor artifact_id that is not a uuid beside a
// block_artifact_id: asks_anchor_artifact (migration 0032) casts the anchor only where
// block_artifact_id is null, so an ask carrying both columns holds an artifact_id no index has
// ever cast. loadAskNodes guards its cast with store.CastableUUID, so that row reads rather than
// answering SQLSTATE 22P02 for the whole read: an artifact_id that casts to no uuid names no
// document, so the ask belongs to its ask block's document, as it would with no artifact_id at
// all and as both search arms link it, at that branch's version - none here, since Dispatch has
// recorded no text of the block and so does not know the version that first held it. The ask
// anchored canonically beside it keeps the document parent and version it had.
func TestAskNodeAnswersOverAnAnchorArtifactIDThatDoesNotCast(t *testing.T) {
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
	canonical := insertAnchoredReferenceAsk(t, tx, "00000000-0000-4000-8000-0000000000a1", "CORE-1", specID, nil)
	uncastable := insertAnchoredReferenceAsk(t, tx, "00000000-0000-4000-8000-0000000000a2", "CORE-1", "not-a-uuid", &blockID)

	node, found, err := Node(ctx, tx, "ask", canonical, false)
	if err != nil || !found {
		t.Fatalf("read the canonically anchored ask node: found=%t err=%v", found, err)
	}
	wantParent := model.NodeParent{Kind: "artifact", ID: specID, Ref: "dispatch://" + specRefKey}
	if node.Parent == nil || *node.Parent != wantParent {
		t.Fatalf("canonically anchored ask parent = %#v, want %#v", node.Parent, wantParent)
	}
	if node.ParentVersion == nil || *node.ParentVersion != 1 {
		t.Fatalf("canonically anchored ask parent version = %v, want the anchored version 1", node.ParentVersion)
	}

	node, found, err = Node(ctx, tx, "ask", uncastable, false)
	if err != nil || !found {
		t.Fatalf("read the ask whose anchor artifact_id does not cast: found=%t err=%v", found, err)
	}
	wantBlock := model.NodeParent{Kind: "artifact", ID: blockID, Ref: "dispatch://CORE/artifact/handbook-md"}
	if node.Parent == nil || *node.Parent != wantBlock {
		t.Fatalf("uncastable anchor ask parent = %#v, want its ask block's document %#v", node.Parent, wantBlock)
	}
	if node.ParentVersion != nil {
		t.Fatalf("uncastable anchor ask parent version = %v, want none: the anchor's version is not this document's", *node.ParentVersion)
	}
}
