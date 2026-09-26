// Package redeliver asks GitHub to redeliver the App webhook's failed deliveries.
//
// GitHub does not redeliver a webhook delivery the receiver failed: a delivery the Envoy
// listener answered 5xx (starting up, a CI-store write that ran out of retries) or that GitHub
// could not complete (the listener down, a reply slower than ten seconds) is lost to every
// consumer unless someone asks for it again. The sweep is GitHub's documented remedy: list the
// App webhook's failed attempts, and redeliver each delivery that has no answered attempt,
// through `POST /app/hook/deliveries/{id}/attempts`. A redelivery carries the original
// X-GitHub-Delivery GUID, and the listener publishes GitHub envelopes under a JetStream MsgId
// of that GUID's dedupe key, so asking again for a delivery that did reach the stream adds
// nothing to it.
//
// The listing asks GitHub for status=failure, a status code from 400 to 599. GitHub records a
// delivery it could not complete in that range too: a refused connection as 502, a reset one as
// 500.
//
// Dispatch runs the sweep because it holds the App's private key, which the list and redeliver
// endpoints require (they take only an App JWT).
package redeliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

const (
	// Bucket is the JetStream KV bucket holding the sweep's cursor and one record per delivery
	// it acted on. Its TTL is GitHub's own horizon: past three days GitHub neither lists nor
	// redelivers an attempt, so nothing older is worth remembering.
	Bucket = "envoy_webhook_redelivery"
	// Interval is how often the running sweep lists failures.
	Interval = 2 * time.Minute
	// Lookback is how far back every continuous sweep lists, and all a first sweep lists: a
	// retry's failed attempt stays inside it for the whole backoff schedule. Failures older than
	// a first sweep's hour are a backlog for an operator to decide about (Options.Since), because
	// a redelivered envelope carries the time it was published, not the time of the event.
	Lookback = time.Hour
	// MaxAttempts bounds the redeliveries GitHub accepts for one delivery, and separately the
	// redelivery requests GitHub refuses for it.
	MaxAttempts = 5

	retention       = 72 * time.Hour
	cursorOverlap   = 2 * time.Minute
	firstBackoff    = 2 * time.Minute
	maxBackoff      = 16 * time.Minute
	defaultRequests = 300
	cursorKey       = "cursor"
	recordPrefix    = "guid."
)

// GitHub is the App webhook delivery API the sweep reads and asks.
type GitHub interface {
	FailedDeliveries(ctx context.Context, since time.Time) ([]githubapp.Delivery, error)
	Redeliver(ctx context.Context, deliveryID int64) error
}

// OpenState opens the sweep's KV bucket, creating it on first use.
func OpenState(js nats.JetStreamContext) (nats.KeyValue, error) {
	kv, err := js.KeyValue(Bucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: Bucket, TTL: retention, Storage: nats.FileStorage, Replicas: 1})
	}
	if err != nil {
		return nil, fmt.Errorf("open %s KV bucket: %w", Bucket, err)
	}
	return kv, nil
}

// Outcome is what a sweep did, or in a dry run would do, with one delivery.
type Outcome string

const (
	// Redelivered: GitHub accepted a redelivery request.
	Redelivered Outcome = "redelivered"
	// WouldRedeliver: a dry run would have requested a redelivery.
	WouldRedeliver Outcome = "would-redeliver"
	// RequestRefused: GitHub refused the redelivery request; it is asked again after the backoff.
	RequestRefused Outcome = "request-refused"
	// Waiting: the last redelivery failed and the next one's backoff has not passed.
	Waiting Outcome = "waiting"
	// Pending: the last redelivery has produced no failure, so it was delivered or GitHub has not
	// attempted it yet.
	Pending Outcome = "pending"
	// Terminal: the receiver answered 4xx, refusing the request itself; the same bytes would fail
	// the same way, so it is never redelivered.
	Terminal Outcome = "terminal"
	// Exhausted: MaxAttempts redeliveries failed, or MaxAttempts requests were refused.
	Exhausted Outcome = "exhausted"
	// Closed: an earlier sweep marked the delivery terminal or exhausted.
	Closed Outcome = "closed"
	// ClaimedElsewhere: another sweeper claimed this attempt first.
	ClaimedElsewhere Outcome = "claimed-elsewhere"
)

