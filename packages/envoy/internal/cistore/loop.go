package cistore

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
)

// Publisher publishes a rendered summary envelope. *bus.Client satisfies this.
type Publisher interface {
	Publish(contracts.Envelope) error
}

// StartSummaryLoop runs a reconcile ticker in a background goroutine until ctx
// is cancelled. On each tick it scans cached commit states and, for any commit
// whose checks have been quiet for the debounce window and whose check set
// changed since the last emit, publishes one rendered summary to pr.<n>.ci.
//
// Emit-once + debounce are enforced by MarkEmitted (a KV compare-and-swap that
// re-validates against fresh state), so the loop is idempotent across replicas
// and safe to run alongside a listener restart: the only in-memory state (the
// WatchAll cache) is rebuilt from durable KV. Cancelling ctx stops the loop
// cleanly, which avoids post-shutdown KV errors once NATS is drained.
func StartSummaryLoop(ctx context.Context, store *Store, pub Publisher, debounce, tick time.Duration, logger *logging.Logger) {
	t := time.NewTicker(tick)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runSummaryTick(store, pub, debounce, logger)
			}
		}
	}()
}

// runSummaryTick performs a single reconcile pass. Split out for testability.
func runSummaryTick(store *Store, pub Publisher, debounce time.Duration, logger *logging.Logger) {
	now := time.Now().UnixMilli()
	for _, st := range store.List() {
		if now-st.LastEventAt < debounce.Milliseconds() {
			continue // still within the quiet window; let more checks accumulate
		}
		h := st.Hash()
		if h == st.LastEmitHash {
			continue // nothing changed since the last emit
		}
		text, err := RenderSummary(st)
		if err != nil {
			logger.Error("ci summary render failed", slog.String("error", err.Error()))
			continue
		}
		env := contracts.Envelope{
			EventID:        id.New(),
			Source:         "github",
			SourceEventID:  id.New(),
			Topic:          contracts.GithubSubject(st.Owner, st.Repo, "pr."+st.Number+".ci"),
			DedupeKey:      "github.ci." + st.Owner + "/" + st.Repo + ".pr." + st.Number + "." + st.SHA + "." + h,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: text,
			TraceID:        id.New(),
		}
		if err := env.Validate(); err != nil {
			logger.Error("ci summary invalid envelope", slog.String("error", err.Error()))
			continue
		}
		// MarkEmitted before Publish: this favors exactly-once over at-least-once.
		// A failed publish drops the summary rather than risking a double-publish,
		// which is acceptable for a status summary — the next check event advances
		// the hash and re-opens emission. MarkEmitted re-validates hash + debounce
		// against fresh KV, so a stale/premature summary can never win the CAS.
		ok, err := store.MarkEmitted(Key(st.Owner, st.Repo, st.Number, st.SHA), h, debounce)
		if err != nil {
			logger.Warn("ci summary mark-emitted failed", slog.String("error", err.Error()))
			continue
		}
		if !ok {
			continue // stale/premature/already-emitted, or another replica won the CAS
		}
		if err := pub.Publish(env); err != nil {
			// Dropped, not retried (MarkEmitted already advanced). Log enough to
			// trace which summary was lost. See docs/solutions/.../envoy-ci-summary.md.
			logger.Warn("ci summary publish failed (dropped)",
				slog.String("error", err.Error()),
				slog.String("topic", env.Topic),
				slog.String("sha", st.SHA),
				slog.String("hash", h),
			)
		}
	}
}
