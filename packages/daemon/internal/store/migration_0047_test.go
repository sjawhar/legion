package store

import (
	"context"
	"strings"
	"testing"
)

// The architect accepts before the merge and the merge closes the issue, so production_check is no
// phase. An issue the old daemon left there, or held from there, has its pull request merged and
// stands at awaiting_merge after 0047, where the new daemon's boot closes it; the phase constraints
// then admit accepting and refuse production_check.
func TestTheOldPostMergePhaseMigratesToAwaitingMergeAndTheConstraintsSwap(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 46)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, held_from)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'checking root', 'production_check', 1, 'retro', 'A', 1, null),
		       ('LEGION-209', 'LEGION-208', 'LEGION', 'held child', 'held', 1, 'retro', 'B', 1, 'production_check'),
		       ('LEGION-210', 'LEGION-208', 'LEGION', 'reviewing child', 'reviewing', 1, 'needs_review', 'C', 1, null)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, want := range []struct{ key, phase, heldFrom string }{
		{"LEGION-208", "awaiting_merge", ""},
		{"LEGION-209", "held", "awaiting_merge"},
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
			t.Fatalf("%s after 0047 = phase %q held_from %q, want %q and %q", want.key, phase, got, want.phase, want.heldFrom)
		}
	}
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-211', 'LEGION-208', 'LEGION', 'accepted child', 'accepting', 1, 'needs_review', 'D', 1)`); err != nil {
		t.Fatalf("insert an issue in accepting: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `update issues set phase = 'held', held_from = 'accepting' where key = 'LEGION-211'`); err != nil {
		t.Fatalf("hold an issue from accepting: %v", err)
	}
	for _, refused := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-212', 'LEGION-208', 'LEGION', 'old phase', 'production_check', 1, 'retro', 'E', 1)`,
		`update issues set held_from = 'production_check' where key = 'LEGION-211'`,
	} {
		_, err := store.Pool().Exec(ctx, refused)
		if err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Fatalf("production_check after 0047 = %v, want the check constraint to refuse it", err)
		}
	}
}