// Decision is one delivery the sweep listed and what became of it.
type Decision struct {
	GUID         string
	DeliveryID   int64
	DeliveredAt  time.Time
	Event        string
	Action       string
	RepositoryID int64
	StatusCode   int
	Attempts     int
	Outcome      Outcome
}

// Report is one sweep's listing and decisions, newest delivery first.
type Report struct {
	Since     time.Time
	Listed    int
	Decisions []Decision
	// Complete is false when the sweep stopped at its request cap before deciding every delivery.
	Complete bool
}

// Count returns how many decisions had outcome.
func (r Report) Count(outcome Outcome) int {
	n := 0
	for _, decision := range r.Decisions {
		if decision.Outcome == outcome {
			n++
		}
	}
	return n
}

// Options select a sweep's window and whether it acts.
type Options struct {
	// Since lists failures delivered at or after it. Zero is the running sweep's window: the last
	// Lookback, reaching back to the stored cursor after a gap, and a complete sweep advances the
	// cursor. A sweep with Since set leaves the cursor alone.
	Since time.Time
	// DryRun decides every delivery and requests nothing, writes nothing and logs no alarm line.
	DryRun bool
}

// Sweeper lists the App webhook's failed deliveries and redelivers them.
type Sweeper struct {
	GitHub GitHub
	State  nats.KeyValue
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Pace is the pause between redelivery requests, so a large backlog does not trip GitHub's
	// abuse limits.
	Pace time.Duration
	// MaxRequests caps redelivery requests per sweep; zero is 300.
	MaxRequests int
}

// record is the sweep's memory of one delivery, keyed by its GUID.
type record struct {
	// Attempts counts redeliveries GitHub accepted.
	Attempts int `json:"attempts"`
	// Refusals counts consecutive redelivery requests GitHub refused; accepting one resets it.
	Refusals int `json:"refusals"`
	// LastAttemptAt is when the last request was made, accepted or refused; backoff runs from it.
	LastAttemptAt time.Time `json:"last_attempt_at,omitzero"`
	// Answered holds the failed attempts a request has already answered. A failed attempt not in
	// it is a failure the sweep has not acted on: the delivery's first, or its last redelivery's.
	Answered  []int64 `json:"answered,omitempty"`
	Terminal  bool    `json:"terminal,omitempty"`
	Exhausted bool    `json:"exhausted,omitempty"`
}

type cursorRecord struct {
	At time.Time `json:"at"`
}

