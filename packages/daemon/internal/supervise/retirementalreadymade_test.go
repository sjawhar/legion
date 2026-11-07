package supervise

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A retirement's answer can be lost on the way back: the store ran the claim write and the fenced
// delete in one transaction and committed it, and the connection died before the daemon read the
// answer. The claim is then holding a delivery the row no longer has, and every retirement of it
// from here on is refused ErrNoSuchDelivery — at the head of every decision, because settle is
// what calls it. Before this change that refusal was returned as a failure, so the claim's every
// event was dropped by supervisor.decisionFailed in internal/daemon: the claim stopped deciding
// anything at all, for good, and only a daemon restart moved it.
//
// Read as the retirement the store has already made, it moves memory to match the row, marks the
// claim write owed — the same transaction rolled it back with the delete, so what the row holds
// of the claim is older than what this retirement wrote — and lets the decision go on.
func TestARetirementAlreadyMadeIsReadAsTheStoresOwnRatherThanAFailure(t *testing.T) {
	h, counted := newCountedHarness(t)
	h.reach(StateWorking)
	delivery := h.pending().ID

	// The store has committed the retirement and the daemon never read the answer: the row holds
	// no such delivery, and memory still holds it.
	h.store.mu.Lock()
	delete(h.store.deliveries, testToken)
	h.store.mu.Unlock()

	// The turn ends, and the settle behind it retires the task the turn ran.
	h.must(StreamTurnEnd{Claim: testToken})

	h.wantState(StateIdle)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending = %+v, want %s gone: the store holds no such delivery, so memory holds none either", p, delivery)
	}
	if h.m.owedWrite == nil || h.m.owedWrite.method != "RetireDelivery" {
		t.Fatalf("the mark is %+v, want the retirement's own claim write owed", h.m.owedWrite)
	}
	if lines := h.logs.lines("the store holds no such delivery", delivery); len(lines) != 1 {
		t.Fatalf("the refusal is logged %d times, want once: %v", len(lines), lines)
	}

	// The claim's next decision reaches the machine rather than being dropped at its head, and
	// the repair makes the claim write that retirement's transaction rolled back.
	attempted := counted.mark()
	h.observe(runtime.Alive)

	if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim"}) {
		t.Fatalf("the repairing decision asked for %v, want the one owed claim write: nothing is left to retire", asked)
	}
	if h.m.owedWrite != nil {
		t.Fatal("the write is still owed after the repair made it")
	}
	stored := h.store.load(testToken)
	if stored.State != StateIdle {
		t.Fatalf("the repaired row reads %s, want idle", stored.State)
	}
	if stored.Pending != nil {
		t.Fatalf("the stored claim holds %+v, want no delivery: the store retired it before the answer was lost", stored.Pending)
	}

	// And the claim goes on deciding: the next event reaches the transition table.
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
}
