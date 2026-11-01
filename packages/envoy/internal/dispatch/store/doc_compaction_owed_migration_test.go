package store

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// beforeDocCompactionOwed is the last migration before 0160_doc_compaction_owed.
const beforeDocCompactionOwed = 159

// 0160 names in doc_compaction_owed every document stored before it with more than one
// doc_updates row, and no other: a document of one row, or of none, has nothing a compaction could
// fold.
func TestMigrate0160NamesEveryDocumentOfMoreThanOneRow(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeDocCompactionOwed)
	documents := map[string]int{"none": 0, "one": 1, "two": 2, "three": 3}
	ids := map[string]string{}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Spec', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed the issue at 0159: %v", err)
	}
	for slug, rows := range documents {
		var id string
		if err := store.Pool.QueryRow(ctx, `
			with a as (
				insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
				values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
				returning id
			), u as (
				insert into doc_updates (artifact_id, version, update, content_changed)
				select a.id, v, '\x00'::bytea, true from a, generate_series(1, $2::int) v
			)
			select id::text from a
		`, slug, rows).Scan(&id); err != nil {
			t.Fatalf("seed the document of %d rows at 0159: %v", rows, err)
		}
		ids[slug] = id
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through 0160: %v", err)
	}

	rows, err := store.Pool.Query(ctx, `select artifact_id::text from doc_compaction_owed`)
	if err != nil {
		t.Fatalf("read doc_compaction_owed after 0160: %v", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read doc_compaction_owed after 0160: %v", err)
	}
	slices.Sort(owed)
	want := []string{ids["two"], ids["three"]}
	slices.Sort(want)
	if !slices.Equal(owed, want) {
		t.Fatalf("doc_compaction_owed names %v after 0160, want the documents of two and three rows %v", owed, want)
	}
}
