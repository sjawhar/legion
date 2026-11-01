package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// stopGrace is how long Dispatch's runtime lets it run after SIGTERM before killing it: ECS's
// default stop timeout, which Dispatch's task definition leaves unset, the compose file's
// stop_grace_period, and the pod the gcp:Dispatch component renders, whose
// terminationGracePeriodSeconds is this value plus its pre-stop wait, since the kubelet spends
// that wait inside the same period (pod_grace_period_test.go fails when the pod's budget stops
// covering it).
const stopGrace = 30 * time.Second

// documentShutdownTimeout is the document service's own budget: docs.Service.Shutdown spends up to
// its drain budget waiting for durable appends and running settlements, sized so a burst of writes
// still landing when shutdown begins has room to finish, not only the ones already queued - and
// keeps a 5 s reserve past it. That reserve is not just time to read back which settlements
// committed: docs.Service.Shutdown's own wait for its per-room workers (service.go's waitGroup)
// runs under this whole timeout, not the drain budget alone, because a settlement the budget cut
// short may still be mid-render or mid-commit, which no context interrupts once the database has
// taken the query. service_test.go's own measured example - a 1 MiB document's settlement took
// 4.5 s at load 90 against a 5 s budget, a 3.4 s overrun past it - is what the reserve is sized to
// outlast; a worker still running once this whole timeout ends makes Shutdown return the
// settlements-unconfirmed error instead of reading back what committed.
const documentShutdownTimeout = docs.ShutdownDrainBudget + 5*time.Second

// httpDrainBeforeDocuments is how long the document service waits for the requests in flight to
// finish before it settles the documents owed with requests still running, so that its budget ends
// 5 s inside stopGrace. An open live socket holds none of it: HTTP shutdown never waits for a
// hijacked connection, and every live socket closes with the process context (the lifetime
// api.Register takes), which is cancelled before shutdown starts.
const httpDrainBeforeDocuments = stopGrace - 5*time.Second - documentShutdownTimeout

// silentProbes is how many health probes (store.Pool.Healthy, two seconds each) in a row must fail,
// with no connection of the shared or document-rooms pool taken back meanwhile, before Dispatch
// counts the database as having stopped answering. A probe that answers, or a connection taken
// back while it ran, starts the count again: a slow database that is still finishing work is
// answering. Three is the count the load balancer waits for before it replaces the task.
const silentProbes = 3

// drainPollInterval is how often the last phase asks whether the requests in flight and the
// connections in use are done, and how often waitUntilIdle's loop pauses between the health
// probes it runs every pass.
const drainPollInterval = 250 * time.Millisecond

// serveUntilStopped serves on listener until ctx ends, then stops Dispatch (shutdown), deregistering
// it once HTTP has drained. It logs the address listener is bound to, whether main bound it or
// inherited it (DISPATCH_LISTEN_FD), so the line names the port a kernel chose. Two things end
// serving before the signal does, each logged at ERROR and returned once shutdown has run, so main
// exits 1 and a supervisor never reads either as a clean stop: serving failed, or the registry
// refused to count this process as live again beside a live process whose signing keys it does not
// share (lost, processes.Registration.Lost). Either cancels ctx, so the stop is the one a signal
// makes: the listener closes at once, the requests in flight are answered, the intakes stop, and
// the registry's row is deleted.
func serveUntilStopped(ctx context.Context, cancel context.CancelFunc, server *http.Server, listener net.Listener,
	documentService *docs.Service, pool *store.Pool, waitBackground, waitIntakes, deregister func(), lost <-chan error) error {
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("dispatch: listening", "addr", listener.Addr().String())
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("dispatch: serve", "error", err)
			serveErr <- err
			cancel()
		}
	}()
	var refused error
	select {
	case <-ctx.Done():
	case refused = <-lost:
		slog.Error("dispatch: this server process no longer shares its signing keys with a live server process", "error", refused)
		cancel()
	}
	slog.Info("dispatch: shutting down")
	shutdown(server, documentService, pool, waitBackground, waitIntakes, deregister)
	select {
	case err := <-serveErr:
		return err
	default:
		return refused
	}
}

