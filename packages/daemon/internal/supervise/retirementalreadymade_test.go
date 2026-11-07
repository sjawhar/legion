package supervise

import (
	"errors"
	"slices"
	"strings"
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
	if h.m.owedWrite == nil {
		t.Fatal("the retirement the store had already made left no claim write owed")
	}
	if owed := h.records.of(owedWriteLine); len(owed) != 1 {
		t.Fatalf("the owed write is logged %d times, want once", len(owed))
	} else if store, ok := owed[0].attr("store"); !ok || store.String() != "RetireDelivery" {
		t.Fatalf("the owed line names the store method %v, want the retirement whose transaction rolled the claim write back", store)
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

// The store's other refusal of a delete that removed no row is no retirement already made, and
// must never be read as one. The claim's row holds a delivery under another id: a write that
// rotated the id (interrupted, takeBackPending) had its answer cut off by the claim-write bound
// and committed all the same, so the store holds B where memory holds A, and the retirement of
// A deletes nothing because the table is keyed on the claim rather than on the delivery.
//
// Read as a retirement the store has already made, this would drop the delivery memory holds and
// move the claim on as though nothing were pending, while the stored delivery stays behind:
// pending, unconfirmed and retired by nobody. So the refusal is returned as the failure it is,
// and the decision that met it is dropped with it.
//
// That decision is the only one lost. The refusal says exactly what the row is missing — the
// delivery memory holds — so that delivery is marked unsaved, and the settle that follows writes
// it over the row the rotation left and retires it there. Here that happens inside the decision
// that met the refusal, because a prompt's result settles twice: once in its own row, which is
// where the refusal is met, and once at the end of Handle, which is where the mark is read. The
// claim then takes every event after it, which is what the count below holds.
//
// The negative control is the mark taken back out (retirePending): nothing then writes that row,
// since this path owes no claim write and no repair makes one, so the settle at the head of
// every decision meets the same refusal and the claim's later events are never applied at all.
func TestARetirementUnderAnotherIdLeavesTheClaimDeciding(t *testing.T) {
	h, counted := newCountedHarness(t)
	h.reach(StateWorking)
	delivery := h.pending().ID

	// The rotation's own write committed after its answer was lost: the row holds the new
	// delivery, and memory still holds the old one the turn ran. The harness has no route to
	// that state — every write it makes writes both sides — so the map is written directly, as
	// the retirement already made above is.
	rotated := Delivery{ID: "delivery-rotated", Task: "the task, sent again", QueuedAt: h.clock.Now()}
	h.store.mu.Lock()
	h.store.deliveries[testToken] = rotated
	h.store.mu.Unlock()

	// The turn ends, and the settle behind it retires the task the turn ran.
	attempted := counted.mark()
	err := h.handle(StreamTurnEnd{Claim: testToken})

	if !errors.Is(err, ErrOtherDelivery) {
		t.Fatalf("the turn end returned %v, want the store's refusal of a row holding another delivery", err)
	}
	if errors.Is(err, ErrNoSuchDelivery) {
		t.Fatal("the refusal reads as a retirement already made; the two are opposite answers and the sentinels must not wrap")
	}
	for _, id := range []string{delivery, rotated.ID} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("the refusal does not name %s: %v", id, err)
		}
	}
	// The retirement was asked for and refused: the answer came from the store's own delete
	// rather than from a guard ahead of it. What the mark then does is the two writes behind it:
	// the delivery memory holds written over the row the rotation left, and the retirement that
	// now finds the delivery it is of.
	asked := counted.writes()[attempted:]
	refused := slices.Index(asked, "RetireDelivery")
	if refused < 0 {
		t.Fatalf("the decision asked for %v, want the retirement of the delivery the turn ran", asked)
	}
	// These are errors rather than fatal, so that the count at the end is reached and reported
	// whatever they say: it is the one this test turns on.
	if want := []string{"RetireDelivery", "PutClaimAndDelivery", "RetireDelivery"}; !slices.Equal(asked[refused:], want) {
		t.Errorf("the refusal was followed by %v, want %v: the row lacks exactly the delivery memory holds", asked[refused:], want)
	}

	// Memory and the row agree again, and on memory's own delivery: the rotation's row was
	// written over with it and then retired with it. Dropped instead, nothing would ever retire
	// the stored one.
	if p := h.claim().Pending; p != nil {
		t.Errorf("pending = %+v, want none: %s was written to the row and retired there", p, delivery)
	}
	if stored, ok := h.store.delivery(testToken); ok {
		t.Errorf("the store holds %+v, want none: %s was written over %s and retired with it", stored, delivery, rotated.ID)
	}
	if h.m.unsaved {
		t.Error("the delivery reads unsaved after the write that put it on the row and the retirement that followed")
	}
	// Nothing is owed by this path: the refusal says the row holds a delivery this machine is not
	// retiring, which no claim write repairs, and a mark would have the claim's next decision
	// write a claim whose delivery the row still disagrees about.
	if h.m.owedWrite != nil {
		t.Fatal("the refused retirement left a claim write owed; only the retirement the store had already made owes one")
	}
	if lines := h.logs.lines("the store holds no such delivery"); len(lines) != 0 {
		t.Fatalf("the refusal was logged as a retirement already made: %v", lines)
	}
	if owed := h.records.of(owedWriteLine); len(owed) != 0 {
		t.Fatalf("the owed write was logged %d times, want never: %v", len(owed), owed)
	}

	// And the claim goes on deciding, event after event: each of the three below reaches the
	// transition table and moves the claim, where the build that left memory alone applies none
	// of them.
	later := []Event{
		StreamTurnStart{Claim: testToken},
		StreamTurnEnd{Claim: testToken},
		StreamTurnStart{Claim: testToken},
	}
	want := []ClaimState{StateWorking, StateIdle, StateWorking}
	applied := 0
	for i, ev := range later {
		if err := h.handleQuiet(ev); err != nil {
			t.Errorf("the claim refused %T after the one decision the refusal cost: %v", ev, err)
			continue
		}
		if got := h.state(); got != want[i] {
			t.Errorf("%T left the claim %s, want %s: the event never reached the transition table", ev, got, want[i])
			continue
		}
		applied++
	}
	if applied != len(later) {
		t.Fatalf("%d of the claim's %d later events were applied, want every one of them: the claim decides from the next event on",
			applied, len(later))
	}
}
