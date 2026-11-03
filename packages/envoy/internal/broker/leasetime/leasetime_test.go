package leasetime

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// at is one renewal a walk is expected to launch: the instant, and whether the outage held it to
// its client timeout rather than refusing it at once.
type at struct {
	when time.Duration
	held bool
}

func (a at) String() string {
	if a.held {
		return a.when.String() + " held"
	}
	return a.when.String()
}

// launched renders a walk's renewals the way the rows below write them.
func launched(attempts []Attempt) string {
	parts := make([]string, len(attempts))
	for i, a := range attempts {
		parts[i] = at{a.At, a.Held}.String()
	}
	return strings.Join(parts, ", ")
}

func want(attempts []at) string {
	parts := make([]string, len(attempts))
	for i, a := range attempts {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", ")
}

// TestSurvivesWalksEachRenewersOwnRetrySchedule pins the worst outage of each renewer at its own
// boundary: when the gap begins, the instants it launches a renewal at, which of those the outage
// held to the client timeout rather than refusing at once, the renewal that succeeds, and whether
// that one came in before the lease ran out. Every figure is arithmetic on the renewer's own
// code, so a change to either loop's waits that this package does not follow fails here.
//
// The held renewals are the half of this the first version of the model missed. It held a
// renewal only when that renewal was the first whose timeout would pass the end of the gap, and
// refused every other at once; a renewer refused at once retries sooner, so it can place a later
// renewal just before the broker returns, and holding that one is what costs it the whole
// timeout. The rows at a 60-second sweep below are that case.
func TestSurvivesWalksEachRenewersOwnRetrySchedule(t *testing.T) {
	const ns = time.Nanosecond
	for _, c := range []struct {
		what         string
		renewer      Renewer
		lease, sweep time.Duration
		gapStart     time.Duration
		attempts     []at
		renewed      time.Duration
		survives     bool
	}{
		{
			// agent-secrets renew holds an 81-second lease, so it renews at 27 s. The broker
			// stops serving an instant before that and is back 15 s later, at 41 s. The
			// renewals at 27, 29 and 33 s are refused at once — two, four and eight seconds of
			// backoff — and the one at 41 s, launched the instant before the broker is back, is
			// black-holed and costs its whole 30-second client timeout. It fails at 71 s, and
			// the ten-second cap puts the next at 81 s: the instant the lease ran out, which is
			// one instant too late.
			what:    "agent-secrets renew, the largest lease that lapses at a 5-second sweep",
			renewer: RenewerAgentSecrets, lease: 81 * time.Second, sweep: 5 * time.Second,
			gapStart: 26000000001 * ns,
			attempts: []at{{27 * time.Second, false}, {29 * time.Second, false}, {33 * time.Second, false}, {41 * time.Second, true}, {81 * time.Second, false}},
			renewed:  81 * time.Second, survives: false,
		},
		{
			// One second more of lease moves every instant of that walk a third of a second
			// later, and the renewal lands two thirds of a second before the lease runs out.
			what:    "agent-secrets renew, one second more",
			renewer: RenewerAgentSecrets, lease: 82 * time.Second, sweep: 5 * time.Second,
			gapStart: 26333333334 * ns,
			attempts: []at{{27333333333 * ns, false}, {29333333333 * ns, false}, {33333333333 * ns, false}, {41333333333 * ns, true}, {81333333333 * ns, false}},
			renewed:  81333333333 * ns, survives: true,
		},
		{
			// The helper's floor is its 30-second MinRenew, so a 90-second lease renews at 30 s
			// and not at a third of 30. Black-holed there, the renewal fails at 60 s, and the
			// lease left is below MinRenew again, so the next is at 90 s: the instant the lease
			// ran out.
			what:    "agent-secrets-helper, the largest lease that lapses at a 5-second sweep",
			renewer: RenewerHelper, lease: 90 * time.Second, sweep: 5 * time.Second,
			gapStart: 15000000001 * ns,
			attempts: []at{{30 * time.Second, true}, {90 * time.Second, false}},
			renewed:  90 * time.Second, survives: false,
		},
		{
			what:    "agent-secrets-helper, one second more at a 5-second sweep",
			renewer: RenewerHelper, lease: 91 * time.Second, sweep: 5 * time.Second,
			gapStart: 15333333334 * ns,
			attempts: []at{{30333333333 * ns, true}, {90333333333 * ns, false}},
			renewed:  90333333333 * ns, survives: true,
		},
		{
			// At a 10-second sweep agent-secrets renew is the stricter of the two, which is why
			// the broker's own floor is the worse of the pair rather than one renewer's. Its
			// 96-second lease renews at 32 s, into a 30-second gap; the renewals at 32, 34, 38
			// and 46 s are refused at once and the one at 56 s, launched the instant before the
			// broker is back, is held to 86 s, which puts the next at 96 s.
			what:    "agent-secrets renew, the largest lease that lapses at a 10-second sweep",
			renewer: RenewerAgentSecrets, lease: 96 * time.Second, sweep: 10 * time.Second,
			gapStart: 26000000001 * ns,
			attempts: []at{{32 * time.Second, false}, {34 * time.Second, false}, {38 * time.Second, false}, {46 * time.Second, false}, {56 * time.Second, true}, {96 * time.Second, false}},
			renewed:  96 * time.Second, survives: false,
		},
		{
			what:    "agent-secrets renew, one second more at a 10-second sweep",
			renewer: RenewerAgentSecrets, lease: 97 * time.Second, sweep: 10 * time.Second,
			gapStart: 26333333334 * ns,
			attempts: []at{{32333333333 * ns, false}, {34333333333 * ns, false}, {38333333333 * ns, false}, {46333333333 * ns, false}, {56333333333 * ns, true}, {96333333333 * ns, false}},
			renewed:  96333333333 * ns, survives: true,
		},
		{
			// The helper at a 60-second sweep, where the gap is three minutes and the worst
			// outage holds two of its renewals and refuses two. A 350-second lease renews at
			// 116.67 s; that one is refused at once, the one at 194.44 s is held to 224.44 s,
			// the one at 266.30 s is refused, and the one at 296.30 s — the instant before the
			// broker is back — is held to 326.30 s, by which time the lease has run out, so the
			// helper waits its floor and renews at 356.30 s.
			what:    "agent-secrets-helper, the largest lease that lapses at a 60-second sweep",
			renewer: RenewerHelper, lease: 350 * time.Second, sweep: time.Minute,
			gapStart: 116296296297 * ns,
			attempts: []at{{116666666666 * ns, false}, {194444444444 * ns, true}, {266296296296 * ns, false}, {296296296296 * ns, true}, {356296296296 * ns, false}},
			renewed:  356296296296 * ns, survives: false,
		},
		{
			what:    "agent-secrets-helper, one second more at a 60-second sweep",
			renewer: RenewerHelper, lease: 351 * time.Second, sweep: time.Minute,
			gapStart: 110555555556 * ns,
			attempts: []at{{117 * time.Second, true}, {215 * time.Second, false}, {260333333333 * ns, false}, {290555555555 * ns, true}, {350555555555 * ns, false}},
			renewed:  350555555555 * ns, survives: true,
		},
		{
			// The review's own worked counterexample, which the model this replaced called
			// safe: at a 60-second sweep a 320-second lease renews at 106.67 s, is refused
			// every two, four, eight and then ten seconds to 280.67 s, and the renewal at
			// 280.67 s is held to 310.67 s, which puts the next at 320.67 s — after the lease.
			what:    "agent-secrets renew, the lease the first model printed as its floor",
			renewer: RenewerAgentSecrets, lease: 320 * time.Second, sweep: time.Minute,
			gapStart: 100666666667 * ns,
			renewed:  320666666666 * ns, survives: false,
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := Production().WorstFor(c.renewer, c.lease, c.sweep)
			if p.GapStart != c.gapStart || p.GapEnd != c.gapStart+NotServingSweeps*c.sweep {
				t.Errorf("gap [%v, %v), want it to begin at %v and last %d sweeps", p.GapStart, p.GapEnd, c.gapStart, NotServingSweeps)
			}
			if c.attempts != nil && launched(p.Attempts) != want(c.attempts) {
				t.Errorf("renewals %s,\n              want %s", launched(p.Attempts), want(c.attempts))
			}
			if p.Renewed != c.renewed || p.Survived() != c.survives {
				t.Errorf("renewed at %v, survived %v; want %v and %v", p.Renewed, p.Survived(), c.renewed, c.survives)
			}
		})
	}
}

