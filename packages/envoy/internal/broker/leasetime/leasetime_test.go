package leasetime

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// launched renders a walk's renewals the way the rows below write them: the instant each went
// out and how long it took to fail, and the one that got through.
func launched(attempts []Attempt) string {
	parts := make([]string, len(attempts))
	for i, a := range attempts {
		if a.Refused() {
			parts[i] = fmt.Sprintf("%v+%v", a.At, a.Delay())
		} else {
			parts[i] = "renewed " + a.At.String()
		}
	}
	return strings.Join(parts, ", ")
}

// delaysOf is the failure delay of each renewal a phase's outage refused, which is what a test
// hands a renewer's own loop to drive that phase again.
func delaysOf(p Phase) []time.Duration {
	var delays []time.Duration
	for _, a := range p.Attempts {
		if a.Refused() {
			delays = append(delays, a.Delay())
		}
	}
	return delays
}

// TestSurvivesWalksEachRenewersOwnRetrySchedule pins the worst outage of each renewer at its own
// boundary: when the gap begins, the instants it launches a renewal at, how long each of those
// took to fail, the renewal that succeeds, and whether that one came in before the lease ran
// out. Every figure is arithmetic on the renewer's own code, so a change to either loop's waits
// that this package does not follow fails here.
//
// A renewal the outage refuses fails at any instant from the one it went out at to ClientTimeout
// after it, not only at one of those two. That is what the rows at a 10-second and a 60-second
// sweep turn on: two renewals refused at once, one that fails part of the way through its
// timeout and so goes out again at the last instant inside the gap, and that one held for the
// whole timeout, which is what pushes the renewal that gets through past the lease.
func TestSurvivesWalksEachRenewersOwnRetrySchedule(t *testing.T) {
	const ns = time.Nanosecond
	for _, c := range []struct {
		what         string
		renewer      Renewer
		lease, sweep time.Duration
		gapStart     time.Duration
		attempts     string
		renewed      time.Duration
		survives     bool
	}{
		{
			// The helper's floor is its 30-second MinRenew, so a 90-second lease renews at 30 s
			// and not at a third of 30. The gap begins there; the renewal is held for the whole
			// timeout and fails at 60 s, the lease left is below MinRenew again, and the next
			// renewal is at 90 s: the instant the lease ran out, one instant too late.
			what:    "agent-secrets-helper, the largest lease that lapses at a 5-second sweep",
			renewer: RenewerHelper, lease: 90 * time.Second, sweep: 5 * time.Second,
			gapStart: 30 * time.Second,
			attempts: "30s+30s, renewed 1m30s",
			renewed:  90 * time.Second, survives: false,
		},
		{
			what:    "agent-secrets-helper, one second more at a 5-second sweep",
			renewer: RenewerHelper, lease: 91 * time.Second, sweep: 5 * time.Second,
			gapStart: 30333333333 * ns,
			attempts: "30.333333333s+30s, renewed 1m30.333333333s",
			renewed:  90333333333 * ns, survives: true,
		},
		{
			// agent-secrets renew at a 10-second sweep, where it is the stricter of the two.
			// A 104-second lease renews at 34.67 s, into a 30-second gap. The renewals at 34.67
			// and 36.67 s are refused at once, two and four seconds of backoff apart; the one at
			// 40.67 s fails 16 s into its timeout, and its eight-second backoff puts the next at
			// the last instant inside the gap; that one is held for the whole timeout and fails
			// at 94.67 s, and the ten-second cap puts the renewal that gets through at 104.67 s,
			// after the lease.
			what:    "agent-secrets renew, the largest lease that lapses at a 10-second sweep",
			renewer: RenewerAgentSecrets, lease: 104 * time.Second, sweep: 10 * time.Second,
			gapStart: 34666666666 * ns,
			attempts: "34.666666666s+0s, 36.666666666s+0s, 40.666666666s+15.999999999s, 1m4.666666665s+30s, renewed 1m44.666666665s",
			renewed:  104666666665 * ns, survives: false,
		},
		{
			what:    "agent-secrets renew, one second more at a 10-second sweep",
			renewer: RenewerAgentSecrets, lease: 105 * time.Second, sweep: 10 * time.Second,
			gapStart: 35 * time.Second,
			attempts: "35s+0s, 37s+0s, 41s+15.999999999s, 1m4.999999999s+30s, renewed 1m44.999999999s",
			renewed:  104999999999 * ns, survives: true,
		},
		{
			// The helper at a 60-second sweep, where the gap is three minutes. Two renewals
			// refused at once, one that fails 17 s in and so goes out again at the last instant
			// inside the gap, and that one held for the whole timeout: by then the lease has
			// run out, which leaves the helper at its floor, and the renewal that gets through
			// is 30 s later.
			what:    "agent-secrets-helper, the largest lease that lapses at a 60-second sweep",
			renewer: RenewerHelper, lease: 359 * time.Second, sweep: time.Minute,
			gapStart: 119666666666 * ns,
			attempts: "1m59.666666666s+0s, 3m19.444444444s+0s, 4m12.629629629s+17.037037036s, 4m59.666666665s+30s, renewed 5m59.666666665s",
			renewed:  359666666665 * ns, survives: false,
		},
		{
			what:    "agent-secrets-helper, one second more at a 60-second sweep",
			renewer: RenewerHelper, lease: 360 * time.Second, sweep: time.Minute,
			gapStart: 120 * time.Second,
			attempts: "2m0s+0s, 3m20s+0s, 4m13.333333333s+16.666666666s, 4m59.999999999s+30s, renewed 5m59.999999999s",
			renewed:  359999999999 * ns, survives: true,
		},
		{
			// The review's two worked lapses at leases the two-outcome model accepted. Each is
			// the same shape: renewals refused at once, one that fails part of the way through
			// its timeout, and the last one held for the whole of it.
			what:    "agent-secrets renew, a lease the two-outcome model accepted",
			renewer: RenewerAgentSecrets, lease: 100 * time.Second, sweep: 10 * time.Second,
			gapStart: 33333333333 * ns,
			attempts: "33.333333333s+0s, 35.333333333s+0s, 39.333333333s+15.999999999s, 1m3.333333332s+30s, renewed 1m43.333333332s",
			renewed:  103333333332 * ns, survives: false,
		},
		{
			what:    "agent-secrets-helper, a lease the two-outcome model accepted",
			renewer: RenewerHelper, lease: 355 * time.Second, sweep: time.Minute,
			gapStart: 118333333333 * ns,
			attempts: "1m58.333333333s+0s, 3m17.222222222s+0s, 4m9.814814814s+18.518518518s, 4m58.333333332s+30s, renewed 5m58.333333332s",
			renewed:  358333333332 * ns, survives: false,
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := Production().WorstFor(c.renewer, c.lease, c.sweep)
			if p.GapStart != c.gapStart || p.GapEnd != c.gapStart+NotServingSweeps*c.sweep {
				t.Errorf("gap [%v, %v), want it to begin at %v and last %d sweeps", p.GapStart, p.GapEnd, c.gapStart, NotServingSweeps)
			}
			if got := launched(p.Attempts); got != c.attempts {
				t.Errorf("renewals %s,\n     want %s", got, c.attempts)
			}
			if p.Renewed != c.renewed || p.Survived() != c.survives {
				t.Errorf("renewed at %v, survived %v; want %v and %v", p.Renewed, p.Survived(), c.renewed, c.survives)
			}
		})
	}
}

