package store

import (
	"context"
	"testing"
)

// The release before 20261105022045 kept a claim's answer through a turn the daemon sent nothing
// for whenever the claim's running process had registered a plugin contract below 46, the
// contract that introduced ready_for_update (supervise.CannotAnswerUpdate). This release reads
// claims.update_answered_by_turn_end in place of that contract, and the column arrives false for
// every row the table already holds, so the migration backfills it: a claim below 46 that had
// answered its open round by the end of its task's turn keeps that answer across the upgrade,
// rather than losing it at its next reminder turn and being asked for it again.
//
// This is the whole proof the finding asks for: the store's harness migrates to any version a
// test names (migrateThrough), so these rows are written as the released daemon left them, at the
// migration before this one, and read after it ran.
func TestTheUpdateAnswerBackfillKeepsTheTurnsEndAnswerOfAClaimBelow46(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 20261102095707)

	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
		plugin_contract, update_round, update_signalled_at)
		values
		('legion-LEGION-208-architect', 'legion', 'LEGION-208', 'LEGION-208', 'architect', 1, 'ses_architect', '',
			'ready', 0, 0, 0, 0, 45, 'pod:a-pod-uid', now()),
		('legion-LEGION-209-implementer', 'legion', 'LEGION-208', 'LEGION-209', 'implementer', 1, 'ses_implementer', '',
			'ready', 0, 0, 0, 0, 45, 'pod:a-pod-uid', null),
		('legion-LEGION-209-tester', 'legion', 'LEGION-208', 'LEGION-209', 'tester', 1, 'ses_tester', '',
			'ready', 0, 0, 0, 0, 46, 'pod:a-pod-uid', now())`); err != nil {
		t.Fatalf("seed the claims of the release this upgrades from: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, want := range []struct {
		token string
		why   string
		mark  bool
	}{
		{token: "legion-LEGION-208-architect", mark: true,
			why: "it answered its round below the contract that introduced ready_for_update, which it could only have done by ending its turn"},
		{token: "legion-LEGION-209-implementer", mark: false,
			why: "it has answered its round in no way at all"},
		{token: "legion-LEGION-209-tester", mark: false,
			why: "it holds the contract that introduced ready_for_update, so its answer may have been the call"},
	} {
		var byTurnEnd, reasked bool
		if err := store.Pool().QueryRow(ctx, `select update_answered_by_turn_end, update_reasked_for_push_check
			from claims where token = $1`, want.token).Scan(&byTurnEnd, &reasked); err != nil {
			t.Fatalf("read the marks of %s: %v", want.token, err)
		}
		if byTurnEnd != want.mark {
			t.Errorf("update_answered_by_turn_end of %s = %t, want %t: %s", want.token, byTurnEnd, want.mark, want.why)
		}
		if reasked {
			t.Errorf("update_reasked_for_push_check of %s = true, want false: no round of the release this upgrades from asked a worker a second time", want.token)
		}
	}
}
