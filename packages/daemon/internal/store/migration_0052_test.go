package store

import (
	"context"
	"strings"
	"testing"
)

// 0052 adds awaiting_person and what it waits on, where on Dispatch that wait began, the root's
// last handout and earliest hand-back since, the gate columns that record the changes request the
// architect was told of and where Dispatch recorded its approval, and the mirror of a root's asks.
// An issue the old daemon left in any phase migrates with no awaiting state, nobody's claim, no
// handout and no hand-back, and a gate it left with no recorded request. Afterwards a root waits on
// exactly one of an ask or a gate version, and only in that phase, a wait's start is set only in
// that phase, the handout and the hand-back are kept in every phase, and a held issue never waits.
func TestAwaitingPersonArrivesWith0052AndItsShapeIsEnforced(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 51)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'admitted root', 'admitted', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var claimedBy string
	var claimSeq, handout int64
	var ask *string
	var gateVersion *int
	var sinceSeq, declared *int64
	var since *string
	if err := store.Pool().QueryRow(ctx, `select claimed_by, claim_seq, awaiting_ask, awaiting_gate_version, awaiting_since_seq, awaiting_since::text, handout_seq, declared_seq
		from issues where key = 'LEGION-208'`).Scan(&claimedBy, &claimSeq, &ask, &gateVersion, &sinceSeq, &since, &handout, &declared); err != nil {
		t.Fatalf("read the migrated root: %v", err)
	}
	if claimedBy != "" || claimSeq != 0 || ask != nil || gateVersion != nil || sinceSeq != nil || since != nil || handout != 0 || declared != nil {
		t.Fatalf("root after 0052 = claimed_by %q claim_seq %d ask %v gate %v since %v/%v handout %d declared %v, want no claim, no wait, no handout and no hand-back",
			claimedBy, claimSeq, ask, gateVersion, sinceSeq, since, handout, declared)
	}

	for _, accepted := range []string{
		`update issues set phase = 'awaiting_person', awaiting_gate_version = 3, awaiting_artifact = '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26' where key = 'LEGION-208'`,
		`update issues set awaiting_answered_at = now(), awaiting_answered_by = 'changes_requested', awaiting_answered_detail = 'version 3: name the rollback path' where key = 'LEGION-208'`,
		`update issues set awaiting_answered_by = 'turn' where key = 'LEGION-208'`,
		`update issues set awaiting_since_seq = 10, awaiting_since = now() where key = 'LEGION-208'`,
		`update issues set awaiting_gate_version = null, awaiting_artifact = null, awaiting_ask = '6c83cb7f-8f4e-4820-9e05-7c94fa725007' where key = 'LEGION-208'`,
		`update issues set phase = 'admitted', awaiting_ask = null, awaiting_answered_at = null, awaiting_answered_by = null, awaiting_answered_detail = null, awaiting_since_seq = null, awaiting_since = null where key = 'LEGION-208'`,
		// The handout and the hand-back are kept at admitted, after the wait's columns cleared.
		`update issues set handout_seq = 10, declared_seq = 12 where key = 'LEGION-208'`,
		`update issues set declared_seq = null where key = 'LEGION-208'`,
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
		{"what answered a wait, outside awaiting_person", `update issues set awaiting_answered_by = 'ask' where key = 'LEGION-208'`},
		{"what answered a wait nobody answered", `update issues set phase = 'awaiting_person', awaiting_gate_version = 3, awaiting_artifact = 'a', awaiting_answered_by = 'approved' where key = 'LEGION-208'`},
		{"an answer of no known kind", `update issues set phase = 'awaiting_person', awaiting_gate_version = 3, awaiting_artifact = 'a', awaiting_answered_at = now(), awaiting_answered_by = 'guess' where key = 'LEGION-208'`},
		{"a gate wait with no document", `update issues set phase = 'awaiting_person', awaiting_ask = null, awaiting_gate_version = 3 where key = 'LEGION-208'`},
		{"an answer's detail with no answer", `update issues set phase = 'awaiting_person', awaiting_gate_version = 3, awaiting_artifact = 'a', awaiting_answered_detail = 'why' where key = 'LEGION-208'`},
		{"a wait's start sequence outside awaiting_person", `update issues set awaiting_since_seq = 10 where key = 'LEGION-208'`},
		{"a wait's start time outside awaiting_person", `update issues set awaiting_since = now() where key = 'LEGION-208'`},
		{"a wait's start sequence of zero", `update issues set phase = 'awaiting_person', awaiting_gate_version = 3, awaiting_since_seq = 0 where key = 'LEGION-208'`},
		{"an empty ask", `update issues set phase = 'awaiting_person', awaiting_ask = '' where key = 'LEGION-208'`},
		{"a gate version of zero", `update issues set phase = 'awaiting_person', awaiting_gate_version = 0 where key = 'LEGION-208'`},
		{"a negative claim sequence", `update issues set claim_seq = -1 where key = 'LEGION-208'`},
		{"a negative handout", `update issues set handout_seq = -1 where key = 'LEGION-208'`},
		{"a hand-back at sequence zero", `update issues set declared_seq = 0 where key = 'LEGION-208'`},
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
func TestTheGatesToldChangesRequestArrivesWith0052(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 51)
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
		t.Fatalf("gate after 0052 = told_changes_version %v reason %q seq %v at %v, want none recorded", version, reason, seq, at)
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

// 0052 also records where Dispatch put the approval a gate holds, cleared with the approval
// itself, the approval request a registration read open, and the mirror of each ask of an issue,
// which goes with the issue.
func TestTheGatesApprovalMarkAndTheAskMirrorArriveWith0052(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 51)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'admitted root', 'admitted', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into design_gates (issue, artifact_id, latest_version, approved_version)
		values ('LEGION-208', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', 4, 4)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var seq *int64
	var at, request *string
	if err := store.Pool().QueryRow(ctx, `select approved_seq, approved_at::text, request_ask from design_gates where issue = 'LEGION-208'`).Scan(&seq, &at, &request); err != nil {
		t.Fatalf("read the migrated gate: %v", err)
	}
	if seq != nil || at != nil || request != nil {
		t.Fatalf("gate after 0052 = approved_seq %v approved_at %v request %v, want the approval it already held placed nowhere and no request", seq, at, request)
	}
	for _, accepted := range []string{
		`update design_gates set approved_seq = 12, approved_at = now(), request_ask = 'ask-approval' where issue = 'LEGION-208'`,
		`insert into root_asks (issue, ask, kind, open, waiting_on, seq, by_tree) values ('LEGION-208', 'ask-question', 'question', true, 'human', 11, true)`,
		`insert into root_asks (issue, ask, open, waiting_on, seq) values ('LEGION-208', 'ask-unnamed', true, 'agent', 12)`,
		`insert into root_asks (issue, ask, kind, open, seq, person_seq, closed_seq, closed_by) values ('LEGION-208', 'ask-approval', 'approval', false, 13, 0, 13, 'person')`,
	} {
		if _, err := store.Pool().Exec(ctx, accepted); err != nil {
			t.Fatalf("%s: %v, want it accepted", accepted, err)
		}
	}
	for _, refused := range []struct{ why, statement string }{
		{"a sequence of zero", `update design_gates set approved_seq = 0 where issue = 'LEGION-208'`},
		{"a mark with no approval", `update design_gates set approved_version = null where issue = 'LEGION-208'`},
		{"an empty request", `update design_gates set request_ask = '' where issue = 'LEGION-208'`},
		{"an ask's sequence of zero", `update root_asks set seq = 0 where ask = 'ask-question'`},
		{"an ask of no known kind", `update root_asks set kind = 'guess' where ask = 'ask-question'`},
		{"an open ask on nobody's turn", `update root_asks set waiting_on = null where ask = 'ask-question'`},
		{"a closed ask on someone's turn", `update root_asks set waiting_on = 'human' where ask = 'ask-approval'`},
		{"a closed ask closed by nobody", `update root_asks set closed_by = null where ask = 'ask-approval'`},
		{"an open ask someone closed", `update root_asks set closed_by = 'tree' where ask = 'ask-question'`},
		{"a turn of no known side", `update root_asks set waiting_on = 'both' where ask = 'ask-question'`},
		{"a negative act", `update root_asks set person_seq = -1 where ask = 'ask-question'`},
	} {
		_, err := store.Pool().Exec(ctx, refused.statement)
		if err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Fatalf("%s = %v, want a check constraint to refuse it", refused.why, err)
		}
	}
	if _, err := store.Pool().Exec(ctx, `delete from design_gates where issue = 'LEGION-208'`); err != nil {
		t.Fatalf("delete the gate: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `delete from issues where key = 'LEGION-208'`); err != nil {
		t.Fatalf("delete the issue: %v", err)
	}
	var left int
	if err := store.Pool().QueryRow(ctx, `select count(*) from root_asks`).Scan(&left); err != nil {
		t.Fatalf("count the mirrored asks: %v", err)
	}
	if left != 0 {
		t.Fatalf("mirrored asks after the issue was deleted = %d, want none", left)
	}
}
