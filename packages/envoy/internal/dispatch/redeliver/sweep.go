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
// The listing takes every attempt and judges each GUID as GitHub's own redelivery script does: a
// GUID with an OK attempt is delivered, whatever its failures, and every attempt whose status is
// not OK is a failure. GitHub's status=failure filter takes only status codes from 400 to 599, and
// nothing shows that GitHub gives an attempt that got no HTTP answer (a timeout, a refused or reset
// connection) a code in that range.
//
// GitHub rate-limits the App: redelivery requests are paced (Sweeper.Pace), and a rate-limited
// answer to any request stops the sweep, which then sends GitHub nothing until the time GitHub
// gave, at least a minute doubled for every limit since a sweep last finished clear. That backoff
// is the sweep's own scope (redeliveryScope) in the GitHub rate limits every Dispatch process
// shares (githubapp/ratelimit), judged by the database's clock.
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
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
)

const (
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

	// retention is the bucket's TTL, GitHub's own horizon: past three days GitHub neither lists
	// nor redelivers an attempt, so nothing older is worth remembering.
	retention       = 72 * time.Hour
	cursorOverlap   = 2 * time.Minute
	firstBackoff    = 2 * time.Minute
	maxBackoff      = 16 * time.Minute
	defaultRequests = 300
	cursorKey       = "cursor"
	recordPrefix    = "guid."

	// redeliveryScope is the sweep's own scope in the shared GitHub rate limits: its doubling
	// backoff after a rate-limited answer. A sweep checks it together with githubapp.AppScope, the
	// App's own budget, which holds GitHub's wait as given; the doubling stays under this scope, so
	// a secondary limit met on redelivery requests never holds the App's token mints and
	// installation lookups past the wait GitHub gave.
	redeliveryScope = "redelivery"
)

// GitHub is the App webhook delivery API the sweep reads and asks.
type GitHub interface {
	// Deliveries calls before ahead of each page's request and stops at its error.
	Deliveries(ctx context.Context, since time.Time, before func() error) ([]githubapp.Delivery, error)
	Redeliver(ctx context.Context, deliveryID int64) error
}

// OpenState opens the sweep's KV bucket, creating it on first use.
func OpenState(js nats.JetStreamContext) (bus.KeyValue, error) {
	kv, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: bus.WebhookRedeliveryBucket, TTL: retention, Storage: nats.FileStorage, Replicas: 1})
	if err != nil {
		return bus.KeyValue{}, fmt.Errorf("open %s KV bucket: %w", bus.WebhookRedeliveryBucket, err)
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
	// Pending: GitHub accepted the last request, a sweeper recorded that answer, and GitHub's log
	// holds no attempt since: it has not made the redelivery yet. Once it has, the listing carries
	// that newer attempt, which decides it. A claim whose answer was never recorded is not pending:
	// it is ClaimedElsewhere until claimDeadline, and then the delivery is asked for again.
	Pending Outcome = "pending"
	// Terminal: the receiver answered 4xx, refusing the request itself; the same bytes would fail
	// the same way, so it is never redelivered.
	Terminal Outcome = "terminal"
	// Exhausted: MaxAttempts redeliveries failed, or MaxAttempts requests were refused.
	Exhausted Outcome = "exhausted"
	// Closed: an earlier sweep settled the delivery (GitHub recorded it OK, or it was terminal or
	// exhausted). Its failed attempt is still listed, and counted, until it is older than the window.
	Closed Outcome = "closed"
	// ClaimedElsewhere: another sweeper claimed the delivery first, or holds a claim younger than
	// claimDeadline: its request to GitHub may still be in flight, and it records GitHub's answer
	// when it has one.
	ClaimedElsewhere Outcome = "claimed-elsewhere"
	// Delivered: GitHub recorded an OK attempt under the delivery's GUID, so it is done whatever
	// its failures, and its record is closed.
	Delivered Outcome = "delivered"
	// RateLimited: GitHub answered the redelivery request over a rate limit. Nothing is counted
	// against the delivery; it is asked again once the limit has passed.
	RateLimited Outcome = "rate-limited"
)