// TestNoOutageRenewsLaterThanWorstForFound checks the forward pass against searches written
// another way, which try failure instants the pass does not reason about at all.
//
// A short sweep is searched whole: at one second the gap is three seconds, so an outage
// refuses two or three renewals and a grid of one second over each one's failure instant
// walks every combination of those instants. A grid can only miss a worse instant, never
// invent one, so it checks the pass from one side, the side that matters: no walk it finds
// renews later than WorstFor. The longer sweeps let an outage refuse twenty renewals, where a
// whole search is nothing any test can run, so they are sampled: ten thousand walks per lease,
// their delays drawn from a seed this row fixes, beside the two walks at the ends of the range.
func TestNoOutageRenewsLaterThanWorstForFound(t *testing.T) {
	m := Production()
	for _, sweep := range []time.Duration{time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute} {
		gap := NotServingSweeps * sweep
		for lease := 30 * time.Second; lease <= 400*time.Second; lease += 11 * time.Second {
			for _, r := range Renewers() {
				worst := m.WorstFor(r, lease, sweep)
				first := m.firstWait(r, lease)
				got, how := time.Duration(0), ""
				if sweep == time.Second {
					got, how = m.scanFailures(r, lease, first+gap, first, 0, time.Second), "a grid of every failure instant"
				} else {
					got, how = m.sampleFailures(r, lease, first+gap, 10000), "ten thousand sampled walks"
				}
				if got > worst.Renewed {
					t.Fatalf("%s at lease %v sweep %v: %s renews at %v, later than the %v WorstFor found",
						r, lease, sweep, how, got, worst.Renewed)
				}
			}
		}
	}
}

