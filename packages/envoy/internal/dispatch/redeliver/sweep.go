// Package redeliver asks GitHub to redeliver the App webhook's failed deliveries.
//
// GitHub does not redeliver a webhook delivery the receiver failed: a delivery the Envoy
// listener did not answer 2xx (starting up, a CI-store write that ran out of retries, a publish
// NATS refused) or that GitHub could not complete (the listener down, a reply slower than ten
// seconds) is lost to every consumer unless someone asks for it again. The sweep is GitHub's
// documented remedy: list the App webhook's failed attempts, and redeliver each delivery that has
// no answered attempt, through `POST /app/hook/deliveries/{id}/attempts`. A redelivery carries the
// original X-GitHub-Delivery GUID, and the listener publishes GitHub envelopes under a JetStream
// MsgId of that GUID's dedupe key, so asking again for a delivery that did reach the stream adds
// nothing to it.
//
// A delivery is asked for again, on a doubling backoff, whatever status code its failed attempts
// carry, until GitHub records an OK attempt for it or 72 hours have passed since the first failed
// attempt the sweep saw: GitHub keeps a delivery 72 hours, and a 404 to the request says it no
// longer has it.
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
// The running sweep reads the deliveries it must decide from an index of its records, keyed by the
// minute each next needs a sweep (index.go), rather than from every record the bucket holds, so
// what one sweep costs follows what is due and not what three days of deliveries left behind. Two
// things an operator reads change with it. The summary line's deliveries= counts the listing's
// deliveries and the due ones, where it counted every open delivery the listing no longer returned,
// so one waiting out its backoff outside the listing's window is no longer among them. Waiting is
// still counted for a delivery the listing does return before its next request is due, as it always
// was.
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
	"github.com/sjawhar/envoy/internal/kvwatch"
)

const (
	// Interval is how often the running sweep lists failures.
	Interval = 2 * time.Minute
	// Lookback is how far back every continuous sweep with a cursor lists. Failures older than
	// it are decided from their records, which the index brings back when they are due (openDue).
	// A first sweep, with no cursor, lists the 72 hours GitHub keeps, so the first failed attempt
	// of every delivery GitHub still has is in its listing.
	//
	// A record must be read while GitHub's answer to its last request is still in the listing's
	// window, or a delivery GitHub did deliver is asked for again and again until it expires: the
	// index brings a record back when it is due and not before, and a sweep whose listing no
	// longer carries that OK attempt reads the delivery as one still failing. So the window has to
	// outlast the longest there is between a request and the sweep that judges it. That is
	// maxBackoff, the longest a record ever waits between requests, plus the sweeps it takes for a
	// due entry to be read: an entry is keyed to its minute, the loop's tick is Interval with up
	// to a tenth of it again in jitter, and a sweep that reaches a record a moment early leaves it
	// to the next one. Three intervals cover those.
	Lookback = maxBackoff + 3*Interval

	// retention is the bucket's TTL, GitHub's own horizon: past three days GitHub neither lists
	// nor redelivers an attempt, so nothing older is worth remembering, and a delivery still
	// failing that long after its first failed attempt is expired.
	retention = 72 * time.Hour
	// cursorOverlap is how far before its cursor a sweep starts, and it covers two things. One is
	// the clock skew between sweepers, the margin claimDeadline's comment calls far above it. The
	// other is the most an entry nothing has acted on can fall behind the cursor of the sweep that
	// wrote it, which is under a minute: an entry is only ever keyed at the minute of a moment at
	// or after its own write (pendingFor).
	cursorOverlap   = 2 * time.Minute
	firstBackoff    = 2 * time.Minute
	maxBackoff      = time.Hour
	defaultRequests = 300
	cursorKey       = "cursor"
	recordPrefix    = "guid."
	// indexKey marks the bucket as one whose open records all have an index entry, so the
	// back-fill a build that writes entries runs over a bucket an older build filled happens once
	// (backfillIndex) rather than every two minutes.
	indexKey = "index"

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

// OpenState opens the sweep's KV bucket, creating it on first use with replicas copies, the
// deployment's ENVOY_NATS_REPLICAS, and raising a bucket found with fewer (bus.EnsureKeyValue).
func OpenState(js nats.JetStreamContext, replicas int) (bus.KeyValue, error) {
	kv, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: bus.WebhookRedeliveryBucket, TTL: retention, Storage: nats.FileStorage, Replicas: replicas})
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
	// Waiting: the delivery has had a request and its next one is not due yet.
	Waiting Outcome = "waiting"
	// Expired: GitHub no longer has the delivery (72 hours since its first failed attempt, or a
	// 404 to the request); it is never asked for again.
	Expired Outcome = "expired"
	// Closed: an earlier sweep settled the delivery (GitHub recorded it OK, or it expired). Its
	// failed attempt is still listed, and counted, until it is older than the window.
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
	Redelivered, WouldRedeliver, RequestRefused, RateLimited, Waiting, Delivered, Expired, Closed,
	ClaimedElsewhere,
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

