package store

import (
	"context"
	"testing"
)

// A claim suspended before 0038 carries no record of who suspended it. Migration 0038 marks those
// rows unknown, and only them: the boot's resume by rule acts on no unknown claim, so the first
// upgraded boot never overrides a drain made before the upgrade.
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
