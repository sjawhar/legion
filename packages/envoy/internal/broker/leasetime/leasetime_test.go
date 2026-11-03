package leasetime

import (
	"slices"
	"testing"
	"time"
)

// TestSurvivesWalksEachRenewersOwnRetrySchedule pins the walk of each renewer at its own
// boundary: the instants it launches a renewal at, the one that succeeds, and whether that one
// came in before the lease ran out. Every figure below is arithmetic on the renewer's own code,
// so a change to either loop's waits that this package does not follow fails here.
func TestSurvivesWalksEachRenewersOwnRetrySchedule(t *testing.T) {
	const ns = time.Nanosecond
	for _, c := range []struct {
		what         string
		renewer      Renewer
		lease, sweep time.Duration
		gapStart     time.Duration
		attempts     []time.Duration
		renewed      time.Duration
		survives     bool
	}{
		{
			// agent-secrets renew holds a 48-second lease, so it renews at 16 s. The broker
			// stops serving at that instant and is back 15 s later, at 31 s; the renewal in
			// flight is black-holed and costs its whole 30-second client timeout, so it fails
			// at 46 s, and the two-second backoff puts the next one at 48 s — the instant the
			// lease ran out, which is one instant too late.
			what:    "agent-secrets renew, the largest lease that lapses at a 5-second sweep",
			renewer: RenewerAgentSecrets, lease: 48 * time.Second, sweep: 5 * time.Second,
			gapStart: 16 * time.Second,
			attempts: []time.Duration{16 * time.Second, 48 * time.Second},
			renewed:  48 * time.Second, survives: false,
		},
		{
			// One second more of lease moves every instant of that walk a third of a second
			// later, and the renewal lands two thirds of a second before the lease runs out.
			what:    "agent-secrets renew, one second more",
			renewer: RenewerAgentSecrets, lease: 49 * time.Second, sweep: 5 * time.Second,
			gapStart: 16333333333 * ns,
			attempts: []time.Duration{16333333333 * ns, 48333333333 * ns},
			renewed:  48333333333 * ns, survives: true,
		},
		{
			// The helper's floor is its 30-second MinRenew, so a 90-second lease renews at 30 s
			// and not at a third of 30. Black-holed there, the attempt fails at 60 s, and the
			// lease left (30 s) is below MinRenew again, so the next renewal is at 90 s: the
			// instant the lease ran out.
			what:    "agent-secrets-helper, the largest lease that lapses at a 5-second sweep",
			renewer: RenewerHelper, lease: 90 * time.Second, sweep: 5 * time.Second,
			gapStart: 30 * time.Second,
			attempts: []time.Duration{30 * time.Second, 90 * time.Second},
			renewed:  90 * time.Second, survives: false,
		},
		{
			what:    "agent-secrets-helper, one second more at a 5-second sweep",
			renewer: RenewerHelper, lease: 91 * time.Second, sweep: 5 * time.Second,
			gapStart: 30333333333 * ns,
			attempts: []time.Duration{30333333333 * ns, 90333333333 * ns},
			renewed:  90333333333 * ns, survives: true,
		},
		{
			// At a 60-second sweep the gap is three minutes, long enough to swallow four
			// renewals. A 319-second lease renews at 106.33 s and the broker is back at
			// 286.33 s; the attempts at 177.22, 224.48 and 255.99 s are refused at once, and
			// the one at 285.99 s is launched 0.34 s before the broker is back, so it is
			// black-holed and fails at 315.99 s. The lease has run out by then, which leaves
			// the helper at its floor, and the renewal that succeeds is at 345.99 s.
			what:    "agent-secrets-helper, the largest lease that lapses at a 60-second sweep",
			renewer: RenewerHelper, lease: 319 * time.Second, sweep: time.Minute,
			gapStart: 106333333333 * ns,
			attempts: []time.Duration{106333333333 * ns, 177222222222 * ns, 224481481481 * ns, 255987654320 * ns, 285987654320 * ns, 345987654320 * ns},
			renewed:  345987654320 * ns, survives: false,
		},
		{
			// One second more and the attempt at 256.79 s is the one the gap black-holes; it
			// fails at 286.79 s, just after the broker is back, and the renewal 30 s later
			// lands 3.21 s before the lease runs out.
			what:    "agent-secrets-helper, one second more at a 60-second sweep",
			renewer: RenewerHelper, lease: 320 * time.Second, sweep: time.Minute,
			gapStart: 106666666666 * ns,
			attempts: []time.Duration{106666666666 * ns, 177777777777 * ns, 225185185184 * ns, 256790123456 * ns, 316790123456 * ns},
			renewed:  316790123456 * ns, survives: true,
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := Production().WorstFor(c.renewer, c.lease, c.sweep)
			if p.GapStart != c.gapStart || p.GapEnd != c.gapStart+NotServingSweeps*c.sweep {
				t.Errorf("gap [%v, %v), want it to begin at %v and last %d sweeps", p.GapStart, p.GapEnd, c.gapStart, NotServingSweeps)
			}
			if !slices.Equal(p.Attempts, c.attempts) {
				t.Errorf("renewals launched at %v, want %v", p.Attempts, c.attempts)
			}
			if p.Renewed != c.renewed || p.Survived() != c.survives {
				t.Errorf("renewed at %v, survived %v; want %v and %v", p.Renewed, p.Survived(), c.renewed, c.survives)
			}
		})
	}
}

