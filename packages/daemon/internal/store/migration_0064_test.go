package store

import (
	"context"
	"strings"
	"testing"
)

// 0064 adds the assignment a completion is held to: what the issue's current phase handed a role
// (phases.assignment), what the claim's worker took (claims.serving_assignment), and the value the
// delivery carries from one to the other (pending_task_deliveries.assignment). The negative
// control is the daemon's own write before the migration, which names the column and is refused.
//
// There is no backfill, by design: every worker live at the release has an empty value on both
// sides, which the completion route reads as nothing to compare, so none of their completions is
// refused for a value only one side has. This asserts that emptiness on rows recorded before the
// migration, and that each column then takes and keeps a value.
func TestTheAssignmentColumnsArriveEmptyWith0064(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 63)
	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'testing', 1, 'testing', 'A', 1)`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict)
			values ('LEGION-208', 'tester', 'legion-LEGION-208-tester', '', 0, '')`,
		`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation)
			values ('legion-LEGION-208-tester', 'legion', 'LEGION-208', 'LEGION-208', 'tester', 1, 'ses_t', '', 'working', 0, 0, 0, 0, 1)`,
		`insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation, queued_at, marked_by, interrupted)
			values ('legion-LEGION-208-tester', 'outbox:7', 'Continue. Phase: testing.', 'testing', 1, now(), '', false)`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before 0064: %v", err)
		}
	}
	// The write the daemon makes once it mints assignments, against the schema before the
	// migration: the row the migration exists for.
	_, err := store.Pool().Exec(ctx,
		"update phases set assignment = 'A1' where issue = 'LEGION-208' and role = 'tester'")
	if err == nil || !strings.Contains(err.Error(), "assignment") {
		t.Fatalf("stamping a phases row's assignment before 0064 = %v, want the column to be unknown", err)
	}
	t.Logf("before the migration the assignment write is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the assignment columns: %v", err)
	}

	for _, read := range []struct {
		what  string
		query string
	}{
		{"the phases row", "select assignment from phases where issue = 'LEGION-208' and role = 'tester'"},
		{"the claim", "select serving_assignment from claims where token = 'legion-LEGION-208-tester'"},
		{"the pending delivery", "select assignment from pending_task_deliveries where delivery_id = 'outbox:7'"},
	} {
		var got string
		if err := store.Pool().QueryRow(ctx, read.query).Scan(&got); err != nil {
			t.Fatalf("read %s after 0064: %v", read.what, err)
		}
		if got != "" {
			t.Fatalf("%s carries assignment %q after 0064, want it empty: no backfill, so a worker live at the release is compared with nothing", read.what, got)
		}
	}

	for _, write := range []struct {
		what  string
		write string
		read  string
	}{
		{"the phases row", "update phases set assignment = 'A1' where issue = 'LEGION-208' and role = 'tester'",
			"select assignment from phases where issue = 'LEGION-208' and role = 'tester'"},
		{"the claim", "update claims set serving_assignment = 'A1' where token = 'legion-LEGION-208-tester'",
			"select serving_assignment from claims where token = 'legion-LEGION-208-tester'"},
		{"the pending delivery", "update pending_task_deliveries set assignment = 'A1' where delivery_id = 'outbox:7'",
			"select assignment from pending_task_deliveries where delivery_id = 'outbox:7'"},
	} {
		if _, err := store.Pool().Exec(ctx, write.write); err != nil {
			t.Fatalf("stamp %s after 0064: %v", write.what, err)
		}
		var got string
		if err := store.Pool().QueryRow(ctx, write.read).Scan(&got); err != nil {
			t.Fatalf("read %s back: %v", write.what, err)
		}
		if got != "A1" {
			t.Fatalf("%s carries assignment %q, want the stamped A1", write.what, got)
		}
	}
	t.Logf("after the migration all three columns take and keep an assignment")

	// The columns refuse null, so no row can be left with an assignment that is neither a value nor
	// the empty string the comparison reads as nothing to compare.
	for _, statement := range []string{
		"update phases set assignment = null where issue = 'LEGION-208'",
		"update claims set serving_assignment = null where token = 'legion-LEGION-208-tester'",
		"update pending_task_deliveries set assignment = null where delivery_id = 'outbox:7'",
	} {
		if _, err := store.Pool().Exec(ctx, statement); err == nil || !strings.Contains(err.Error(), "null value") {
			t.Fatalf("setting an assignment to null = %v, want the not-null constraint to refuse it", err)
		}
	}
}
