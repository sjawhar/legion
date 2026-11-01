package store

import (
	"context"
	"testing"
)

// 0068 marks every existing claim as file-backed. Those claims predate the
// session-store record, and a later Kubernetes runtime must retain their
// volume until an operator imports or explicitly forgets the session.
func TestClaimsBefore0068AreConservativelyFileBacked(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 67)
	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('legion-legion-LEGION-208-architect', 'legion', 'LEGION-208', 'LEGION-208', 'architect', 1,
		'ses_legacy', '/sessions/ses_legacy.jsonl', 'suspended', 0, 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var sessionStore string
	if err := store.Pool().QueryRow(ctx, `select session_store from claims where token = 'legion-legion-LEGION-208-architect'`).Scan(&sessionStore); err != nil {
		t.Fatal(err)
	}
	if sessionStore != "pvc" {
		t.Fatalf("session_store after 0068 = %q, want pvc", sessionStore)
	}
}
