package cistore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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
// is cancelled. On each tick it emits one `pr.<n>.checks` envelope when a head
// commit has been quiet, all recorded suites are complete, and all check runs
// are terminal. The store's CAS makes the loop idempotent across replicas.
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
	staleBefore := now - (2 * debounce).Milliseconds()
	for _, cached := range store.List() {
		if now-cached.LastEventAt < debounce.Milliseconds() {
			continue
		}
		head, knownHead := store.Head(cached.Owner, cached.Repo, cached.Number)
		if !knownHead {
			head = cached.SHA
		}
		if cached.SHA != head || cached.SettledEmitted || !settlementReady(cached) {
			continue
		}
		key := Key(cached.Owner, cached.Repo, cached.Number, cached.SHA)
		if cached.Claim != nil {
			claimGeneration := cached.Claim.Generation
			if claimGeneration == cached.Generation && cached.Claim.ClaimedAt >= staleBefore {
				continue
			}
			reclaimed, err := store.ReclaimSettlement(key, claimGeneration, staleBefore)
			if err != nil {
				logger.Warn("checks reclaim failed", slog.String("error", err.Error()), slog.String("sha", cached.SHA))
				continue
			}
			if !reclaimed {
				continue
			}
		}

		state, claimed, err := store.ClaimSettlement(key, cached.Hash(), cached.Generation, now, debounce)
		if err != nil {
			logger.Warn("checks claim failed", slog.String("error", err.Error()), slog.String("sha", cached.SHA))
			continue
		}
		if !claimed {
			continue
		}
		sum, err := renderSummary(state)
		if err != nil {
			logger.Error("checks render failed", slog.String("error", err.Error()))
			if _, releaseErr := store.ReleaseClaim(key, state.Generation); releaseErr != nil {
				logger.Warn("checks release failed", slog.String("error", releaseErr.Error()), slog.String("sha", state.SHA))
			}
			continue
		}
		issuedAt := contracts.NowMillis()
		sum.SettledAt = issuedAt
		payload, err := json.Marshal(sum)
		if err != nil {
			logger.Error("checks payload failed", slog.String("error", err.Error()))
			continue
		}
		env := contracts.Envelope{
			EventID:       id.New(),
			Source:        "github",
			SourceEventID: id.New(),
			Topic:         contracts.GithubSubject(state.Owner, state.Repo, "pr."+state.Number+".checks"),
			DedupeKey: fmt.Sprintf(
				"github.checks.%s/%s.pr.%s.%s.g%d",
				state.Owner,
				state.Repo,
				state.Number,
				state.SHA,
				state.Generation,
			),
			IssuedAt:       issuedAt,
			PayloadSummary: settledPayloadSummary(sum),
			Payload:        string(payload),
			TraceID:        id.New(),
		}
		if err := env.Validate(); err != nil {
			logger.Error("checks invalid envelope", slog.String("error", err.Error()))
			if _, releaseErr := store.ReleaseClaim(key, state.Generation); releaseErr != nil {
				logger.Warn("checks release failed", slog.String("error", releaseErr.Error()), slog.String("sha", state.SHA))
			}
			continue
		}
		held, err := store.ClaimStillHeld(key, state.Generation)
		if err != nil {
			logger.Warn("checks claim verification failed", slog.String("error", err.Error()), slog.String("sha", state.SHA))
			continue
		}
		if !held {
			headMatches, headErr := store.durableHeadMatches(state)
			if headErr != nil {
				logger.Warn("checks head verification failed", slog.String("error", headErr.Error()), slog.String("sha", state.SHA))
			} else if !headMatches {
				if _, releaseErr := store.ReleaseClaim(key, state.Generation); releaseErr != nil {
					logger.Warn("checks release failed", slog.String("error", releaseErr.Error()), slog.String("sha", state.SHA))
				}
			}
			continue
		}
		if err := pub.Publish(env); err != nil {
			logger.Warn("checks publish failed",
				slog.String("error", err.Error()),
				slog.String("topic", env.Topic),
				slog.String("sha", state.SHA),
			)
			if _, releaseErr := store.ReleaseClaim(key, state.Generation); releaseErr != nil {
				logger.Warn("checks release failed", slog.String("error", releaseErr.Error()), slog.String("sha", state.SHA))
			}
			continue
		}
		if _, err := store.MarkSettled(key, state.Generation); err != nil {
			logger.Warn("checks mark-settled failed", slog.String("error", err.Error()))
		}
	}
}

func settledPayloadSummary(sum Summary) string {
	summary := fmt.Sprintf(
		"checks settled on %s#%s @ %s: %d passed, %d failed, %d cancelled, %d skipped",
		sum.Repo, sum.Number, sha7(sum.SHA), sum.Passed.Count, sum.Failed.Count,
		sum.Cancelled.Count, sum.Skipped.Count,
	)
	if sum.Failed.Count > 0 {
		summary += "; failing: " + strings.Join(sum.Failed.Checks, ", ")
	}
	if sum.SupersededSettlement == "true" {
		summary += " (re-settled)"
	}
	return contracts.OneLineSummary(summary)
}

func sha7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
