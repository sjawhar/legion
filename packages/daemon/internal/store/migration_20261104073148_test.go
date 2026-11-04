package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// issueMemoryGroupVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const issueMemoryGroupVersion = 20261104073148

// 20261104073148 adds the tree's memory group to the root's issue row: memory_group, the group
// drawn when the tree's lifecycle row was first created; memory_draw_mode, the project's mode in
// force at that draw; and memory_drawn_at, when it was drawn. The negative control is the draw the
// daemon makes once it draws groups, which names the columns before the migration and is refused.
//
// There is no backfill, by design: a row recorded before the migration has memory_drawn_at null,
// which means no group and no memory, so a tree open at this release keeps running as it has until
// now. This asserts that emptiness on such a row, and that each column then takes and keeps a
// value.
func TestTheMemoryColumnsArriveEmptyWith20261104073148(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, issueMemoryGroupVersion-1)
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'admitted', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatalf("record a root before the memory columns: %v", err)
	}
	// The write the daemon makes once it draws a tree's group, against the schema before the
	// migration: the row the migration exists for.
	_, err := store.Pool().Exec(ctx, `update issues set memory_group = 'memory', memory_draw_mode = 'random',
		memory_drawn_at = now() where key = 'LEGION-208'`)
	if err == nil || !strings.Contains(err.Error(), "memory_group") {
		t.Fatalf("drawing a tree's memory group before 20261104073148 = %v, want the column to be unknown", err)
	}
	t.Logf("before the migration the draw is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the memory columns: %v", err)
	}

	const read = "select memory_group, memory_draw_mode, memory_drawn_at from issues where key = 'LEGION-208'"
	var group, mode string
	var drawnAt *time.Time
	if err := store.Pool().QueryRow(ctx, read).Scan(&group, &mode, &drawnAt); err != nil {
		t.Fatalf("read the memory columns after 20261104073148: %v", err)
	}
	if group != "" || mode != "" || drawnAt != nil {
		t.Fatalf("a root recorded before the migration carries group %q drawn as %q at %v, want all three empty: "+
			"no backfill, so a tree opened before this release has no group", group, mode, drawnAt)
	}

	at := time.Date(2026, 11, 4, 7, 31, 48, 123456000, time.UTC)
	if _, err := store.Pool().Exec(ctx, `update issues set memory_group = 'memory', memory_draw_mode = 'random',
		memory_drawn_at = $1 where key = 'LEGION-208'`, at); err != nil {
		t.Fatalf("draw the tree's memory group after 20261104073148: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, read).Scan(&group, &mode, &drawnAt); err != nil {
		t.Fatalf("read the drawn group back: %v", err)
	}
	if group != "memory" || mode != "random" || drawnAt == nil || !drawnAt.Equal(at) {
		t.Fatalf("the root carries group %q drawn as %q at %v, want the drawn memory, random and %s", group, mode, drawnAt, at)
	}
	t.Logf("after the migration the three columns take and keep a draw")

	// The two words refuse null, so no row is left with a group or a mode that is neither a value
	// nor the empty string a tree with no memory carries.
	for _, column := range []string{"memory_group", "memory_draw_mode"} {
		if _, err := store.Pool().Exec(ctx, "update issues set "+column+" = null where key = 'LEGION-208'"); err == nil ||
			!strings.Contains(err.Error(), "null value") {
			t.Fatalf("setting %s to null = %v, want the not-null constraint to refuse it", column, err)
		}
	}
}
