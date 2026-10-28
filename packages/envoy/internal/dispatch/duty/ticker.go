// Package duty runs envoy-dispatch's periodic background loops and keeps each to one pass per
// period across the processes serving one database. RunJittered is the "jittered-start, run every
// interval until cancelled" loop every background GitHub-App importer uses (architecture.Run,
// delivery.Reconcile.Run): the jitter avoids a restart stampeding GitHub, and the idle check lets
// a caller with no App credentials configured skip the loop entirely rather than tick forever
// doing nothing. RunLeased is the loop of every other leased maintenance loop, and Exclusive is
// the lease and period gate a pass of either runs through.
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
// pass returns the wait before the next tick, which must be positive: interval for a loop of a
// fixed cadence, or a wait of its own for a loop that speeds up while work remains. A tick whose
// pass did not run - the lease held elsewhere, the period already served, the lease or the record
// unreadable - waits interval, since the process that holds the lease is doing this work. A pass
// that shutdown cut short still returns its wait, which goes unused: the loop returns.
//
// The period of a tick, the span Exclusive gives it, is the wait before it - interval for the
// first - and its due time is the moment its timer fired, a timer armed once the tick before it
// has returned. So two consecutive ticks are due at least the later one's period apart, plus the
// earlier tick's own time (its lease and its pass), and a lone loop's ticks always fall in
// different periods on this process's clock (periodBoundary): no tick of a lone loop is refused
// by its own previous pass. The cadence is therefore interval plus each pass's time, not a
// ticker's fixed interval: a slow pass delays the next tick by its own length rather than
// queueing a tick behind it, and no tick is dropped because one ran long.
//
// ctx ending returns at once from a wait, and from a tick once its pass returns.
func RunLeased(ctx context.Context, leases Leases, name string, interval time.Duration, first FirstTick, pass func(context.Context) time.Duration) {
	period := interval
	due := time.Now()
	if first == TickAfterInterval {
		select {
		case <-ctx.Done():
			return
		case due = <-time.After(interval):
		}
	}
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
