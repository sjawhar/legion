package store

import (
	"context"
	"strings"
	"testing"
)

// 0056 adds two tables, the asks mirror and the claims mirror, and changes no existing table: rows
// an older daemon left migrate as they were, and the writes an older daemon makes still land. Each
// part of an ask's row carries its own fence: a turn only with its sequence, a close only with its
// sequence (kept when the ask reopens, and absent from a closed row whose close has not arrived),
// and an open row only with the sequence that opened it. A claim needs no issue row of its own.
func TestTheAskAndClaimMirrorsArriveWith0056(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 55)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'admitted root', 'admitted', 1, 'in_progress', 'A', 1)`)
	exec(`insert into design_gates (issue, artifact_id, latest_version) values ('LEGION-208', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', 4)`)
	exec(`insert into dispatch_issues (key, project, status, blockers, dispatch_seq) values ('LEGION-208', 'LEGION', 'in_progress', '{}', 3)`)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, accepted := range []string{
		// The older daemon's writes.
		`insert into dispatch_issues (key, project, status, blockers, dispatch_seq) values ('LEGION-209', 'LEGION', 'todo', '{}', 2)`,
		`update dispatch_issues set status = 'testing', dispatch_seq = 4 where key = 'LEGION-208' and dispatch_seq < 4`,
		`insert into design_gates (issue, artifact_id, latest_version, approved_version) values ('LEGION-208', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', 5, null)
			on conflict (issue) do update set latest_version = excluded.latest_version, approved_version = excluded.approved_version`,
		// This release's writes.
		`insert into dispatch_claims (key, claimed_by, claim_seq) values ('LEGION-208', 'user:alice', 6)`,
		`insert into dispatch_claims (key, claimed_by, claim_seq) values ('LEGION-999', '', 2)`,
		`insert into root_asks (issue, ask, kind, open, seq, waiting_on, waiting_on_seq, by_tree) values ('LEGION-208', 'ask-q', 'question', true, 7, 'human', 7, true)`,
		`insert into root_asks (issue, ask, kind, artifact, open, seq, waiting_on, waiting_on_seq, closed_by, closed_seq, person_answer_seq)
			values ('LEGION-208', 'ask-closed', 'approval', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', false, 8, 'human', 6, 'person', 8, 8)`,
		`insert into root_asks (issue, ask, open, seq) values ('LEGION-208', 'ask-close-unseen', false, 9)`,
		`insert into root_asks (issue, ask, kind, open, seq, closed_by, closed_seq, person_seq)
			values ('LEGION-208', 'ask-reopened', 'question', true, 10, 'settlement', 8, 9)`,
		`insert into root_asks (issue, ask, kind, artifact, open, seq, waiting_on, waiting_on_seq)
			values ('LEGION-208', 'ask-turn-only', 'approval', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', false, 0, 'agent', 5)`,
	} {
		exec(accepted)
	}
	for _, refused := range []struct{ why, statement string }{
		{"an unknown ask kind", `insert into root_asks (issue, ask, kind, open, seq) values ('LEGION-208', 'ask-a', 'action', true, 9)`},
		{"an empty document", `insert into root_asks (issue, ask, artifact, open, seq) values ('LEGION-208', 'ask-a', '', true, 9)`},
		{"an open ask with no sequence", `insert into root_asks (issue, ask, open, seq) values ('LEGION-208', 'ask-b', true, 0)`},
		{"a turn with no sequence", `insert into root_asks (issue, ask, open, seq, waiting_on) values ('LEGION-208', 'ask-c', true, 9, 'human')`},
		{"a turn sequence with no turn", `insert into root_asks (issue, ask, open, seq, waiting_on_seq) values ('LEGION-208', 'ask-d', true, 9, 9)`},
		{"a closer with no close", `insert into root_asks (issue, ask, open, seq, closed_by) values ('LEGION-208', 'ask-e', false, 9, 'person')`},
		{"a close with no closer", `insert into root_asks (issue, ask, open, seq, closed_seq) values ('LEGION-208', 'ask-f', false, 9, 9)`},
		{"a turn nobody takes", `insert into root_asks (issue, ask, open, seq, waiting_on, waiting_on_seq) values ('LEGION-208', 'ask-g', true, 9, 'someone', 9)`},
		{"a negative sequence", `insert into root_asks (issue, ask, open, seq) values ('LEGION-208', 'ask-h', false, -1)`},
		{"a claim with no sequence", `insert into dispatch_claims (key, claimed_by, claim_seq) values ('LEGION-210', 'user:alice', 0)`},
		{"a claim of no key", `insert into dispatch_claims (key, claimed_by, claim_seq) values ('', 'user:alice', 3)`},
	} {
		_, err := store.Pool().Exec(ctx, refused.statement)
		if err == nil || !strings.Contains(err.Error(), "constraint") {
			t.Fatalf("%s = %v, want a constraint to refuse it", refused.why, err)
		}
	}
	// An ask's rows go with its root's record.
	exec(`delete from design_gates where issue = 'LEGION-208'`)
	exec(`delete from issues where key = 'LEGION-208'`)
	var asks int
	if err := store.Pool().QueryRow(ctx, `select count(*) from root_asks where issue = 'LEGION-208'`).Scan(&asks); err != nil || asks != 0 {
		t.Fatalf("asks after the root's record went = %d, %v; want none", asks, err)
	}
}
