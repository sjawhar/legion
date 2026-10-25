package store

import (
	"context"
	"strings"
	"testing"
)

// 0051 adds awaiting_person and what it waits on, where on Dispatch that wait began, and the gate
// columns that record the changes request the architect was told of. An issue the old daemon left
// in any phase migrates with no awaiting state and nobody's claim, and a gate it left with no
// recorded request. Afterwards a root waits on exactly one of an ask or a gate version, and only in
// that phase, a wait's start is set only in that phase, and a held issue never waits.
func TestAwaitingPersonArrivesWith0051AndItsShapeIsEnforced(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 50)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'admitted root', 'admitted', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var claimedBy string
	var claimSeq int64
	var ask *string
	var gateVersion *int
	var sinceSeq *int64
	var since *string
	if err := store.Pool().QueryRow(ctx, `select claimed_by, claim_seq, awaiting_ask, awaiting_gate_version, awaiting_since_seq, awaiting_since::text
		from issues where key = 'LEGION-208'`).Scan(&claimedBy, &claimSeq, &ask, &gateVersion, &sinceSeq, &since); err != nil {
		t.Fatalf("read the migrated root: %v", err)
	}
	if claimedBy != "" || claimSeq != 0 || ask != nil || gateVersion != nil || sinceSeq != nil || since != nil {
		t.Fatalf("root after 0051 = claimed_by %q claim_seq %d ask %v gate %v since %v/%v, want no claim and no wait", claimedBy, claimSeq, ask, gateVersion, sinceSeq, since)
	}

	for _, accepted := range []string{
		`update issues set phase = 'awaiting_person', awaiting_gate_version = 3 where key = 'LEGION-208'`,
		`update issues set awaiting_answered_at = now() where key = 'LEGION-208'`,
		`update issues set awaiting_since_seq = 10, awaiting_since = now() where key = 'LEGION-208'`,
		`update issues set awaiting_gate_version = null, awaiting_ask = '6c83cb7f-8f4e-4820-9e05-7c94fa725007' where key = 'LEGION-208'`,
		`update issues set phase = 'admitted', awaiting_ask = null, awaiting_answered_at = null, awaiting_since_seq = null, awaiting_since = null where key = 'LEGION-208'`,
	} {
		if _, err := store.Pool().Exec(ctx, accepted); err != nil {
			t.Fatalf("%s: %v, want it accepted", accepted, err)
		}
	}
	for _, refused := range []struct{ why, statement string }{
		{"awaiting_person with neither wait", `update issues set phase = 'awaiting_person' where key = 'LEGION-208'`},
		{"awaiting_person with both waits", `update issues set phase = 'awaiting_person', awaiting_gate_version = 3, awaiting_ask = 'ask' where key = 'LEGION-208'`},
		{"a held issue waiting", `update issues set phase = 'awaiting_person', awaiting_gate_version = 3, held_from = 'planning' where key = 'LEGION-208'`},
		{"a wait outside awaiting_person", `update issues set awaiting_gate_version = 3 where key = 'LEGION-208'`},
		{"an answer outside awaiting_person", `update issues set awaiting_answered_at = now() where key = 'LEGION-208'`},
		{"a wait's start sequence outside awaiting_person", `update issues set awaiting_since_seq = 10 where key = 'LEGION-208'`},
		{"a wait's start time outside awaiting_person", `update issues set awaiting_since = now() where key = 'LEGION-208'`},
		{"a wait's start sequence of zero", `update issues set phase = 'awaiting_person', awaiting_gate_version = 3, awaiting_since_seq = 0 where key = 'LEGION-208'`},
		{"an empty ask", `update issues set phase = 'awaiting_person', awaiting_ask = '' where key = 'LEGION-208'`},
		{"a gate version of zero", `update issues set phase = 'awaiting_person', awaiting_gate_version = 0 where key = 'LEGION-208'`},
		{"a negative claim sequence", `update issues set claim_seq = -1 where key = 'LEGION-208'`},
	} {
		_, err := store.Pool().Exec(ctx, refused.statement)
		if err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Fatalf("%s = %v, want a check constraint to refuse it", refused.why, err)
		}
	}
}

// An old gate migrates with no changes request recorded as told, and the record takes a version
// with its reason and the review's sequence and time, or nothing: never a version of zero, a
// sequence of zero, and never a reason, sequence or time without a version.
func TestTheGatesToldChangesRequestArrivesWith0051(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 50)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'admitted root', 'admitted', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into design_gates (issue, artifact_id, latest_version)
		values ('LEGION-208', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', 4)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var version *int
	var reason string
	var seq *int64
	var at *string
	if err := store.Pool().QueryRow(ctx, `select told_changes_version, told_changes_reason, told_changes_seq, told_changes_at::text
		from design_gates where issue = 'LEGION-208'`).Scan(&version, &reason, &seq, &at); err != nil {
		t.Fatalf("read the migrated gate: %v", err)
	}
	if version != nil || reason != "" || seq != nil || at != nil {
		t.Fatalf("gate after 0051 = told_changes_version %v reason %q seq %v at %v, want none recorded", version, reason, seq, at)
	}
	for _, accepted := range []string{
		`update design_gates set told_changes_version = 4, told_changes_reason = 'name the rollback path' where issue = 'LEGION-208'`,
		`update design_gates set told_changes_seq = 12, told_changes_at = now() where issue = 'LEGION-208'`,
	} {
		if _, err := store.Pool().Exec(ctx, accepted); err != nil {
			t.Fatalf("%s: %v, want it accepted", accepted, err)
		}
	}
	for _, refused := range []struct{ why, statement string }{
		{"a version of zero", `update design_gates set told_changes_version = 0 where issue = 'LEGION-208'`},
		{"a sequence of zero", `update design_gates set told_changes_seq = 0 where issue = 'LEGION-208'`},
		{"a reason with no version", `update design_gates set told_changes_version = null, told_changes_seq = null, told_changes_at = null where issue = 'LEGION-208'`},
		{"a sequence with no version", `update design_gates set told_changes_version = null, told_changes_reason = '', told_changes_at = null where issue = 'LEGION-208'`},
		{"a time with no version", `update design_gates set told_changes_version = null, told_changes_reason = '', told_changes_seq = null where issue = 'LEGION-208'`},
	} {
		_, err := store.Pool().Exec(ctx, refused.statement)
		if err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Fatalf("%s = %v, want a check constraint to refuse it", refused.why, err)
		}
	}
}
