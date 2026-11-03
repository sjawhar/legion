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

	// RenewDivisor is the share of a lease both renewers let pass before they renew: a third
	// (cmd/agent-secrets/main.go renewLoop, internal/broker/helper/server.go renewLoop).
	RenewDivisor = 3

	// RenewBackoffInitial and RenewBackoffMax are agent-secrets renew's wait after a failed
	// renewal: two seconds, doubling to a ten-second cap (cmd/agent-secrets/main.go nextBackoff).
	RenewBackoffInitial = 2 * time.Second
	RenewBackoffMax     = 10 * time.Second

	// HelperMinRenew is the floor under agent-secrets-helper's renew cadence, the MinRenew its
	// Server runs with (internal/broker/helper.Server).
	HelperMinRenew = 30 * time.Second

	// RenewClientTimeout is how long one renewal that the outage black-holes — a connection a
	// front door accepts while nothing serves behind it, or a node that is gone — holds its
	// renewer before it gives up. It is agent-secrets' whole-call HTTP timeout
	// (cmd/agent-secrets/client.go newClient). The helper's own client carries no deadline, so
	// the model charges it the same thirty seconds: an unbounded wait would make no lease
	// BROKER_LEASE_SECONDS accepts safe, and the figure is the one the two renewers share.
	RenewClientTimeout = 30 * time.Second

	// maxSearchedLease is how far Shortest looks: an hour of the model's own seconds, which is
	// BROKER_LEASE_SECONDS' own maximum.
	maxSearchedLease = 3600
)

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

// Phase is one renewer's walk through one outage, measured from the renewal that granted the
// lease: the broker stops serving at GapStart and serves again at GapEnd, the renewer launches
// a renewal at each of Attempts, and the last of those, Renewed, is the one that succeeds.
type Phase struct {
	Renewer          Renewer
	Lease            time.Duration
	GapStart, GapEnd time.Duration
	Attempts         []time.Duration
	Renewed          time.Duration
}

// Survived answers whether the renewal came in before the lease ran out. On the instant it runs
// out the enrollment is no longer live, so the renewal has to be strictly before it.
func (p Phase) Survived() bool { return p.Renewed < p.Lease }

// Survives answers whether a lease outlives every outage of NotServingSweeps sweeps, whatever
// instant one begins at, for both renewers.
func (m Model) Survives(lease, sweep time.Duration) bool {
	for _, r := range Renewers() {
		if !m.WorstFor(r, lease, sweep).Survived() {
			return false
		}
	}
	return true
}

// WorstFor answers the gap start that renews latest for one renewer. Only a gap that begins at
// or before the renewer's first attempt is walked: a later one lets that attempt through, and
// the lease it answers with starts the walk again.
func (m Model) WorstFor(r Renewer, lease, sweep time.Duration) Phase {
	gap := NotServingSweeps * sweep
	var worst Phase
	for _, start := range m.gapStarts(r, lease, gap) {
		if p := m.Walk(r, lease, gap, start); p.Renewed > worst.Renewed {
			worst = p
		}
	}
	return worst
}

// Walk answers what one renewer does when the broker stops serving gapStart after the renewal
// that granted the lease and serves again gap later.
//
// A renewal launched at or after the broker serves again succeeds at once. One launched before
// it fails, at its own instant when the connection is refused, or ClientTimeout later when the
// outage black-holed it — which the walk charges whenever that lands past the instant the
// broker serves again, the only phase where holding the connection costs the renewer anything.
// Neither renewer moves its lease when a renewal fails: agent-secrets renew keeps the expiry of
// its last answer and the helper logs "keeping the current lease", so the lease left always
// counts down from the renewal the walk starts at.
func (m Model) Walk(r Renewer, lease, gap, gapStart time.Duration) Phase {
	p := Phase{Renewer: r, Lease: lease, GapStart: gapStart, GapEnd: gapStart + gap}
	at, wait := m.firstWait(r, lease), time.Duration(0)
	for {
		p.Attempts = append(p.Attempts, at)
		if at >= p.GapEnd {
			p.Renewed = at
			return p
		}
		failed := at
		if at+m.ClientTimeout > p.GapEnd {
			failed = at + m.ClientTimeout
		}
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
	wait := lease / RenewDivisor
	if r == RenewerHelper {
		return max(m.HelperMinRenew, wait)
	}
	return wait
}

// waitAfterFailure is how long the renewer waits after a renewal that failed, holding remaining
// of its lease at the instant it failed and having waited previous after the failure before it,
// zero for the first. remaining is negative once the lease has run out, which leaves the helper
// at its floor, as its own max does.
func (m Model) waitAfterFailure(r Renewer, previous, remaining time.Duration) time.Duration {
	if r == RenewerHelper {
		return max(m.HelperMinRenew, remaining/RenewDivisor)
	}
	if previous == 0 {
		return m.BackoffInitial
	}
	return min(2*previous, m.BackoffMax)
}

// gapStarts are the gap starts worth walking: the renewer's first attempt, and, one nanosecond
// before each instant an attempt of the walk that nothing holds up is launched or times out at,
// the start that puts the broker back just after it. Which attempt is the first to find a
// broker serving changes only at those instants, so no start between two of them walks longer
// than the pair.
func (m Model) gapStarts(r Renewer, lease, gap time.Duration) []time.Duration {
	first := m.firstWait(r, lease)
	starts := []time.Duration{first}
	at, wait := first, time.Duration(0)
	for at <= first+gap {
		for _, done := range [...]time.Duration{at, at + m.ClientTimeout} {
			if start := done + 1 - gap; start > 0 && start <= first {
				starts = append(starts, start)
			}
		}
		wait = m.waitAfterFailure(r, wait, lease-at)
		at += wait
	}
	return starts
}
