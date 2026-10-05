package daemon

import (
	"context"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
)

// readinessRetry is the boot's wait for an unreachable NATS or Dispatch: one second, doubling,
// capped at one minute, retried forever (bootprobe.Retry's unbounded zero Attempts) — a
// dependency that is merely not up yet never makes the boot give up and exit into the crash a
// supervisor restarts straight back into the same dial (LEGION-580).
var readinessRetry = bootprobe.Retry{Initial: time.Second, Max: time.Minute}

// natsConnectOutcome maps one attempt at workflow.connect to the bootprobe.Outcome run()'s
// bootprobe.Run drives the gate with: passed, a detail to log and retry (natsauth.Unreachable), or
// a refusal to return at once — a malformed seed, a NATS permission or authorization violation, or
// anything else natsauth.Unreachable does not recognize as merely not up yet.
func natsConnectOutcome(err error) bootprobe.Outcome {
	switch {
	case err == nil:
		return bootprobe.Outcome{Passed: true}
	case natsauth.Unreachable(err):
		return bootprobe.Outcome{Detail: err.Error()}
	default:
		return bootprobe.Outcome{Refusal: err}
	}
}

// dispatchReconcileOutcome is workflow.reconcile's bootprobe.Outcome mapping. reconcile touches
// three dependencies in one call — Dispatch's issue listing, the notification stream's own
// JetStream reads, and the Postgres transaction admission.Reconcile commits in — so a failure is
// judged unreachable by either dispatch.Unreachable or natsauth.Unreachable, the second covering a
// raw network failure under any of the three (Postgres included: pgconn wraps a dropped
// connection in the same *net.OpError a NATS dial would). None of the three is a write this call
// must protect from being dropped, so all of them failing the same way is simply the dependency
// not being up yet.
func dispatchReconcileOutcome(err error) bootprobe.Outcome {
	switch {
	case err == nil:
		return bootprobe.Outcome{Passed: true}
	case dispatch.Unreachable(err), natsauth.Unreachable(err):
		return bootprobe.Outcome{Detail: err.Error()}
	default:
		return bootprobe.Outcome{Refusal: err}
	}
}

// boundedAttempt gives one bootprobe.Run attempt its own deadline rather than the daemon's
// unbounded ctx: the NATS dial (nats.go's own 2 s Timeout), every JetStream call (jetstream's 5 s
// default when ctx carries none) and every Dispatch call (dispatch.requestTimeout, 10 s) already
// bound themselves, but the Postgres transaction admission.Reconcile commits inside
// workflow.reconcile does not — a Postgres that accepts a connection and then stops answering
// would otherwise hang the boot forever, logging nothing, since bootprobe.Run logs only once an
// attempt returns. Callers pass bootTimeout, the budget every other boot step already gets.
func boundedAttempt(ctx context.Context, timeout time.Duration, attempt func(context.Context) error) error {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return attempt(bounded)
}
