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
// of lease left: a third of it, with no floor (cmd/agent-secrets/main.go, renewLoop). It is the
// same arithmetic at a scaled second, since it divides whatever it is given.
func RenewAfter(leaseLeft time.Duration) time.Duration { return leaseLeft / RenewDivisor }

// NextBackoff is agent-secrets renew's wait after one more failed renewal: the first is
// RenewBackoffInitial and each after it doubles, to RenewBackoffMax
// (cmd/agent-secrets/main.go, renewLoop). Model.NextBackoff is the same step at a model's own
// second, which a scaled model needs and this one cannot give.
func NextBackoff(current time.Duration) time.Duration { return Production().NextBackoff(current) }

// HelperRenewAfter is agent-secrets-helper's own interval, kept after a renewal and after a
// failed one alike: a third of the lease left, never below minRenew, which is HelperMinRenew in
// production (internal/broker/helper/server.go, renewLoop). minRenew carries the second, so
// this is the arithmetic at any of them.
func HelperRenewAfter(leaseLeft, minRenew time.Duration) time.Duration {
	return max(minRenew, leaseLeft/RenewDivisor)
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

// NextBackoff is agent-secrets renew's wait after one more failed renewal, at this model's
// second; current is zero before the first failure.
func (m Model) NextBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return m.BackoffInitial
	}
	return min(2*current, m.BackoffMax)
}

// Attempt is one renewal a walk launched: At is when the renewer sent it and Failed the instant
// it failed, which is anywhere from At to At plus ClientTimeout. The renewal that got through
// carries a zero Failed.
type Attempt struct {
	At     time.Duration
	Failed time.Duration
}

// Refused answers whether this renewal is one the outage refused rather than the one that got
// through, and Delay how long it took to fail.
func (a Attempt) Refused() bool        { return a.Failed != 0 }
func (a Attempt) Delay() time.Duration { return a.Failed - a.At }

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
		if renewed, _ := m.worstRenewal(r, lease, NotServingSweeps*sweep); renewed >= lease {
			return false
		}
	}
	return true
}

// WorstFor answers the outage that renews latest for one renewer: when it begins, and how long
// each renewal it refuses took to fail.
//
// Two things vary. The first is when the gap begins, and the worst is the latest it can: the
// renewer's own first renewal. A gap that began later would let that renewal through and start
// the lease again, and one that began earlier would end earlier and let an earlier renewal
// through. The second is how long each refused renewal takes to fail, which is anything from
// nothing to ClientTimeout: a connection the outage refuses fails at once, one a front door
// accepts with nothing behind it is held to the whole timeout, and one a proxy gives up on at
// its own upstream timeout, or that a terminating pod resets, ends between the two. Both
// renewers wait from the instant the failure comes back (cmd/agent-secrets/main.go's renewLoop
// and internal/broker/helper/server.go's), so that instant decides where the next renewal
// falls.
//
// Taking the whole timeout is not always the worse of those: a renewer that hears sooner
// retries sooner, which can put a later renewal just before the broker returns, where that one
// then costs the whole timeout. So the walk pushes each renewal as late as it can while the
// renewal after it still falls inside the gap, and spends the whole timeout on the last one
// that does. Both next-launch functions are non-decreasing in the instant a renewal failed,
// which is what makes that the worst case rather than one case: the instants each step can
// reach form one interval, and this walks its upper end.
//
// Neither renewer moves its lease when a renewal fails: agent-secrets renew keeps the expiry of
// its last answer and the helper logs "keeping the current lease", so the lease left always
// counts down from the renewal the walk starts at.
func (m Model) WorstFor(r Renewer, lease, sweep time.Duration) Phase {
	gap := NotServingSweeps * sweep
	_, refusals := m.worstRenewal(r, lease, gap)
	return m.Walk(r, lease, gap, m.firstWait(r, lease), m.worstDelays(r, lease, gap, refusals)...)
}

// worstRenewal is the forward pass: it answers the latest renewal any outage of this gap can
// make this renewer wait for, and how many renewals that outage refuses before it.
//
// At each step the instants the renewal can go out at are one interval, because both
// next-launch functions are non-decreasing in the instant the renewal before it failed. The
// pass carries that interval's two ends: lo, where every renewal so far failed at once, and
// hi, where each was held as long as it could be. A renewal that goes out at the latest
// instant still inside the gap and is then held for the whole timeout is the worst this step
// can do, and the renewal after it is the one that gets through when the broker is back by
// then. A later step can do worse than an earlier one, because agent-secrets renew's backoff
// grows with the number of failures, which is why every step is read and not only the last.
func (m Model) worstRenewal(r Renewer, lease, gap time.Duration) (renewed time.Duration, refusals int) {
	first := m.firstWait(r, lease)
	end := first + gap
	lo, loWait := first, time.Duration(0)
	hi, hiWait := first, time.Duration(0)
	for step := 1; lo < end; step++ {
		_, next, kept := m.step(r, lease, min(hi, end-1), hiWait, m.ClientTimeout)
		if next >= end && next > renewed {
			renewed, refusals = next, step
		}
		_, loNext, loKept := m.step(r, lease, lo, loWait, 0)
		lo, loWait = loNext, loKept
		hi, hiWait = next, kept
	}
	return renewed, refusals
}

