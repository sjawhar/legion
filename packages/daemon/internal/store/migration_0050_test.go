package store

import (
	"context"
	"strings"
	"testing"
)

// 0050 adds awaiting_person and what it waits on. An issue the old daemon left in any phase
// migrates with no awaiting state and nobody's claim. Afterwards a root waits on exactly one of an
// ask or a gate version, and only in that phase, and a held issue never waits.
func TestAwaitingPersonArrivesWith0050AndItsShapeIsEnforced(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 49)
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
	if err := store.Pool().QueryRow(ctx, `select claimed_by, claim_seq, awaiting_ask, awaiting_gate_version from issues where key = 'LEGION-208'`).
		Scan(&claimedBy, &claimSeq, &ask, &gateVersion); err != nil {
		t.Fatalf("read the migrated root: %v", err)
	}
	if claimedBy != "" || claimSeq != 0 || ask != nil || gateVersion != nil {
		t.Fatalf("root after 0050 = claimed_by %q claim_seq %d ask %v gate %v, want no claim and no wait", claimedBy, claimSeq, ask, gateVersion)
	}

	for _, accepted := range []string{
		`update issues set phase = 'awaiting_person', awaiting_gate_version = 3 where key = 'LEGION-208'`,
		`update issues set awaiting_answered_at = now() where key = 'LEGION-208'`,
		`update issues set awaiting_gate_version = null, awaiting_ask = '6c83cb7f-8f4e-4820-9e05-7c94fa725007' where key = 'LEGION-208'`,
		`update issues set phase = 'admitted', awaiting_ask = null, awaiting_answered_at = null where key = 'LEGION-208'`,
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
