package store

import (
	"context"
	"strings"
	"testing"
)

// The daemon submits no merge and changes no merge setting, so the three outbox kinds of that
// merge have no executor left and the notices only their rows wrote have no writer. A row of each
// an older daemon queued is deleted at the upgrade rather than left to fail its decode for good,
// every other row is kept, and the kind check then refuses the three.
func TestTheDaemonsMergeRowsAndNoticesAreDroppedAt0077(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 76)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'merging', 1, 'retro', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ kind, payload string }{
		{"ready_merge", `{"number":42,"head":"ready-head"}`},
		{"auto_merge_disable", `{"number":42}`},
		{"merge_queue_publish", `{"role":"merge-queue","packet":"READY #42"}`},
		{"notice", `{"kind":"merge-refused","role":"architect"}`},
		{"notice", `{"kind":"merge-stalled","role":"architect"}`},
		{"notice", `{"kind":"merge-removed","role":"architect"}`},
		{"notice", `{"kind":"merge-waiting","role":"architect"}`},
		{"notice", `{"kind":"ready-refused","role":"architect"}`},
		{"notice", `{"kind":"phase-finished","role":"merger"}`},
		{"dispatch_message", `{"body":"READY #42 at head"}`},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', $2, 0, now(), '')`, row.kind, row.payload); err != nil {
			t.Fatalf("seed a %s row: %v", row.kind, err)
		}
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var kinds []string
	rows, err := store.Pool().Query(ctx, `select kind || ':' || coalesce(payload->>'kind', '') from outbox order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"notice:phase-finished", "dispatch_message:"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("the outbox after 0077 holds %v, want only %v", kinds, want)
	}
	for _, kind := range []string{"ready_merge", "auto_merge_disable", "merge_queue_publish"} {
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind); err == nil {
			t.Fatalf("a %s row was accepted after 0077, want the kind check to refuse it", kind)
		}
	}
	var index bool
	if err := store.Pool().QueryRow(ctx, `select exists (select 1 from pg_indexes
		where indexname = 'outbox_ready_merge_issue_head_unique')`).Scan(&index); err != nil {
		t.Fatal(err)
	}
	if index {
		t.Fatal("outbox_ready_merge_issue_head_unique still exists after 0077, want it dropped with its kind")
	}
}
