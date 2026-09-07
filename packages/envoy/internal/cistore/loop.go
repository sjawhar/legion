package cistore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

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
		head, knownHead := store.Head(st.Owner, st.Repo, st.Number)
		if !knownHead {
			head = st.SHA // states recorded before head tracking are treated as current
		}
		sum, text, err := renderSummary(st, head)
		if err != nil {
			logger.Error("ci summary render failed", slog.String("error", err.Error()))
			continue
		}
		if knownHead && !sum.IsHead {
			continue
		}
		h := st.Hash()
		key := Key(st.Owner, st.Repo, st.Number, st.SHA)
		if h != st.LastEmitHash {
			env := contracts.Envelope{
				EventID:        id.New(),
				Source:         "github",
				SourceEventID:  id.New(),
				Topic:          contracts.GithubSubject(st.Owner, st.Repo, "pr."+st.Number+".ci"),
				DedupeKey:      "github.ci." + st.Owner + "/" + st.Repo + ".pr." + st.Number + "." + st.SHA + "." + h,
				IssuedAt:       contracts.NowMillis(),
				PayloadSummary: ciPayloadSummary(sum),
				Payload:        text,
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
			ok, err := store.MarkEmitted(key, h, debounce)
			if err != nil {
				logger.Warn("ci summary mark-emitted failed", slog.String("error", err.Error()))
				continue
			}
			if !ok {
				continue // stale/premature/already-emitted, or another replica won the CAS
			}
			if err := pub.Publish(env); err != nil {
				if _, markErr := store.MarkCIPublished(key, h, false); markErr != nil {
					logger.Warn("ci summary failed-publish record failed", slog.String("error", markErr.Error()))
				}
				logger.Warn("ci summary publish failed (dropped)",
					slog.String("error", err.Error()),
					slog.String("topic", env.Topic),
					slog.String("sha", st.SHA),
					slog.String("hash", h),
				)
				continue
			}
			if _, err := store.MarkCIPublished(key, h, true); err != nil {
				logger.Warn("ci summary publish record failed", slog.String("error", err.Error()))
			}
		}

		if st.SettledEmitted || !terminal(st) {
			continue
		}

		settled := sum
		settled.Kind = "checks_settled"
		payload, err := json.Marshal(settled)
		if err != nil {
			logger.Error("checks settled payload failed", slog.String("error", err.Error()))
			continue
		}
		settledEnv := contracts.Envelope{
			EventID:        id.New(),
			Source:         "github",
			SourceEventID:  id.New(),
			Topic:          contracts.GithubSubject(st.Owner, st.Repo, "pr."+st.Number+".checks.settled"),
			DedupeKey:      "github.checks-settled." + st.Owner + "/" + st.Repo + ".pr." + st.Number + "." + st.SHA,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: settledPayloadSummary(settled),
			Payload:        string(payload),
			TraceID:        id.New(),
		}
		if err := settledEnv.Validate(); err != nil {
			logger.Error("checks settled invalid envelope", slog.String("error", err.Error()))
			continue
		}
		ok, err := store.MarkSettled(key, h)
		if err != nil {
			logger.Warn("checks settled mark-emitted failed", slog.String("error", err.Error()))
			continue
		}
		if !ok {
			continue
		}
		if err := pub.Publish(settledEnv); err != nil {
			logger.Warn("checks settled publish failed (dropped)",
				slog.String("error", err.Error()),
				slog.String("topic", settledEnv.Topic),
				slog.String("sha", st.SHA),
			)
		}
	}
}

func ciPayloadSummary(sum Summary) string {
	return payloadSummary(fmt.Sprintf(
		"CI for %s#%s @ %s: %d passed, %d failed, %d running, %d queued, %d cancelled, %d skipped",
		sum.Repo, sum.Number, sha7(sum.SHA), sum.Passed.Count, sum.Failed.Count,
		sum.Running.Count, sum.Queued.Count, sum.Cancelled.Count, sum.Skipped.Count,
	))
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
	return payloadSummary(summary)
}

func sha7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func payloadSummary(summary string) string {
	summary = strings.NewReplacer("\r", " ", "\n", " ").Replace(summary)
	if utf8.RuneCountInString(summary) <= 160 {
		return summary
	}
	runes := []rune(summary)
	return string(runes[:159]) + "…"
}