// Run sweeps now and then every interval until ctx ends, logging each sweep.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		report, err := s.Sweep(ctx, Options{})
		if err != nil {
			s.Logger.Warn("webhook redelivery sweep failed", "error", err)
		} else {
			s.Logger.Info("webhook redelivery sweep",
				"since", report.Since.Format(time.RFC3339),
				"failed_attempts", report.Listed,
				"deliveries", len(report.Decisions),
				"redelivered", report.Count(Redelivered),
				"request_refused", report.Count(RequestRefused),
				"waiting", report.Count(Waiting),
				"terminal", report.Count(Terminal),
				"exhausted", report.Count(Exhausted),
				"complete", report.Complete,
			)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep lists the failed deliveries in its window and decides each: redeliver it, wait out its
// backoff, leave a pending redelivery alone, or give up on it.
func (s *Sweeper) Sweep(ctx context.Context, opts Options) (Report, error) {
	now := s.now()
	continuous := opts.Since.IsZero()
	since := opts.Since
	var cursorRevision uint64
	if continuous {
		since = now.Add(-Lookback)
		cursor, revision, err := s.readCursor()
		if err != nil {
			return Report{}, err
		}
		cursorRevision = revision
		if resume := cursor.Add(-cursorOverlap); revision != 0 && resume.Before(since) {
			if oldest := now.Add(-retention); resume.Before(oldest) {
				resume = oldest
			}
			s.Logger.Warn("webhook redelivery sweep resuming after a gap", "cursor", cursor.Format(time.RFC3339), "since", resume.Format(time.RFC3339))
			since = resume
		}
	}
	failed, err := s.GitHub.FailedDeliveries(ctx, since)
	if err != nil {
		return Report{}, fmt.Errorf("list failed webhook deliveries since %s: %w", since.Format(time.RFC3339), err)
	}

	byGUID := map[string][]githubapp.Delivery{}
	var order []string
	for _, delivery := range failed {
		if _, seen := byGUID[delivery.GUID]; !seen {
			order = append(order, delivery.GUID)
		}
		byGUID[delivery.GUID] = append(byGUID[delivery.GUID], delivery)
	}

	report := Report{Since: since, Listed: len(failed), Complete: true}
	limit := s.MaxRequests
	if limit <= 0 {
		limit = defaultRequests
	}
	requests := 0
	for _, guid := range order {
		if requests >= limit {
			report.Complete = false
			break
		}
		decision, requested, err := s.decide(ctx, guid, byGUID[guid], now, opts.DryRun)
		if err != nil {
			return report, err
		}
		report.Decisions = append(report.Decisions, decision)
		if requested {
			requests++
			if s.Pace > 0 {
				select {
				case <-ctx.Done():
					return report, ctx.Err()
				case <-time.After(s.Pace):
				}
			}
		}
	}
	if continuous && report.Complete && !opts.DryRun {
		s.writeCursor(now, cursorRevision)
	}
	return report, nil
}

// decide settles one delivery from its failed attempts (newest first) and its record. requested
// reports that a redelivery request was made, accepted or not.
func (s *Sweeper) decide(ctx context.Context, guid string, attempts []githubapp.Delivery, now time.Time, dryRun bool) (Decision, bool, error) {
	newest := attempts[0]
	decision := Decision{
		GUID:         guid,
		DeliveryID:   newest.ID,
		DeliveredAt:  newest.DeliveredAt,
		Event:        newest.Event,
		Action:       newest.Action,
		RepositoryID: newest.RepositoryID,
		StatusCode:   newest.StatusCode,
	}
	current, revision, err := s.readRecord(guid)
	if err != nil {
		return decision, false, err
	}
	decision.Attempts = current.Attempts
	if current.Terminal || current.Exhausted {
		decision.Outcome = Closed
		return decision, false, nil
	}

	if newest.StatusCode >= 400 && newest.StatusCode < 500 {
		decision.Outcome = Terminal
		if dryRun {
			return decision, false, nil
		}
		next := current
		next.Terminal = true
		if s.writeRecord(guid, next, revision) {
			s.Logger.Error("webhook delivery refused terminally", s.attrs(decision)...)
		}
		return decision, false, nil
	}

	unanswered := slices.ContainsFunc(attempts, func(attempt githubapp.Delivery) bool {
		return !slices.Contains(current.Answered, attempt.ID)
	})
	if revision != 0 && !unanswered && current.Refusals == 0 {
		decision.Outcome = Pending
		return decision, false, nil
	}
	if current.Attempts >= MaxAttempts || current.Refusals >= MaxAttempts {
		decision.Outcome = Exhausted
		if dryRun {
			return decision, false, nil
		}
		next := current
		next.Exhausted = true
		if s.writeRecord(guid, next, revision) {
			s.Logger.Error("webhook redelivery exhausted", append(s.attrs(decision), "refusals", current.Refusals)...)
		}
		return decision, false, nil
	}
	if revision != 0 && now.Before(current.LastAttemptAt.Add(backoff(current.Attempts+current.Refusals))) {
		decision.Outcome = Waiting
		return decision, false, nil
	}
	if dryRun {
		decision.Outcome = WouldRedeliver
		return decision, false, nil
	}

	claim := current
	claim.Attempts++
	claim.Refusals = 0
	claim.LastAttemptAt = now
	for _, attempt := range attempts {
		if !slices.Contains(claim.Answered, attempt.ID) {
			claim.Answered = append(claim.Answered, attempt.ID)
		}
	}
	claimRevision, claimed := s.claimRecord(guid, claim, revision)
	if !claimed {
		decision.Outcome = ClaimedElsewhere
		return decision, false, nil
	}
	if err := s.GitHub.Redeliver(ctx, newest.ID); err != nil {
		refused := claim
		refused.Attempts = current.Attempts
		refused.Refusals = current.Refusals + 1
		if !s.writeRecord(guid, refused, claimRevision) {
			s.Logger.Error("webhook redelivery refusal not recorded; the delivery reads as pending", s.attrs(decision)...)
		}
		decision.Outcome = RequestRefused
		s.Logger.Warn("webhook redelivery request refused", append(s.attrs(decision), "error", err.Error())...)
		return decision, true, nil
	}
	decision.Attempts = claim.Attempts
	decision.Outcome = Redelivered
	s.Logger.Info("webhook redelivered", append(s.attrs(decision), "attempt", claim.Attempts)...)
	return decision, true, nil
}

func (s *Sweeper) attrs(decision Decision) []any {
	return []any{
		"guid", decision.GUID,
		"delivery_id", decision.DeliveryID,
		"event", decision.Event,
		"action", decision.Action,
		"repository_id", decision.RepositoryID,
		"status_code", decision.StatusCode,
		"delivered_at", decision.DeliveredAt.Format(time.RFC3339),
	}
}

// backoff is the pause before the next request after tries requests: 2, 4, 8, then 16 minutes.
func backoff(tries int) time.Duration {
	wait := firstBackoff
	for i := 1; i < tries && wait < maxBackoff; i++ {
		wait *= 2
	}
	return min(wait, maxBackoff)
}

func (s *Sweeper) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Sweeper) readCursor() (time.Time, uint64, error) {
	entry, err := s.State.Get(cursorKey)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return time.Time{}, 0, nil
	}
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("read the redelivery cursor: %w", err)
	}
	var cursor cursorRecord
	if err := json.Unmarshal(entry.Value(), &cursor); err != nil {
		return time.Time{}, 0, fmt.Errorf("decode the redelivery cursor: %w", err)
	}
	return cursor.At, entry.Revision(), nil
}

