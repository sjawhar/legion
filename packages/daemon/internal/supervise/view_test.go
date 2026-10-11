package supervise

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// View never waits on a decision in flight, where Claim does: while a relaunch waits in the
// runtime it reports the claim as the relaunch last wrote it — launching at the next generation,
// with no process yet — and once the decision ends, as the decision left it. A reader of every
// claim reads it, so one claim's relaunch never holds the others' answer (LEGION-650).
func TestViewAnswersWhileARelaunchWaitsInTheRuntime(t *testing.T) {
	h := newBareHarness(t)
	gated := &gatedResume{Runtime: h.rt, entered: make(chan struct{}, 1), release: make(chan struct{})}
	h.deps.Runtime = gated
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	h.reach(StateIdle)
	dead := h.locator()

	died := make(chan error, 1)
	go func() {
		died <- h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now()}})
	}()
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the relaunch never reached the runtime")
	}
	viewed := make(chan ClaimView, 1)
	go func() { viewed <- h.m.View() }()
	select {
	case c := <-viewed:
		if c.State != StateLaunching || c.Generation != 2 || c.Locator != nil {
			t.Errorf("while the relaunch waits View reports %s at generation %d with locator %+v, want launching at 2 with none",
				c.State, c.Generation, c.Locator)
		}
	case <-time.After(time.Second):
		t.Fatal("View waited on the relaunch in the runtime")
	}

	close(gated.release)
	if err := <-died; err != nil {
		t.Fatalf("handle the death: %v", err)
	}
	if c := h.m.View(); c.State != StateLaunching || c.Generation != 2 || c.Locator == nil || *c.Locator == dead {
		t.Errorf("once the relaunch returns View reports %s at generation %d with locator %+v, want launching at 2 with the new process",
			c.State, c.Generation, c.Locator)
	}
}

// View says a claim's process is being stopped (ClaimView.Stopping) only while the decision that
// stops it ends the claim's run, as the operator's suspension does. A process the registration
// deadline retires is relaunched in the same decision, so its claim keeps its role throughout and
// View does not say it is stopping: notice routing reads Stopping, and a sub-architect being
// relaunched would otherwise lose its notices to the architect above it (LEGION-650).
func TestViewSaysAProcessIsStoppingOnlyWhenItsClaimDoesNotRunAgain(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reach    ClaimState
		stop     func(h *harness)
		stopping bool
	}{
		{name: "the operator's suspension", reach: StateIdle, stopping: true,
			stop: func(h *harness) { _ = h.m.Handle(h.ctx, RequestSuspend{Claim: testToken}) }},
		{name: "the registration deadline's retirement and relaunch", reach: StateLaunching,
			stop: func(h *harness) { h.clock.Advance(deadline) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBareHarness(t)
			gated := &gatedRuntime{Runtime: h.rt, entered: make(chan struct{}, 1), release: make(chan struct{})}
			h.deps.Runtime = gated
			if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
				t.Fatal(err)
			}
			h.start(queuedClaim())
			h.reach(tc.reach)

			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				tc.stop(h)
			}()
			select {
			case <-gated.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the suspension never reached the runtime")
			}
			if v := h.m.View(); v.Stopping != tc.stopping {
				t.Errorf("while the runtime stops the process View says stopping %t, want %t", v.Stopping, tc.stopping)
			}
			close(gated.release)
			<-stopped
			h.m.Wait()
			if h.m.View().Stopping {
				t.Error("once the decision returned View still says the process is stopping")
			}
		})
	}
}
