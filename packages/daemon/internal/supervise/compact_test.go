package supervise

import (
	"errors"
	"testing"
	"time"
)

// compactInstructions stands for what the API sends with every compaction: Oh My Pi writes a
// text summary only for a compaction that carries instructions, and Machine.Compact refuses one
// that carries none.
const testCompactInstructions = "Write the summary as text."

// ladderStuck is the claim the provider-errors notice names, idle between the ladder's sends:
// six turns of it in a row ended in a provider error, so the project's controller has been told
// once, the task it owes is unconfirmed on the claim, and the ladder's next step is ten minutes
// away. ladderSends is how many prompts the connection has seen by then.
const ladderSends = providerErrorRunTold

func ladderStuck(t *testing.T, notices *int) *harness {
	t.Helper()
	h := workingClaim(t, func(d *Deps) {
		d.ProviderErrorRunReached = func(Claim, string) { *notices++ }
	})
	for turn := range providerErrorRunTold {
		h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
		if turn+1 < providerErrorRunTold {
			h.climbs(turn+1, promptRetryDelay(turn+1))
		}
	}
	if got := h.claim().ProviderErrorRun; got != providerErrorRunTold {
		t.Fatalf("the run of provider errors is %d, want the %d the controller is told at", got, providerErrorRunTold)
	}
	if *notices != 1 {
		t.Fatalf("the controller was told %d times, want once", *notices)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s, want it idle between the ladder's sends", got)
	}
	h.wantPrompts(ladderSends)
	return h
}

// A compaction that ran starts the claim's run of provider-error turns again, so the project's
// controller is told a second time after six more refused turns. Without the reset the
// controller would be told once and never again until a turn of that claim ended with no error
// at all, and the compaction it asked for would buy it no second look.
//
// The owed task goes again at once rather than at the end of the ladder step the compaction
// interrupted: the next step of that ladder is ten minutes away, and putting the task in front
// of the shortened session is the whole point of the operation.
func TestACompactionStartsTheRunOfProviderErrorsAgainSoTheControllerIsToldASecondTime(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends

	if err := h.m.Compact(h.ctx, testCompactInstructions); err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()

	if got := h.claim().ProviderErrorRun; got != 0 {
		t.Fatalf("the run of provider errors is %d after the compaction, want 0", got)
	}
	if got := h.store.load(h.token).ProviderErrorRun; got != 0 {
		t.Fatalf("the stored run of provider errors is %d after the compaction, want 0", got)
	}
	if got := h.claim().Session; got != session {
		t.Fatalf("the compacted claim records session %q, want the session it had, %q", got, session)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the compacted claim is %s, want idle again", got)
	}
	h.wantPrompts(sent + 1)
	h.must(StreamTurnStart{Claim: h.token})

	for turn := range providerErrorRunTold {
		h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
		if turn+1 < providerErrorRunTold {
			h.climbs(sent+1+turn, promptRetryMax)
		}
	}

	if notices != 2 {
		t.Fatalf("the controller was told %d times, want a second notice after six more refused turns", notices)
	}
}

// A compaction Oh My Pi refused or skipped resets nothing: the session is as it was, so the run
// of refused turns it carried still stands and the controller is not told again for it. The
// owed task goes again all the same.
func TestACompactionThatWasRefusedKeepsTheRunOfProviderErrors(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends
	h.conn.RefuseCompact("Nothing to compact (session too small)")

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if reason := runtimeRefusalReason(err); reason != "Nothing to compact (session too small)" {
		t.Fatalf("compact answered %v, want Oh My Pi's own refusal", err)
	}
	h.m.Wait()
	if got := h.claim().ProviderErrorRun; got != providerErrorRunTold {
		t.Errorf("the run of provider errors is %d, want the %d it carried: nothing was compacted", got, providerErrorRunTold)
	}
	if got := h.state(); got != StateIdle {
		t.Errorf("the claim is %s, want idle again", got)
	}
	h.wantPrompts(sent + 1)
}

// runtimeRefusalReason is the words the far side refused with, as runtime.RefusalReason reads
// them; it is spelled here so this file needs no import for one call.
func runtimeRefusalReason(err error) string {
	var refusal interface{ RefusalReason() string }
	if errors.As(err, &refusal) {
		return refusal.RefusalReason()
	}
	return ""
}

// A claim being compacted is busy exactly as a claim in a turn is: it is working, a newer task
// waits rather than being sent into the compaction, a second compaction is refused, and the end
// of the compaction is what releases the task.
func TestAClaimBeingCompactedIsBusyUntilTheCompactionEnds(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.must(RequestDeliver{Claim: h.token, Task: "A newer task.", ID: "newer"})
	second := h.m.Handle(h.ctx, RequestCompact{Claim: h.token})

	var refused *RefusedError
	if !errors.As(second, &refused) {
		t.Fatalf("a second compaction answered %v, want a refusal", second)
	}
	h.wantPrompts(sent)
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s after the compaction, want idle", got)
	}
	prompts := h.wantPrompts(sent + 1)
	if prompts[sent].DeliveryID != "newer" {
		t.Fatalf("the send after the compaction carried %q, want the newer task", prompts[sent].DeliveryID)
	}
}

// waitForCompacting waits until the machine reports the claim being compacted.
func waitForCompacting(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c := h.m.View().Claim; c.Compacting && c.State == StateWorking {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the claim never reported itself being compacted")
}

// A turn that ends while the session is being compacted leaves the claim exactly where it is: a
// compaction aborts whatever turn its session was running, so a background turn's end can
// arrive under one, and taking it for the compaction's end would hand the claim its owed task
// while Oh My Pi is still writing the summary.
func TestATurnThatEndsDuringACompactionLeavesTheClaimWorking(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	if got := h.state(); got != StateWorking {
		t.Fatalf("the claim is %s after a turn ended under the compaction, want it still working", got)
	}
	h.wantPrompts(sent)
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	h.wantPrompts(sent + 1)
}

// A compaction asked for with no instructions is refused before anything is sent: Oh My Pi
// archives the history onto snapcompact's images for a compaction that carries none, and a
// worker resumed on images needs a vision model to read its own past.
func TestACompactionWithNoInstructionsIsRefusedBeforeAnythingIsSent(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)

	err := h.m.Compact(h.ctx, "  ")

	if !errors.Is(err, ErrCompactInstructions) {
		t.Fatalf("compact with no instructions answered %v, want ErrCompactInstructions", err)
	}
	if got := len(h.conn.Compactions()); got != 0 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want none", got)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s, want it left idle", got)
	}
}
