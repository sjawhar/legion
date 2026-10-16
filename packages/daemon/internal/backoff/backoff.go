// Package backoff is the wait before trying again something that failed for a reason a later try
// may not meet: it doubles with each failure, stops growing at 32 times its first step, and is
// jittered, so that many callers that failed together do not all try again together.
package backoff

import (
	"context"
	"math/rand/v2"
	"time"
)

// doublings is how often the step doubles before it stops growing: the sixth wait and every one
// after it are drawn from a step 32 times the first.
const doublings = 5

// Wait is the wait before the try that follows failures failed ones (failures ≥ 1): a step of base
// for the first failure, doubled for each one after it up to 32 times base, and the wait drawn
// uniformly from between half that step and the whole of it. No wait is shorter than half its
// step.
func Wait(base time.Duration, failures int) time.Duration {
	step := base << min(max(failures-1, 0), doublings)
	half := step / 2
	return half + rand.N(step-half+1)
}

// Sleep waits d, or until ctx ends, whichever comes first, and returns ctx's error when it ended
// first.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
