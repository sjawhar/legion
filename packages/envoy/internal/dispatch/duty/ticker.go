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
// expected to have logged why inside idle itself).
func RunJittered(ctx context.Context, interval time.Duration, idle func() bool, pass func(context.Context)) {
	if idle() {
		return
	}
	// #nosec G404 — jitter, not a secret.
	start := time.Duration(rand.Int64N(int64(interval)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(start):
	}
	for {
		pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