// Outcomes is every outcome, in the order a sweep's summary counts them.
var Outcomes = []Outcome{
	Redelivered, WouldRedeliver, RequestRefused, RateLimited, Waiting, Pending, Delivered, Terminal,
	Exhausted, Closed, ClaimedElsewhere,
}

// logKey is the outcome's name as a log attribute key: request-refused is request_refused.
func (o Outcome) logKey() string { return strings.ReplaceAll(string(o), "-", "_") }

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

// Report is one sweep's listing and decisions: the listed deliveries newest first, then the
// deliveries the listing no longer returned that their records still owe a request: refused, or
// claimed by a sweeper that never recorded GitHub's answer.
type Report struct {
	Since time.Time
	// Listed counts the failed attempts the listing returned.
	Listed    int
	Decisions []Decision
	// Complete is false when the sweep stopped before deciding every delivery: at its request
	// cap, or at a rate limit.
	Complete bool
	// RateLimitedUntil is set when GitHub rate-limited the App: the sweep stopped, and no sweep
	// sends GitHub anything before it.
	RateLimitedUntil time.Time
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
	State  bus.KeyValue
	// Limits is the GitHub rate limits every process shares (github_rate_limits): a sweep reads
	// its own backoff (redeliveryScope) and the App's limit before every request, and strikes its
	// backoff at a rate-limited answer. Nil shares none: the sweep stops at GitHub's answer and
	// nothing holds the next sweep (tests that are not about limits).
	Limits *ratelimit.Store
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Pace is the pause between redelivery requests. GitHub asks for a second between POSTs
	// ("Best practices for using the REST API"), under a secondary limit of 900 points a minute
	// per endpoint at 5 points a POST.
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
	// Delivered is GitHub having recorded an OK attempt for the delivery: nothing more is asked.
	Delivered bool `json:"delivered,omitempty"`
	// Delivery is the attempt the last request, or the claim in flight, named. A refused request
	// adds no attempt to GitHub's log, so this is how a sweep asks again for a delivery its listing
	// no longer returns.
	Delivery githubapp.Delivery `json:"delivery"`
	// InFlight is a claim: a sweeper is asking GitHub for the delivery and has not recorded the
	// answer. The claim is the only write before the request, so a sweeper that dies before its
	// outcome write leaves the record as it was but for this, ClaimedAt and Delivery.
	InFlight bool `json:"in_flight,omitempty"`
	// ClaimedAt is when the claim was written, on the claiming sweeper's clock. The claim stands
	// until claimDeadline after it.
	ClaimedAt time.Time `json:"claimed_at,omitzero"`
}

// claimDeadline is how long a claim stands: three times githubapp.RequestTimeout, the longest one
// redelivery request takes before its client gives up on it, so a live claimant has GitHub's answer
// and writes it well before another sweeper takes the delivery from it. A claim older than that is
// void: its sweeper died between the claim and the outcome write, and the delivery is decided
// again. One sweeper's clock writes ClaimedAt and another's reads it; a minute is far above the
// clock skew between nodes.
func claimDeadline() time.Duration { return 3 * githubapp.RequestTimeout }

type cursorRecord struct {
	At time.Time `json:"at"`
}

// webhookRedeliveryLease names the lease one sweep of Run's loop runs under. It is not what makes
// the sweep safe to run twice: every redelivery is already claimed by a compare-and-set on the
// delivery's record in the sweep's NATS KV bucket, so two processes sweeping at once ask GitHub
// for no delivery twice. It is the GitHub rate limit: each sweep lists every failed attempt in
// its window, page by page, against one installation's limit, and N processes sweeping is N
// times those list calls for one result.
const webhookRedeliveryLease = "webhook-redelivery"

