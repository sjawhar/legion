package store

import (
	"context"
	"strings"
	"testing"
)

// 0058 adds each issue's catch-up cursor. An issue recorded before it has none (null), so its
// first boot catch-up after the deploy replays nothing; one recorded after it starts at 0, which
// the catch-up reads from the root's last handout. A cursor is never negative.
func TestTheCatchUpCursorIsNullForIssuesRecordedBeforeIt(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 57)
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'admitted root', 'admitted', 1, 'in_progress', 'A', 4)`); err != nil {
		t.Fatalf("seed an issue before 0058: %v", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-300', 'LEGION-300', 'LEGION', 'later root', 'admitted', 1, 'todo', 'B', 2)`); err != nil {
		t.Fatalf("seed an issue after 0058: %v", err)
	}
	for key, want := range map[string]string{"LEGION-208": "<nil>", "LEGION-300": "0"} {
		var cursor *int64
		if err := store.Pool().QueryRow(ctx, `select caught_up_seq from issues where key = $1`, key).Scan(&cursor); err != nil {
			t.Fatalf("read the cursor of %s: %v", key, err)
		}
		got := "<nil>"
		if cursor != nil {
			got = "0"
			if *cursor != 0 {
				got = "nonzero"
			}
		}
		if got != want {
			t.Fatalf("cursor of %s = %s, want %s", key, got, want)
		}
	}
	if _, err := store.Pool().Exec(ctx, `update issues set caught_up_seq = -1 where key = 'LEGION-300'`); err == nil || !strings.Contains(err.Error(), "constraint") {
		t.Fatalf("a negative cursor = %v, want a constraint to refuse it", err)
	}
}
