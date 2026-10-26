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
// reviews without the column, and its trigger gives such a review the sequence of the artifact.*
// event its own transaction appended, at the commit, and leaves a review whose transaction
// appended none without one; its check is added `not valid`, so the migration reads no row of the
// table it locks; and a sequence below 1 is refused all the same.
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
	// The release before the column inserts a review naming no sequence, then appends its event in
	// the same transaction; the trigger gives the review that event's sequence when it commits.
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the old release's review: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var oldRelease string
	if err := tx.QueryRow(ctx, review+` returning id::text`, artifactID).Scan(&oldRelease); err != nil {
		t.Fatalf("insert the old release's review: %v", err)
	}
	if _, err := tx.Exec(ctx, `insert into events (issue_key, seq, type, actor, payload, notify)
		values ('TEST-1', 7, 'artifact.changes_requested', '{"kind":"user","id":"alice"}',
		        jsonb_build_object('artifact_id', $1::text, 'version', 1), true)`, artifactID); err != nil {
		t.Fatalf("append the old release's review event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the old release's review: %v", err)
	}
	var oldReleaseSeq int64
	if err := store.Pool.QueryRow(ctx, `select coalesce(event_seq, 0) from artifact_reviews where id = $1`, oldRelease).Scan(&oldReleaseSeq); err != nil {
		t.Fatalf("read the old release's review sequence: %v", err)
	}
	if oldReleaseSeq != 7 {
		t.Fatalf("the old release's review sequence = %d (0 is none), want its event's 7", oldReleaseSeq)
	}
	// A review whose transaction appends no event stays without a sequence.
	if _, err := store.Pool.Exec(ctx, review, artifactID); err != nil {
		t.Fatalf("insert a review with no event after the migration: %v", err)
	}
	var unsequenced int
	if err := store.Pool.QueryRow(ctx, `select count(*) from artifact_reviews where artifact_id = $1 and event_seq is null`, artifactID).Scan(&unsequenced); err != nil {
		t.Fatalf("count reviews with no sequence: %v", err)
	}
	if unsequenced != 2 {
		t.Fatalf("reviews with no sequence = %d, want two: the one before the column and the one with no event", unsequenced)
	}
	if _, err := store.Pool.Exec(ctx, `update artifact_reviews set event_seq = 0 where artifact_id = $1`, artifactID); err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("a review sequence of zero = %v, want a check constraint to refuse it", err)
	}
	// `not valid` is what keeps the migration off the rows: it writes the catalog alone, while an
	// inline check would read every row under the table's ACCESS EXCLUSIVE lock.
	var validated bool
	if err := store.Pool.QueryRow(ctx, `select convalidated from pg_constraint where conname = 'artifact_reviews_event_seq_positive'
		and conrelid = 'artifact_reviews'::regclass`).Scan(&validated); err != nil {
		t.Fatalf("read the review sequence's check constraint: %v", err)
	}
	if validated {
		t.Fatalf("the review sequence's check constraint is validated, want it added not valid")
	}
}