// TestWorstForIsTheWorstOfEveryGapStartAndEveryHold checks WorstFor's search against one written
// another way: a scan of the instants a gap can begin at, and under each of them every choice of
// refusing or holding each renewal, walked without WorstFor's own reasoning about which of them
// can matter. Nothing it finds renews later than what WorstFor answered.
func TestWorstForIsTheWorstOfEveryGapStartAndEveryHold(t *testing.T) {
	m := Production()
	for _, sweep := range []time.Duration{time.Second, 5 * time.Second, 10 * time.Second, time.Minute} {
		gap := NotServingSweeps * sweep
		for lease := 30 * time.Second; lease <= 400*time.Second; lease += 7 * time.Second {
			for _, r := range Renewers() {
				worst := m.WorstFor(r, lease, sweep)
				first := m.firstWait(r, lease)
				step := max(first/499, time.Millisecond)
				for start := step; start <= first; start += step {
					if got := m.worstHeldPattern(r, lease, start+gap, first, 0); got > worst.Renewed {
						t.Fatalf("%s at lease %v sweep %v: a gap from %v renews at %v, later than the %v WorstFor found at %v",
							r, lease, sweep, start, got, worst.Renewed, worst.GapStart)
					}
				}
			}
		}
	}
}

// worstHeldPattern answers the latest renewal a gap ending at gapEnd can produce, by walking
// every choice of refusing or holding each renewal. It is the plain recursion WorstFor's
// memoised search replaces, kept here to check that search rather than to be used by anything.
func (m Model) worstHeldPattern(r Renewer, lease, gapEnd, at, wait time.Duration) time.Duration {
	if at >= gapEnd {
		return at
	}
	worst := time.Duration(0)
	for _, held := range [...]bool{false, true} {
		failed := at
		if held {
			failed = at + m.ClientTimeout
		}
		w := m.waitAfterFailure(r, wait, lease-failed)
		if got := m.worstHeldPattern(r, lease, gapEnd, failed+w, w); got > worst {
			worst = got
		}
	}
	return worst
}

