// Package duty holds the shapes Dispatch's background loops run in, and the lease that keeps two
// processes from doing one loop's work twice. RunJittered is the timer loop, for work that has no
// change to wake on - the GitHub-App importers (architecture.Run, delivery.Reconcile.Run), since
// GitHub has no feed of what they would miss: a jittered start and a jittered wait after every pass,
// so a restart does not stampede GitHub and two processes' ticks do not fall into step, and an idle
// check so a caller with no App credentials configured skips the loop entirely rather than tick
// forever doing nothing. RunWoken is the change-driven loop, for work a Postgres notification can
// announce: one pass at start, then a pass only when woken or when the last pass asked for one, so
// an idle database costs it nothing. Exclusive is the lease either may run its pass under.
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

// jittered is d plus up to a tenth of d again. The jitter only ever adds, so a jittered wait is
// never shorter than the d it was asked for - which is what keeps a periodic pass from landing
// inside a period duty_passes already records as served (Exclusive): a wait of at least one period
// always reaches the next boundary.
func jittered(d time.Duration) time.Duration {
	return d + jitterN(d/10)
}

// RunJittered calls pass every interval until ctx is cancelled, starting after a jittered delay in
// [0, interval) so a restart does not stampede whatever pass calls. Each wait after a pass is the
// interval plus up to a tenth of it again (jittered), drawn afresh each time, so two processes that
// booted with nearby phases drift apart rather than ticking in step for the life of both: a gap
// between passes is at least one interval and under 1.1 of one, plus the pass's own run time. idle
// is checked once, before the first tick: when it returns true the loop never starts (the caller is
// expected to have logged why inside idle itself).
func RunJittered(ctx context.Context, interval time.Duration, idle func() bool, pass func(context.Context)) {
	if idle() {
		return
	}
	wait := jitterN(interval)
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		pass(ctx)
		wait = jittered(interval)
	}
}