// Report is one sweep's listing and decisions: the listed deliveries newest first, then the open
// deliveries the listing no longer returned and the index says are due: still owed a request, or
// claimed by a sweeper that never recorded GitHub's answer.
type Report struct {
	Since time.Time
	// Listed counts the failed attempts the listing returned.
	Listed int
	// IndexEntries counts the entries the running sweep's index watch delivered, which is the work
	// that window cost it; a sweep given a window of its own (Options.Since) reads no index and
	// counts none.
	IndexEntries int
	Decisions    []Decision
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
	// Lookback, reaching back to the stored cursor after a gap, or the last 72 hours with no
	// cursor, and a complete sweep advances the cursor. A sweep with Since set leaves the cursor
	// alone.
	Since time.Time
	// DryRun decides every delivery and requests nothing, writes nothing and logs no alarm line.
	DryRun bool
}

// Sweeper lists the App webhook's failed deliveries and redelivers them.
type Sweeper struct {
	GitHub GitHub
	// Open opens the sweep's state bucket (OpenState). A sweep calls it until it has once
	// succeeded, and every later sweep uses that bucket: a sweep it fails for returns its error
	// and asks GitHub nothing, so a sweeper made while NATS is down opens its bucket on the first
	// sweep that finds NATS connected.
	Open func() (bus.KeyValue, error)
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

	// state is the bucket Open opened, the zero KeyValue until a sweep has opened it (openState).
	// Every read and write of the sweep's state goes to it.
	state bus.KeyValue
}

