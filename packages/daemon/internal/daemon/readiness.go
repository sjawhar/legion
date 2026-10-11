package daemon

import (
	"context"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
)

// readinessRetry is the boot's wait for an unreachable NATS or Dispatch: one second, doubling,
// capped at one minute, retried forever (bootprobe.Retry's unbounded zero Attempts) — a
// dependency that is merely not up yet never makes the boot give up and exit into the crash a
// supervisor restarts straight back into the same dial (LEGION-580).
var readinessRetry = bootprobe.Retry{Initial: time.Second, Max: time.Minute}

// readinessAttempt adapts attempt to bootprobe.Run's Outcome: passed, retried (with err's own
// text as the Detail bootprobe.Run logs) when unreachable recognizes the failure, refused at once
// otherwise. Each attempt runs on its own bootTimeout deadline rather than the daemon's unbounded
// ctx — the NATS dial (nats.go's own 2 s Timeout) and every Dispatch call
// (dispatch.requestTimeout, 10 s) already bound themselves, but a context deadline also turns off
// jetstream's own 5 s default, so a JetStream call that would otherwise have timed out at 5 s now
// waits up to bootTimeout (30 s) before this package calls it refused — and a Postgres
// transaction that accepts a connection and then stops answering, inside workflow.reconcile,
// would otherwise hang the boot forever with nothing logged, since bootprobe.Run logs only once
// an attempt returns.
func readinessAttempt(attempt func(context.Context) error, unreachable func(error) bool) func(context.Context) bootprobe.Outcome {
	return func(ctx context.Context) bootprobe.Outcome {
		bounded, cancel := context.WithTimeout(ctx, bootTimeout)
		defer cancel()
		err := attempt(bounded)
		if err == nil {
			return bootprobe.Outcome{Passed: true}
		}
		if unreachable(err) {
			return bootprobe.Outcome{Detail: err.Error()}
		}
		return bootprobe.Outcome{Refusal: err}
	}
}
