package store

import (
	"context"
	"strings"
	"testing"
)

// 0056 adds the asks mirror, the gate's approval request and the claim on the Dispatch mirror,
// all additive: rows an older daemon left migrate with no request and no claim seen, and the
// writes an older daemon makes, which name none of the new columns, still land. A closed ask may
// keep the turn it last had; an open one has no closer and a closed one has one.
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

	var requestAsk *string
	if err := store.Pool().QueryRow(ctx, `select request_ask from design_gates where issue = 'LEGION-208'`).Scan(&requestAsk); err != nil || requestAsk != nil {
		t.Fatalf("migrated gate's request = %v, %v; want none", requestAsk, err)
	}
	var claimedBy string
	var claimSeq int64
	if err := store.Pool().QueryRow(ctx, `select claimed_by, claim_seq from dispatch_issues where key = 'LEGION-208'`).Scan(&claimedBy, &claimSeq); err != nil || claimedBy != "" || claimSeq != 0 {
		t.Fatalf("migrated mirror row's claim = %q at %d, %v; want none seen", claimedBy, claimSeq, err)
	}

	for _, accepted := range []string{
		// The older daemon's writes, which name no new column.
		`insert into dispatch_issues (key, project, status, blockers, dispatch_seq) values ('LEGION-209', 'LEGION', 'todo', '{}', 2)`,
		`update dispatch_issues set status = 'testing', dispatch_seq = 4 where key = 'LEGION-208' and dispatch_seq < 4`,
		`insert into design_gates (issue, artifact_id, latest_version, approved_version) values ('LEGION-208', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', 5, null)
			on conflict (issue) do update set latest_version = excluded.latest_version, approved_version = excluded.approved_version`,
		// This release's writes.
		`update design_gates set request_ask = 'ask-approval' where issue = 'LEGION-208'`,
		`update dispatch_issues set claimed_by = 'user:alice', claim_seq = 6 where key = 'LEGION-208'`,
		`insert into root_asks (issue, ask, kind, open, waiting_on, seq, by_tree) values ('LEGION-208', 'ask-q', 'question', true, 'human', 7, true)`,
		`insert into root_asks (issue, ask, kind, open, waiting_on, seq, closed_seq, closed_by, person_seq)
			values ('LEGION-208', 'ask-closed', null, false, 'human', 8, 8, 'person', 8)`,
	} {
		exec(accepted)
	}
	for _, refused := range []struct{ why, statement string }{
		{"an unknown ask kind", `insert into root_asks (issue, ask, kind, open, waiting_on, seq) values ('LEGION-208', 'ask-a', 'action', true, 'human', 9)`},
		{"an open ask with a closer", `insert into root_asks (issue, ask, open, waiting_on, seq, closed_by) values ('LEGION-208', 'ask-b', true, 'human', 9, 'person')`},
		{"a closed ask with no closer", `insert into root_asks (issue, ask, open, seq) values ('LEGION-208', 'ask-c', false, 9)`},
		{"a turn nobody takes", `insert into root_asks (issue, ask, open, waiting_on, seq) values ('LEGION-208', 'ask-d', true, 'someone', 9)`},
		{"an ask with no sequence", `insert into root_asks (issue, ask, open, waiting_on, seq) values ('LEGION-208', 'ask-e', true, 'human', 0)`},
		{"an empty request", `update design_gates set request_ask = '' where issue = 'LEGION-208'`},
		{"a negative claim sequence", `update dispatch_issues set claim_seq = -1 where key = 'LEGION-208'`},
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