// worstDelays answers how long each of that outage's refused renewals took to fail: the last
// one the whole timeout, and the ones before it as much of theirs as puts the last of them at
// the latest instant still inside the gap. They are spent latest first, since a delay late in
// the walk moves every renewal after it one for one while an early one is worn down by each
// wait that follows it, and how much to spend is found by halving the range, the instant the
// last renewal goes out at being non-decreasing in it.
func (m Model) worstDelays(r Renewer, lease, gap time.Duration, refusals int) []time.Duration {
	if refusals <= 0 {
		return nil
	}
	target := m.firstWait(r, lease) + gap - 1
	lo, hi := time.Duration(0), time.Duration(refusals-1)*m.ClientTimeout
	for lo < hi {
		if budget := lo + (hi-lo+1)/2; m.spend(r, lease, refusals, budget).at <= target {
			lo = budget
		} else {
			hi = budget - 1
		}
	}
	return append(m.spend(r, lease, refusals, lo).delays, m.ClientTimeout)
}

// spent is one laying of a delay budget over a walk: the delay of each renewal before the last
// one the outage refuses, and the instant that last one then goes out at.
type spent struct {
	delays []time.Duration
	at     time.Duration
}

// spend lays budget over the delays of the refusals-1 renewals before the last one, latest
// first, and walks them.
func (m Model) spend(r Renewer, lease time.Duration, refusals int, budget time.Duration) spent {
	s := spent{delays: make([]time.Duration, refusals-1)}
	for i := len(s.delays) - 1; i >= 0 && budget > 0; i-- {
		s.delays[i] = min(budget, m.ClientTimeout)
		budget -= s.delays[i]
	}
	at, wait := m.firstWait(r, lease), time.Duration(0)
	for _, delay := range s.delays {
		_, next, kept := m.step(r, lease, at, wait, delay)
		at, wait = next, kept
	}
	s.at = at
	return s
}

// step answers what one refused renewal costs: the instant it failed, delay after it went out
// and never more than ClientTimeout, the instant the renewer launches the next one, and the
// wait it kept. It is the one place either renewer's schedule is applied, so the search, Walk
// and the tests all read the same steps.
func (m Model) step(r Renewer, lease, at, previous, delay time.Duration) (failed, next, kept time.Duration) {
	failed = at + min(max(delay, 0), m.ClientTimeout)
	kept = m.waitAfterFailure(r, previous, lease-failed)
	return failed, failed + kept, kept
}

// Walk answers what one renewer does when the broker stops serving gapStart after the renewal
// that granted the lease, serves again gap later, and each renewal it refuses takes the delay
// at that renewal's own place in delays to fail, failing at once once they run out.
func (m Model) Walk(r Renewer, lease, gap, gapStart time.Duration, delays ...time.Duration) Phase {
	p := Phase{Renewer: r, Lease: lease, GapStart: gapStart, GapEnd: gapStart + gap}
	at, wait := m.firstWait(r, lease), time.Duration(0)
	for i := 0; ; i++ {
		if at < p.GapStart || at >= p.GapEnd {
			p.Attempts = append(p.Attempts, Attempt{At: at})
			p.Renewed = at
			return p
		}
		var delay time.Duration
		if i < len(delays) {
			delay = delays[i]
		}
		failed, next, kept := m.step(r, lease, at, wait, delay)
		p.Attempts = append(p.Attempts, Attempt{At: at, Failed: failed})
		at, wait = next, kept
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
		return HelperRenewAfter(lease, m.HelperMinRenew)
	}
	return RenewAfter(lease)
}

// waitAfterFailure is how long the renewer waits after a renewal that failed, holding remaining
// of its lease at the instant it failed and having waited previous after the failure before it,
// zero for the first. remaining is negative once the lease has run out, which leaves the helper
// at its floor, as its own max does.
func (m Model) waitAfterFailure(r Renewer, previous, remaining time.Duration) time.Duration {
	if r == RenewerHelper {
		return HelperRenewAfter(remaining, m.HelperMinRenew)
	}
	return m.NextBackoff(previous)
}