// TestShortestIsTheWorseOfTheTwoRenewers pins the shortest lease the broker can accept at the
// sweeps the documents and the tests name, and that it is the worse of the two renewers' own
// floors at every sweep the broker takes. Which renewer sets it changes with the sweep: the
// helper waits at least 30 s after a failed renewal, which costs it at small sweeps, while
// agent-secrets renew retries every two to ten seconds, which puts one of its renewals closer to
// the end of a longer gap, where the outage holds it for the whole client timeout.
func TestShortestIsTheWorseOfTheTwoRenewers(t *testing.T) {
	m := Production()
	for _, c := range []struct {
		sweep time.Duration
		floor time.Duration
		by    Renewer
	}{
		{time.Second, 91 * time.Second, RenewerHelper},
		{5 * time.Second, 91 * time.Second, RenewerHelper},
		{10 * time.Second, 97 * time.Second, RenewerAgentSecrets},
		{30 * time.Second, 203 * time.Second, RenewerHelper},
		{time.Minute, 351 * time.Second, RenewerHelper},
	} {
		if got := m.Shortest(c.sweep); got != c.floor {
			t.Errorf("Shortest(%v) = %v, want %v", c.sweep, got, c.floor)
		}
		if got := m.ShortestFor(c.by, c.sweep); got != c.floor {
			t.Errorf("at sweep %v the floor %v is meant to be %s's own, but that renewer's is %v", c.sweep, c.floor, c.by, got)
		}
	}
	for seconds := 1; seconds <= 60; seconds++ {
		sweep := time.Duration(seconds) * time.Second
		shortest := m.Shortest(sweep)
		if shortest <= 0 {
			t.Fatalf("Shortest(%v) found no lease that survives", sweep)
		}
		if worse := max(m.ShortestFor(RenewerAgentSecrets, sweep), m.ShortestFor(RenewerHelper, sweep)); worse != shortest {
			t.Errorf("at sweep %v the shortest lease is %v, but the worse of the two renewers' own floors is %v", sweep, shortest, worse)
		}
	}
}

