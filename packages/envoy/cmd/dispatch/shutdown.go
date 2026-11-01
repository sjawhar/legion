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
// covering it). The shutdown's one deadline (shutdownContext) ends shutdownMargin inside it.
const stopGrace = 30 * time.Second

// shutdownMargin is what the shutdown's deadline leaves of stopGrace for the steps that run once
// the ordered shutdown is over: closing the pools once nothing is in use (milliseconds), the stops
// main defers, writing the log lines still buffered (logs.Flush, bounded by logFlushTimeout, 2 s)
// and the process's exit. A process that gives up at the deadline still says why and exits on its
// own, rather than being killed mid-line.
const shutdownMargin = 3 * time.Second

// documentReserve is the least of the shutdown's deadline the document service gets: the HTTP drain
// stops holding it up this long, and silentProbeReserve, before the deadline. docs.Service.Shutdown
// spends up to its drain budget (docs.ShutdownDrainBudget) waiting for durable appends and running
// settlements, sized so a burst of writes still landing when shutdown begins has room to finish, not
// only the ones already queued - and this keeps a 5 s reserve past it. That reserve is not just time
// to read back which settlements committed: docs.Service.Shutdown's own wait for its per-room
// workers (service.go's waitGroup) runs until the deadline it is given, not the drain budget alone,
// because a settlement the budget cut short may still be mid-render or mid-commit, which no context
// interrupts once the database has taken the query. service_test.go's own measured example - a
// 1 MiB document's settlement took 4.5 s at load 90 against a 5 s budget, a 3.4 s overrun past it -
// is what the reserve is sized to outlast; a worker still running at that deadline makes Shutdown
// return the settlements-unconfirmed error instead of reading back what committed.
//
// An open live socket holds none of the HTTP drain: HTTP shutdown never waits for a hijacked
// connection, and every live socket closes with the process context (the lifetime api.Register
// takes), which is cancelled before shutdown starts.
const documentReserve = docs.ShutdownDrainBudget + 5*time.Second

// silentProbes is how many health probes (store.Pool.Healthy, two seconds each) in a row must fail,
// with no connection of the pools that carry writes (writePools) handed back finished meanwhile
// (store.ConnectionUse.Finished), before Dispatch counts the database as having stopped answering.
// A probe that answers, or a connection handed back finished while it ran, starts the count again:
// a slow database that is still finishing work is answering. A connection discarded because it
// went unanswered finishes nothing, so it does not. Three is the count the load balancer waits for
// before it replaces the task.
const silentProbes = 3

// silentProbeReserve is the end of the shutdown's deadline kept for the wait for writePools to come
// back: the document service's share ends this long before the deadline, so a database that has
// stopped answering still fails silentProbes probes in a row - each store.Pool.Healthy's two
// seconds, with a poll interval between them, 6.5 s - and Dispatch says so before the deadline. Two
// poll intervals more absorb the document service's and delivery's own returns and a loaded
// machine's late timers: 7 s. With the 27 s deadline and documentReserve's 14 s, the HTTP drain
// holds the document service up for at most the first 6 s after the signal.
const silentProbeReserve = silentProbes*2*time.Second + (silentProbes+1)*drainPollInterval

// writePools are the pools whose connections carry work a client or a document is owed: the
// request pool, the document rooms' and the settlements'. The drain waits for every one of their
// connections to come back before it closes the pools, so a write in flight commits and is
// answered. The other pools carry no write anyone is waiting on - the background loops and the
// leased passes end with the process context and run again at their next tick, the live readers
// only read, the listener's connections are hijacked, and the outbox's work stays in the outbox
// for the next process - so none of them holds the drain up.
var writePools = []string{store.RequestsPool, store.RoomsPool, store.SettlementsPool}

// drainPollInterval is how often waitUntilIdle's loop pauses between the health probes it runs
// every pass, asking each time whether the requests in flight and the connections in use are done.
const drainPollInterval = 250 * time.Millisecond

// shutdownContext carries the shutdown's one deadline, stopGrace less shutdownMargin from now: main
// makes it as the signal ends the process context, and every stage of shutdown, and every stop
// main runs after it, spends what is left of it rather than a budget of its own.
func shutdownContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), stopGrace-shutdownMargin)
}

// serveUntilStopped serves on listener until ctx ends, and returns then. Stopping is the caller's
// (shutdown). It logs the address listener is bound to, whether main bound it or inherited it
// (DISPATCH_LISTEN_FD), so the line names the port a kernel chose. Two things end serving before
// the signal does, and it returns either one, so main exits 1 once shutdown has run and a
// supervisor never reads either as a clean stop: serving failed, or the registry refused to count
// this process as live again beside a live process whose signing keys it does not share (lost,
// processes.Registration.Lost), which it logs at ERROR. Either cancels ctx, so the stop is the one
// a signal makes: the listener closes at once, the requests in flight are answered, the intakes
// stop, and the registry's row is deleted.
func serveUntilStopped(ctx context.Context, cancel context.CancelFunc, server *http.Server, listener net.Listener, lost <-chan error) error {
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("dispatch: listening", "addr", listener.Addr().String())
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	select {
	case err := <-serveErr:
		return err
	default:
		return refused
	}
}

