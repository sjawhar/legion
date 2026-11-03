// packages/envoy/internal/broker/leasetime/leasetime.go
// Package leasetime holds the waits the broker's two enrollment renewers keep, and answers
// whether an enrollment lease outlives the longest outage the broker does not give back.
//
// A gap of up to NotServingSweeps sweeps between two records that a broker serves still counts
// as a broker serving, so the broker gives none of it back: a lease that runs out inside such a
// gap is simply over, and its session is refused until something enrolls it again. A lease is
// long enough when, for every instant such a gap can begin at, the renewer that holds the lease
// renews it again before it runs out. That is not one wait but each renewer's whole retry
// schedule, which this package walks.
//
// Each renewer calls the same steps this package's model walks — RenewAfter and NextBackoff for
// agent-secrets renew, HelperRenewAfter for agent-secrets-helper, and RenewClientTimeout for the
// HTTP client of each — so a change to a renewer's schedule moves the refusal with it.
//
// It is a leaf on purpose: the standard library and nothing else. Both renewers are client
// binaries (cmd/agent-secrets, cmd/agent-secrets-helper), and internal/broker/config, the one
// place that refuses a lease, pulls in the broker's API, policy, secret and Postgres packages,
// which neither binary has any other reason to carry.
package leasetime

import "time"

const (
	// NotServingSweeps is how many BROKER_SWEEP_SECONDS a gap between two records that a broker
	// serves may last and still count as a broker serving. A longer gap is time no broker was
	// serving, which the broker gives back to every lease that was running when it began.
	NotServingSweeps = 3

	// RenewDivisor is the share of a lease both renewers let pass before they renew: a third.
	// RenewAfter and HelperRenewAfter are the two steps that divide by it, and both renewers
	// call them.
	RenewDivisor = 3

	// RenewBackoffInitial and RenewBackoffMax are agent-secrets renew's wait after a failed
	// renewal: two seconds, doubling to a ten-second cap. NextBackoff is that step, and
	// cmd/agent-secrets/main.go's renewLoop calls it.
	RenewBackoffInitial = 2 * time.Second
	RenewBackoffMax     = 10 * time.Second

	// HelperMinRenew is the floor under agent-secrets-helper's renew cadence, the MinRenew its
	// Server runs with (cmd/agent-secrets-helper/main.go sets it).
	HelperMinRenew = 30 * time.Second

	// RenewClientTimeout is how long one renewal that the outage black-holes — a connection a
	// front door accepts while nothing serves behind it, or a node that is gone — holds its
	// renewer before it gives up. Both renewers' HTTP clients carry it: cmd/agent-secrets'
	// newClient and the client cmd/agent-secrets-helper builds for helper.Broker, which is what
	// the helper's renewals go through (internal/broker/helper/broker.go, httpClient).
	RenewClientTimeout = 30 * time.Second

	// maxSearchedLease is how far Shortest looks: an hour of the model's own seconds, which is
	// BROKER_LEASE_SECONDS' own maximum.
	maxSearchedLease = 3600
)

// RenewAfter is how long agent-secrets renew waits after a renewal that answered with leaseLeft
// of lease left: a third of it, with no floor (cmd/agent-secrets/main.go, renewLoop).
func RenewAfter(leaseLeft time.Duration) time.Duration { return Production().RenewAfter(leaseLeft) }

// NextBackoff is agent-secrets renew's wait after one more failed renewal: the first is
// RenewBackoffInitial and each after it doubles, to RenewBackoffMax
// (cmd/agent-secrets/main.go, nextBackoff).
func NextBackoff(current time.Duration) time.Duration { return Production().NextBackoff(current) }

// HelperRenewAfter is agent-secrets-helper's own interval, kept after a renewal and after a
// failed one alike: a third of the lease left, never below minRenew, which is HelperMinRenew in
// production (internal/broker/helper/server.go, renewLoop).
func HelperRenewAfter(leaseLeft, minRenew time.Duration) time.Duration {
	return Production().HelperRenewAfter(leaseLeft, minRenew)
}

// Renewer names one of the two loops that renew an enrollment lease.
type Renewer string

