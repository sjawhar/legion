package store

import (
	"context"
	"testing"
)

// artifactsProjectDocumentsVersion is the migration that adds artifacts_project_documents, the
// partial index of a project's unlinked documents a copied ask block's lookups read (LEGION-651).
const artifactsProjectDocumentsVersion = 87

// TestMigrationIndexesAProjectsUnlinkedDocuments runs the shipped migration files, never a constant
// copy of them: a database at the version before, holding an issue's spec, a project document and
// an agent conversation's file, migrates to this one, keeps all three rows, and indexes only the
// project document, under the predicate the copy lookups use.
func TestMigrationIndexesAProjectsUnlinkedDocuments(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, artifactsProjectDocumentsVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", artifactsProjectDocumentsVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by) values
			('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}'),
			(null, 'CORE', 'notes', 'notes.md', 'doc', false, '{"kind":"user","id":"alice"}');
		insert into artifacts (session_id, slug, name, kind, created_by)
			values ('01a1058e-f14f', 'shot-png', 'shot.png', 'image', '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed artifacts stored before the migration: %v", err)
	}

	if err := store.migrate(ctx, migrationsThrough(t, artifactsProjectDocumentsVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", artifactsProjectDocumentsVersion, err)
	}
	var artifacts int
	if err := store.Pool.QueryRow(ctx, `select count(*) from artifacts`).Scan(&artifacts); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if artifacts != 3 {
		t.Errorf("artifacts = %d after the migration, want the 3 stored before it", artifacts)
	}
	var definition string
	if err := store.Pool.QueryRow(ctx, `
		select indexdef from pg_indexes where tablename = 'artifacts' and indexname = 'artifacts_project_documents'
	`).Scan(&definition); err != nil {
		t.Fatalf("read artifacts_project_documents: %v", err)
	}
	const want = "CREATE INDEX artifacts_project_documents ON public.artifacts USING btree (project_key) WHERE ((issue_key IS NULL) AND (session_id IS NULL))"
	if definition != want {
		t.Errorf("artifacts_project_documents = %q, want %q", definition, want)
	}
	// The index holds the project document alone: the issue's spec and the conversation's file
	// fall outside its predicate.
	var indexed []string
	rows, err := store.Pool.Query(ctx, `
		select slug from artifacts where issue_key is null and session_id is null and project_key = 'CORE'
	`)
	if err != nil {
		t.Fatalf("read the project's unlinked documents: %v", err)
	}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			t.Fatal(err)
		}
		indexed = append(indexed, slug)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(indexed) != 1 || indexed[0] != "notes" {
		t.Errorf("the project's unlinked documents = %v, want [notes]", indexed)
	}
}
