package supervise

import (
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// reported is one durable transition the daemon was told about (Machine.OnDurableState), with the
// claim copy it was told it under: the daemon reads the issue, the role and the generation from
// that copy, so which claim a report carries is as much of the fact as the state is.
type reported struct {
	claim Claim
	state ClaimState
}

// watchReports installs the daemon's callback and returns a reader of what it was told, in order.
func watchReports(h *harness) func() []reported {
	var mu sync.Mutex
	var seen []reported
	h.m.OnDurableState(func(c Claim, state ClaimState) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, reported{claim: c, state: state})
	})
	return func() []reported {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// states is the transitions alone, for a test that holds which were reported and in what order.
func states(reports []reported) []ClaimState {
	seen := make([]ClaimState, 0, len(reports))
	for _, r := range reports {
		seen = append(seen, r.state)
	}
	return seen
}

// A report is made after the write of the transition it reports, so a decision whose write the
// database did not take makes none: the daemon would otherwise be told a claim fact the row does
// not hold and nothing would ever put there. The report is owed with the write, and the repair
// makes it when the write lands — unless what it would tell the daemon has stopped being true,
// which is the two drops below.
//
// The measured incident lost the working report of a turn that started, and with it the daemon's
// record that a woken pod's role came back and took work.
func TestTheOwedReportsAreMadeWhenTheWriteLandsAndNotBefore(t *testing.T) {
	// Two decisions fail under the same outage, each owing the report its write would have
	// carried. Both are made when the repair lands, in the order the decisions made them: the
	// daemon learns what it would have learnt had every write been taken, only later. Neither is
	// made before, because until the write lands the row says neither.
	t.Run("two decisions' reports are made in the order they were owed", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateRegistered)
		reports := watchReports(h)
		h.store.fail("PutClaim", brokenConnection())

		if err := h.handle(RequestReady{Claim: testToken, Generation: h.generation(), Session: session}); err == nil {
			t.Fatal("the ready whose write failed returned no error")
		}
		if err := h.handle(StreamTurnStart{Claim: testToken}); err == nil {
			t.Fatal("the turn start whose write failed returned no error")
		}
		h.wantState(StateWorking)
		if seen := reports(); len(seen) != 0 {
			t.Fatalf("the daemon was told %v while the writes were still owed", states(seen))
		}

		h.store.fail("PutClaim", nil)
		h.observe(runtime.Alive)

		if seen := states(reports()); !slices.Equal(seen, []ClaimState{StateReady, StateWorking}) {
			t.Fatalf("the repair reported %v, want ready then working, in the order the decisions owed them", seen)
		}
		if stored := h.store.load(testToken); stored.State != StateWorking {
			t.Fatalf("the repaired row reads %s, want working: the reports follow the write, never lead it", stored.State)
		}
	})

	// A report the claim has reported past is dropped rather than made: a later report has
	// already said where the claim is, and making the older one would tell the daemon the claim
	// is somewhere it has left. Here the delivery's table is the one the outage is on, so the
	// confirmation's write — the claim and the delivery together — is refused while the claim's
	// own writes land, and the foreign turn that follows reports working of its own.
	t.Run("a report the claim has reported past is dropped", func(t *testing.T) {
		h := newHarness(t)
		reports := watchReports(h)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		h.store.fail("PutDelivery", brokenConnection())

		if err := h.handle(StreamTurnStart{Claim: testToken}); err == nil {
			t.Fatal("the confirmation whose write failed returned no error")
		}
		if h.m.owedWrite == nil {
			t.Fatal("the failed confirmation left no mark")
		}
		// No prompt result follows this turn's end, so the task is still the claim's and the
		// repair still owes both rows; the claim goes idle, then its agent starts a turn of its
		// own, which confirms nothing and whose claim-only write lands.
		_ = h.handleQuiet(StreamTurnEnd{Claim: testToken})
		h.wantState(StateIdle)
		h.must(StreamTurnStart{Claim: testToken})
		h.wantState(StateWorking)
		if seen := states(reports()); !slices.Equal(seen, []ClaimState{StateReady, StateWorking}) {
			t.Fatalf("the daemon was told %v, want the ready and the foreign turn's own working", seen)
		}

		h.store.fail("PutDelivery", nil)
		h.observe(runtime.Alive)

		if seen := states(reports()); !slices.Equal(seen, []ClaimState{StateReady, StateWorking}) {
			t.Fatalf("the daemon was told %v; the owed working was made after the claim had reported past it", seen)
		}
		if dropped := h.logs.lines("an owed report is dropped", "the claim has reported since it was owed"); len(dropped) != 1 {
			t.Fatalf("the dropped report is logged %d times, want once: %v", len(dropped), dropped)
		}
		if h.m.owedWrite != nil {
			t.Fatal("the write is still owed after the repair made it")
		}
	})

	// A report of a generation the claim has left is dropped too: the daemon keys the workflow
	// fact on the claim's generation, so the report names a fact of a run that is over. The
	// process died while the write was owed and the claim relaunched into a new generation, which
	// is the run that is now standing.
	t.Run("a report of a generation the claim has left is dropped", func(t *testing.T) {
		h := newHarness(t)
		reports := watchReports(h)
		h.reach(StateReady)
		owedAt := h.generation()
		h.store.fail("PutClaim", brokenConnection())

		if err := h.handle(StreamTurnStart{Claim: testToken}); err == nil {
			t.Fatal("the turn start whose write failed returned no error")
		}
		// The process dies: the claim relaunches into the next generation, and that launch's own
		// write is refused by the same outage, so the mark is still standing when it lands.
		_ = h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}})
		if now := h.generation(); now == owedAt {
			t.Fatalf("the claim is still at generation %d; the death was to relaunch it into the next", now)
		}
		if h.m.owedWrite == nil {
			t.Fatal("the failed launch write cleared the mark the turn start left")
		}

		h.store.fail("PutClaim", nil)
		h.advance(testProbe) // the relaunch waiting on the probe interval; its decision repairs first

		if seen := states(reports()); !slices.Equal(seen, []ClaimState{StateReady}) {
			t.Fatalf("the daemon was told %v; the owed working named a generation the claim has left", seen)
		}
		if dropped := h.logs.lines("an owed report is dropped", "the claim has left the generation it was owed at"); len(dropped) != 1 {
			t.Fatalf("the dropped report is logged %d times, want once: %v", len(dropped), dropped)
		}
	})

	// The report a repair makes carries the claim of its own decision, not the claim the repair
	// wrote. The two differ whenever the claim decided on while the database was away: here the
	// turn the report is of has ended, so the repair writes an idle claim, and the working the
	// daemon is told is the turn's own.
	t.Run("each report carries the claim of its own decision", func(t *testing.T) {
		h := newHarness(t)
		reports := watchReports(h)
		h.reach(StateReady)
		h.store.fail("PutClaim", brokenConnection())

		if err := h.handle(StreamTurnStart{Claim: testToken}); err == nil {
			t.Fatal("the turn start whose write failed returned no error")
		}
		_ = h.handle(StreamTurnEnd{Claim: testToken})
		h.wantState(StateIdle)

		h.store.fail("PutClaim", nil)
		h.observe(runtime.Alive)

		seen := reports()
		if states := states(seen); !slices.Equal(states, []ClaimState{StateReady, StateWorking}) {
			t.Fatalf("the daemon was told %v, want ready then the owed working", states)
		}
		owed := seen[len(seen)-1]
		if owed.claim.State != StateWorking {
			t.Fatalf("the owed report carries a claim reading %s, want the working claim of the decision that owed it", owed.claim.State)
		}
		if owed.claim.Generation != h.generation() {
			t.Fatalf("the owed report carries generation %d, want %d: the daemon keys the fact on it",
				owed.claim.Generation, h.generation())
		}
		if stored := h.store.load(testToken); stored.State != StateIdle {
			t.Fatalf("the repaired row reads %s, want idle: the repair writes the claim as the machine holds it now", stored.State)
		}
	})
}
