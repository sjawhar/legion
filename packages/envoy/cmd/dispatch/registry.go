package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// deregisterTimeout bounds the delete that takes this process out of the registry of live server
// processes at shutdown. It is one delete by primary key, so five seconds is a slow round trip
// rather than a slow statement; a deregistration that does not land leaves a row whose heartbeat
// has stopped, which drops out of the live window (processes.LiveWindow) on its own.
const deregisterTimeout = 5 * time.Second

// joinRegistry enters this process into the registry of live server processes (INFRA-555) as
// instance, once it holds its listener (bound, or inherited as DISPATCH_LISTEN_FD) and before it
// serves its first request, after one probe that the database answers, and starts the loops that
// run once it is registered. The kernel queues the connections that arrive meanwhile on the
// listener, and none is served until the registration admits this process. The registration
// refuses a key set a live peer does not share (processes.Register): a cookie this process minted
// would not verify there, or the reverse, and the people it signed in would be signed out on the
// next request the load balancer sends to the other. A refusal stands only once the refusing
// process has kept heartbeating for a live window and a heartbeat after it (Register's wait), so a
// process restarted after a kill starts once the dead process's row lapses; a refusal that stands
// ends the process with 1 in main, before anything is served, so a roll stalls on the new pod
// while the old ones keep serving, and a signal during the wait ends it with 0, logged at INFO,
// since a process stopped before it served anything has not failed. The registration's heartbeat
// runs until shutdown deregisters it, after the HTTP drain (shutdown), so a draining process
// counts as live until its last request is answered. A heartbeat that lapsed (the process frozen,
// or cut from the database) or whose row was pruned checks the keys again before the process
// counts as live once more; beside a live process whose keys it does not share it refuses
// (Registration.Lost), and serving stops as at a signal and ends the process with 1 once drained
// (serveUntilStopped). The prune loop deletes what processes that died without deregistering left
// behind, once overall under its lease. The announcer starts only now, so its first pass counts
// this process among the live ones and never announces a build the fleet is rolling away from.
func joinRegistry(ctx context.Context, database *store.Store, instance processes.Instance, leases duty.Leases,
	announcer *startup.Announcer) (*processes.Registration, error) {
	if _, err := database.Pool.Healthy(ctx); err != nil {
		return nil, fmt.Errorf("probe the database: %w", err)
	}
	registration, err := processes.Register(ctx, database.Pool, instance)
	if err != nil {
		return nil, err
	}
	slog.Info("dispatch: registered this server process", "instance", registration.ID)
	go processes.RunPrune(ctx, database.Pool, leases)
	go announcer.Run(ctx)
	return registration, nil
}

// deregisterAfterDrain is the deregistration shutdown runs once HTTP has drained: registration's
// row deleted within deregisterTimeout, a failure logged.
func deregisterAfterDrain(registration *processes.Registration) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), deregisterTimeout)
		defer cancel()
		if err := registration.Deregister(ctx); err != nil {
			slog.Warn("dispatch: deregister this server process", "instance", registration.ID, "error", err)
			return
		}
		slog.Info("dispatch: deregistered this server process", "instance", registration.ID)
	}
}