// shutdown stops Dispatch once its process context is cancelled, every stage inside ctx's deadline
// (shutdownContext), which ctx must carry:
//
//  1. HTTP drains the requests in flight until every one has answered, or until the document
//     service has to start for its reserve and the probes' to fit before the deadline (documentReserve,
//     silentProbeReserve), when it logs that it settles documents with requests still in flight. A
//     message send among those requests finishes, bounded by the listener client's receipt timeout.
//  2. The document service settles the documents owed until silentProbeReserve before the deadline.
//  3. waitBackground waits for every keyed handler and detached delivery, the delivery hub, every
//     live socket and the subscriptions they ran to finish their lifetime-driven shutdown.
//  4. waitIntakes waits for the GitHub intakes to finish their stop (delivery.StartIntakes' stop),
//     which began at the signal, beside the HTTP drain and the document shutdown: a message in its
//     handler finishes and is acknowledged, or is given up and handed back to NATS, delivery's
//     intakeStopBudget (15 s) after the signal at the latest, the one deadline of that stop, which
//     counts from the signal and which a bind under way at the signal spends rather than extends.
//     That budget ends well inside this shutdown's deadline, so it never makes the shutdown longer
//     than the stages above already can. It comes before the pools close because nothing else keeps
//     them open for a handler: one waiting on GitHub holds no connection, so stage 5 finds the
//     pools idle, and its writes would fail on a closed pool once GitHub answered, with its message
//     acknowledged all the same.
//  5. It waits until every request has answered and no connection of writePools is in use.
//  6. deregister takes this process out of the registry of live server processes, within what is
//     left of ctx: only once HTTP has drained, so a draining process counts as live until its last
//     request is answered, and before the pools close.
//  7. It closes the database pools; main then writes the buffered log lines last.
//
// While the database answers, only the deadline bounds the wait of stage 5, so a write in flight
// commits and is answered if it finishes before it. A database that has stopped answering
// (silentProbes) holds its connections forever, so Dispatch exits without them, and the database
// rolls back whatever they held open; it deregisters nothing then, since the delete would wait on
// that database too, and its row's heartbeat stops with the process instead. Writes carry no
// deadline of their own, so a write still in flight at the deadline is cut off with the process,
// as the runtime's kill would cut it moments later.
func shutdown(ctx context.Context, server *http.Server, documentService *docs.Service, pool *store.Pool,
	waitBackground, waitIntakes func(), deregister func(context.Context)) {
	deadline, _ := ctx.Deadline()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		// Only the requests end this wait, so drained means every one has answered; the stages
		// below stop waiting for it at their own points of the deadline.
		if err := server.Shutdown(context.Background()); err != nil {
			slog.Warn("dispatch: shutdown", "error", err)
		}
	}()
	select {
	case <-drained:
	case <-time.After(time.Until(deadline.Add(-silentProbeReserve - documentReserve))):
		slog.Warn("dispatch: settle documents with requests still in flight")
	}
	documentCtx, cancelDocuments := context.WithDeadline(ctx, deadline.Add(-silentProbeReserve))
	defer cancelDocuments()
	if err := documentService.Shutdown(documentCtx); err != nil {
		slog.Warn("dispatch: shutdown document service", "error", err)
	}
	background := make(chan struct{})
	go func() {
		defer close(background)
		waitBackground()
	}()
	select {
	case <-background:
	case <-ctx.Done():
		slog.Warn("dispatch: exit with background work still running at the shutdown deadline")
		return
	}
	intakes := make(chan struct{})
	go func() {
		defer close(intakes)
		waitIntakes()
	}()
	select {
	case <-intakes:
	case <-ctx.Done():
		slog.Warn("dispatch: exit with the GitHub intakes still stopping at the shutdown deadline")
		return
	}
	if waitUntilIdle(ctx, drained, pool) {
		deregister(ctx)
		pool.Close()
	}
}

// waitUntilIdle waits until HTTP has drained and no connection of writePools is in use, and
// reports whether that happened. It gives up once the database has stopped answering, when the
// connections in use never come back: closing the pools would wait on them. It gives up at ctx's
// deadline too, with requests or connections still in use: the process exits without them.
func waitUntilIdle(ctx context.Context, drained <-chan struct{}, pool *store.Pool) bool {
	failed := 0
	for {
		before := pool.Use(writePools...)
		answered := false
		select {
		case <-drained:
			if before.InUse == 0 {
				return true
			}
			answered = true
		default:
		}
		if ctx.Err() != nil {
			slog.Warn("dispatch: exit at the shutdown deadline with requests or database connections still in use",
				"in_use", before.InUse, "requests_answered", answered)
			return false
		}
		_, err := pool.Healthy(ctx)
		if ctx.Err() != nil {
			// A probe the deadline cut short says nothing about the database.
			continue
		}
		after := pool.Use(writePools...)
		if err == nil || after.Finished > before.Finished {
			failed = 0
		} else if failed++; failed >= silentProbes {
			slog.Warn("dispatch: exit with database connections still in use once the database stopped answering",
				"in_use", after.InUse, "failed_probes", failed, "error", err)
			return false
		}
		select {
		case <-time.After(drainPollInterval):
		case <-ctx.Done():
		}
	}
}
