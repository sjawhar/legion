package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// httpShutdownTimeout bounds http.Server.Shutdown at SIGTERM: the requests in flight finishing. An
// open event stream holds none of it, since every stream ends with the process context
// (api.Deps.Lifetime), which is cancelled before shutdown starts.
const httpShutdownTimeout = 5 * time.Second

// documentShutdownTimeout is the document service's own budget, counted from when HTTP shutdown
// returns: docs.Service.Shutdown spends up to its drain budget settling the documents owed, and
// needs the rest to finish what that budget cut short and read back which settlements committed.
// The database pool's close gets what is left of it.
const documentShutdownTimeout = 2 * docs.ShutdownDrainBudget

// shutdown stops Dispatch once its process context is cancelled: HTTP within httpShutdownTimeout,
// then the document service within documentShutdownTimeout of its own, so a slow request takes no
// time from the settlements owed, then the database pool within what is left of that budget. A
// runtime's stop grace period has to allow both budgets.
func shutdown(server *http.Server, documentService *docs.Service, pool *store.Pool) {
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancelHTTP()
	if err := server.Shutdown(httpCtx); err != nil {
		slog.Warn("dispatch: shutdown", "error", err)
	}
	documentCtx, cancelDocuments := context.WithTimeout(context.Background(), documentShutdownTimeout)
	defer cancelDocuments()
	if err := documentService.Shutdown(documentCtx); err != nil {
		slog.Warn("dispatch: shutdown document service", "error", err)
	}
	// Closing the pool waits for every connection still in use, and one waiting on a database that
	// has stopped answering never comes back: the process exits without it once the budget is spent,
	// and the database rolls back whatever that connection held open.
	closed := make(chan struct{})
	go func() {
		pool.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-documentCtx.Done():
		slog.Warn("dispatch: exit with database connections still in use at the end of the shutdown budget",
			"in_use", pool.Stat().AcquiredConns())
	}
}
