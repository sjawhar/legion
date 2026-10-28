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
// served the tick's period. first says whether the first tick is at start or one interval after.
//
// pass returns the wait before the next tick, which must be positive: interval, for the loops of a
// fixed cadence this serves. A tick whose pass did not run - the lease held elsewhere, the period
// already served, the lease or the record unreadable - waits interval, since the process that
// holds the lease is doing this work. A pass that shutdown cut short still returns its wait, which
// goes unused: the loop returns.
//
// The ticks keep a fixed schedule, as a ticker's do: each is due its wait after the moment the
// tick before it was due, not after that tick returned, and that scheduled moment is the due time
// it hands Exclusive; its period is that wait, interval for the first. A lone loop of a fixed
// cadence therefore has due times exactly one interval apart, which fall in consecutive periods on
// this process's clock (periodBoundary), so no period is lost however long each pass takes. A tick
// armed only once the pass before it returned would be due one interval plus that pass's time
// after it, and as those delays add up its due times would drift across the period boundaries and
// jump over a period no pass serves. A pass that overruns its wait gets what a ticker gives a slow
// receiver: the next tick comes at once, due at the latest scheduled moment not after now
// (nextDue), and the moments it overran are dropped rather than run late one after another, so
// due times stay whole waits apart and lag never accumulates.
//
// ctx ending returns at once from a wait, and from a tick once its pass returns.
func RunLeased(ctx context.Context, leases Leases, name string, interval time.Duration, first FirstTick, pass func(context.Context) time.Duration) {
	period := interval
	due := time.Now()
	if first == TickAfterInterval {
		due = due.Add(interval)
		if !sleepUntil(ctx, due) {
			return
		}
	}
	for {
		wait := interval
		Exclusive(ctx, leases, name, due, period, func(ctx context.Context) {
			wait = pass(ctx)
		})
		period = wait
		due = nextDue(due, wait, time.Now())
		if !sleepUntil(ctx, due) {
			return
		}
	}
}

// nextDue is the moment RunLeased's next tick is due at now, after a tick due at due whose wait is
// wait: due plus wait while that is not yet past, and otherwise the latest due plus a whole number
// of waits not after now - the tick a ticker delivers to a receiver that overran, the moments
// before it dropped.
func nextDue(due time.Time, wait time.Duration, now time.Time) time.Time {
	next := due.Add(wait)
	if late := now.Sub(next); late > 0 {
		next = next.Add(late - late%wait)
	}
	return next
}

// sleepUntil waits until t, returning at once when ctx ends first, and reports whether ctx is
// still live: a t already past returns at once too.
func sleepUntil(ctx context.Context, t time.Time) bool {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

// RunLeasedAfterEachPass runs pass under the lease named name as RunLeased does, one Exclusive
// call per tick until ctx ends, for a loop paced by its own passes: its first tick is at start,
// and each next tick comes the wait its pass returned after that pass returned, not after the tick
// before it was due. pass returns that wait, which must be positive: the interval, or a wait of
// its own for a loop that speeds up while work remains, as the reference-excerpt and block-index
// reconciliations step two seconds after a full batch. A tick whose pass did not run waits
// interval, and a pass that shutdown cut short still returns its wait, which goes unused.
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
func RunLeasedAfterEachPass(ctx context.Context, leases Leases, name string, interval time.Duration, pass func(context.Context) time.Duration) {
	period := interval
	due := time.Now()
	for {
		wait := interval
		Exclusive(ctx, leases, name, due, period, func(ctx context.Context) {
			wait = pass(ctx)
		})
		period = wait
		select {
		case <-ctx.Done():
			return
		case due = <-time.After(wait):
		}
	}
}
