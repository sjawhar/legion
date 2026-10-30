package supervise

// A worker the environment kills is restarted in its own session and told why, and the deaths that
// restart it are counted against the work they interrupted: dispatch://INFRA-714 R6. A root
// architect died six times in 54 minutes, was never told why, and its claim kept nothing a reader
// could have asked.

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The claim keeps the runtime's own account of the death it last suffered, the agent's first
// prompt after the restart names that account and what the death took with it, and the claim's
// last ready time dates both.
func TestADeadWorkerIsRestartedInItsSessionAndToldWhyItDied(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	if h.claim().LastReady.IsZero() {
		t.Fatal("the ready claim records no ready time")
	}
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
	h.must(StreamTurnStart{Claim: testToken})
	dead := h.locator()

	const detail = "pod legion-legion-209 (uid 8a1f) ended: the node was drained"
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now(), Detail: detail}})

	if got := h.claim().LastDeathCause; got != string(runtime.Gone)+": "+detail {
		t.Fatalf("the claim records the death as %q, want the runtime's own account of it", got)
	}
	h.relaunched()
	resumed := h.wantCalls("Resume", 1)[0]
	if resumed.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("the restart was %+v, want a resume of the claim's own session %s", resumed.Spec, sessionFile)
	}
	prompt := h.wantPrompts(2)[1].Message
	for _, want := range []string{detail, "tool calls in flight were never answered", "subagents and background jobs of that turn are gone", "implement the plan"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the restarted worker's first prompt is %q, want it to name %q", prompt, want)
		}
	}
	if got := h.claim().LastReady; !got.Equal(h.clock.Now()) {
		t.Errorf("the relaunched claim's ready time is %s, want the moment its new agent said it was ready, %s", got, h.clock.Now())
	}
}

// The live case: three deaths inside turns fail the claim at the launch-failure limit, and a turn
// that ends normally between the second and the third clears the count, so those three deaths are
// two and the claim is relaunched again. A death in a turn counts whether or not a task is
// pending — the second and third turns here are an Envoy message's, with nothing delivered.
func TestThreeDeathsInTurnsFailTheClaimAndANormalTurnBetweenThemClearsTheCount(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
	h.must(StreamTurnStart{Claim: testToken})

	// The first death ends the task's own turn.
	h.observe(runtime.Gone)
	h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 1})
	h.relaunched()

	// The second ends a turn a message started, with the task taken back and not yet re-confirmed.
	h.must(StreamTurnStart{Claim: testToken})
	h.observe(runtime.Gone)
	h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 2})
	h.relaunched()

	// A turn that ends normally says the agent got through what it was doing.
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantBudgets(Budgets{})

	// So the third death starts the count again rather than failing the claim.
	h.must(StreamTurnStart{Claim: testToken})
	h.observe(runtime.Gone)
	h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 1})
	h.wantState(StateLaunching)
}

// A death with nothing pending and no turn open is the environment killing a parked agent: it is
// relaunched however often it happens, and nothing is charged against it.
func TestADeathWithNoTurnOpenAndNothingPendingIsNotCounted(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("the idle claim holds %+v, want nothing pending", p)
	}

	for death := 1; death <= 3; death++ {
		h.observe(runtime.Gone)
		h.wantBudgets(Budgets{LaunchFailures: 1})
		h.relaunched()
		h.must(StreamTurnStart{Claim: testToken})
		h.must(StreamTurnEnd{Claim: testToken})
	}
	h.wantState(StateIdle)
}

// A start that failed leaves its cause on the claim beside the death's, since a claim that cannot
// start and a claim that keeps dying are different questions, and neither answer overwrites the
// other.
func TestAFailedStartIsKeptBesideTheLastDeathsCause(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
	h.must(StreamTurnStart{Claim: testToken})
	const detail = "pod legion-legion-209 (uid 8a1f) ended: OOMKilled"
	dead := h.locator()
	h.rt.ScriptResume(fake.SpawnResult{Err: errBoom})

	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now(), Detail: detail}})

	c := h.claim()
	if !strings.Contains(c.LastDeathCause, detail) {
		t.Errorf("the claim records the death as %q, want %q", c.LastDeathCause, detail)
	}
	if !strings.Contains(c.LastStartFailure, errBoom.Error()) {
		t.Errorf("the claim records the failed start as %q, want the runtime's refusal %q", c.LastStartFailure, errBoom)
	}
	if stored := h.store.load(testToken); stored.LastDeathCause != c.LastDeathCause || stored.LastStartFailure != c.LastStartFailure {
		t.Errorf("stored causes %q / %q, want them persisted as %q / %q",
			stored.LastDeathCause, stored.LastStartFailure, c.LastDeathCause, c.LastStartFailure)
	}
}

// A claim rebuilt from the store answers with the causes and the ready time it was written with:
// the operator reading a claim after a restart is the one who has to act on them.
func TestTheCausesAndTheReadyTimeSurviveARestart(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
	h.must(StreamTurnStart{Claim: testToken})
	h.observe(runtime.Gone)
	before := h.claim()

	h.restart()

	after := h.claim()
	switch {
	case after.LastDeathCause != before.LastDeathCause:
		t.Errorf("after the restart the death cause is %q, want %q", after.LastDeathCause, before.LastDeathCause)
	case !after.LastReady.Equal(before.LastReady):
		t.Errorf("after the restart the ready time is %s, want %s", after.LastReady, before.LastReady)
	case after.LastReady.IsZero():
		t.Error("the restarted claim records no ready time")
	}
}
