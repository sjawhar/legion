// Package duty is the shared "jittered-start, run every interval until cancelled" loop every
// background GitHub-App importer uses (architecture.Run, delivery.Reconcile.Run): the jitter
// avoids a restart stampeding GitHub, and the idle check lets a caller with no App credentials
// configured skip the loop entirely rather than tick forever doing nothing.
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
