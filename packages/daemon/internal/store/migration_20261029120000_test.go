package store

import (
	"context"
	"testing"
)

// 20261029120000 splits the pass a phase stands in off rounds, which counted both it and the review rounds
// review_round_cap bounds. A phase row written before it carries its returns in rounds, so the
// pass it takes is that count: the completion key the handoff route builds from the pass means
// what the same issue's key meant before the deploy.
func TestAPhaseRecordedBefore20261029120000TakesItsRoundsAsItsPass(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 67)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into phases
		(issue, role, claim, handoff_commit, rounds, verdict, summary, last_handoff)
		values ('LEGION-208', 'implementer', 'legion-legion-legion-208-implementer', '', 3, '', '', 'handoff-3')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var pass, rounds int
	if err := store.Pool().QueryRow(ctx, `select pass, rounds from phases where issue = 'LEGION-208' and role = 'implementer'`).
		Scan(&pass, &rounds); err != nil {
		t.Fatalf("read the phase after 20261029120000: %v", err)
	}
	if pass != 3 || rounds != 3 {
		t.Fatalf("pass=%d rounds=%d after 20261029120000, want the legacy row's 3 counted as both", pass, rounds)
	}
}
