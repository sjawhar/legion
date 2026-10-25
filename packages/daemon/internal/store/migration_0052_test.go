package store

import (
	"context"
	"testing"
)

// A tree Legion takes back after it has finished and lingered starts again whatever Dispatch says
// it waits on, while a never-started child the same tree hands over as a root of its own is
// checked like any other root. Re-admission leaves the two records the same shape, so 0052 gives
// the issue the mark that tells them apart, durably: a returning root whose project has no free
// slot is judged again by a later pass, which has nothing of the re-admission left to read. Its
// default leaves every row already recorded judged exactly as it was: not exempt until its own
// next re-admission says so.
func TestTheTakenBackMarkArrivesWith0052(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 51)
	var before int
	if err := store.Pool().QueryRow(ctx, `select count(*) from information_schema.columns
		where table_name = 'issues' and column_name = 'taken_back'`).Scan(&before); err != nil {
		t.Fatalf("look for issues.taken_back before 0052: %v", err)
	}
	if before != 0 {
		t.Fatalf("issues.taken_back exists at 51, want it to arrive with 0052")
	}
	// A root recorded by the older daemon, which knew no such mark.
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over)
		values ('LEGION-910', 'LEGION-910', 'LEGION', 'an older root', 'admitted', 2, 'todo', 'A', 1, true)`); err != nil {
		t.Fatalf("record a root before 0052: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var existing bool
	if err := store.Pool().QueryRow(ctx, `select taken_back from issues where key = 'LEGION-910'`).Scan(&existing); err != nil {
		t.Fatalf("read the older root's mark: %v", err)
	}
	if existing {
		t.Fatalf("a root recorded before 0052 = taken back, want it judged as it was")
	}
	// The column takes the mark both ways, and a row written without it is not exempt either.
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over, taken_back)
		values ('LEGION-911', 'LEGION-911', 'LEGION', 'a tree taken back', 'admitted', 3, 'todo', 'B', 4, true, true)`); err != nil {
		t.Fatalf("record a taken-back root: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over)
		values ('LEGION-912', 'LEGION-912', 'LEGION', 'a handed-over child', 'admitted', 2, 'todo', 'C', 5, true)`); err != nil {
		t.Fatalf("record a handed-over child: %v", err)
	}
	for key, want := range map[string]bool{"LEGION-911": true, "LEGION-912": false} {
		var got bool
		if err := store.Pool().QueryRow(ctx, `select taken_back from issues where key = $1`, key).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if got != want {
			t.Errorf("%s taken_back = %t, want %t", key, got, want)
		}
	}
}