// Run sweeps now and then every interval until ctx ends, logging each sweep's counts: the failed
// attempts listed, the deliveries they belong to, and how many deliveries had each outcome. Each
// sweep runs under webhookRedeliveryLease for one interval, so one sweep's listings are what the
// installation's rate limit pays for that interval however many processes sweep - a process that
// starts in the middle of another's interval, which is what a rolling update makes, finds that
// interval swept and skips. The period is interval itself, the loop's own tick, and not the
// package's Interval. leases is required, as it is of every leased loop (cmd/dispatch exits at
// boot if the lease pool cannot be opened).
func (s *Sweeper) Run(ctx context.Context, interval time.Duration, leases duty.Leases) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		duty.Exclusive(ctx, leases, webhookRedeliveryLease, interval, s.sweepAndReport)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepAndReport is one sweep of the running loop and its log line.
func (s *Sweeper) sweepAndReport(ctx context.Context) {
	report, err := s.Sweep(ctx, Options{})
	if err != nil {
		s.Logger.Warn("webhook redelivery sweep failed", "error", err)
		return
	}
	attrs := []any{
		"since", report.Since.Format(time.RFC3339),
		"failed_attempts", report.Listed,
		"deliveries", len(report.Decisions),
	}
	for _, outcome := range Outcomes {
		attrs = append(attrs, outcome.logKey(), report.Count(outcome))
	}
	attrs = append(attrs, "complete", report.Complete)
	if !report.RateLimitedUntil.IsZero() {
		attrs = append(attrs, "rate_limited_until", report.RateLimitedUntil.Format(time.RFC3339))
	}
	s.Logger.Info("webhook redelivery sweep", attrs...)
}

// Sweep lists the failed deliveries in its window, adds the deliveries the listing no longer
// returns whose records still owe a request (refusedUnlisted), and decides each: redeliver it, wait
// out its backoff or another sweeper's claim, leave a pending redelivery alone, or give up on it.
// A rate-limited answer from GitHub ends the sweep there.
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
	report := Report{Since: since, Complete: true}
	read, recorded, err := s.readBackoff(ctx)
	if err != nil {
		return Report{}, err
	}
	// The limits are read again ahead of every request, the listing's first page included, so a
	// limit in force now stops the sweep before it asks GitHub anything.
	listed, err := s.GitHub.Deliveries(ctx, since, func() error { return s.limitHeld(ctx) })
	if s.stoppedAtLimit(ctx, &report, err, opts.DryRun) {
		return report, nil
	}
	if err != nil {
		return Report{}, fmt.Errorf("list webhook deliveries since %s: %w", since.Format(time.RFC3339), err)
	}
	byGUID := map[string][]githubapp.Delivery{}
	delivered := map[string]bool{}
	var order []string
	for _, delivery := range listed {
		if delivery.Status == "OK" {
			delivered[delivery.GUID] = true
			continue
		}
		report.Listed++
		if _, seen := byGUID[delivery.GUID]; !seen {
			order = append(order, delivery.GUID)
		}
		byGUID[delivery.GUID] = append(byGUID[delivery.GUID], delivery)
	}
	refused, err := s.refusedUnlisted(byGUID)
	if err != nil {
		return Report{}, err
	}
	for _, delivery := range refused {
		order = append(order, delivery.GUID)
		byGUID[delivery.GUID] = []githubapp.Delivery{delivery}
	}

	maxRequests := s.MaxRequests
	if maxRequests <= 0 {
		maxRequests = defaultRequests
	}
	requests := 0
	for _, guid := range order {
		if requests >= maxRequests {
			report.Complete = false
			break
		}
		decision, requested, err := s.decide(ctx, guid, byGUID[guid], delivered[guid], now, opts.DryRun)
		if s.stoppedAtLimit(ctx, &report, err, opts.DryRun) {
			report.Decisions = append(report.Decisions, decision)
			return report, nil
		}
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
	if recorded && !opts.DryRun {
		s.clearBackoff(ctx, read)
	}
	if continuous && report.Complete && !opts.DryRun {
		s.writeCursor(now, cursorRevision)
	}
	return report, nil
}

