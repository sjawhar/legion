package architecture

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// SyncInterval is how often the server re-imports every enabled architecture
// source. The Refresh button and the dispatch_architecture_sync tool are the
// impatient paths; this one keeps a model honest on its own.
const SyncInterval = 5 * time.Minute

// Run re-imports every enabled architecture source on SyncInterval, one
// project at a time, until ctx is cancelled. The first tick is jittered so a
// restart does not stampede GitHub, and a server without App credentials logs
// once and stops: no sync can be signed. Shares its jittered-ticker loop with
// delivery.Reconcile.Run via duty.RunJittered.
func Run(ctx context.Context, importer *Importer) {
	duty.RunJittered(ctx, SyncInterval, func() bool {
		if importer.HasApp() {
			return false
		}
		slog.Info("dispatch architecture: no GitHub App key — the architecture importer is idle")
		return true
	}, func(ctx context.Context) { syncAll(ctx, importer) })
}

func syncAll(ctx context.Context, importer *Importer) {
	projects, err := importer.ListEnabled(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Error("dispatch architecture: list sources", "error", err)
		}
		return
	}
	for _, project := range projects {
		if ctx.Err() != nil {
			return
		}
		source, err := importer.Sync(ctx, project)
		switch {
		case err == nil:
			commit := ""
			if source.LastCommit != nil {
				commit = *source.LastCommit
			}
			slog.Debug("dispatch architecture: synced", "project", project, "commit", commit)
		case errors.Is(err, context.Canceled):
			return
		default:
			// The failure is already on the source row (and, on a change, in a
			// sync_failed event); the log is for the operator watching stderr.
			slog.Warn("dispatch architecture: sync failed", "project", project, "error", err)
		}
	}
}
