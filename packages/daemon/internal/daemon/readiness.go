package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// readinessRetry is the boot readiness gate's capped exponential backoff: one second, doubling,
// capped at one minute, retried forever (Attempts: 0, bootprobe.Retry's unbounded zero value) — a
// dependency dependencyUnavailable judges merely not up yet never makes the boot give up and exit
// into the crash a supervisor restarts straight back into the same dial (LEGION-580).
var readinessRetry = bootprobe.Retry{Initial: time.Second, Max: time.Minute}

// readinessLogInterval throttles awaitReady's own logging far below its retry rate: doubling from
// one second means six attempts inside the first minute alone, so logging every attempt the way
// bootprobe.Run does would flood the log long before the backoff ever reaches its cap. One line a
// minute is enough for an operator watching the log to see the daemon is still waiting, and why.
var readinessLogInterval = time.Minute

// awaitReady runs attempt until it passes, ctx ends, or judge decides a failure describes a
// misconfiguration — a refusal no wait fixes — rather than a dependency that is simply not up yet:
// the boot's readiness gate, which delays the boot with readinessRetry's capped backoff instead of
// exiting. name identifies the wait in its log lines and in the error a refusal or a ctx
// cancellation returns. ctx is the daemon's own, never the bounded boot budget: a wait this long
// needs a budget of its own, the way the App mint and the image probe already get one (daemon.go's
// cancelAfterMint, cancelAfterProbe).
func awaitReady(ctx context.Context, name string, log *slog.Logger, judge func(error) bool, attempt func(context.Context) error) error {
	start := time.Now()
	var lastLogged time.Time
	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		err := attempt(ctx)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%s: %w", name, ctxErr)
		}
		if !judge(err) {
			return fmt.Errorf("%s: %w", name, err)
		}
		if now := time.Now(); now.Sub(lastLogged) >= readinessLogInterval {
			log.Warn(name+" is not ready yet; the boot keeps retrying",
				"waitingFor", time.Since(start).Round(time.Second).String(), "error", err)
			lastLogged = now
		}
		wait := time.NewTimer(bootprobe.Delay(readinessRetry, i))
		select {
		case <-ctx.Done():
			wait.Stop()
			return fmt.Errorf("%s: %w", name, ctx.Err())
		case <-wait.C:
		}
	}
}

// dependencyUnavailable reports whether err describes a configured dependency that is simply not
// reachable yet — NATS not up yet (nats.ErrNoServers, a dial the network refused or timed out), or
// a Dispatch outage dispatch.PermanentRefusal does not call permanent — rather than a
// misconfiguration no wait fixes: a malformed nkey seed, a NATS permission the server itself
// refused, or a schema mismatch in what Dispatch sent back. Only the former is worth awaitReady's
// unbounded, capped-backoff wait.
//
// Postgres is deliberately not judged here: an unreachable Postgres stays the immediate boot
// refusal TestRunRefusesAnUnreachablePostgresByHostAndNotByPassword and
// scripts/e2e/stage1-skeleton.sh already prove and require, and the crash-loop evidence this gate
// answers (LEGION-580) never showed Postgres as a cause.
func dependencyUnavailable(err error) bool {
	if _, permanent := dispatch.PermanentRefusal(err); permanent {
		return false
	}
	var dispatchErr *dispatch.Error
	if errors.As(err, &dispatchErr) {
		// Every *Error PermanentRefusal did not call permanent above is exactly the outage it
		// rides out: every 5xx, a codeless 4xx, "come back later" (408, 429) and a credential
		// answer (401, 403) alike.
		return true
	}
	if errors.Is(err, nats.ErrNoServers) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
