package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// MaintenanceSweepInterval is how often one process deletes the Idempotency-Keys past their
// window and sends the deliveries a process that died left owed. Nothing here is in a request's
// latency budget: a key is read by a repeat long before it is swept, and an owed delivery is one
// whose sender already died, so a minute of delay costs nothing a caller can see, and a
// minutes-scale interval keeps both statements off any tight loop in production.
const MaintenanceSweepInterval = time.Minute

// maintenanceLease names the lease one pass of this sweep runs under. The name is the one the
// suggestion outcome sweep carried this work under before it moved here, and it stays so: during
// a rolling deploy the earlier build's pass, which swept the keys and the owed deliveries inside
// its own suggestion pass, and this build's timer still exclude each other on that work, which a
// new name would not.
const maintenanceLease = "suggestion-outcomes"

// RunMaintenanceSweep deletes every Idempotency-Key past its window (sweepRequestKeys) and then
// sends every delivery a process that died left owed (resumeDeliveries, the server's
// resumeOwedDeliveries). resumeDeliveries is nil where nothing delivers. It stops when
// ctx is done.
//
// It is a timer and not a woken loop (duty.RunWoken), the shape every other sweep of this server
// took, because no commit announces either piece of its work: a key passes its window by the
// clock alone, and a delivery is owed because the process that claimed it died, which commits
// nothing for a notification to carry. The two share one loop rather than taking one each: a loop
// of its own would need a lease connection of its own (store.leasePoolSize, whose headroom is
// zero) for two statements that cost milliseconds.
//
// Each pass runs under maintenanceLease for one interval, so one process sweeps per interval
// however many serve the database and a second process's tick, which falls between the first's
// rather than on it, finds that interval served; leases is required, as it is of every leased
// loop (cmd/dispatch exits at boot if the lease pool cannot be opened). The period is interval
// itself and not MaintenanceSweepInterval: this loop ticks on what its caller asked for. The
// ticks are duty.RunLeased's, a ticker's: the first one interval after the call, then one each
// interval; the first tick a pass overran comes at once with the moment it was due, and only the
// ones after it are dropped. The two sweeps fail apart: one's failure is logged and the other
// still runs.
func RunMaintenanceSweep(
	ctx context.Context, pool *store.Pool, interval time.Duration, leases duty.Leases,
	resumeDeliveries func(context.Context) error,
) {
	duty.RunLeased(ctx, leases, maintenanceLease, interval, duty.TickAfterInterval, func(ctx context.Context) {
		if err := sweepRequestKeys(ctx, pool); err != nil {
			slog.Warn("dispatch: idempotency key sweep failed", "error", err)
		}
		if resumeDeliveries == nil {
			return
		}
		if err := resumeDeliveries(ctx); err != nil {
			slog.Warn("dispatch: delivery sweep failed", "error", err)
		}
	})
}
