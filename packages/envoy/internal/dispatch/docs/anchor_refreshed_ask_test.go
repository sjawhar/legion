package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// anchorDocument is the document aa resolves to in an ask read, read straight from artifacts, so a
// test compares the fields the read resolves against the row it resolves them from.
func anchorDocument(t *testing.T, service *Service, artifactID string) model.AskAnchorArtifact {
	t.Helper()
	var document model.AskAnchorArtifact
	if err := service.store.Pool.QueryRow(context.Background(), `
		select project_key, slug, name, is_primary from artifacts where id = $1
	`, artifactID).Scan(&document.Project, &document.Slug, &document.Name, &document.Primary); err != nil {
		t.Fatalf("read document %q: %v", artifactID, err)
	}
	return document
}

// An ask an earlier release could store holds an anchor artifact_id that is not a uuid beside a
// block_artifact_id: asks_anchor_artifact (migration 0032) casts the anchor only where
// block_artifact_id is null, so an ask carrying both columns holds an artifact_id no index has
// ever cast. loadAnchorRefreshedAsk guards its cast with store.CastableUUID, so the refresh event
// such a row would need is built - with no anchor document, which is what its anchor names -
// instead of the read answering SQLSTATE 22P02; the ask anchored canonically beside it keeps the
// anchor document the read resolved before.
func TestLoadAnchorRefreshedAskAnswersOverAnAnchorArtifactIDThatDoesNotCast(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The quick brown fox")
	blockDocument := createIssueDocument(t, service.store, 2, "# Block document")
	const canonical, uncastable = "00000000-0000-4000-8000-0000000000a1", "00000000-0000-4000-8000-0000000000a2"
	insertAnchorRow(t, service, artifactID, "ask", canonical, "canonical", "brown")
	insertAnchorRow(t, service, artifactID, "ask", uncastable, "uncastable", "brown")
	ctx := context.Background()
	if _, err := service.store.Pool.Exec(ctx, `
		update asks
		set anchor = jsonb_set(anchor, '{artifact_id}', '"not-a-uuid"'), block_artifact_id = $2
		where id = $1
	`, uncastable, blockDocument); err != nil {
		t.Fatalf("store the anchor an earlier release could write: %v", err)
	}

	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the refreshed ask read: %v", err)
	}
	defer tx.Rollback(ctx)

	ask, err := loadAnchorRefreshedAsk(ctx, tx, canonical)
	if err != nil {
		t.Fatalf("load the canonically anchored refreshed ask: %v", err)
	}
	want := anchorDocument(t, service, artifactID)
	if ask.AnchorArtifact == nil || *ask.AnchorArtifact != want {
		t.Fatalf("canonically anchored ask's anchor document = %#v, want %#v", ask.AnchorArtifact, want)
	}

	ask, err = loadAnchorRefreshedAsk(ctx, tx, uncastable)
	if err != nil {
		t.Fatalf("load the refreshed ask whose anchor artifact_id does not cast: %v", err)
	}
	if ask.AnchorArtifact != nil {
		t.Fatalf("uncastable anchor ask's anchor document = %#v, want none: its anchor names no document", *ask.AnchorArtifact)
	}
	if ask.BlockArtifact == nil || ask.BlockArtifact.ID != blockDocument {
		t.Fatalf("uncastable anchor ask's block document = %#v, want %q", ask.BlockArtifact, blockDocument)
	}
}
