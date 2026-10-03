package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// stopGrace is how long Dispatch's runtime lets it run after SIGTERM before killing it: ECS's
// default stop timeout, which Dispatch's task definition leaves unset, and the compose file's
// stop_grace_period.
const stopGrace = 30 * time.Second

// shutdownBudget bounds the whole ordered shutdown, from SIGTERM. The rest of stopGrace is the
// process's own exit.
const shutdownBudget = stopGrace - 5*time.Second

// documentShutdownTimeout is the document service's own budget: docs.Service.Shutdown spends up to
// its drain budget settling the documents owed, and needs the rest to finish what that budget cut
// short and read back which settlements committed.
const documentShutdownTimeout = 2 * docs.ShutdownDrainBudget

// httpDrainBeforeDocuments is how long the document service waits for the requests in flight to
// finish before it settles the documents owed with requests still running: what shutdownBudget
// leaves once the document service has its own. An open event stream holds none of it, since every
// stream ends with the process context (api.Deps.Lifetime), which is cancelled before shutdown
// starts.
const httpDrainBeforeDocuments = shutdownBudget - documentShutdownTimeout

// drainPollInterval is how often the last phase asks whether the requests in flight and the
// connections in use are done, and whether the database still answers.
const drainPollInterval = 250 * time.Millisecond

// shutdown stops Dispatch once its process context is cancelled. HTTP drains the requests in flight
// first, for as long as shutdownBudget allows. The document service settles the documents owed
// within documentShutdownTimeout once those requests are done, or httpDrainBeforeDocuments after the
// signal with some still running. The database pool closes once every request has answered and
// every connection is back: a request against a database that answers gets the whole budget to
// commit and answer. A database that has stopped answering holds its connections forever, so the
// process exits without them as soon as the health probe finds it silent, and the database rolls
// back whatever they held open.
func shutdown(server *http.Server, documentService *docs.Service, pool *store.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		if err := server.Shutdown(ctx); err != nil {
			slog.Warn("dispatch: shutdown", "error", err)
		}
	}()
	select {
	case <-drained:
	case <-time.After(httpDrainBeforeDocuments):
		slog.Warn("dispatch: settle documents with requests still in flight")
	}
	documentCtx, cancelDocuments := context.WithTimeout(ctx, documentShutdownTimeout)
	defer cancelDocuments()
	if err := documentService.Shutdown(documentCtx); err != nil {
		slog.Warn("dispatch: shutdown document service", "error", err)
	}
	if !waitUntilIdle(ctx, drained, pool) {
		return
	}
	closed := make(chan struct{})
	go func() {
		pool.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-ctx.Done():
		slog.Warn("dispatch: exit at the end of the shutdown budget with requests or database connections still in use",
			"in_use", pool.Stat().AcquiredConns())
	}
}

// waitUntilIdle waits until HTTP has drained and no connection of the pool is in use, and reports
// whether that happened. It gives up when the database stops answering or the budget ends: either
// way the process exits without closing the pool, whose close would wait on those connections.
func waitUntilIdle(ctx context.Context, drained <-chan struct{}, pool *store.Pool) bool {
	for {
		select {
		case <-drained:
			if pool.Stat().AcquiredConns() == 0 {
				return true
			}
		default:
		}
		if _, err := pool.Healthy(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("dispatch: exit with database connections still in use once the database stopped answering",
				"in_use", pool.Stat().AcquiredConns(), "error", err)
			return false
		}
		select {
		case <-ctx.Done():
			slog.Warn("dispatch: exit at the end of the shutdown budget with requests or database connections still in use",
				"in_use", pool.Stat().AcquiredConns())
			return false
		case <-time.After(drainPollInterval):
		}
	}
}