// stoppedAtLimit reports whether err ends the sweep at a rate limit, and marks the report so:
// GitHub's own rate-limited answer, which strikes the sweep's backoff (rateLimited), or a limit
// already recorded, found right before a request.
func (s *Sweeper) stoppedAtLimit(ctx context.Context, report *Report, err error, dryRun bool) bool {
	var held *heldLimit
	limited, isLimit := githubapp.AsRateLimit(err)
	switch {
	case errors.As(err, &held):
		report.Complete, report.RateLimitedUntil = false, held.until
	case isLimit && limited.Held:
		// The App's client found a limit recorded after this sweep's own read; GitHub was not asked.
		report.Complete, report.RateLimitedUntil = false, s.now().Add(limited.Wait)
	case isLimit:
		s.rateLimited(ctx, report, limited, dryRun)
	default:
		return false
	}
	return true
}

// rateLimited ends a sweep at a rate-limited answer. It strikes the sweep's backoff: the wait is
// GitHub's, and at least a minute doubled for each limit since a sweep last finished clear of one
// (up to 64 minutes), as GitHub asks of a request that keeps failing on a secondary limit. A dry
// run records nothing, and reports the limit it would have recorded.
func (s *Sweeper) rateLimited(ctx context.Context, report *Report, limited *githubapp.RateLimitError, dryRun bool) {
	next := ratelimit.Limit{Until: s.now().Add(max(limited.Wait, time.Minute)), Strikes: 1}
	if s.Limits != nil {
		strike := s.Limits.Strike
		if dryRun {
			strike = s.Limits.PreviewStrike
		}
		struck, err := strike(ctx, redeliveryScope, limited.Wait)
		if err != nil {
			s.Logger.Error("webhook redelivery rate limit not recorded; the next sweep may ask GitHub before it passes", "until", next.Until.Format(time.RFC3339), "error", err)
		} else {
			next = struck
		}
	}
	report.Complete, report.RateLimitedUntil = false, next.Until
	s.Logger.Warn("webhook redelivery rate-limited by GitHub", "status", limited.Status, "until", next.Until.Format(time.RFC3339), "strikes", next.Strikes, "error", limited.Error())
}

// refusedUnlisted returns the deliveries, newest first, whose failed attempts the listing did not
// return and whose record still owes a request: GitHub refused the last one, or a sweeper claimed
// the delivery and has not recorded GitHub's answer. A refused request adds no attempt to GitHub's
// log, and a sweeper that died before its request made none, so once a delivery's failed attempts
// are older than the window, its record is all that brings it back: to be asked again, or closed
// when the listing shows GitHub delivered it. A claim inside its deadline comes back as
// ClaimedElsewhere.
func (s *Sweeper) refusedUnlisted(listed map[string][]githubapp.Delivery) ([]githubapp.Delivery, error) {
	watcher, err := s.State.Watch(recordPrefix+"*", nats.IgnoreDeletes())
	if err != nil {
		return nil, fmt.Errorf("list the redelivery records: %w", err)
	}
	defer watcher.Stop()
	var refused []githubapp.Delivery
	for entry := range watcher.Updates() {
		if entry == nil {
			break
		}
		guid := strings.TrimPrefix(entry.Key(), recordPrefix)
		if _, seen := listed[guid]; seen {
			continue
		}
		var current record
		if err := json.Unmarshal(entry.Value(), &current); err != nil {
			return nil, fmt.Errorf("decode the redelivery record of %s: %w", guid, err)
		}
		if (current.Refusals == 0 && !current.InFlight) || current.Terminal || current.Exhausted || current.Delivered {
			continue
		}
		if current.Delivery.GUID != guid {
			return nil, fmt.Errorf("the redelivery record of %s owes a request (%d refused, in flight %t) and names delivery %q", guid, current.Refusals, current.InFlight, current.Delivery.GUID)
		}
		refused = append(refused, current.Delivery)
	}
	slices.SortFunc(refused, func(a, b githubapp.Delivery) int { return b.DeliveredAt.Compare(a.DeliveredAt) })
	return refused, nil
}