// TestShortestLeavesEveryDeploymentBooting is the other half of the refusal: a bound that
// refused a lease someone is running would stop a broker from starting at all. The default
// lease passes at every sweep the broker takes, and above each sweep's floor nothing up to
// BROKER_LEASE_SECONDS' maximum is refused, which is also what lets Load name one floor.
func TestShortestLeavesEveryDeploymentBooting(t *testing.T) {
	m := Production()
	for seconds := 1; seconds <= 60; seconds++ {
		sweep := time.Duration(seconds) * time.Second
		if !m.Survives(900*time.Second, sweep) {
			t.Errorf("the default lease of 900 s is refused at a sweep of %v", sweep)
		}
		floor := m.Shortest(sweep)
		for lease := floor; lease <= maxSearchedLease*time.Second; lease += time.Second {
			if !m.Survives(lease, sweep) {
				t.Fatalf("at sweep %v the floor is %v but a lease of %v is refused", sweep, floor, lease)
			}
		}
	}
}

// TestSurvivesAtAScaledUnitHoldsItsShape pins what the helper's loop-drive row rests on: a
// model whose every wait and whose second are shortened by the same factor walks the same
// phases, so a row that drives the real loop in a few hundred milliseconds proves the same
// arithmetic as the one the broker refuses by.
func TestSurvivesAtAScaledUnitHoldsItsShape(t *testing.T) {
	m, scaled := Production(), Production().Scale(time.Millisecond)
	if got, msWant := scaled.Shortest(5*time.Millisecond), 91*time.Millisecond; got != msWant {
		t.Errorf("the scaled floor at a 5 ms sweep is %v, want %v", got, msWant)
	}
	for _, r := range Renewers() {
		full := m.WorstFor(r, 120*time.Second, 5*time.Second)
		short := scaled.WorstFor(r, 120*time.Millisecond, 5*time.Millisecond)
		if full.Renewed/1000 != short.Renewed || full.Survived() != short.Survived() {
			t.Errorf("%s renews at %v in production and at %v a thousand times faster", r, full.Renewed, short.Renewed)
		}
		if !slices.EqualFunc(full.Attempts, short.Attempts, func(a, b Attempt) bool { return a.Held == b.Held }) {
			t.Errorf("%s: the scaled walk holds %s, the production walk %s", r, launched(short.Attempts), launched(full.Attempts))
		}
	}
}

// TestWalkTakesTheHoldsItIsGiven pins the Walk a test drives a real renew loop by: the outage it
// describes holds exactly the renewals the caller names and refuses the rest at once, which is
// what lets a loop-drive row reproduce the worst phase this package found.
func TestWalkTakesTheHoldsItIsGiven(t *testing.T) {
	m := Production()
	worst := m.WorstFor(RenewerHelper, 350*time.Second, time.Minute)
	holds := make([]bool, 0, len(worst.Attempts))
	for _, a := range worst.Attempts {
		holds = append(holds, a.Held)
	}
	replayed := m.Walk(RenewerHelper, worst.Lease, worst.GapEnd-worst.GapStart, worst.GapStart, holds...)
	if launched(replayed.Attempts) != launched(worst.Attempts) || replayed.Renewed != worst.Renewed {
		t.Errorf("replaying the worst phase launched %s renewing at %v; the worst phase was %s renewing at %v",
			launched(replayed.Attempts), replayed.Renewed, launched(worst.Attempts), worst.Renewed)
	}
	if refusedEverything := m.Walk(RenewerHelper, worst.Lease, worst.GapEnd-worst.GapStart, worst.GapStart); refusedEverything.Renewed >= worst.Renewed {
		t.Errorf("an outage that refuses every renewal at once renews at %v, no sooner than the worst phase's %v; the holds are meant to be what makes that phase the worst",
			refusedEverything.Renewed, worst.Renewed)
	}
	fmt.Fprintln(testWriter{t}, "worst helper phase at lease 350 s, sweep 60 s:", launched(worst.Attempts))
}

// testWriter prints a line through t.Log, so the row above records the phase it pinned.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