// record is the sweep's memory of one delivery, keyed by its GUID.
type record struct {
	// Attempts counts redeliveries GitHub accepted.
	Attempts int `json:"attempts"`
	// Refusals counts redelivery requests GitHub refused. Attempts and Refusals add up to the
	// requests made for the delivery.
	Refusals int `json:"refusals"`
	// LastAttemptAt is when the last request was made, accepted or refused.
	LastAttemptAt time.Time `json:"last_attempt_at,omitzero"`
	// NextAttemptAt is when the next request is due: the last request plus the backoff. A record
	// without it is due.
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
	// FirstDeliveredAt is the oldest failed attempt the sweep has seen for the delivery, kept as
	// the minimum on every write; the delivery expires 72 hours after it.
	FirstDeliveredAt time.Time `json:"first_delivered_at,omitzero"`
	// Delivered is GitHub having recorded an OK attempt for the delivery: nothing more is asked.
	Delivered bool `json:"delivered,omitempty"`
	// Expired is the sweep having stopped asking for an undelivered delivery, for ExpiredReason.
	Expired       bool   `json:"expired,omitempty"`
	ExpiredReason string `json:"expired_reason,omitempty"`
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
	// Pending is the index entry this record's last writer wrote (pendingFor), empty once the
	// record is closed and in a record an older build wrote, which had no index to write to. An
	// entry some other record names, or one no record names at all, is a stray a sweep that reads
	// it deletes, so this is what makes that repair exact.
	Pending string `json:"pending,omitempty"`
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

// jittered is the wait Run leaves after each sweep (duty.Jittered). It is a variable only so a test
// can fix the draw.
var jittered = duty.Jittered

// Run sweeps now and then every interval until ctx ends, logging each sweep's counts: the failed
// attempts listed, the deliveries they belong to, and how many deliveries had each outcome. After
// each sweep it waits the interval plus up to a tenth of it again (duty.Jittered), drawn afresh
// every period, so two processes started together drift apart rather than reaching the lease in
// step every interval for the life of both. The first sweep is not delayed: a process that starts
// sweeps at once, as it always has.
//
// Each sweep runs under webhookRedeliveryLease for one interval, so one sweep's listings are what
// the installation's rate limit pays for that interval however many processes sweep - a process
// that starts in the middle of another's interval, which is what a rolling update makes, finds that
// interval swept and skips. The period is interval itself, the loop's own tick, and not the
// package's Interval. Each sweep's due time, the moment it hands duty.Exclusive and the one its
// period is judged by, is the moment Run started for the first and the moment its timer fired for
// every later one - never the moment the lease was taken, which a slow lease would carry into the
// next period and so cost the sweep after it that period. A timer armed once the sweep before has
// returned, for at least one interval, always reaches the next period, so neither a slow lease nor
// the jitter ever makes a process skip its own next sweep. leases is required, as it is of every
// leased loop (cmd/dispatch exits at boot if the lease pool cannot be opened).
func (s *Sweeper) Run(ctx context.Context, interval time.Duration, leases duty.Leases) {
	due := time.Now()
	for {
		duty.Exclusive(ctx, leases, webhookRedeliveryLease, due, interval, s.sweepAndReport)
		timer := time.NewTimer(jittered(interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case due = <-timer.C:
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
		"index_entries", report.IndexEntries,
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

// Sweep lists the failed deliveries in its window, adds the open deliveries the listing no longer
// returns -- the ones the index says are due in the running sweep (openDue), every one there is in
// a sweep given a window of its own (scanOpenRecords) -- and decides each: redeliver it, wait until
// its next request is due or another sweeper's claim ends, or expire it. A rate-limited answer from
// GitHub ends the sweep there. It opens the state first unless an earlier sweep has (openState).
func (s *Sweeper) Sweep(ctx context.Context, opts Options) (Report, error) {
	if err := s.openState(); err != nil {
		return Report{}, err
	}
	now := s.now()
	continuous := opts.Since.IsZero()
	since := opts.Since
	// floor is where the running sweep's index read starts (openDue), clamped to the oldest the
	// bucket holds: nothing older than retention is worth reading, and a wider window would cost
	// more filters than maxPendingFilters.
	floor := now.Add(-retention)
	var cursorRevision uint64
	if continuous {
		since = now.Add(-Lookback)
		cursor, revision, err := s.readCursor()
		if err != nil {
			return Report{}, err
		}
		cursorRevision = revision
		if revision == 0 {
			since = now.Add(-retention)
		} else {
			if resume := cursor.Add(-cursorOverlap); resume.After(floor) {
				floor = resume
			}
			if floor.Before(since) {
				s.Logger.Warn("webhook redelivery sweep resuming after a gap", "cursor", cursor.Format(time.RFC3339), "since", floor.Format(time.RFC3339))
				since = floor
			}
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
	// The running sweep reads the records the index says are due in its window; a sweep given a
	// window of its own reads every record, as it always has, since an operator asking what the
	// sweep would do with a delivery must be answered for one not due for an hour too.
	var unlisted []openRecord
	if continuous {
		if err := s.backfillIndex(&report, byGUID, now, opts.DryRun); err != nil {
			return Report{}, err
		}
		unlisted, report.IndexEntries, err = s.openDue(byGUID, floor, now, opts.DryRun)
	} else {
		unlisted, err = s.scanOpenRecords(byGUID)
	}
	if err != nil {
		return Report{}, err
	}
	for _, open := range unlisted {
		order = append(order, open.guid)
		byGUID[open.guid] = []githubapp.Delivery{open.record.Delivery}
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
		switch {
		case errors.Is(err, errPendingNotWritten):
			// Nothing was written for this delivery, so it is decided again: the cursor stays
			// where it was, and the rest of the window is still worth deciding now. Its decision
			// settled nothing, and on every path but the closing ones carries no outcome at all,
			// so it is left out of the report rather than counted as a delivery no outcome
			// accounts for.
			report.Complete = false
		case err != nil:
			return report, err
		default:
			report.Decisions = append(report.Decisions, decision)
		}
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

// openRecord is one open delivery a sweep decides from its record rather than from a listing: the
// GUID the record is keyed by, the record, and the revision it was read at.
type openRecord struct {
	guid     string
	record   record
	revision uint64
}

// sortOpenRecords puts the newest delivery first, the order a listing returns them in.
func sortOpenRecords(open []openRecord) {
	slices.SortFunc(open, func(a, b openRecord) int {
		return b.record.Delivery.DeliveredAt.Compare(a.record.Delivery.DeliveredAt)
	})
}

// scanOpenRecords reads every record the bucket holds and returns the deliveries, newest first,
// whose record is open (neither Delivered nor Expired) and whose failed attempts the listing did
// not return. A refused request adds no attempt to GitHub's log, and a failed attempt leaves the
// window long before 72 hours have passed, so once a delivery's failed attempts are older than the
// window, its record is all that brings it back: to be asked again when it is due, expired, or
// closed when the listing shows GitHub delivered it. A record in flight is a sweeper's claim, which
// made no request yet: it comes back too, as ClaimedElsewhere while the claim stands
// (claimDeadline) and asked for again after it. A record that names no delivery is a claim undone
// before its request was sent; no request was made, and the listing still brings that delivery
// back.
//
// A sweep given a window of its own reads this rather than the index, so `envoy-dispatch
// redeliver-webhooks`, dry run or not, answers exactly as it did before there was one: it reports
// every open delivery, due or not, and pays three days of records for it. The index's back-fill
// reads it too (backfillIndex).
//
// nats.go ends a watch three ways and only one of them is a complete read, so this drains it
// through the one place that tells them apart (kvwatch.DrainExistingKeys). A scan its idle timer
// ended used to read here as a bucket with nothing more in it, which under-reports the open
// deliveries and says nothing.
func (s *Sweeper) scanOpenRecords(listed map[string][]githubapp.Delivery) ([]openRecord, error) {
	watcher, err := s.state.Watch(recordPrefix+"*", nats.IgnoreDeletes())
	if err != nil {
		return nil, fmt.Errorf("list the redelivery records: %w", err)
	}
	var open []openRecord
	var failed error
	if _, err := kvwatch.DrainExistingKeys(watcher, "webhook redelivery records", s.Logger, func(entry nats.KeyValueEntry) {
		if failed != nil {
			return
		}
		guid := strings.TrimPrefix(entry.Key(), recordPrefix)
		if _, seen := listed[guid]; seen {
			return
		}
		var current record
		if err := json.Unmarshal(entry.Value(), &current); err != nil {
			failed = fmt.Errorf("decode the redelivery record of %s: %w", guid, err)
			return
		}
		if current.Delivered || current.Expired || current.Delivery.GUID == "" {
			return
		}
		if current.Delivery.GUID != guid {
			failed = fmt.Errorf("the redelivery record of %s names delivery %q", guid, current.Delivery.GUID)
			return
		}
		open = append(open, openRecord{guid: guid, record: current, revision: entry.Revision()})
	}); err != nil {
		return nil, err
	}
	if failed != nil {
		return nil, failed
	}
	sortOpenRecords(open)
	return open, nil
}

// openDue returns the open deliveries the index says need a sweep in the window from floor to now,
// newest first, and how many entries its watch delivered. One the listing returned is left to the
// listing, which has its attempts, and an entry no record claims is deleted as it is read: a
// sweeper that died between its index write and its record write, or between a record write and the
// delete of the entry before it, leaves one stray entry, and the sweep that reads it repairs it. A
// dry run repairs nothing, since it writes nothing at all: it leaves the delivery out of what it
// returns, the record naming no entry of its own, and leaves the entry itself to the next sweep
// that acts.
//
// A floor is safe because of what a complete sweep leaves behind. After one that started at T,
// every entry still in the bucket names a record whose attention time is at or after T: every entry
// that sweep selected was either acted on, which moved the entry forward, or left in place with an
// entry already at or after T. The floor is taken down to its own minute because an entry is keyed
// by the minute: one due at 10:00:30 is keyed 10:00, and a sweep that started at 10:00:10 and found
// it not yet due must still be able to select it afterwards.
//
// The completeness check matters more here than anywhere else in the sweep. nats.go ends a watch
// three ways and only one is a complete read (kvwatch.DrainExistingKeys); a sweep that read some of
// its due entries and called itself complete would advance its cursor past the rest, and nothing
// would ever bring those deliveries back. That is the one way this design could lose a delivery.
func (s *Sweeper) openDue(listed map[string][]githubapp.Delivery, floor, now time.Time, dryRun bool) ([]openRecord, int, error) {
	watcher, err := s.state.WatchFiltered(pendingFilters(floor, now), nats.MetaOnly(), nats.IgnoreDeletes())
	if err != nil {
		return nil, 0, fmt.Errorf("list the due redelivery index entries: %w", err)
	}
	// An entry's value is empty and its record is read by its own key, so the watch asks for no
	// value, and the keys are taken first: a Get for each inside the watch's loop would hold the
	// watch open for as long as the records take to read.
	var keys []string
	scan, err := kvwatch.DrainExistingKeys(watcher, "webhook redelivery index", s.Logger, func(entry nats.KeyValueEntry) {
		keys = append(keys, entry.Key())
	})
	if err != nil {
		return nil, 0, err
	}
	var open []openRecord
	seen := map[string]bool{}
	for _, key := range keys {
		_, guid, ok := parsePendingKey(key)
		if !ok {
			continue
		}
		current, revision, err := s.readRecord(guid)
		if err != nil {
			return nil, scan.Puts, err
		}
		if revision == 0 || current.Delivered || current.Expired || current.Delivery.GUID == "" || current.Pending != key {
			if !dryRun {
				s.dropPending(key)
			}
			continue
		}
		if seen[guid] {
			continue
		}
		seen[guid] = true
		if _, listedNow := listed[guid]; listedNow {
			continue
		}
		open = append(open, openRecord{guid: guid, record: current, revision: revision})
	}
	sortOpenRecords(open)
	return open, scan.Puts, nil
}

// backfillIndex gives an index entry, and the Pending that names it, to every open record that has
// none, and then marks the bucket indexed. A build that writes entries meets a bucket an older one
// filled with records that have none at all, and an index read would never return them.
//
// The marker lives in the same bucket under its 72-hour TTL, so it expires and this runs again at
// most once every three days: a standing repair rather than a one-off, which is also what covers a
// sweeper that died in the middle of one. A delivery an older build still writes records for is
// listed within Lookback and indexed by the sweep that decides it (decide), so a rolling deploy's
// overlap needs nothing more than that; a rollback needs no rule at all, since an older build
// ignores these entries and derives its open deliveries from the records itself. A dry run writes
// neither the entries nor the marker.
func (s *Sweeper) backfillIndex(report *Report, listed map[string][]githubapp.Delivery, now time.Time, dryRun bool) error {
	if dryRun {
		return nil
	}
	marked, err := s.indexMarked()
	if err != nil || marked {
		return err
	}
	open, err := s.scanOpenRecords(listed)
	if err != nil {
		return err
	}
	indexed := true
	for _, stored := range open {
		if stored.record.Pending != "" {
			continue
		}
		next := stored.record
		if _, _, err := s.writeRecord(stored.guid, &next, "", stored.revision, now); err != nil {
			report.Complete, indexed = false, false
		}
	}
	if !indexed {
		return nil
	}
	if _, err := s.state.Put(indexKey, nil); err != nil {
		s.Logger.Warn("webhook redelivery index not marked back-filled; the next sweep reads every record again", "error", err)
	}
	return nil
}

// decide settles one delivery from its failed attempts (newest first), whether GitHub recorded an
// OK attempt for it, and its record, in this order: closed by an earlier sweep, held by another
// sweeper's claim, delivered, expired 72 hours after its first failed attempt, waiting for its
// next request, or asked for. requested reports that a redelivery request was made, accepted or
// not.
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
	if current.Delivered || current.Expired {
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
	requests := current.Attempts + current.Refusals
	first := firstDeliveredAt(current, attempts)
	if delivered {
		// GitHub's own test, whatever the failures and whatever a request's answer said: an
		// accepted request whose 202 was lost left a refusal here.
		decision.Outcome = Delivered
		if revision != 0 && !dryRun {
			next := current
			next.Delivered = true
			next.FirstDeliveredAt = first
			_, written, err := s.writeRecord(guid, &next, current.Pending, revision, now)
			if err != nil {
				return decision, false, err
			}
			if written && requests > 0 {
				s.Logger.Info("webhook delivery recovered", append(s.attrs(decision), "requests", requests)...)
			}
		}
		return decision, false, nil
	}
	if !now.Before(first.Add(retention)) {
		decision.Outcome = Expired
		if !dryRun {
			next := current
			next.FirstDeliveredAt = first
			if err := s.expire(guid, next, current.Pending, revision, now, decision, "GitHub keeps a delivery 72 hours"); err != nil {
				return decision, false, err
			}
		}
		return decision, false, nil
	}
	if !dryRun && revision != 0 && current.Pending == "" {
		// A record an older build wrote names no index entry, so nothing but a listing would
		// bring its delivery back once its failed attempts leave the window. Writing it here
		// indexes every record of that kind GitHub still lists, which is every one an older build
		// is still writing, within Lookback of a deploy. The write moves the record's revision,
		// and the claim below is made against that one.
		indexed, written, err := s.writeRecord(guid, &current, "", revision, now)
		if err != nil {
			return decision, false, err
		}
		if written {
			revision = indexed
		}
	}
	if now.Before(current.NextAttemptAt) {
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
	claimRevision, claimed, err := s.writeRecord(guid, &claim, current.Pending, revision, now)
	if err != nil {
		return decision, false, err
	}
	if !claimed {
		decision.Outcome = ClaimedElsewhere
		return decision, false, nil
	}
	// Another process may have recorded a limit since this sweep read them; nothing is sent under it.
	if err := s.limitHeld(ctx); err != nil {
		s.undoClaim(guid, current, claim, claimRevision, decision)
		decision.Outcome = RateLimited
		return decision, false, err
	}
	err = s.GitHub.Redeliver(ctx, newest.ID)
	var limited *githubapp.RateLimitError
	if errors.As(err, &limited) {
		// GitHub refused to take the request at all.
		s.undoClaim(guid, current, claim, claimRevision, decision)
		decision.Outcome = RateLimited
		return decision, true, err
	}
	answered := current
	answered.LastAttemptAt = now
	answered.NextAttemptAt = now.Add(backoff(requests + 1))
	answered.FirstDeliveredAt = first
	answered.Delivery = newest
	if err != nil {
		answered.Refusals++
		if errors.Is(err, githubapp.ErrNoDelivery) {
			decision.Outcome = Expired
			if err := s.expire(guid, answered, claim.Pending, claimRevision, now, decision, "GitHub no longer has the delivery"); err != nil {
				return decision, true, err
			}
			return decision, true, nil
		}
		decision.Outcome = RequestRefused
		recorded := s.recordOutcome(guid, answered, claim, claimRevision, now, decision)
		s.Logger.Warn("webhook redelivery request refused", append(s.attrs(decision), "error", err.Error())...)
		return decision, true, recorded
	}
	answered.Attempts++
	decision.Attempts = answered.Attempts
	decision.Outcome = Redelivered
	recorded := s.recordOutcome(guid, answered, claim, claimRevision, now, decision)
	s.Logger.Info("webhook redelivered", append(s.attrs(decision), "attempt", answered.Attempts)...)
	return decision, true, recorded
}

// expire closes a delivery the sweep stops asking for, for reason, and logs it: only the write that
// closes the record logs, so a delivery is logged expired once. A closed record is asked for no
// more, so the write takes its index entry away with it (writeRecord).
func (s *Sweeper) expire(guid string, next record, previous string, revision uint64, now time.Time, decision Decision, reason string) error {
	next.Expired = true
	next.ExpiredReason = reason
	_, written, err := s.writeRecord(guid, &next, previous, revision, now)
	if err != nil {
		return err
	}
	if written {
		s.Logger.Error("webhook delivery expired undelivered", append(s.attrs(decision),
			"requests", next.Attempts+next.Refusals,
			"first_delivered_at", next.FirstDeliveredAt.Format(time.RFC3339),
			"reason", reason)...)
	}
	return nil
}

// firstDeliveredAt is the oldest failed attempt the sweep has seen for a delivery: the record's
// and the attempts listed now, whichever is older. A record written before it kept one takes the
// attempt its last request named.
func firstDeliveredAt(current record, attempts []githubapp.Delivery) time.Time {
	first := current.FirstDeliveredAt
	if first.IsZero() {
		first = current.Delivery.DeliveredAt
	}
	for _, attempt := range attempts {
		if first.IsZero() || attempt.DeliveredAt.Before(first) {
			first = attempt.DeliveredAt
		}
	}
	return first
}

// recordOutcome writes GitHub's answer to a request over the claim, ending it. A write that fails
// leaves the claim standing until it expires, when a sweep decides the delivery again from GitHub's
// log: an OK attempt closes it, and anything else asks GitHub again. The entry this one replaces is
// the claim's, since the claim's own write deleted the entry before it.
func (s *Sweeper) recordOutcome(guid string, answered record, claim record, claimRevision uint64, now time.Time, decision Decision) error {
	_, written, err := s.writeRecord(guid, &answered, claim.Pending, claimRevision, now)
	if err != nil {
		return err
	}
	if !written {
		s.Logger.Error("webhook redelivery outcome not recorded; the delivery is decided again once its claim expires",
			append(s.attrs(decision), "outcome", string(decision.Outcome), "claim_expires_at", claim.ClaimedAt.Add(claimDeadline()).Format(time.RFC3339))...)
	}
	return nil
}

// undoClaim puts a delivery's record back as it was before this sweep claimed it, when the
// request was never taken: nothing counts against the delivery.
//
// It is the one record write that leaves the index alone. The claim's write moved the delivery's
// entry to the claim's own key and deleted the entry before it, so the record put back here keeps
// the claim's key and this deletes nothing: the restored record is due now, and the claim's entry,
// a claimDeadline after the claim, is what the sweep after this one finds it by. Deleting it here
// would leave a delivery nothing looks at again until its failed attempt is listed, which for an
// attempt already older than the window never happens.
func (s *Sweeper) undoClaim(guid string, before record, claim record, claimRevision uint64, decision Decision) {
	before.Pending = claim.Pending
	if _, written := s.putRecord(guid, before, claimRevision); !written {
		s.Logger.Error("webhook redelivery claim not undone after a rate limit; the delivery is decided again once its claim expires",
			append(s.attrs(decision), "claim_expires_at", claim.ClaimedAt.Add(claimDeadline()).Format(time.RFC3339))...)
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

// backoff is the pause before the next request after tries requests: 2, 4, 8, 16, 32, then 60
// minutes.
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

// openState opens the state with Open unless a sweep already has, and keeps it for every sweep
// after: the one place the bucket is opened. A sweep it fails for opens nothing, so the next one
// calls Open again.
func (s *Sweeper) openState() error {
	if s.state.KeyValue != nil {
		return nil
	}
	state, err := s.Open()
	if err != nil {
		return err
	}
	s.state = state
	return nil
}

func (s *Sweeper) readCursor() (time.Time, uint64, error) {
	cursor, revision, err := readState[cursorRecord](s.state, cursorKey, "cursor")
	return cursor.At, revision, err
}

// writeCursor records the sweep's start. A lost race means another sweeper wrote a cursor at
// the same moment; either value serves.
func (s *Sweeper) writeCursor(at time.Time, revision uint64) {
	if _, err := writeState(s.state, cursorKey, cursorRecord{At: at}, revision); err != nil && !isConflict(err) {
		s.Logger.Warn("webhook redelivery cursor not written", "error", err)
	}
}

func (s *Sweeper) readRecord(guid string) (record, uint64, error) {
	return readState[record](s.state, recordPrefix+guid, "record of "+guid)
}

// putRecord writes next only if the record is still at revision (absent when zero), so two
// sweepers never both request one attempt, and returns the new revision. It is the record write on
// its own; every caller but undoClaim goes through writeRecord, which keeps the index with it.
func (s *Sweeper) putRecord(guid string, next record, revision uint64) (uint64, bool) {
	written, err := writeState(s.state, recordPrefix+guid, next, revision)
	if err != nil {
		if !isConflict(err) {
			s.Logger.Warn("webhook redelivery record not written", "guid", guid, "error", err)
		}
		return 0, false
	}
	return written, true
}

// errPendingNotWritten is a write whose index entry the bucket refused. The record was not written
// either, and the sweep that met it is incomplete (Sweep).
var errPendingNotWritten = errors.New("webhook redelivery index entry not written")

// writeRecord writes next over revision and keeps the delivery's index entry with it, which is why
// every record write but undoClaim's comes through here: the order lives in one place. previous is
// the entry the record named before this write (record.Pending), and next.Pending is left as the
// entry this one wrote.
//
// The order is the new entry, then the record, then the entry the record named before it. Entries
// are written with Put and Delete and never Create or Update, so two sweepers writing one entry is
// nothing to settle, and the first Create against a delivery is still its claim.
//
//   - An entry that cannot be written stops the write there and returns errPendingNotWritten. The
//     record is left as it was, the sweep is marked incomplete, its cursor stays where it was and
//     the delivery is decided again by the sweep after it. That is what keeps a lost index write
//     from turning into a lost delivery, and why nothing here reads the whole bucket from time to
//     time looking for records the index forgot.
//   - A record write that loses its compare-and-set is written false and no error: another sweeper
//     decided the delivery first, and the entry this one wrote is a stray the sweep that reads it
//     deletes (openDue).
//   - A delete that fails is logged and no more: what it leaves is that same stray.
func (s *Sweeper) writeRecord(guid string, next *record, previous string, revision uint64, now time.Time) (uint64, bool, error) {
	want := pendingFor(*next, guid, now)
	next.Pending = want
	if want != "" && want != previous {
		if _, err := s.state.Put(want, nil); err != nil {
			s.Logger.Error("webhook redelivery index entry not written; the delivery is decided again by the next sweep",
				"guid", guid, "key", want, "error", err)
			return 0, false, fmt.Errorf("%w: %s: %w", errPendingNotWritten, guid, err)
		}
	}
	written, ok := s.putRecord(guid, *next, revision)
	if !ok {
		return 0, false, nil
	}
	if previous != "" && previous != want {
		s.dropPending(previous)
	}
	return written, true, nil
}

// dropPending deletes an index entry no record names any more: the record it named is closed, gone,
// or indexed elsewhere, so nothing is owed a sweep by it. A delete that fails leaves an entry a
// later sweep reads once more and repairs then, so it is logged and not raised.
func (s *Sweeper) dropPending(key string) {
	if err := s.state.Delete(key); err != nil {
		s.Logger.Warn("webhook redelivery index entry not deleted", "key", key, "error", err)
	}
}

// indexMarked reports whether a sweep has back-filled the index since the marker last expired
// (backfillIndex).
func (s *Sweeper) indexMarked() (bool, error) {
	_, err := s.state.Get(indexKey)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the redelivery index marker: %w", err)
	}
	return true, nil
}

func isConflict(err error) bool {
	return errors.Is(err, nats.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence")
}
