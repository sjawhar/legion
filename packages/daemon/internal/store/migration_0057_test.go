package store

import (
	"context"
	"strings"
	"testing"
)

// 0057 adds the awaiting_person phase, what a root waiting on a person waits on, and the two
// sequences the decision rests on. Its backfill takes each issue's handout from the highest
// sequence the record already holds of it — its last Dispatch observation, the claim its mirror
// row shows, its mirrored asks and its gate's review marks — so no event the daemon applied
// before this release, and none of the same work redelivered after it, declares a hand-back.
func TestTheMigrationBackfillDeclaresNothingOld(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 56)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'admitted root', 'admitted', 1, 'in_progress', 'A', 4)`)
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-300', 'LEGION-300', 'LEGION', 'waiting root', 'admitted', 1, 'todo', 'B', 2)`)
	exec(`insert into dispatch_issues (key, project, status, blockers, dispatch_seq, claimed_by, claim_seq)
		values ('LEGION-208', 'LEGION', 'in_progress', '{}', 4, 'session:ses_architect', 9)`)
	exec(`insert into root_asks (issue, ask, kind, open, waiting_on, seq, by_tree)
		values ('LEGION-208', 'ask-q', 'question', true, 'human', 11, true)`)
	exec(`insert into design_gates (issue, artifact_id, latest_version, told_changes_version, told_changes_reason, told_changes_seq)
		values ('LEGION-208', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', 4, 4, 'say more', 14)`)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, want := range []struct {
		issue   string
		handout int64
	}{
		// The gate's told review (14) is the highest the record holds of the admitted root.
		{"LEGION-208", 14},
		// A root with nothing but its own observation takes that.
		{"LEGION-300", 2},
	} {
		var handout int64
		var declared *int64
		if err := store.Pool().QueryRow(ctx, `select handout_seq, declared_seq from issues where key = $1`, want.issue).Scan(&handout, &declared); err != nil {
			t.Fatalf("read the handout of %s: %v", want.issue, err)
		}
		if handout != want.handout || declared != nil {
			t.Fatalf("%s migrated with handout_seq %d and declared_seq %v, want %d and nothing declared", want.issue, handout, declared, want.handout)
		}
	}

	// Every column of the wait is the phase's: a root in another phase holds none of them, and one
	// in awaiting_person waits on exactly one of an ask and a spec version.
	for _, refused := range []struct{ why, statement string }{
		{"a wait outside the phase", `update issues set awaiting_ask = 'ask-q' where key = 'LEGION-208'`},
		{"a phase with no wait", `update issues set phase = 'awaiting_person' where key = 'LEGION-208'`},
		{"both an ask and a version", `update issues set phase = 'awaiting_person', awaiting_ask = 'ask-q', awaiting_gate_version = 3,
			awaiting_artifact = '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26' where key = 'LEGION-208'`},
		{"an answer with no time", `update issues set phase = 'awaiting_person', awaiting_ask = 'ask-q', awaiting_answered_by = 'ask' where key = 'LEGION-208'`},
		{"an answer nothing names", `update issues set phase = 'awaiting_person', awaiting_ask = 'ask-q', awaiting_answered_at = now(),
			awaiting_answered_by = 'whenever' where key = 'LEGION-208'`},
		{"a detail with no answer", `update issues set phase = 'awaiting_person', awaiting_ask = 'ask-q', awaiting_answered_detail = 'ask-q' where key = 'LEGION-208'`},
		{"a gate version with no document", `update issues set phase = 'awaiting_person', awaiting_gate_version = 3 where key = 'LEGION-208'`},
		{"a declared sequence of zero", `update issues set declared_seq = 0 where key = 'LEGION-208'`},
		{"a negative handout", `update issues set handout_seq = -1 where key = 'LEGION-208'`},
	} {
		_, err := store.Pool().Exec(ctx, refused.statement)
		if err == nil || !strings.Contains(err.Error(), "constraint") {
			t.Fatalf("%s = %v, want a constraint to refuse it", refused.why, err)
		}
	}
	// The shapes the phase allows: a question wait, and the gate wait the next release writes.
	for _, accepted := range []string{
		`update issues set phase = 'awaiting_person', awaiting_ask = 'ask-q', awaiting_since_seq = 11, awaiting_since = now() where key = 'LEGION-208'`,
		`update issues set awaiting_answered_at = now(), awaiting_answered_by = 'ask', awaiting_answered_detail = 'ask-q' where key = 'LEGION-208'`,
		`update issues set awaiting_ask = null, awaiting_gate_version = 4, awaiting_artifact = '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26' where key = 'LEGION-208'`,
		`update issues set phase = 'admitted', awaiting_gate_version = null, awaiting_artifact = null, awaiting_answered_at = null,
			awaiting_answered_by = null, awaiting_answered_detail = null, awaiting_since_seq = null, awaiting_since = null where key = 'LEGION-208'`,
	} {
		exec(accepted)
	}
}
