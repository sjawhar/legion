package supervise

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

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

	// A claim write that lands while a write is owed is the repair, whichever decision made it:
	// the mark goes, the report the failed decision owed is made, and no later repair is left
	// anything to do. Here the delivery's table is the one the outage is on, so the
	// confirmation's write — the claim and the delivery together — is refused while the claim's
	// own writes land, and the foreign turn that follows writes the claim alone and settles it.
	// What the delivery's own write still owes is untouched by that: the outage on its table
	// outlives the claim write, and when it lifts the delivery is written with nothing reported,
	// since a report belongs to a claim write and this one was made long ago.
	t.Run("a claim write that lands makes the owed report", func(t *testing.T) {
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
		if seen := states(reports()); !slices.Equal(seen, []ClaimState{StateReady, StateWorking, StateWorking}) {
			t.Fatalf("the daemon was told %v, want the ready, the confirmation's owed working, and the foreign turn's own", seen)
		}
		if h.m.owedWrite != nil {
			t.Fatal("the claim write that landed left the write owed; a landing claim write is the repair")
		}

		h.store.fail("PutDelivery", nil)
		h.observe(runtime.Alive)

		if seen := states(reports()); !slices.Equal(seen, []ClaimState{StateReady, StateWorking, StateWorking}) {
			t.Fatalf("the daemon was told %v; the owed report was made when the claim write landed, and nothing is owed since", seen)
		}
	})

	// The order is what this one holds, so the reports it owes and the report the landing
	// decision makes of its own are of different transitions: a reader of the three can tell
	// which came first. The claim owes a ready and a working under an outage on the claims
	// table; a task queued while the agent is in its turn gives it a pending delivery, written
	// to a table the outage is not on; then the outage moves to that delivery's table, so the
	// repair — which writes the claim and the delivery together where one is pending — is
	// refused at the head of the next decision, while that decision's own claim-only write
	// lands. The write that lands is the repair, so it makes the two owed reports before the
	// report of its own transition. Left to a later repair, the daemon would hear the newest
	// fact first and the two older ones behind it, which is the order nothing lived.
	t.Run("a repair that fails at the head of a decision whose own write lands reports the owed first", func(t *testing.T) {
		h, counted := newCountedHarness(t)
		reports := watchReports(h)
		h.reach(StateRegistered)
		h.store.fail("PutClaim", brokenConnection())

		if err := h.handle(RequestReady{Claim: testToken, Generation: h.generation(), Session: session}); err == nil {
			t.Fatal("the ready whose write failed returned no error")
		}
		if err := h.handle(StreamTurnStart{Claim: testToken}); err == nil {
			t.Fatal("the turn start whose write failed returned no error")
		}
		h.wantState(StateWorking)
		// The task is queued while the agent is in a turn, so nothing is sent and the only write
		// is the delivery's own, which this outage is not on: the claim comes to hold a pending
		// delivery with its claim write still owed, which is what makes the repair a write of
		// both rows.
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		if h.claim().Pending == nil {
			t.Fatal("the queued task left no pending delivery; a repair of both rows is what this test needs")
		}
		if h.m.owedWrite == nil {
			t.Fatal("the delivery's own write cleared the mark the two failed decisions left")
		}
		if seen := reports(); len(seen) != 0 {
			t.Fatalf("the daemon was told %v while the writes were still owed", states(seen))
		}

		// The outage moves from the claims table to the deliveries table.
		h.store.fail("PutClaim", nil)
		h.store.fail("PutDelivery", brokenConnection())
		attempted := counted.mark()

		// The agent says it is ready again, which persists the claim alone (reready).
		h.must(RequestReady{Claim: testToken, Generation: h.generation(), Session: session})

		if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaimAndDelivery", "PutClaim"}) {
			t.Fatalf("the decision asked for %v, want the repair of both rows, refused, and then its own claim write", asked)
		}
		if seen := states(reports()); !slices.Equal(seen, []ClaimState{StateReady, StateWorking, StateReady}) {
			t.Fatalf("the daemon was told %v, want the owed ready and working, in that order, and the landing decision's own ready last", seen)
		}
		if h.m.owedWrite != nil {
			t.Fatal("the claim write that landed left the write owed; a landing claim write is the repair")
		}
		if repaired := h.records.of(repairedWriteLine); len(repaired) != 1 {
			t.Fatalf("the repaired line was written %d times, want once", len(repaired))
		} else if store, ok := repaired[0].attr("store"); !ok || store.String() != "PutClaim" {
			t.Fatalf("the repaired line names the store method %v, want the claim write that landed", store)
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

	// The other drop, and with the generation one the only drop there is: a repair whose own
	// write the server judged. Such a refusal is one R2 never repeats, so no write of this
	// claim is coming to put the row where the owed reports say it is, and a report of a state
	// the store never took would tell the daemon more than the store can say. The reports go
	// with the mark, and the line the drop writes is all that is left of them (R6), which is
	// why this row reads it whole rather than counting it.
	t.Run("a repair whose own write the server judged drops the reports it held", func(t *testing.T) {
		h, counted := newCountedHarness(t)
		reports := watchReports(h)
		h.reach(StateReady)
		h.store.fail("PutClaim", brokenConnection())

		if err := h.handle(StreamTurnStart{Claim: testToken}); err == nil {
			t.Fatal("the turn start whose write failed returned no error")
		}
		h.wantState(StateWorking)
		// A second decision fails under the same outage and owes a report of its own: the mark
		// is still one, and the reports it holds are two, which is what the count on the drop's
		// line is of. The turn end between them owes no report — idle is no transition the
		// daemon is told — and its write fails with the rest.
		if err := h.handleQuiet(StreamTurnEnd{Claim: testToken}); err == nil {
			t.Fatal("the turn end whose write failed returned no error")
		}
		if err := h.handle(StreamTurnStart{Claim: testToken}); err == nil {
			t.Fatal("the second turn start whose write failed returned no error")
		}
		h.wantState(StateWorking)
		if h.m.owedWrite == nil {
			t.Fatal("the failed turn starts left no mark; the working reports they owe are what this row drops")
		}
		if held := len(h.m.owedWrite.reports); held != 2 {
			t.Fatalf("the mark holds %d reports, want the two working transitions whose writes failed", held)
		}
		written := len(h.records.of(owedWriteLine))

		// The outage lifts, and what refuses the repair is the server itself: a unique
		// violation is a decision about that very write, which R2 never makes again.
		refusal := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint", Severity: "ERROR"}
		h.store.fail("PutClaim", refusal)
		attempted := counted.mark()

		// A task queued while the agent is in its turn: the repair runs at the head of that
		// decision and is refused there, while the decision's own write is of the delivery, a
		// table the refusal is not on.
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})

		if h.m.owedWrite != nil {
			t.Fatal("the repair the server refused kept the mark; that write would be made again at every decision for the life of the daemon")
		}
		if seen := states(reports()); !slices.Equal(seen, []ClaimState{StateReady}) {
			t.Fatalf("the daemon was told %v, want the ready alone: the owed working names a state the store never took", seen)
		}
		lines := h.records.of(owedWriteLine)[written:]
		if len(lines) != 1 {
			t.Fatalf("the drop wrote %d lines, want the one that is the whole record of it: %v", len(lines), lines)
		}
		line := lines[0]
		if got, ok := line.attr("claim"); !ok || got.String() != string(h.token) {
			t.Fatalf("the line carries claim %v (present %v), want %s", got, ok, h.token)
		}
		if got, ok := line.attr("store"); !ok || got.String() != "PutClaim" {
			t.Fatalf("the line names the store method %v, want the repair's own claim write", got)
		}
		if got, ok := line.attr("error"); !ok {
			t.Fatal("the line carries no error; it is the only record of what the server refused")
		} else if err, isErr := got.Any().(error); !isErr || !errors.Is(err, refusal) {
			t.Fatalf("the line carries error %v, want the server's own refusal %v", got, refusal)
		}
		if got, ok := line.attr("owed"); !ok || got.Bool() {
			t.Fatalf("the line reads owed %v (present %v), want false: the write is dropped, not kept", got, ok)
		}
		// The reports went with the mark, and this line is the only record that they did: it
		// says how many of them the write was holding (R6).
		if got, ok := line.attr("droppedReports"); !ok || got.Int64() != 2 {
			t.Fatalf("the line reads droppedReports %v (present %v), want 2: the reports the dropped write held", got, ok)
		}

		// The decision went on to its own row rather than being dropped behind the refusal.
		if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim", "PutDelivery"}) {
			t.Fatalf("the decision asked for %v, want the refused repair and then the task's own delivery write", asked)
		}
		if p := h.claim().Pending; p == nil || p.Task != "the task" {
			t.Fatalf("pending = %+v, want the task the deliver carried: the event was applied, not dropped", p)
		}

		// Nor does a later write make them true. The store takes claim writes again, the turn
		// ends, that write lands, and the daemon is told nothing of what the drop took.
		h.store.fail("PutClaim", nil)
		h.mustQuiet(StreamTurnEnd{Claim: testToken})
		h.wantState(StateIdle)
		if stored := h.store.load(testToken); stored.State != StateIdle {
			t.Fatalf("the turn's end stored %s, want idle: this row needs a later write that lands", stored.State)
		}
		if seen := states(reports()); !slices.Equal(seen, []ClaimState{StateReady}) {
			t.Fatalf("the daemon was told %v after a write that landed; the dropped reports are gone for good", seen)
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
