package store

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The turns a claim's producer reported are a jsonb window on the claim's own row, keyed by
// (generation, sequence). 0076 adds the column with the empty array, which every row an older
// daemon wrote takes: no claim that ran before this release reported a turn, and the first report
// of each fills it.
func TestTheTurnOutcomesWindowIsAddedAt0076(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 75)
	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('legion-LEGION-209-implementer', 'legion', 'LEGION-208', 'LEGION-209', 'implementer', 1, 'ses_implementer', '', 'idle', 0, 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if got := onlyClaim(t, store); len(got.TurnOutcomes) != 0 {
		t.Fatalf("a claim written before 0076 reads back %+v, want no turn reported", got.TurnOutcomes)
	}
}

// The window round-trips as it is kept: in (generation, sequence) order, with each report's
// outcome and the provider's message, so a restarted daemon derives the same run of refused turns
// the one before it did.
func TestAClaimsTurnOutcomesRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if got := onlyClaim(t, store); len(got.TurnOutcomes) != 0 {
		t.Fatalf("a claim that reported no turn reads back %+v, want none", got.TurnOutcomes)
	}

	c.TurnOutcomes = []supervise.TurnOutcome{
		{Generation: 1, Sequence: 7, Refused: false},
		{Generation: 2, Sequence: 1, Refused: true, Message: "the provider refused the turn"},
	}
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the claim with its reports: %v", err)
	}

	got := onlyClaim(t, store)
	sameClaim(t, got, c)
	if got.RefusedRun() != 1 || got.LastRefusal() != "the provider refused the turn" {
		t.Fatalf("the claim read back derives a run of %d ending %q, want 1 and the provider's message",
			got.RefusedRun(), got.LastRefusal())
	}
}