const (
	// RenewerAgentSecrets is `agent-secrets renew`, the renewer of a pod's, a VM's or an agent
	// box's own enrollment: it waits a third of the lease, then two, four, eight, ten, ten …
	// seconds after each failed renewal.
	RenewerAgentSecrets Renewer = "agent-secrets renew"
	// RenewerHelper is agent-secrets-helper's renewLoop, which renews a machine session's
	// enrollment every max(MinRenew, a third of the lease left) and waits that same interval,
	// recomputed from the lease left, after a failed renewal.
	RenewerHelper Renewer = "agent-secrets-helper"
)

// Renewers are the two renewers, in the order Survives walks them.
func Renewers() []Renewer { return []Renewer{RenewerAgentSecrets, RenewerHelper} }

// Model is the four waits that decide how long a renewal takes, and the length of the second
// the broker's settings count in. Production is the model of the renewers as they run; Scale
// answers the same model at a shorter second, so a test can drive a real renew loop through a
// whole outage in a few hundred milliseconds and hold this model's shape. Every wait of a
// model is positive: a renewer that waited nothing would walk an outage forever.
type Model struct {
	// BackoffInitial and BackoffMax are agent-secrets renew's wait after a failed renewal.
	BackoffInitial, BackoffMax time.Duration
	// HelperMinRenew is the floor under the helper's renew cadence.
	HelperMinRenew time.Duration
	// ClientTimeout is how long a renewal the outage black-holes holds its renewer.
	ClientTimeout time.Duration
	// Unit is what one second of BROKER_LEASE_SECONDS and BROKER_SWEEP_SECONDS lasts.
	Unit time.Duration
}

// Production is the model of the two renewers as they run.
func Production() Model {
	return Model{
		BackoffInitial: RenewBackoffInitial,
		BackoffMax:     RenewBackoffMax,
		HelperMinRenew: HelperMinRenew,
		ClientTimeout:  RenewClientTimeout,
		Unit:           time.Second,
	}
}

// Scale answers this model with every wait and its second shortened by the same factor, unit
// standing in for a second. Every lease and sweep given to the scaled model is in that unit
// too, so a lease of 91 at unit a millisecond is 91 ms.
func (m Model) Scale(unit time.Duration) Model {
	at := func(d time.Duration) time.Duration { return d * unit / m.Unit }
	return Model{
		BackoffInitial: at(m.BackoffInitial),
		BackoffMax:     at(m.BackoffMax),
		HelperMinRenew: at(m.HelperMinRenew),
		ClientTimeout:  at(m.ClientTimeout),
		Unit:           unit,
	}
}

// RenewAfter is agent-secrets renew's wait after a renewal, at this model's second.
func (m Model) RenewAfter(leaseLeft time.Duration) time.Duration { return leaseLeft / RenewDivisor }

// NextBackoff is agent-secrets renew's wait after one more failed renewal, at this model's
// second; current is zero before the first failure.
func (m Model) NextBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return m.BackoffInitial
	}
	return min(2*current, m.BackoffMax)
}

// HelperRenewAfter is the helper's interval at this model's second.
func (m Model) HelperRenewAfter(leaseLeft, minRenew time.Duration) time.Duration {
	return max(minRenew, leaseLeft/RenewDivisor)
}

// Attempt is one renewal a walk launched: At is when the renewer sent it, Held whether the
// outage black-holed it, so that it failed ClientTimeout later rather than at once, and Failed
// the instant it failed. The renewal that got through carries a zero Failed.
type Attempt struct {
	At     time.Duration
	Held   bool
	Failed time.Duration
}

// Phase is one renewer's walk through one outage, measured from the renewal that granted the
// lease: the broker stops serving at GapStart and serves again at GapEnd, the renewer launches
// a renewal at each of Attempts, and the last of those, Renewed, is the one that succeeds.
type Phase struct {
	Renewer          Renewer
	Lease            time.Duration
	GapStart, GapEnd time.Duration
	Attempts         []Attempt
	Renewed          time.Duration
}

// Survived answers whether the renewal came in before the lease ran out. On the instant it runs
// out the enrollment is no longer live, so the renewal has to be strictly before it.
func (p Phase) Survived() bool { return p.Renewed < p.Lease }

