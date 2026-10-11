package store

import (
	"context"
	"testing"
)

// A release drain made before 0038 left claims suspended without recording who suspended them.
// Migration 0038 must conservatively mark those rows unknown: resume by rule treats unknown as
// held, so the first upgraded boot never overrides the operator's pre-upgrade drain.
func TestSuspendedClaimsPredating0038MigrateAsUnknown(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 36)
	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('legion-LEGION-208-architect', 'legion', 'LEGION-208', 'LEGION-208', 'architect', 1, 'ses_architect', '', 'suspended', 0, 0, 0, 0),
		('legion-LEGION-209-tester', 'legion', 'LEGION-208', 'LEGION-209', 'tester', 1, 'ses_tester', '', 'ready', 0, 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var suspended, live string
	if err := store.Pool().QueryRow(ctx, `select suspended_by from claims where token = 'legion-LEGION-208-architect'`).Scan(&suspended); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `select suspended_by from claims where token = 'legion-LEGION-209-tester'`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if suspended != "unknown" || live != "" {
		t.Fatalf("migration recorded suspended=%q live=%q, want unknown and empty", suspended, live)
	}
}
