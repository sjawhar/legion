package store

import (
	"context"
	"testing"
	"time"
)

// issueMemoryCarriedVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const issueMemoryCarriedVersion = 20261104153554

// 20261104153554 adds memory_carried, the hand-over's own answer about a root handed over out of
// another tree, and settles with it the hand-overs the release before this one recorded. Those
// rows are the reason the backfill exists: handed_over_from has been written since 0059, the old
// trees ran with no memory because the setting did not exist then, no carry ever ran for them, and
// a child still waiting for a slot has no lifecycle row of its own, so its first open would
// otherwise draw it and give work that has been running without memory the overlays and the memory
// instructions at its next launch.
//
// The rule is record.Store.CarryMemory's: the old tree's tree_lifecycles row, matched on the
// project's normalized token and the tree, is the fact that the tree was opened at all, and a row
// that already carries a draw is left alone. The rows are inserted as the previous release leaves
// them, which is after 20261104073148 (the memory columns do not exist before it) and before this
// migration.
func TestThePreviousReleasesHandOversAreCarriedBy20261104153554(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, issueMemoryCarriedVersion-1)

	// Two of the three projects have a Dispatch key the runtime token is not equal to, so the
	// backfill's regexp_replace is what matches them rather than the key itself.
	for _, row := range []struct {
		key, project, from string
		drawn              bool
	}{
		// Handed over out of a tree that was opened: the hand-over settles it.
		{key: "LEGION-301", project: "LEGION-X", from: "LEGION-200"},
		// Handed over out of a tree nothing ever opened: its own first open draws it.
		{key: "LEGION-302", project: "LEGION", from: "LEGION-201"},
		// A root that left no tree at all.
		{key: "LEGION-303", project: "LEGION", from: ""},
		// Its old tree's key holds a lifecycle row, but under another project's token.
		{key: "LEGION-304", project: "LEGION", from: "LEGION-202"},
		// Already carries a draw of its own, which the backfill must not touch.
		{key: "LEGION-305", project: "Acme Widgets", from: "LEGION-203", drawn: true},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into issues
			(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over_from)
			values ($1, $1, $2, 'root', 'admitted', 1, 'in_progress', 'A', 1, $3)`,
			row.key, row.project, row.from); err != nil {
			t.Fatalf("record %s as the previous release leaves it: %v", row.key, err)
		}
		if !row.drawn {
			continue
		}
		at := time.Date(2026, 11, 4, 15, 35, 54, 0, time.UTC)
		if _, err := store.Pool().Exec(ctx, `update issues set memory_group = 'memory',
			memory_draw_mode = 'random', memory_drawn_at = $2 where key = $1`, row.key, at); err != nil {
			t.Fatalf("give %s a draw of its own: %v", row.key, err)
		}
	}
	// The trees those roots left, as the lifecycle table keyed by the normalized runtime token
	// holds them: LEGION-X is legionx and Acme Widgets is acmewidgets, and LEGION-201 is the tree
	// nothing ever opened, which has no row here at all.
	for _, life := range []struct{ project, tree string }{
		{project: "legionx", tree: "LEGION-200"},
		{project: "acmewidgets", tree: "LEGION-202"},
		{project: "acmewidgets", tree: "LEGION-203"},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority)
			values ($1, $2, 1, 'workflow')`, life.project, life.tree); err != nil {
			t.Fatalf("open the tree %s of %s: %v", life.tree, life.project, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the hand-over's answer: %v", err)
	}

	var opened, unopened, noTree, otherProject, drawn bool
	if err := store.Pool().QueryRow(ctx, `select
		(select memory_carried from issues where key = 'LEGION-301'),
		(select memory_carried from issues where key = 'LEGION-302'),
		(select memory_carried from issues where key = 'LEGION-303'),
		(select memory_carried from issues where key = 'LEGION-304'),
		(select memory_carried from issues where key = 'LEGION-305')`).
		Scan(&opened, &unopened, &noTree, &otherProject, &drawn); err != nil {
		t.Fatalf("read memory_carried after 20261104153554: %v", err)
	}
	if !opened {
		t.Errorf("a child the previous release handed over out of the opened tree LEGION-200 carries false, "+
			"want the backfill to settle it: its first open would draw it otherwise (project %q, token legionx)", "LEGION-X")
	}
	if unopened {
		t.Error("a child handed over out of LEGION-201, a tree with no lifecycle row, carries true, " +
			"want false: nothing was ever opened to hand an answer over, so its own first open draws it")
	}
	if noTree {
		t.Error("a root with an empty handed_over_from carries true, want false: it left no tree")
	}
	if otherProject {
		t.Error("a child whose old tree's key holds a lifecycle row under another project's token carries true, " +
			"want false: the row is matched on the project's token as well as the tree")
	}
	if drawn {
		t.Error("a root that already carries a draw of its own carries true, " +
			"want the backfill to leave a settled draw alone")
	}

	// The draw is still the one the row carried before the migration, and the four others still
	// carry none, so the backfill wrote the mark and nothing else.
	var group, mode string
	var drawnAt *time.Time
	if err := store.Pool().QueryRow(ctx, `select memory_group, memory_draw_mode, memory_drawn_at
		from issues where key = 'LEGION-305'`).Scan(&group, &mode, &drawnAt); err != nil {
		t.Fatalf("read the draw of LEGION-305 back: %v", err)
	}
	if group != "memory" || mode != "random" || drawnAt == nil {
		t.Errorf("LEGION-305 carries group %q drawn as %q at %v, want its own draw kept", group, mode, drawnAt)
	}
	var drawsLeft int
	if err := store.Pool().QueryRow(ctx, `select count(*) from issues
		where key <> 'LEGION-305' and (memory_group <> '' or memory_draw_mode <> '' or memory_drawn_at is not null)`).
		Scan(&drawsLeft); err != nil {
		t.Fatalf("count the draws the backfill wrote: %v", err)
	}
	if drawsLeft != 0 {
		t.Errorf("%d rows the backfill settled carry a draw, want none: those old trees ran with no memory, "+
			"so the carry is the mark alone", drawsLeft)
	}
}