// Survives answers whether a lease outlives every outage of NotServingSweeps sweeps, whatever
// instant one begins at and whatever each renewal in it meets, for both renewers.
func (m Model) Survives(lease, sweep time.Duration) bool {
	for _, r := range Renewers() {
		if !m.WorstFor(r, lease, sweep).Survived() {
			return false
		}
	}
	return true
}

// WorstFor answers the outage that renews latest for one renewer: the instant it begins, and
// what each renewal it refuses met.
//
// Two things vary, and the worst outage is the worst over both. The first is when the gap
// begins, which decides which renewal is the first to find a broker serving again. The second
// is what each renewal before that one meets: a connection the outage refuses fails at once,
// while one a front door accepts with nothing behind it, or one to a node that is gone, is held
// to ClientTimeout — and one outage can do both, refusing while its pods are gone and hanging
// while one starts. Holding a renewal is not always the worse of the two: a renewer refused at
// once retries sooner, which can place a later renewal just before the broker returns, where
// holding that one costs the whole timeout. So each renewal is searched both ways.
//
// The search is over the walk in which every renewal fails. Each of its renewals may be the
// last one the outage refuses: an outage ending just after it leaves every earlier renewal
// inside the gap and lets the next one through, so the renewal that gets through is that next
// one. The gap must also begin after the renewal that granted the lease and no later than the
// renewer's first attempt, which bounds which of them can be the last the outage refuses.
// Neither renewer moves its lease when a renewal fails: agent-secrets renew keeps the expiry of
// its last answer and the helper logs "keeping the current lease", so the lease left always
// counts down from the renewal the walk starts at.
func (m Model) WorstFor(r Renewer, lease, sweep time.Duration) Phase {
	gap := NotServingSweeps * sweep
	first := m.firstWait(r, lease)
	w := &walk{m: m, r: r, lease: lease, gap: gap, last: first + gap, seen: map[walkState]walkChoice{}}
	best := w.from(walkState{at: first})
	if !best.found {
		// No outage of this gap refuses a renewal of this renewer at all, so its first one gets
		// through.
		return Phase{Renewer: r, Lease: lease, Renewed: first}
	}
	p := Phase{Renewer: r, Lease: lease, GapEnd: best.gapEnd, GapStart: best.gapEnd - gap, Renewed: best.renewed}
	s, c := walkState{at: first}, best
	for {
		failed := s.at
		if c.held {
			failed = s.at + m.ClientTimeout
		}
		p.Attempts = append(p.Attempts, Attempt{At: s.at, Held: c.held, Failed: failed})
		wait := m.waitAfterFailure(r, s.wait, lease-failed)
		if c.stop {
			p.Attempts = append(p.Attempts, Attempt{At: failed + wait})
			return p
		}
		s = walkState{at: failed + wait, wait: wait}
		c = w.seen[s]
	}
}

// walkState is where a search stands: the instant a renewal goes out, and the wait that
// preceded it, which is what agent-secrets renew's next backoff doubles. The helper's next wait
// reads the lease left instead, so for it the wait is only ever carried along.
type walkState struct{ at, wait time.Duration }

// walkChoice is the best an outage can do from one state: the renewal it ends with, the instant
// the broker serves again that produces it, whether this state's own renewal was held, and
// whether the gap ends just after this renewal (stop) or further down the walk.
type walkChoice struct {
	renewed, gapEnd time.Duration
	held, stop      bool
	found           bool
}

// walk searches one renewer's schedule for the outage that renews latest. last is the greatest
// instant a refused renewal can be launched at: a gap that begins at the renewer's first
// attempt ends there, and one beginning later would let that first attempt through.
type walk struct {
	m     Model
	r     Renewer
	lease time.Duration
	gap   time.Duration
	last  time.Duration
	seen  map[walkState]walkChoice
}

