package store

import (
	"context"
	"strings"
	"testing"
)

// The merger merges, so `merging` runs to the merge and `awaiting_merge` is gone. An issue the old
// daemon left there, and a hold taken from there, stand in `merging` after 0072, where the new
// daemon reads them; the phase and held_from constraints then refuse the old value.
func TestAwaitingMergeMigratesToMergingAndTheConstraintsSwap(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 71)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, held_from)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'awaiting root', 'awaiting_merge', 1, 'retro', 'A', 1, null),
		       ('LEGION-209', 'LEGION-208', 'LEGION', 'held child', 'held', 1, 'retro', 'B', 1, 'awaiting_merge'),
		       ('LEGION-210', 'LEGION-208', 'LEGION', 'reviewing child', 'reviewing', 1, 'needs_review', 'C', 1, null)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, want := range []struct{ key, phase, heldFrom string }{
		{"LEGION-208", "merging", ""},
		{"LEGION-209", "held", "merging"},
		{"LEGION-210", "reviewing", ""},
	} {
		var phase string
		var heldFrom *string
		if err := store.Pool().QueryRow(ctx, `select phase, held_from from issues where key = $1`, want.key).Scan(&phase, &heldFrom); err != nil {
			t.Fatalf("read %s: %v", want.key, err)
		}
		got := ""
		if heldFrom != nil {
			got = *heldFrom
		}
		if phase != want.phase || got != want.heldFrom {
			t.Fatalf("%s after 0072 is phase %q held from %q, want %q and %q", want.key, phase, got, want.phase, want.heldFrom)
		}
	}
	for _, statement := range []string{
		`update issues set phase = 'awaiting_merge' where key = 'LEGION-210'`,
		`update issues set phase = 'held', held_from = 'awaiting_merge' where key = 'LEGION-210'`,
	} {
		if _, err := store.Pool().Exec(ctx, statement); err == nil {
			t.Fatalf("%q was accepted after 0072, want the constraint to refuse awaiting_merge", statement)
		} else if !strings.Contains(err.Error(), "check") {
			t.Fatalf("%q failed with %v, want a check constraint violation", statement, err)
		}
	}
	// The stopped columns keep what they hold, so a rollback to a daemon that reads them finds its
	// data: neither is dropped.
	for _, column := range []struct{ table, name string }{
		{"issues", "ready_pending_version"},
		{"phases", "merge_note"},
	} {
		var exists bool
		if err := store.Pool().QueryRow(ctx, `select exists (select 1 from information_schema.columns
			where table_name = $1 and column_name = $2)`, column.table, column.name).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("%s.%s is gone after 0072, want it stopped and left in place", column.table, column.name)
		}
	}
}
