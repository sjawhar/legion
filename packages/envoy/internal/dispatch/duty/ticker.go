// Package duty holds the shapes Dispatch's background loops run in, and the lease that keeps two
// processes from doing one loop's work twice. RunJittered is the timer loop, for work that has no
// change to wake on - the GitHub-App importers (architecture.Run, delivery.Reconcile.Run), since
// GitHub has no feed of what they would miss: a jittered start and a jittered wait after every pass,
// so a restart does not stampede GitHub and two processes' ticks do not fall into step, and an idle
// check so a caller with no App credentials configured skips the loop entirely rather than tick
// forever doing nothing. RunLeased is the loop of every other leased maintenance loop of a fixed
// cadence, and RunLeasedAfterEachPass the loop of those paced by their own passes. RunWoken is the
// change-driven loop, for work a Postgres notification can announce: one pass at start, then a pass
// only when woken or when the last pass asked for one, so an idle database costs it nothing.
// Exclusive is the lease and period gate a pass of any of them may run through.
package duty

import (
	"context"
	"math/rand/v2"
	"time"
)

// jitterN draws a duration in [0, n), and zero for an n of zero or less. Every jittered wait in this
// package draws from it, and it is a variable only so a test can fix the draw.
var jitterN = func(n time.Duration) time.Duration {
	if n <= 0 {
		return 0
	}
	// #nosec G404 — jitter, not a secret.
	return rand.N(n)
}

// Jittered is d plus up to a tenth of d again, drawn afresh each call, so the waits of processes
// that booted together drift apart. The jitter only ever adds, so a jittered wait is never shorter
// than the d it was asked for - which is what keeps a periodic pass from landing inside a period
// duty_passes already records as served (Exclusive): a wait of at least one period always reaches
// the next boundary.
func Jittered(d time.Duration) time.Duration {
	return d + jitterN(d/10)
}

// RunJittered calls pass every interval until ctx is cancelled, starting after a jittered delay in
// [0, interval) so a restart does not stampede whatever pass calls. Each wait after a pass is the
// interval plus up to a tenth of it again (jittered), drawn afresh each time, so two processes that
// booted with nearby phases drift apart rather than ticking in step for the life of both: a gap
// between passes is at least one interval and under 1.1 of one, plus the pass's own run time. idle
// is checked once, before the first tick: when it returns true the loop never starts (the caller is
// expected to have logged why inside idle itself).
//
// pass receives the moment its tick was due, the time its timer delivered, which is what a leased
// pass hands Exclusive. Each timer is armed once the pass before it has returned and waits at least
// one interval, so two due times are at least one interval apart and a lone loop's tick is never
// refused by the period its own previous pass recorded, however late either one's lease came.
func RunJittered(ctx context.Context, interval time.Duration, idle func() bool, pass func(ctx context.Context, due time.Time)) {
	if idle() {
		return
	}
	wait := jitterN(interval)
	for {
		timer := time.NewTimer(wait)
		var due time.Time
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case due = <-timer.C:
		}
		pass(ctx, due)
		wait = Jittered(interval)
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
// before it was due. pass returns that wait, which must be positive: the interval, or a wait of
// its own for a loop that speeds up while work remains, as the block-index reconciliation steps
// two seconds after a full batch. A tick whose pass did not run waits interval, and a pass that
// shutdown cut short still returns its wait, which goes unused.
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