// from answers the best an outage can do once a renewal has gone out at s, given that every
// renewal up to it was refused. The states a schedule reaches inside one gap are few — the
// instants are the first attempt plus sums of the renewer's own waits, and the walk stops at
// last — so the search is memoised on the state and runs in that many steps rather than in two
// to the power of the renewals.
func (w *walk) from(s walkState) walkChoice {
	if c, ok := w.seen[s]; ok {
		return c
	}
	// Written before the branches, so a schedule that somehow revisited a state would read an
	// answer rather than recur forever. Every wait is positive, so none does.
	w.seen[s] = walkChoice{}
	var best walkChoice
	for _, held := range [...]bool{false, true} {
		failed := s.at
		if held {
			failed = s.at + w.m.ClientTimeout
		}
		wait := w.m.waitAfterFailure(w.r, s.wait, w.lease-failed)
		next := failed + wait
		// This renewal is the last the outage refuses, so the next one gets through. The
		// broker is back a moment after this renewal went out, or, when this one went out
		// before an outage could even have begun, at the earliest instant one can end: the gap
		// has to begin after the renewal that granted the lease, which puts its end after gap
		// itself, and no later than the renewer's first attempt, which puts its end at last.
		gapEnd := max(s.at+1, w.gap+1)
		if gapEnd <= w.last && next >= gapEnd {
			if c := (walkChoice{renewed: next, gapEnd: gapEnd, held: held, stop: true, found: true}); c.renewed > best.renewed || !best.found {
				best = c
			}
		}
		// Or a later renewal is: this one fails too, and the search goes on from the next.
		if next < w.last {
			if c := w.from(walkState{at: next, wait: wait}); c.found && (c.renewed > best.renewed || !best.found) {
				best = walkChoice{renewed: c.renewed, gapEnd: c.gapEnd, held: held, found: true}
			}
		}
	}
	w.seen[s] = best
	return best
}

// Walk answers what one renewer does when the broker stops serving gapStart after the renewal
// that granted the lease, serves again gap later, and holds each renewal it refuses when held
// says so, one entry per renewal, refusing at once once the entries run out.
func (m Model) Walk(r Renewer, lease, gap, gapStart time.Duration, held ...bool) Phase {
	p := Phase{Renewer: r, Lease: lease, GapStart: gapStart, GapEnd: gapStart + gap}
	at, wait := m.firstWait(r, lease), time.Duration(0)
	for i := 0; ; i++ {
		if at >= p.GapEnd {
			p.Attempts = append(p.Attempts, Attempt{At: at})
			p.Renewed = at
			return p
		}
		failed := at
		if i < len(held) && held[i] {
			failed = at + m.ClientTimeout
		}
		p.Attempts = append(p.Attempts, Attempt{At: at, Held: failed != at, Failed: failed})
		wait = m.waitAfterFailure(r, wait, lease-failed)
		at = failed + wait
	}
}

// Shortest answers the shortest whole second of lease that Survives this sweep, and zero when
// no lease up to maxSearchedLease does. It is the figure a refused broker prints: the operator
// is told the lease to set, not only that this one is too short.
func (m Model) Shortest(sweep time.Duration) time.Duration {
	return m.shortest(func(lease time.Duration) bool { return m.Survives(lease, sweep) })
}

// ShortestFor answers the shortest whole second of lease one renewer survives this sweep. The
// broker's own floor is the longer of the two; this is each renewer's own term, which a test
// that drives that renewer's loop runs at.
func (m Model) ShortestFor(r Renewer, sweep time.Duration) time.Duration {
	return m.shortest(func(lease time.Duration) bool { return m.WorstFor(r, lease, sweep).Survived() })
}

// shortest counts up a second at a time rather than halving the range, so the answer is the
// shortest lease that survives even where a longer one would not.
func (m Model) shortest(survives func(lease time.Duration) bool) time.Duration {
	for seconds := 1; seconds <= maxSearchedLease; seconds++ {
		if lease := time.Duration(seconds) * m.Unit; survives(lease) {
			return lease
		}
	}
	return 0
}

// firstWait is how long the renewer waits after a renewal that answered with lease left.
func (m Model) firstWait(r Renewer, lease time.Duration) time.Duration {
	if r == RenewerHelper {
		return m.HelperRenewAfter(lease, m.HelperMinRenew)
	}
	return m.RenewAfter(lease)
}

// waitAfterFailure is how long the renewer waits after a renewal that failed, holding remaining
// of its lease at the instant it failed and having waited previous after the failure before it,
// zero for the first. remaining is negative once the lease has run out, which leaves the helper
// at its floor, as its own max does.
func (m Model) waitAfterFailure(r Renewer, previous, remaining time.Duration) time.Duration {
	if r == RenewerHelper {
		return m.HelperRenewAfter(remaining, m.HelperMinRenew)
	}
	return m.NextBackoff(previous)
}
