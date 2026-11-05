// Package wakepace paces the wakes a Legion daemon causes — the role processes it launches, and
// the tasks and notices it queues to wake a live session in place of a launch — so that a restart
// or a wave of plugin deliveries does not push an Anthropic organization over its per-minute
// uncached-input cap. A caller reserves a moment for every wake it is about to cause and holds
// that wake until the moment it is given.
//
// The limiter holds no clock. Every caller passes its own now and arms its own timer or writes its
// own next_at from the moment Reserve returns, which is what lets internal/supervise keep its own
// Clock, the outbox keep its next_at column and the workflow engine keep its timer, and what lets
// a test decide the whole behaviour with no clock at all. golang.org/x/time/rate takes a caller's
// instant too (ReserveN, AllowN), but it has no place for the jitter — a spread added outside a
// limiter does not push its next reservation out, so the spacing between one paced wake and the
// next would not hold — and its float token arithmetic would not reproduce the exact instants this
// package's tests assert, which is why the daemon paces its wakes here instead.
package wakepace

import (
	"sync"
	"time"
)

// Limiter is one wake queue's pace: at most rate wakes a minute, let out in bursts of at most
// burst, every paced release spread by the caller's jitter so that two daemons restarted together
// do not release on the same instants. It is a token bucket in its GCRA form — one theoretical
// arrival time rather than a counter and a refill timer — and is safe for use from several
// goroutines.
type Limiter struct {
	// interval is what one wake costs, time.Minute / rate. Zero is the pacing off.
	interval time.Duration
	// burst is how many wakes may leave at once before the pacing bites, never below 1.
	burst int
	// jitter draws the part of an interval added to a paced release, given the interval it is a
	// part of, and is nil for none.
	jitter func(interval time.Duration) time.Duration

	mu sync.Mutex
	// tat is the theoretical arrival time: the moment the bucket is empty again, counting every
	// wake reserved so far.
	tat time.Time
}

// New builds a limiter of rate wakes a minute in bursts of burst, spreading each paced release by
// jitter, which is handed the interval one wake costs. The daemon passes math/rand/v2's
// rand.N[time.Duration], a draw from [0, interval); a caller that wants an exact spacing — a test,
// mostly — passes nil, which adds nothing.
//
// A rate of zero or less turns the pacing off: every Reserve then returns its own now. A burst
// below 1 is treated as 1. The configuration layer refuses a smaller one by name, and a burst of
// zero would hold each release a further interval out and so hand the operator half the rate it
// asked for.
func New(rate, burst int, jitter func(interval time.Duration) time.Duration) *Limiter {
	l := &Limiter{burst: max(burst, 1), jitter: jitter}
	if rate > 0 {
		l.interval = time.Minute / time.Duration(rate)
	}
	return l
}

// Reserve returns the moment this wake may run, given the caller's own now: now itself while the
// rate is unspent — a fresh limiter, a burst still open, or a queue that has been idle long enough
// for the bucket to refill — and otherwise one interval plus a part of one after the release
// before it. It never returns a moment before now.
//
// The wake is charged on the reserve, not on the release, so a caller that reserves a wake and
// then drops it has still spent a wake's worth of the rate.
//
// now is stripped of its monotonic reading (Round(0)) before anything reads it, so that the one
// limiter is fed one kind of clock whoever reserves: the workflow engine's now is Clock().UTC()
// (workflow.Engine.now), which has none, while a machine's is time.Now() (supervise's
// reserveWake), which has one, and a theoretical arrival time set from one and compared against
// the other would mix the two. Stripping here rather than giving the engine a monotonic now is
// what keeps every caller free to pass its own clock, which is this package's whole shape: a
// moment this limiter returns is a wall-clock moment, the kind a caller writes into next_at or
// hands to a timer, and a wall-clock step moves the waits of every lane together rather than one
// lane's alone.
func (l *Limiter) Reserve(now time.Time) time.Time {
	now = now.Round(0)
	if l.interval == 0 {
		return now
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// The bucket is full as far back as burst-1 intervals before the theoretical arrival time, so
	// a queue idle for that long — a fresh limiter among them, whose tat is the zero time — wakes
	// at once and pays no jitter.
	release, arrival := now, now
	if base := l.tat.Add(-time.Duration(l.burst-1) * l.interval); base.After(now) {
		release = base
		if l.jitter != nil {
			release = release.Add(l.jitter(l.interval))
		}
		// A jitter is meant to draw from [0, interval), but the contract that no caller is sent
		// into its own past is the limiter's to keep whatever a caller's jitter returns.
		if release.Before(now) {
			release = now
		}
		// A paced release is burst-1 intervals before the arrival time it charges, so its own
		// jitter pushes that time out with it: without this the next base would be exactly one
		// interval after this base, whatever jitter this release drew, and at a burst of 2 or more
		// the gaps would collapse — measured here at rate 8 over 200 reservations, a burst of 2
		// held a minimum gap of 0.77s and sustained 7.97 wakes a minute where the operator's docs
		// promise a gap of one to two intervals and about two thirds of the rate. At a burst of 1
		// the arrival is the release and nothing moves (TestBurstLetsItsFirstWakesGoAtOnce,
		// TestJitterAtABurstOfTwoKeepsEveryGapBetweenOneIntervalAndTwo).
		arrival = release.Add(time.Duration(l.burst-1) * l.interval)
	}
	if l.tat.Before(arrival) {
		l.tat = arrival
	}
	l.tat = l.tat.Add(l.interval)
	return release
}
