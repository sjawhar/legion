// Package duty runs envoy-dispatch's periodic background loops and keeps each to one pass per
// period across the processes serving one database. RunJittered is the "jittered-start, run every
// interval until cancelled" loop every background GitHub-App importer uses (architecture.Run,
// delivery.Reconcile.Run): the jitter avoids a restart stampeding GitHub, and the idle check lets
// a caller with no App credentials configured skip the loop entirely rather than tick forever
// doing nothing. RunLeased is the loop of every other leased maintenance loop of a fixed cadence,
// RunLeasedAfterEachPass the loop of those paced by their own passes, and Exclusive is the lease
// and period gate a pass of any of them runs through.
package duty

import (
	"context"
	"math/rand/v2"
	"time"
)

// RunJittered calls pass on every tick of interval until ctx is cancelled, starting after a
// jittered delay in [0, interval) so a restart does not stampede whatever pass calls. idle is
// checked once, before the first tick: when it returns true the loop never starts (the caller is
// expected to have logged why inside idle itself). pass receives the moment its tick was due, the
// time its timer delivered, which is what a leased pass hands Exclusive.
func RunJittered(ctx context.Context, interval time.Duration, idle func() bool, pass func(ctx context.Context, due time.Time)) {
	if idle() {
		return
	}
	// #nosec G404 — jitter, not a secret.
	start := time.Duration(rand.Int64N(int64(interval)))
	var due time.Time
	select {
	case <-ctx.Done():
		return
	case due = <-time.After(start):
	}
	for {
		pass(ctx, due)
		select {
		case <-ctx.Done():
			return
		case due = <-time.After(interval):
		}
	}
}

// FirstTick is when RunLeased makes a loop's first tick.
type FirstTick int

const (
	// TickAtStart makes the first tick the moment the loop starts.
	TickAtStart FirstTick = iota
	// TickAfterInterval makes the first tick one interval after the loop starts.
	TickAfterInterval
)

// RunLeased runs pass under the lease named name, once per tick, until ctx ends: each tick is one
// Exclusive call, so pass runs only when this process takes the lease and no process has already
// served the tick's period, which is always interval. first says whether the first tick is at
// start or one interval after.
//
// The ticks are a ticker's, time.NewTicker(interval): TickAfterInterval starts at the ticker's
// first tick, and TickAtStart makes one tick at the moment the loop starts before it. A tick's due
// time, the moment it hands Exclusive, is the value the ticker delivered, the clock read at that
// tick and not a moment worked out from a reading taken at start, so a step of the wall clock
// while the loop runs moves the due times after it with it. The ticker's ticks are one interval
// apart while each pass takes less than the interval, so a lone loop's due times fall in
// consecutive periods on this process's clock (periodBoundary) and no period is lost. A tick armed
// only once the pass before it returned would be due one interval plus that pass's time after it,
// and as those delays add up its due times would drift across the period boundaries and jump over
// a period no pass serves. A pass that overruns the interval gets what a ticker gives a slow
// receiver (Go's runtime, time.go): the first tick it overran comes at once, with the moment it
// was due, and only the ticks after it are dropped, so due times stay whole intervals apart and
// overrun ticks are not run late one after another.
//
// ctx ending returns at once from a wait, and from a tick once its pass returns.
func RunLeased(ctx context.Context, leases Leases, name string, interval time.Duration, first FirstTick, pass func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if first == TickAtStart {
		// Read after the ticker started, so the ticker's first tick is no more than one interval
		// after this one: in the next period, or in this one and refused, but never one beyond.
		Exclusive(ctx, leases, name, time.Now(), interval, pass)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case due := <-ticker.C:
			Exclusive(ctx, leases, name, due, interval, pass)
		}
	}
}

// RunLeasedAfterEachPass runs pass under the lease named name as RunLeased does, one Exclusive
// call per tick until ctx ends, for a loop paced by its own passes: its first tick is at start,
// and each next tick comes the wait its pass returned after that pass returned, not after the tick
// before it was due. pass is given the period of its tick, the span Exclusive gave it, and returns
// that wait, which must be positive: the interval, or a wait of its own for a loop that speeds up
// while work remains, as the reference-excerpt and block-index reconciliations step two seconds
// after a full batch. A tick whose pass did not run waits interval, and a pass that shutdown cut
// short still returns its wait, which goes unused.
//
// The period of a tick, the span Exclusive gives it, is the wait before it - interval for the
// first - and its due time is the moment its timer fired, a timer armed once the tick before it
// has returned. So two consecutive ticks are due at least the later one's period apart, plus the
// earlier tick's own time (its lease and its pass), and a lone loop's ticks always fall in
// different periods on this process's clock (periodBoundary): no tick of a lone loop is refused
// by its own previous pass. The cadence is therefore each wait plus each pass's time: a slow pass
// delays the next tick by its own length, and a step is never cut short by the pass before it.
//
// ctx ending returns at once from a wait, and from a tick once its pass returns.
func RunLeasedAfterEachPass(ctx context.Context, leases Leases, name string, interval time.Duration, pass func(context.Context, time.Duration) time.Duration) {
	period := interval
	due := time.Now()
	for {
		wait := interval
		Exclusive(ctx, leases, name, due, period, func(ctx context.Context) {
			wait = pass(ctx, period)
		})
		period = wait
		select {
		case <-ctx.Done():
			return
		case due = <-time.After(wait):
		}
	}
}