// shutdown stops Dispatch once its process context is cancelled. HTTP drains the requests in flight
// first, with no deadline of its own: a message send among them finishes, bounded by the listener
// client's receipt timeout. The document service settles the documents owed within
// documentShutdownTimeout once those requests are done, or httpDrainBeforeDocuments after the
// signal with some still running. waitBackground then waits for the delivery hub, every live
// socket and the subscriptions they ran to finish their Lifetime-driven shutdown, and waitIntakes
// for the GitHub intakes to finish their stop (delivery.StartIntakes' stop), which began at the
// signal, beside the HTTP drain and the document shutdown: a message in its handler finishes and is
// acknowledged, or is given up and handed back to NATS, delivery's intakeStopBudget (15 s) after
// the signal at the latest, the one deadline of that stop, which counts from the signal and which a
// take-over under way at the signal spends rather than extends. Each intake then releases its hold
// through the pool, so another process takes its durable over at once. That wait ends 10 s inside
// the latest the document service's budget ends (stopGrace less its 5 s reserve, 25 s after the
// signal), which leaves the given-up handlers' prompt return, their negative acknowledgements, the
// holds' release and the intakes' clients closing room inside it, so it never makes the shutdown
// longer than the document budget already can, nor pushes deregister and the pool's close past
// stopGrace. It comes before the pool closes because nothing else keeps the pool open for a
// handler: one waiting on GitHub holds no connection, so waitUntilIdle finds the pool idle, and
// its writes would fail on the closed pool once GitHub answered, with its message acknowledged all
// the same, and because a hold released through a closed pool is a hold left in place for its TTL.
// The database pool closes once every request has answered and no connection of the shared or
// document-rooms pool is in use. While the database answers, nothing but the runtime's kill bounds
// that wait, so a write in flight commits and is answered if it finishes before the kill. Then
// deregister takes this process out of the registry of live server processes: it runs only once
// HTTP has drained, so a draining process counts as live until its last request is answered, and
// before the pool closes. A database that has stopped answering (silentProbes) holds its
// connections forever, so Dispatch exits without them, and the database rolls back whatever they
// held open; it deregisters nothing then, since the delete would wait on that database too, and
// its row's heartbeat stops with the process instead.
func shutdown(server *http.Server, documentService *docs.Service, pool *store.Pool, waitBackground, waitIntakes, deregister func()) {
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		if err := server.Shutdown(context.Background()); err != nil {
			slog.Warn("dispatch: shutdown", "error", err)
		}
	}()
	select {
	case <-drained:
	case <-time.After(httpDrainBeforeDocuments):
		slog.Warn("dispatch: settle documents with requests still in flight")
	}
	documentCtx, cancelDocuments := context.WithTimeout(context.Background(), documentShutdownTimeout)
	defer cancelDocuments()
	if err := documentService.Shutdown(documentCtx); err != nil {
		slog.Warn("dispatch: shutdown document service", "error", err)
	}
	waitBackground()
	waitIntakes()
	if waitUntilIdle(drained, pool) {
		deregister()
		pool.Close()
	}
}

// waitUntilIdle waits until HTTP has drained and no connection of the shared or document-rooms pool
// is in use, and reports whether that happened. It gives up only once the database has stopped
// answering, when the connections in use never come back: closing the pool would wait on them.
func waitUntilIdle(drained <-chan struct{}, pool *store.Pool) bool {
	failed := 0
	for {
		before := pool.Use()
		select {
		case <-drained:
			if before.InUse == 0 {
				return true
			}
		default:
		}
		_, err := pool.Healthy(context.Background())
		after := pool.Use()
		if err == nil || after.Returned > before.Returned {
			failed = 0
		} else if failed++; failed >= silentProbes {
			slog.Warn("dispatch: exit with database connections still in use once the database stopped answering",
				"in_use", after.InUse, "failed_probes", failed, "error", err)
			return false
		}
		time.Sleep(drainPollInterval)
	}
}
