package store

import (
	"context"
	"testing"
	"time"
)

// 0070 records the turn each claim last started and the one it last ended. A claim written before
// it recorded neither, and nothing in the row says when its agent last worked, so both columns
// stay null rather than taking a time the migration would have to invent: a reader tells "no turn
// is recorded" from a turn that happened by the null.
func TestAClaimRecordedBefore0070HasNoTurnTimes(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 69)
	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('legion-LEGION-209-implementer', 'legion', 'LEGION-208', 'LEGION-209', 'implementer', 1, 'ses_implementer', '', 'idle', 0, 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var started, ended *time.Time
	if err := store.Pool().QueryRow(ctx,
		`select last_turn_start, last_turn_end from claims where token = 'legion-LEGION-209-implementer'`).
		Scan(&started, &ended); err != nil {
		t.Fatalf("read the claim after 0070: %v", err)
	}
	if started != nil || ended != nil {
		t.Fatalf("turn times after 0070 = %v and %v, want both null for a claim that ran before them", started, ended)
	}
}