// TestSurvivesFindsTheWorstGapStart checks the shortcut WorstFor takes: it walks the handful of
// gap starts where which renewal is the first to find a broker serving changes, and nothing
// between them. A scan of a thousand starts across the whole interval a gap can begin in finds
// none that renews later than the one WorstFor picked.
func TestSurvivesFindsTheWorstGapStart(t *testing.T) {
	m := Production()
	for _, sweep := range []time.Duration{time.Second, 5 * time.Second, time.Minute} {
		gap := NotServingSweeps * sweep
		for lease := 30 * time.Second; lease <= 400*time.Second; lease += time.Second {
			for _, r := range Renewers() {
				worst := m.WorstFor(r, lease, sweep)
				first := m.firstWait(r, lease)
				for start := time.Duration(1); start <= first; start += max(first/997, 1) {
					if p := m.Walk(r, lease, gap, start); p.Renewed > worst.Renewed {
						t.Fatalf("%s at lease %v sweep %v: a gap from %v renews at %v, later than the %v WorstFor found at %v",
							r, lease, sweep, start, p.Renewed, worst.Renewed, worst.GapStart)
					}
				}
			}
		}
	}
}

// TestShortestIsTheHelpersFloor pins the shortest lease the broker can accept at the sweeps the
// documents and the tests name, and that agent-secrets-helper is the renewer that sets it at
// every sweep the broker takes: it waits at least 30 s after a failed renewal, where
// agent-secrets renew waits at most 10 s.
func TestShortestIsTheHelpersFloor(t *testing.T) {
	m := Production()
	for _, c := range []struct {
		sweep time.Duration
		floor time.Duration
	}{
		{time.Second, 91 * time.Second},
		{5 * time.Second, 91 * time.Second},
		{time.Minute, 320 * time.Second},
	} {
		if got := m.Shortest(c.sweep); got != c.floor {
			t.Errorf("Shortest(%v) = %v, want %v", c.sweep, got, c.floor)
		}
	}
	for seconds := 1; seconds <= 60; seconds++ {
		sweep := time.Duration(seconds) * time.Second
		shortest := m.Shortest(sweep)
		if shortest <= 0 {
			t.Fatalf("Shortest(%v) found no lease that survives", sweep)
		}
		if helper := m.ShortestFor(RenewerHelper, sweep); helper != shortest {
			t.Errorf("at sweep %v the shortest lease is %v but the helper's own floor is %v; the helper is meant to be the stricter renewer", sweep, shortest, helper)
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
	if got, want := scaled.Shortest(5*time.Millisecond), 91*time.Millisecond; got != want {
		t.Errorf("the scaled floor at a 5 ms sweep is %v, want %v", got, want)
	}
	for _, r := range Renewers() {
		full := m.WorstFor(r, 120*time.Second, 5*time.Second)
		short := scaled.WorstFor(r, 120*time.Millisecond, 5*time.Millisecond)
		if full.Renewed/1000 != short.Renewed || full.Survived() != short.Survived() {
			t.Errorf("%s renews at %v in production and at %v a thousand times faster", r, full.Renewed, short.Renewed)
		}
	}
}
