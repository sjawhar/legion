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
	for _, st := range store.List() {
		if now-st.LastEventAt < debounce.Milliseconds() {
			continue
		}
		head, knownHead := store.Head(st.Owner, st.Repo, st.Number)
		if !knownHead {
			continue
		}
		sum, _, err := renderSummary(st, head)
		if err != nil {
			logger.Error("checks render failed", slog.String("error", err.Error()))
			continue
		}
		if !sum.IsHead {
			continue
		}
		if st.SettledEmitted || !settlementReady(st) {
			continue
		}

		h := st.Hash()
		payload, err := json.Marshal(sum)
		if err != nil {
			logger.Error("checks payload failed", slog.String("error", err.Error()))
			continue
		}
		env := contracts.Envelope{
			EventID:        id.New(),
			Source:         "github",
			SourceEventID:  id.New(),
			Topic:          contracts.GithubSubject(st.Owner, st.Repo, "pr."+st.Number+".checks"),
			DedupeKey:      "github.checks." + st.Owner + "/" + st.Repo + ".pr." + st.Number + "." + st.SHA + "." + h,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: settledPayloadSummary(sum),
			Payload:        string(payload),
			TraceID:        id.New(),
		}
		if err := env.Validate(); err != nil {
			logger.Error("checks invalid envelope", slog.String("error", err.Error()))
			continue
		}
		ok, err := store.MarkSettled(Key(st.Owner, st.Repo, st.Number, st.SHA), h, debounce)
		if err != nil {
			logger.Warn("checks mark-settled failed", slog.String("error", err.Error()))
			continue
		}
		if !ok {
			continue
		}
		if err := pub.Publish(env); err != nil {
			logger.Warn("checks publish failed (dropped)",
				slog.String("error", err.Error()),
				slog.String("topic", env.Topic),
				slog.String("sha", st.SHA),
			)
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
