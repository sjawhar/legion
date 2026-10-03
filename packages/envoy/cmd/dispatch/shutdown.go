package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// httpShutdownTimeout bounds http.Server.Shutdown at SIGTERM: the requests in flight finishing. An
// open event stream holds none of it, since every stream ends with the process context
// (api.Deps.Lifetime), which is cancelled before shutdown starts.
const httpShutdownTimeout = 5 * time.Second

// documentShutdownTimeout is the document service's own budget, counted from when HTTP shutdown
// returns: docs.Service.Shutdown spends up to its drain budget settling the documents owed, and
// needs the rest to finish what that budget cut short and read back which settlements committed.
const documentShutdownTimeout = 2 * docs.ShutdownDrainBudget

// shutdown stops Dispatch once its process context is cancelled: HTTP within httpShutdownTimeout,
// then the document service within documentShutdownTimeout of its own, so a slow request takes no
// time from the settlements owed. A runtime's stop grace period has to allow both budgets.
func shutdown(server *http.Server, documentService *docs.Service) {
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
}