// decide settles one delivery from its failed attempts (newest first), whether GitHub recorded an
// OK attempt for it, and its record. requested reports that a redelivery request was made,
// accepted or not.
//
// A request is made under a claim: one compare-and-set write that marks the record in flight and
// changes nothing else, then the request, then one write over the claim's revision that records
// GitHub's answer and ends the claim. A record in flight is another sweeper's while the claim is
// younger than claimDeadline, and void after it, when the delivery is decided from the record as
// it was before the claim.
func (s *Sweeper) decide(ctx context.Context, guid string, attempts []githubapp.Delivery, delivered bool, now time.Time, dryRun bool) (Decision, bool, error) {
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
	if current.Terminal || current.Exhausted || current.Delivered {
		decision.Outcome = Closed
		return decision, false, nil
	}
	if current.InFlight {
		if s.now().Before(current.ClaimedAt.Add(claimDeadline())) {
			decision.Outcome = ClaimedElsewhere
			return decision, false, nil
		}
		// The claimant died between its claim and its outcome write. The claim wrote nothing but
		// itself and Delivery, so this is the record the claimant decided to ask from, and the
		// delivery is decided, and asked for, again.
		current.InFlight, current.ClaimedAt = false, time.Time{}
	}
	if delivered {
		// GitHub's own test, whatever the failures and whatever a request's answer said: an
		// accepted request whose 202 was lost left a refusal here.
		decision.Outcome = Delivered
		if revision != 0 && !dryRun {
			next := current
			next.Delivered = true
			s.writeRecord(guid, next, revision)
		}
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
	claim.InFlight = true
	claim.ClaimedAt = s.now()
	claim.Delivery = newest
	claimRevision, claimed := s.claimRecord(guid, claim, revision)
	if !claimed {
		decision.Outcome = ClaimedElsewhere
		return decision, false, nil
	}
	// Another process may have recorded a limit since this sweep read them; nothing is sent under it.
	if err := s.limitHeld(ctx); err != nil {
		s.undoClaim(guid, current, claimRevision, claim.ClaimedAt, decision)
		decision.Outcome = RateLimited
		return decision, false, err
	}
	err = s.GitHub.Redeliver(ctx, newest.ID)
	var limited *githubapp.RateLimitError
	if errors.As(err, &limited) {
		// GitHub refused to take the request at all.
		s.undoClaim(guid, current, claimRevision, claim.ClaimedAt, decision)
		decision.Outcome = RateLimited
		return decision, true, err
	}
	answered := current
	answered.LastAttemptAt = now
	answered.Delivery = newest
	for _, attempt := range attempts {
		if !slices.Contains(answered.Answered, attempt.ID) {
			answered.Answered = append(answered.Answered, attempt.ID)
		}
	}
	if err != nil {
		answered.Refusals++
		decision.Outcome = RequestRefused
		s.recordOutcome(guid, answered, claimRevision, claim.ClaimedAt, decision)
		s.Logger.Warn("webhook redelivery request refused", append(s.attrs(decision), "error", err.Error())...)
		return decision, true, nil
	}
	answered.Attempts++
	answered.Refusals = 0
	decision.Attempts = answered.Attempts
	decision.Outcome = Redelivered
	s.recordOutcome(guid, answered, claimRevision, claim.ClaimedAt, decision)
	s.Logger.Info("webhook redelivered", append(s.attrs(decision), "attempt", answered.Attempts)...)
	return decision, true, nil
}

// recordOutcome writes GitHub's answer to a request over the claim made at claimedAt, ending it.
// A write that fails leaves the claim standing until it expires, when a sweep decides the delivery
// again from GitHub's log: an OK attempt closes it, and anything else asks GitHub again.
func (s *Sweeper) recordOutcome(guid string, answered record, claimRevision uint64, claimedAt time.Time, decision Decision) {
	if !s.writeRecord(guid, answered, claimRevision) {
		s.Logger.Error("webhook redelivery outcome not recorded; the delivery is decided again once its claim expires",
			append(s.attrs(decision), "outcome", string(decision.Outcome), "claim_expires_at", claimedAt.Add(claimDeadline()).Format(time.RFC3339))...)
	}
}

// undoClaim puts a delivery's record back as it was before this sweep claimed it, when the
// request was never taken: nothing counts against the delivery.
func (s *Sweeper) undoClaim(guid string, before record, claimRevision uint64, claimedAt time.Time, decision Decision) {
	if !s.writeRecord(guid, before, claimRevision) {
		s.Logger.Error("webhook redelivery claim not undone after a rate limit; the delivery is decided again once its claim expires",
			append(s.attrs(decision), "claim_expires_at", claimedAt.Add(claimDeadline()).Format(time.RFC3339))...)
	}
}

// heldLimit is a limit already recorded in the shared GitHub rate limits, found right before a
// request: the sweep's own backoff or the App's limit, whichever ends last.
type heldLimit struct{ until time.Time }

func (e *heldLimit) Error() string {
	return "GitHub rate-limited the App until " + e.until.Format(time.RFC3339)
}

// limitHeld reads the shared limits right before a request to GitHub, since another sweep or
// process may have recorded one after this sweep started. It is a *heldLimit while the sweep's
// backoff or the App's limit is in force by the database's clock. A request already sent when
// another process records a limit is not recalled.
func (s *Sweeper) limitHeld(ctx context.Context) error {
	if s.Limits == nil {
		return nil
	}
	limit, found, err := s.Limits.InForce(ctx, redeliveryScope, githubapp.AppScope)
	if err != nil {
		return err
	}
	if found {
		return &heldLimit{until: limit.Until}
	}
	return nil
}

// readBackoff returns the sweep's backoff row as it stands, in force or passed; recorded is false
// when there is none.
func (s *Sweeper) readBackoff(ctx context.Context) (backoff ratelimit.Limit, recorded bool, err error) {
	if s.Limits == nil {
		return ratelimit.Limit{}, false, nil
	}
	return s.Limits.Get(ctx, redeliveryScope)
}

// clearBackoff forgets the backoff a sweep read once the sweep has finished clear of it, so the
// next limit starts again at one minute; a strike another sweep recorded meanwhile stands.
func (s *Sweeper) clearBackoff(ctx context.Context, backoff ratelimit.Limit) {
	if err := s.Limits.Clear(ctx, backoff); err != nil {
		s.Logger.Warn("webhook redelivery rate limit not cleared", "error", err)
	}
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

// readState reads key's JSON value and its revision; an absent key is the zero value at revision
// zero. what names the value in an error.
func readState[T any](kv bus.KeyValue, key, what string) (T, uint64, error) {
	var value T
	entry, err := kv.Get(key)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return value, 0, nil
	}
	if err != nil {
		return value, 0, fmt.Errorf("read the redelivery %s: %w", what, err)
	}
	if err := json.Unmarshal(entry.Value(), &value); err != nil {
		return value, 0, fmt.Errorf("decode the redelivery %s: %w", what, err)
	}
	return value, entry.Revision(), nil
}

// writeState writes value's JSON at key only over revision (absent when zero), so a lost race is
// a conflict (isConflict), and returns the new revision.
func writeState(kv bus.KeyValue, key string, value any, revision uint64) (uint64, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	if revision == 0 {
		return kv.Create(key, data)
	}
	return kv.Update(key, data, revision)
}

func (s *Sweeper) readCursor() (time.Time, uint64, error) {
	cursor, revision, err := readState[cursorRecord](s.State, cursorKey, "cursor")
	return cursor.At, revision, err
}

// writeCursor records the sweep's start. A lost race means another sweeper wrote a cursor at
// the same moment; either value serves.
func (s *Sweeper) writeCursor(at time.Time, revision uint64) {
	if _, err := writeState(s.State, cursorKey, cursorRecord{At: at}, revision); err != nil && !isConflict(err) {
		s.Logger.Warn("webhook redelivery cursor not written", "error", err)
	}
}

func (s *Sweeper) readRecord(guid string) (record, uint64, error) {
	return readState[record](s.State, recordPrefix+guid, "record of "+guid)
}

// claimRecord writes next only if the record is still at revision (absent when zero), so two
// sweepers never both request one attempt. It returns the new revision.
func (s *Sweeper) claimRecord(guid string, next record, revision uint64) (uint64, bool) {
	written, err := writeState(s.State, recordPrefix+guid, next, revision)
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
