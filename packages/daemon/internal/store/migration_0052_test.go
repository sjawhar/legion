package store

import (
	"context"
	"strings"
	"testing"
)

// 0052 adds parked_decisions: a root keeps one row per Dispatch reference, and the rows go with
// the issue they name.
func TestParkedDecisionsArriveWith0052AndGoWithTheirRoot(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 51)
	var exists bool
	if err := store.Pool().QueryRow(ctx, `select to_regclass('parked_decisions') is not null`).Scan(&exists); err != nil {
		t.Fatalf("look for parked_decisions at 0051: %v", err)
	}
	if exists {
		t.Fatal("parked_decisions exists at 0051, want it added by 0052")
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var key []string
	if err := store.Pool().QueryRow(ctx, `select array_agg(a.attname::text order by k.ord)
		from pg_index i
		cross join lateral unnest(i.indkey) with ordinality as k(attnum, ord)
		join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.attnum
		where i.indrelid = 'parked_decisions'::regclass and i.indisprimary`).Scan(&key); err != nil {
		t.Fatalf("read the primary key of parked_decisions: %v", err)
	}
	if strings.Join(key, ",") != "root,ref" {
		t.Fatalf("parked_decisions primary key = %v, want (root, ref)", key)
	}

	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-210', 'LEGION-210', 'LEGION', 'parked root', 'done', 1, 'backlog', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into parked_decisions (root, ref, decision, actor, at)
		values ('LEGION-210', 'dispatch://LEGION-210/spec', 'design-approved', 'user:alice', now())`); err != nil {
		t.Fatalf("keep a decision: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into parked_decisions (root, ref, decision, actor, at)
		values ('LEGION-210', 'dispatch://LEGION-210/spec', 'design-changes-requested', 'user:bob', now())`); err == nil ||
		!strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("a second row for one ref = %v, want the primary key to refuse it", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into parked_decisions (root, ref, decision, actor, at)
		values ('LEGION-404', 'dispatch://LEGION-404/spec', 'design-approved', 'user:alice', now())`); err == nil ||
		!strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("a decision on an unrecorded root = %v, want the foreign key to refuse it", err)
	}
	if _, err := store.Pool().Exec(ctx, `delete from issues where key = 'LEGION-210'`); err != nil {
		t.Fatalf("delete the root: %v", err)
	}
	var kept int
	if err := store.Pool().QueryRow(ctx, `select count(*) from parked_decisions`).Scan(&kept); err != nil {
		t.Fatalf("count the kept decisions: %v", err)
	}
	if kept != 0 {
		t.Fatalf("%d decisions kept after their root was deleted, want none", kept)
	}
}