// writeCursor records the sweep's start. A lost race means another sweeper wrote a cursor at
// the same moment; either value serves.
func (s *Sweeper) writeCursor(at time.Time, revision uint64) {
	value, err := json.Marshal(cursorRecord{At: at})
	if err != nil {
		s.Logger.Warn("webhook redelivery cursor not encoded", "error", err)
		return
	}
	if revision == 0 {
		_, err = s.State.Create(cursorKey, value)
	} else {
		_, err = s.State.Update(cursorKey, value, revision)
	}
	if err != nil && !isConflict(err) {
		s.Logger.Warn("webhook redelivery cursor not written", "error", err)
	}
}

func (s *Sweeper) readRecord(guid string) (record, uint64, error) {
	entry, err := s.State.Get(recordPrefix + guid)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return record{}, 0, nil
	}
	if err != nil {
		return record{}, 0, fmt.Errorf("read the redelivery record of %s: %w", guid, err)
	}
	var current record
	if err := json.Unmarshal(entry.Value(), &current); err != nil {
		return record{}, 0, fmt.Errorf("decode the redelivery record of %s: %w", guid, err)
	}
	return current, entry.Revision(), nil
}

// claimRecord writes next only if the record is still at revision (absent when zero), so two
// sweepers never both request one attempt. It returns the new revision.
func (s *Sweeper) claimRecord(guid string, next record, revision uint64) (uint64, bool) {
	value, err := json.Marshal(next)
	if err != nil {
		s.Logger.Warn("webhook redelivery record not encoded", "guid", guid, "error", err)
		return 0, false
	}
	var written uint64
	if revision == 0 {
		written, err = s.State.Create(recordPrefix+guid, value)
	} else {
		written, err = s.State.Update(recordPrefix+guid, value, revision)
	}
	if err != nil {
		if !isConflict(err) {
			s.Logger.Warn("webhook redelivery record not written", "guid", guid, "error", err)
		}
		return 0, false
	}
	return written, true
}

func (s *Sweeper) writeRecord(guid string, next record, revision uint64) bool {
	_, written := s.claimRecord(guid, next, revision)
	return written
}

func isConflict(err error) bool {
	return errors.Is(err, nats.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence")
}
