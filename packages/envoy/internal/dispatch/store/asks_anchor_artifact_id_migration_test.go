package store

import (
	"context"
	"strings"
	"testing"
)

const asksAnchorArtifactIDVersion = 148

// 0148 must boot over any data a schema before it accepted. asks_anchor_artifact (0032) casts
// anchor->>'artifact_id' only where block_artifact_id is null, so an ask holding both columns can
// carry an artifact_id no index has ever cast, and production's asks cannot be counted to show
// none does. The index expression's regex guard answers null for such a row instead of raising,
// so 0148 applies, refuses nothing (its census answers 0), and leaves those rows out of
// asks_anchor_artifact_id, where AsksOutsideAnchorArtifactIndex counts them for the boot log. A
// non-canonical spelling of a real document id is left out the same way.
func TestMigration0148BootsOverAnAskWhoseAnchorArtifactIDDoesNotCast(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, asksAnchorArtifactIDVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", asksAnchorArtifactIDVersion-1, err)
	}
	var documentA, documentB string
	if err := store.Pool.QueryRow(ctx, `
		with project as (
			insert into projects (key, name) values ('CORE', 'Core') returning key
		), issue as (
			insert into issues (key, project_key, number, title, created_by, rank)
			select 'CORE-1', key, 1, 'One', '{"kind":"user","id":"alice"}', 'U' from project returning key
		), documents as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			select key, 'CORE', slug, slug || '.md', 'doc', slug = 'a', '{"kind":"user","id":"alice"}'
			from issue, unnest(array['a', 'b']) slug
			returning id, slug
		)
		select (select id::text from documents where slug = 'a'), (select id::text from documents where slug = 'b')
	`).Scan(&documentA, &documentB); err != nil {
		t.Fatalf("seed two documents: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into asks (issue_key, author, question, anchor, block_artifact_id)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Both columns?',
			jsonb_build_object('artifact_id', $1::text, 'mark_id', 'kept'), $2::uuid)
	`, documentA, documentB); err != nil {
		t.Fatalf("seed an ask naming both documents: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into asks (issue_key, author, question, anchor, block_artifact_id)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Uncastable anchor?',
			'{"artifact_id":"not-a-uuid","mark_id":"uncastable"}', $1::uuid)
	`, documentB); err != nil {
		t.Fatalf("seed an ask whose anchor's artifact_id does not cast: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into asks (issue_key, author, question, anchor, block_artifact_id)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Upper-case anchor?',
			jsonb_build_object('artifact_id', $1::text, 'mark_id', 'upper'), $2::uuid)
	`, strings.ToUpper(documentA), documentB); err != nil {
		t.Fatalf("seed an ask whose anchor's artifact_id is upper-case: %v", err)
	}

	if got := censusCount(t, store, asksAnchorArtifactIDVersion); got != 0 {
		t.Fatalf("census = %d, want 0: 0148 refuses no row", got)
	}
	if err := store.migrate(ctx, migrationsThrough(t, asksAnchorArtifactIDVersion)); err != nil {
		t.Fatalf("migrate through %d over an anchored ask whose artifact_id does not cast: %v", asksAnchorArtifactIDVersion, err)
	}
	if got := schemaVersion(t, store); got != asksAnchorArtifactIDVersion {
		t.Fatalf("schema version = %d, want %d", got, asksAnchorArtifactIDVersion)
	}
	outside, err := store.AsksOutsideAnchorArtifactIndex(ctx)
	if err != nil {
		t.Fatalf("count the asks left out of asks_anchor_artifact_id: %v", err)
	}
	if outside != 2 {
		t.Fatalf("asks left out of asks_anchor_artifact_id = %d, want 2: the uncastable and the upper-case anchor", outside)
	}
	var indexed bool
	if err := store.Pool.QueryRow(ctx, `
		select exists (select 1 from pg_indexes where indexname = 'asks_anchor_artifact_id')
	`).Scan(&indexed); err != nil {
		t.Fatalf("look up asks_anchor_artifact_id: %v", err)
	}
	if !indexed {
		t.Fatal("asks_anchor_artifact_id was not created")
	}
}
