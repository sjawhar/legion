package store

import (
	"context"
	"testing"
)

const asksAnchorArtifactIDVersion = 137

// 0137 builds asks_anchor_artifact_id over (anchor->>'artifact_id')::uuid on every anchored ask.
// asks_anchor_artifact (0032) casts the same text only where block_artifact_id is null, so an ask
// that names a block document and whose anchor's artifact_id does not cast is a row the schema
// before 0137 holds and 0137 refuses: the census counts it, so the pre-deploy gate refuses before
// the boot would, while an ask whose two columns both name documents counts nothing.
func TestCensusCountsAnAskWhoseBlockColumnHidesAnAnchorArtifactIDThatDoesNotCast(t *testing.T) {
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
	if got := censusCount(t, store, asksAnchorArtifactIDVersion); got != 0 {
		t.Fatalf("census over an ask whose anchor names a document = %d, want 0", got)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into asks (issue_key, author, question, anchor, block_artifact_id)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Uncastable anchor?',
			'{"artifact_id":"not-a-uuid","mark_id":"refused"}', $1::uuid)
	`, documentB); err != nil {
		t.Fatalf("seed an ask whose anchor's artifact_id does not cast: %v", err)
	}
	if got := censusCount(t, store, asksAnchorArtifactIDVersion); got != 1 {
		t.Fatalf("census = %d, want 1: the ask whose anchor names not-a-uuid is a row 0137's index refuses", got)
	}
	if err := store.migrate(ctx, migrationsThrough(t, asksAnchorArtifactIDVersion)); err == nil {
		t.Fatal("0137 applied over an anchored ask whose artifact_id does not cast")
	}
	if got := schemaVersion(t, store); got != asksAnchorArtifactIDVersion-1 {
		t.Fatalf("schema version after the refusal = %d, want %d", got, asksAnchorArtifactIDVersion-1)
	}
}
