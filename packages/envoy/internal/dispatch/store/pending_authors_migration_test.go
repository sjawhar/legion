package store

import (
	"context"
	"testing"
)

// 0084 adds doc_pending_authors and a nullable last_actor on doc_settlements_pending. A database
// that already holds documents, updates and owed settlements keeps every row: each owed
// settlement reads back with no latest edit source, and the new table starts empty and takes an
// author per writing update.
func TestMigrate0084KeepsEveryOwedSettlementAndStartsWithNoPendingAuthors(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 83)
	var artifactID string
	if err := store.Pool.QueryRow(ctx, `
		with p as (insert into projects (key, name) values ('CORE', 'Core') returning key),
		i as (insert into issues (key, project_key, number, title, created_by, rank) select 'CORE-1', key, 1, 'Spec', '{"kind":"user","id":"alice"}', 'U' from p returning key),
		a as (insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by) select key, 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}' from i returning id),
		u as (insert into doc_updates (artifact_id, version, update, content_changed) select id, 1, '\x00'::bytea, true from a),
		s as (insert into doc_settlements_pending (artifact_id) select id from a)
		select id::text from a
	`).Scan(&artifactID); err != nil {
		t.Fatalf("seed a document owing a settlement at 0083: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through 0084: %v", err)
	}

	var owed int
	var lastActorNull bool
	if err := store.Pool.QueryRow(ctx, `
		select count(*), bool_and(last_actor is null) from doc_settlements_pending where artifact_id = $1
	`, artifactID).Scan(&owed, &lastActorNull); err != nil {
		t.Fatalf("read the owed settlement after 0084: %v", err)
	}
	if owed != 1 || !lastActorNull {
		t.Fatalf("owed settlements after 0084 = %d (last_actor null %t), want the one row with no latest edit source", owed, lastActorNull)
	}
	var pending int
	if err := store.Pool.QueryRow(ctx, `select count(*) from doc_pending_authors`).Scan(&pending); err != nil {
		t.Fatalf("count pending authors after 0084: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending authors after 0084 = %d, want none", pending)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into doc_pending_authors (artifact_id, actor_kind, actor_id, actor, written_through) values
			($1, 'user', 'bob', '{"kind":"user","id":"bob"}', 1),
			($1, 'user', 'bob', '{"kind":"user","id":"bob"}', 2)
	`, artifactID); err != nil {
		t.Fatalf("record one author at two writing updates after 0084: %v", err)
	}
}
