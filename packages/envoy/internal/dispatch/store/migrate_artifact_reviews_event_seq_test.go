package store

import (
	"context"
	"strings"
	"testing"
)

// artifactReviewsEventSeqVersion is the migration that adds the sequence of each review's
// artifact.* event to its row.
const artifactReviewsEventSeqVersion = 117

// TestMigrationAddsTheReviewsEventSequence: a review recorded before the column reads back with no
// sequence, so the approval read gives it its time alone; the release before it keeps inserting
// reviews without the column; and a sequence below 1 is refused.
func TestMigrationAddsTheReviewsEventSequence(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, artifactReviewsEventSeqVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", artifactReviewsEventSeqVersion-1, err)
	}
	var artifactID string
	for _, seed := range []string{
		`insert into projects (key, name) values ('TEST', 'Test')`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('TEST-1', 'TEST', 1, 'Spec', '{"kind":"user","id":"alice"}', 'U')`,
	} {
		if _, err := store.Pool.Exec(ctx, seed); err != nil {
			t.Fatalf("seed before the migration: %v", err)
		}
	}
	if err := store.Pool.QueryRow(ctx, `insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('TEST-1', 'TEST', 'spec', 'spec.md', 'doc', '{"kind":"user","id":"alice"}') returning id::text`).Scan(&artifactID); err != nil {
		t.Fatalf("seed a document before the migration: %v", err)
	}
	review := `insert into artifact_reviews (artifact_id, version, state, actor, reason) values ($1, 1, 'changes_requested', '{"kind":"user","id":"alice"}', 'Name the rollback path.')`
	if _, err := store.Pool.Exec(ctx, review, artifactID); err != nil {
		t.Fatalf("seed a review before the migration: %v", err)
	}
	if err := store.migrate(ctx, migrationsThrough(t, artifactReviewsEventSeqVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", artifactReviewsEventSeqVersion, err)
	}
	// The release before the column inserts a review naming no sequence.
	if _, err := store.Pool.Exec(ctx, review, artifactID); err != nil {
		t.Fatalf("insert a review without its sequence after the migration: %v", err)
	}
	var unsequenced int
	if err := store.Pool.QueryRow(ctx, `select count(*) from artifact_reviews where artifact_id = $1 and event_seq is null`, artifactID).Scan(&unsequenced); err != nil {
		t.Fatalf("count reviews with no sequence: %v", err)
	}
	if unsequenced != 2 {
		t.Fatalf("reviews with no sequence = %d, want both: the one before the column and the one the old release wrote", unsequenced)
	}
	if _, err := store.Pool.Exec(ctx, `update artifact_reviews set event_seq = 0 where artifact_id = $1`, artifactID); err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("a review sequence of zero = %v, want a check constraint to refuse it", err)
	}
}