// scanFailures answers the latest renewal it can find by trying, for every renewal the outage
// refuses, each failure instant on a grid of step. It is the plain recursion the forward pass
// replaces, kept here to check that pass rather than to be used by anything.
func (m Model) scanFailures(r Renewer, lease, gapEnd, at, wait, step time.Duration) time.Duration {
	if at >= gapEnd {
		return at
	}
	worst := time.Duration(0)
	for delay := time.Duration(0); delay <= m.ClientTimeout; delay += step {
		_, next, kept := m.step(r, lease, at, wait, delay)
		if got := m.scanFailures(r, lease, gapEnd, next, kept, step); got > worst {
			worst = got
		}
	}
	return worst
}

// sampleFailures walks the outage walks times, each renewal it refuses failing at an instant
// drawn from a fixed seed, and answers the latest renewal any of them reached. The two walks
// at the ends of the range — every renewal refused at once, and every one held for the whole
// timeout — are walked as well, since a draw is unlikely to land on either.
func (m Model) sampleFailures(r Renewer, lease, gapEnd time.Duration, walks int) time.Duration {
	draw := rand.New(rand.NewPCG(uint64(lease), uint64(gapEnd)))
	worst := time.Duration(0)
	for walk := range walks + 2 {
		at, wait := m.firstWait(r, lease), time.Duration(0)
		for at < gapEnd {
			delay := time.Duration(draw.Int64N(int64(m.ClientTimeout) + 1))
			switch walk {
			case 0:
				delay = 0
			case 1:
				delay = m.ClientTimeout
			}
			_, next, kept := m.step(r, lease, at, wait, delay)
			at, wait = next, kept
		}
		worst = max(worst, at)
	}
	return worst
}

// TestShortestIsTheWorseOfTheTwoRenewers pins the shortest lease the broker can accept at the
// sweeps the documents and the tests name, and that it is the worse of the two renewers' own
// floors at every sweep the broker takes. Which renewer sets it changes with the sweep:
// agent-secrets renew's floor grows steadily with the sweep, while the helper's stays at 91 s
// up to a 10-second sweep, where its gap can refuse only its first renewal, and rises faster
// after that. So agent-secrets renew is the stricter only at sweeps of 7 to 10 seconds.
func TestShortestIsTheWorseOfTheTwoRenewers(t *testing.T) {
	m := Production()
	for _, c := range []struct {
		sweep time.Duration
		floor time.Duration
		by    Renewer
	}{
		{time.Second, 91 * time.Second, RenewerHelper},
		{5 * time.Second, 91 * time.Second, RenewerHelper},
		{10 * time.Second, 105 * time.Second, RenewerAgentSecrets},
		{15 * time.Second, 158 * time.Second, RenewerHelper},
		{20 * time.Second, 180 * time.Second, RenewerHelper},
		{30 * time.Second, 225 * time.Second, RenewerHelper},
		{time.Minute, 360 * time.Second, RenewerHelper},
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
		if len(full.Attempts) != len(short.Attempts) {
			t.Errorf("%s: the scaled walk is %s, the production walk %s", r, launched(short.Attempts), launched(full.Attempts))
		}
	}
}

// TestWalkTakesTheDelaysItIsGiven pins the Walk a test drives a real renew loop by: the outage
// it describes fails each renewal after the delay the caller names, which is what lets a
// loop-drive row reproduce the worst phase this package found.
func TestWalkTakesTheDelaysItIsGiven(t *testing.T) {
	m := Production()
	worst := m.WorstFor(RenewerAgentSecrets, 100*time.Second, 10*time.Second)
	delays := delaysOf(worst)
	replayed := m.Walk(RenewerAgentSecrets, worst.Lease, worst.GapEnd-worst.GapStart, worst.GapStart, delays...)
	if launched(replayed.Attempts) != launched(worst.Attempts) || replayed.Renewed != worst.Renewed {
		t.Errorf("replaying the worst phase launched %s renewing at %v; the worst phase was %s renewing at %v",
			launched(replayed.Attempts), replayed.Renewed, launched(worst.Attempts), worst.Renewed)
	}
	if refusedAtOnce := m.Walk(RenewerAgentSecrets, worst.Lease, worst.GapEnd-worst.GapStart, worst.GapStart); refusedAtOnce.Renewed >= worst.Renewed {
		t.Errorf("an outage that refuses every renewal at once renews at %v, no sooner than the worst phase's %v; the delays are meant to be what makes that phase the worst",
			refusedAtOnce.Renewed, worst.Renewed)
	}
	if partial := slices.ContainsFunc(delays, func(d time.Duration) bool { return d > 0 && d < m.ClientTimeout }); !partial {
		t.Errorf("the worst phase's delays are %v; this row exists for one that fails part of the way through its timeout", delays)
	}
}
