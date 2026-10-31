package store

import (
	"context"
	"testing"
)

// A root re-parented under a running tree after it ran as its own tree is told once per parent
// value, and the parent it was told under is kept on the issue (record.Issue.ToldParent). 0074
// adds the column, leaving every row an older daemon wrote with no parent told, which the daemon
// reads as the empty string; a second telling under another parent is one update of the same row.
func TestTheToldParentIsAddedAt0074(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 73)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'a root that ran', 'done', 2, 'todo', 'A', 7)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var told *string
	if err := store.Pool().QueryRow(ctx, `select told_parent from issues where key = 'LEGION-208'`).Scan(&told); err != nil {
		t.Fatalf("read told_parent after 0074: %v", err)
	}
	if told != nil {
		t.Fatalf("told_parent of a row written before 0074 = %q, want it null: nothing has been told", *told)
	}
	for _, parent := range []string{"LEGION-300", "LEGION-400"} {
		if _, err := store.Pool().Exec(ctx, `update issues set told_parent = $1 where key = 'LEGION-208'`, parent); err != nil {
			t.Fatalf("record %s as the parent told: %v", parent, err)
		}
		if err := store.Pool().QueryRow(ctx, `select told_parent from issues where key = 'LEGION-208'`).Scan(&told); err != nil {
			t.Fatalf("read told_parent back: %v", err)
		}
		if told == nil || *told != parent {
			t.Fatalf("told_parent = %v, want %s", told, parent)
		}
	}
}
