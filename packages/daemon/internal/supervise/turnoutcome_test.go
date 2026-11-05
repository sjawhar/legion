package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// reportingMachine is a machine over a ready claim, which is where a turn of its agent ends.
func reportingMachine(t *testing.T) *harness {
	t.Helper()
	h := newBareHarness(t)
	c := fixture(StateReady)
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

func TestTheNewestTurnOutcomeIsKeptAndAnOlderOneDoesNotReplaceIt(t *testing.T) {
	h := reportingMachine(t)
	writes := len(h.store.history())

	for _, report := range []RequestTurnOutcome{
		{Claim: h.token, Generation: 3, Sequence: 1, Outcome: claim.TurnClean},
		{Claim: h.token, Generation: 3, Sequence: 3, Outcome: claim.TurnRefused, Category: "reasoning_extraction"},
		// Out of order behind the one above, and behind it in the order the turns ran.
		{Claim: h.token, Generation: 3, Sequence: 2, Outcome: claim.TurnProviderError},
		{Claim: h.token, Generation: 2, Sequence: 9, Outcome: claim.TurnClean},
	} {
		if err := h.handle(report); err != nil {
			t.Fatalf("report %d/%d: %v", report.Generation, report.Sequence, err)
		}
	}

	kept := h.m.lastTurnOutcome
	if kept == nil {
		t.Fatal("the machine kept no report")
	}
	want := TurnOutcome{Generation: 3, Sequence: 3, Outcome: claim.TurnRefused, Category: "reasoning_extraction"}
	if *kept != want {
		t.Errorf("kept %+v, want %+v", *kept, want)
	}
	if got := len(h.store.history()); got != writes {
		t.Errorf("wrote the claim %d times; a turn outcome is memory only", got-writes)
	}
}

// A later generation's first turn is newer than anything the generation before it reported: the
// claim's process was replaced, and its plugin starts counting again from 1.
func TestATurnOutcomeOfALaterGenerationReplacesTheOneBefore(t *testing.T) {
	h := reportingMachine(t)
	for _, report := range []RequestTurnOutcome{
		{Claim: h.token, Generation: 3, Sequence: 7, Outcome: claim.TurnRefused, Category: "reasoning_extraction"},
		{Claim: h.token, Generation: 4, Sequence: 1, Outcome: claim.TurnClean},
	} {
		if err := h.handle(report); err != nil {
			t.Fatalf("report %d/%d: %v", report.Generation, report.Sequence, err)
		}
	}

	want := TurnOutcome{Generation: 4, Sequence: 1, Outcome: claim.TurnClean}
	if kept := h.m.lastTurnOutcome; kept == nil || *kept != want {
		t.Errorf("kept %+v, want %+v", kept, want)
	}
}
