package docs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Migration 0071 stores an artifact an agent's conversation owns with issue_key and project_key both
// null and ref_key agent/<session id>/<slug>. Neither read here can reach one: lockArtifactOwner
// serves document writes and settlement, and a conversation holds no document (ARTIFACT_INPUT); the
// anchor-refresh payload follows a comment's anchor, which only a document carries. Both read
// artifacts.project_key into a Go string all the same, so both are held to the row as 0071 stores it
// (the shape a Dispatch from before 0071 meets, sjawhar/legion#1802); a document with its project
// nulled, the other no-project shape, is one artifacts_one_owner refuses.
func TestDocumentOwnerReadsServeARowWithNoProject(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	issueDocument := createDocument(t, database, "# Notes\n\nThe plan stands.\n")
	var pgErr *pgconn.PgError
	if _, err := database.Pool.Exec(ctx, `update artifacts set project_key = null where id = $1`, issueDocument); !errors.As(err, &pgErr) || pgErr.ConstraintName != "artifacts_one_owner" {
		t.Fatalf("nulling an issue document's project = %v, want artifacts_one_owner to refuse it", err)
	}
	const session = "session-0123456789abcdef"
	var conversationFile string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ($1, 'photo-png', 'document.md', 'image', jsonb_build_object('kind', 'session', 'id', $1::text))
		returning id::text
	`, session).Scan(&conversationFile); err != nil {
		t.Fatalf("store a conversation's file: %v", err)
	}
	const commentID = "00000000-0000-4000-8000-000000000542"
	anchor, err := json.Marshal(model.Anchor{ArtifactID: conversationFile, MarkID: commentID, Version: 1, Quote: "plan"})
	if err != nil {
		t.Fatalf("encode anchor: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (id, artifact_id, author, body, anchor)
		values ($1, $2, '{"kind":"user","id":"alice"}', 'Anchored comment', $3)
	`, commentID, conversationFile, anchor); err != nil {
		t.Fatalf("insert anchored comment: %v", err)
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	owner, open, err := lockArtifactOwner(ctx, tx, conversationFile)
	if err != nil {
		t.Fatalf("lockArtifactOwner: %v", err)
	}
	if owner.Project != "" || owner.IssueKey != nil || !open {
		t.Errorf("lockArtifactOwner = %#v open %t, want no project, no issue, open", owner, open)
	}
	payload, err := loadAnchorRefreshedCommentPayload(ctx, tx, commentID)
	if err != nil {
		t.Fatalf("loadAnchorRefreshedCommentPayload: %v", err)
	}
	if payload.ProjectKey != "" || payload.ArtifactName != "document.md" || payload.ID != commentID {
		t.Errorf("loadAnchorRefreshedCommentPayload = project %q name %q comment %q, want no project", payload.ProjectKey, payload.ArtifactName, payload.ID)
	}
}
