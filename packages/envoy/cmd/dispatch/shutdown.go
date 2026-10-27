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
// 5 s inside stopGrace. An open event stream holds none of it, since every stream ends with the
// process context (api.Deps.Lifetime), which is cancelled before shutdown starts.
const httpDrainBeforeDocuments = stopGrace - 5*time.Second - documentShutdownTimeout

// silentProbes is how many health probes (store.Pool.Healthy, two seconds each) in a row must fail,
// with no connection of the pools that carry writes (writePools) taken back meanwhile, before
// Dispatch counts the database as having stopped answering. A probe that answers, or a connection
// taken back while it ran, starts the count again: a slow database that is still finishing work is
// answering. Three is the count the load balancer waits for before it replaces the task.
const silentProbes = 3

// writePools are the pools whose connections carry work a client or a document is owed: the
// request pool, the document rooms' and the settlements'. The drain waits for every one of their
// connections to come back before it closes the pools, so a write in flight commits and is
// answered. The other pools carry no write anyone is waiting on - the background loops and the
// leased passes end with the process context and run again at their next tick, the live readers
// only read, the listener's connections are hijacked, and the outbox's work stays in the outbox
// for the next process - so none of them holds the drain up.
var writePools = []string{store.RequestsPool, store.RoomsPool, store.SettlementsPool}

// drainPollInterval is how often the last phase asks whether the requests in flight and the
// connections in use are done, and how often waitUntilIdle's loop pauses between the health
// probes it runs every pass.
const drainPollInterval = 250 * time.Millisecond

// serveUntilStopped serves on listener until ctx ends, then stops Dispatch (shutdown). It returns
// the error that ended serving when serving failed, which cancels ctx.
func serveUntilStopped(ctx context.Context, cancel context.CancelFunc, server *http.Server, listener net.Listener,
	addr string, documentService *docs.Service, pool *store.Pool, waitDelivery func()) error {
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("dispatch: listening", "addr", addr)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			cancel()
		}
	}()
	<-ctx.Done()
	slog.Info("dispatch: shutting down")
	shutdown(server, documentService, pool, waitDelivery)
	select {
	case err := <-serveErr:
		return err
	default:
		return nil
	}
}

// shutdown stops Dispatch once its process context is cancelled. HTTP drains the requests in flight
// first, with no deadline of its own. The document service settles the documents owed within
// documentShutdownTimeout once those requests are done, or httpDrainBeforeDocuments after the
// signal with some still running. Hijacked delivery sockets and the delivery hub then finish their
// Lifetime-driven shutdown. The database pools close once every request has answered and no
// connection of writePools is in use. While the database answers, that wait is bounded by each
// request's own deadline (20 s for an ordinary route) and then the runtime's kill, so a write in
// flight commits and is answered if it finishes within both. A
// database that has stopped answering (silentProbes) holds its connections forever, so Dispatch
// exits without them, and the database rolls back whatever they held open.
func shutdown(server *http.Server, documentService *docs.Service, pool *store.Pool, waitDelivery func()) {
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
	waitDelivery()
	if waitUntilIdle(drained, pool) {
		pool.Close()
	}
}

// waitUntilIdle waits until HTTP has drained and no connection of writePools is in use, and
// reports whether that happened. It gives up only once the database has stopped answering, when
// the connections in use never come back: closing the pools would wait on them.
func waitUntilIdle(drained <-chan struct{}, pool *store.Pool) bool {
	failed := 0
	for {
		before := pool.Use(writePools...)
		select {
		case <-drained:
			if before.InUse == 0 {
				return true
			}
		default:
		}
		_, err := pool.Healthy(context.Background())
		after := pool.Use(writePools...)
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
