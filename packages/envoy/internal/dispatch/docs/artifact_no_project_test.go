package docs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// LEGION-541 stores an artifact an agent's conversation owns with issue_key and project_key both
// null and ref_key agent/<session id>/<slug>; a Dispatch reverted past that change still meets such
// rows. Neither read here can reach one today: lockArtifactOwner serves document writes and
// settlement, which no write path on main runs on an artifact with no project, and LEGION-541
// refuses a markdown document on a conversation (ARTIFACT_INPUT), so none of its rows is a document
// either; and the anchor-refresh payload follows a comment's anchor, which only a document carries.
// Both read artifacts.project_key into a Go string all the same, so this gives the test's own database
// (storetest.Open's clone) the schema that change brings and holds both to a row with no project:
// an issue's document whose project is nulled, and a project document turned into the conversation
// shape.
func TestDocumentOwnerReadsServeARowWithNoProject(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	issueDocument := createDocument(t, database, "# Notes\n\nThe plan stands.\n")
	conversationFile := createProjectDocument(t, database, "# Shared\n")
	for _, statement := range []string{
		`alter table artifacts alter column project_key drop not null`,
		`alter table artifacts alter column ref_key drop expression`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("simulate artifacts with no project (%s): %v", statement, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `update artifacts set project_key = null where id = $1`, issueDocument); err != nil {
		t.Fatalf("null the issue document's project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		update artifacts set project_key = null, ref_key = 'agent/session-0123456789abcdef/' || slug where id = $1
	`, conversationFile); err != nil {
		t.Fatalf("give the project document the conversation shape: %v", err)
	}
	commentIDs := map[string]string{
		issueDocument:    "00000000-0000-4000-8000-000000000541",
		conversationFile: "00000000-0000-4000-8000-000000000542",
	}
	for artifactID, commentID := range commentIDs {
		anchor, err := json.Marshal(model.Anchor{ArtifactID: artifactID, MarkID: commentID, Version: 1, Quote: "plan"})
		if err != nil {
			t.Fatalf("encode anchor: %v", err)
		}
		issueKey, ownerArtifact := new("DOC-1"), (*string)(nil)
		if artifactID == conversationFile {
			issueKey, ownerArtifact = nil, &artifactID
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into comments (id, issue_key, artifact_id, author, body, anchor)
			values ($1, $2, $3, '{"kind":"user","id":"alice"}', 'Anchored comment', $4)
		`, commentID, issueKey, ownerArtifact, anchor); err != nil {
			t.Fatalf("insert anchored comment: %v", err)
		}
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	for artifactID, wantIssue := range map[string]bool{issueDocument: true, conversationFile: false} {
		owner, open, err := lockArtifactOwner(ctx, tx, artifactID)
		if err != nil {
			t.Errorf("lockArtifactOwner(%s): %v", artifactID, err)
			continue
		}
		if owner.Project != "" || (owner.IssueKey != nil) != wantIssue || !open {
			t.Errorf("lockArtifactOwner(%s) = %#v open %t, want no project, issue owned %t, open", artifactID, owner, open, wantIssue)
		}
	}
	for artifactID, commentID := range commentIDs {
		payload, err := loadAnchorRefreshedCommentPayload(ctx, tx, commentID)
		if err != nil {
			t.Errorf("loadAnchorRefreshedCommentPayload on %s: %v", artifactID, err)
			continue
		}
		if payload.ProjectKey != "" || payload.ArtifactName != "document.md" || payload.ID != commentID {
			t.Errorf("loadAnchorRefreshedCommentPayload on %s = project %q name %q comment %q, want no project", artifactID, payload.ProjectKey, payload.ArtifactName, payload.ID)
		}
	}
}
