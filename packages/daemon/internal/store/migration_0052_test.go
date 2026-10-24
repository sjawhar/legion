package store

import (
	"context"
	"strings"
	"testing"
)

// 0052 adds parked_decisions and readmitted_approvals: a root keeps one decision row per Dispatch
// reference and one approval below its spec's latest version, and both go with the issue they name.
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
	for _, refused := range []struct{ why, statement, error string }{
		{"an approval at the latest version", `insert into readmitted_approvals (root, approved_version, latest_version) values ('LEGION-210', 3, 3)`, "check constraint"},
		{"an approval of version 0", `insert into readmitted_approvals (root, approved_version, latest_version) values ('LEGION-210', 0, 3)`, "check constraint"},
		{"an approval on an unrecorded root", `insert into readmitted_approvals (root, approved_version, latest_version) values ('LEGION-404', 2, 3)`, "foreign key"},
	} {
		if _, err := store.Pool().Exec(ctx, refused.statement); err == nil || !strings.Contains(err.Error(), refused.error) {
			t.Fatalf("%s = %v, want %s to refuse it", refused.why, err, refused.error)
		}
	}
	if _, err := store.Pool().Exec(ctx, `insert into readmitted_approvals (root, approved_version, latest_version) values ('LEGION-210', 3, 4)`); err != nil {
		t.Fatalf("keep an older approval: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into readmitted_approvals (root, approved_version, latest_version) values ('LEGION-210', 2, 4)`); err == nil ||
		!strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("a second approval for one root = %v, want the primary key to refuse it", err)
	}
	if _, err := store.Pool().Exec(ctx, `delete from issues where key = 'LEGION-210'`); err != nil {
		t.Fatalf("delete the root: %v", err)
	}
	var kept, approvals int
	if err := store.Pool().QueryRow(ctx, `select (select count(*) from parked_decisions), (select count(*) from readmitted_approvals)`).Scan(&kept, &approvals); err != nil {
		t.Fatalf("count the kept rows: %v", err)
	}
	if kept != 0 || approvals != 0 {
		t.Fatalf("%d decisions and %d approvals kept after their root was deleted, want none", kept, approvals)
	}
}
