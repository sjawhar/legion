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
// default stop timeout, which Dispatch's task definition leaves unset, and the compose file's
// stop_grace_period.
const stopGrace = 30 * time.Second

// documentShutdownTimeout is the document service's own budget: docs.Service.Shutdown spends up to
// its drain budget settling the documents owed, and needs the rest to finish what that budget cut
// short and read back which settlements committed.
const documentShutdownTimeout = 2 * docs.ShutdownDrainBudget

// httpDrainBeforeDocuments is how long the document service waits for the requests in flight to
// finish before it settles the documents owed with requests still running, so that its budget ends
// 5 s inside stopGrace. An open event stream holds none of it, since every stream ends with the
// process context (api.Deps.Lifetime), which is cancelled before shutdown starts.
const httpDrainBeforeDocuments = stopGrace - 5*time.Second - documentShutdownTimeout

// silentProbes is how many health probes (store.Pool.Healthy, two seconds each) in a row must fail,
// with no connection of the shared or document-rooms pool taken back meanwhile, before Dispatch
// counts the database as having stopped answering. A probe that answers, or a connection taken
// back while it ran, starts the count again: a slow database that is still finishing work is
// answering. Three is the count the load balancer waits for before it replaces the task.
const silentProbes = 3

// drainPollInterval is how often the last phase asks whether the requests in flight and the
// connections in use are done.
const drainPollInterval = 250 * time.Millisecond

// serveUntilStopped serves on listener until ctx ends, then stops Dispatch (shutdown). It returns
// the error that ended serving when serving failed, which cancels ctx.
func serveUntilStopped(ctx context.Context, cancel context.CancelFunc, server *http.Server, listener net.Listener,
	addr string, documentService *docs.Service, pool *store.Pool) error {
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
	shutdown(server, documentService, pool)
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
// signal with some still running. The database pool then closes once every request has answered
// and no connection of the shared or document-rooms pool is in use. While the database answers,
// nothing but the runtime's kill bounds that wait, so a write in flight commits and is answered if
// it finishes before the kill. A database that has stopped answering (silentProbes) holds its
// connections forever, so Dispatch exits without them, and the database rolls back whatever they
// held open.
func shutdown(server *http.Server, documentService *docs.Service, pool *store.Pool) {
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
	if waitUntilIdle(drained, pool) {
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
