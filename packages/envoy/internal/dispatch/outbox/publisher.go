// Package outbox publishes durable Dispatch events to NATS.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/roles"
)

const (
	// batchSize is the most events a batch sends, and the most it reads of one owner.
	batchSize       = 100
	compactInterval = 24 * time.Hour
	// dispatchTopicPrefix is the prefix of every owner's topic family: an issue's, a document's
	// and a project's events are published under it.
	dispatchTopicPrefix = "notifications.dispatch."
	documentTopicPrefix = dispatchTopicPrefix + "document."
	// maxCommentThreadDepth bounds the reply_to walk in loadCommentAuthors so a malformed cycle
	// (comments.reply_to has no acyclicity constraint) cannot spin the recursive query and stall
	// the outbox scan.
	maxCommentThreadDepth = 32
	// recordTimeout bounds the write of a batch's outcome (recordBatch), which a stopping process
	// still makes after its context has ended: one statement, which a healthy database answers in
	// milliseconds, so a write that takes longer is left undone, and the batch is read again.
	recordTimeout = 5 * time.Second
)

// Publisher is the NATS client the outbox publishes through (bus.Client).
type Publisher interface {
	// Window starts one batch's sends: each is sent without waiting for NATS's answer, all on one
	// connection or not at all, in no order of its own (bus.Client.Window); sendChains keeps the
	// order per owner and destination.
	Window() bus.Sender
	// Connected reports whether the client has a live connection now. A standby takes the
	// publisher hold only while it does, and a holder whose client has not for natsLossGrace
	// releases the hold.
	Connected() bool
	// ConnectionChanged returns a channel closed at the client's next connection event
	// (bus.Client.ConnectionChanged).
	ConnectionChanged() <-chan struct{}
}

// documentCompactionLease names the lease the daily document compaction runs under. It walks
// every document, so two envoy-dispatch processes compacting at the same moment is twice the work
// for one result.
const documentCompactionLease = "document-compaction"

// Deps are the durable state and delivery dependencies for the outbox.
type Deps struct {
	// Store is what the outbox reads and records events through, and its pool is the one the
	// publisher hold is taken and renewed on. cmd/dispatch gives it a store on a pool of its own
	// (store.Pool.Outbox), so the requests queued on the shared pool never hold a publish or a
	// renewal back.
	Store     *store.Store
	Publisher Publisher
	// Broker wakes the outbox for every event this process commits.
	Broker *events.Broker
	// Listen dials the outbox's LISTEN connection (pglisten.Listener.Dial): cmd/dispatch passes
	// store.Pool.Listener.
	Listen func(ctx context.Context) (*pgx.Conn, error)
	Docs   docs.API
	// Leases is where the compaction tick takes documentCompactionLease (compactDocuments). It
	// is required of a Run that ticks: there is no nil-means-unleased path, and cmd/dispatch
	// exits at boot if the lease pool cannot be opened.
	Leases duty.Leases
	// RoleClaims and Sessions are where a role route's holder is resolved (roles.ResolveHolder,
	// resolveRoleHolders): role claims read from the role bucket, released there when their holder
	// is gone, and holders read from the session bucket. cmd/dispatch passes one roles.Buckets for
	// both, reading on Publisher's connection.
	RoleClaims roles.ClaimStore
	Sessions   roles.SessionLookup
}

// compactDaily runs compactDocuments once per compactInterval until ctx ends, on a goroutine of its
// own: a compaction walks every document for minutes, and on the outbox's loop nothing would
// publish meanwhile. It goes through the document service's store, not the outbox's pool.
func compactDaily(ctx context.Context, deps Deps) {
	ctx = store.WithTransactionTracking(ctx)
	compact := time.NewTicker(compactInterval)
	defer compact.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-compact.C:
			compactDocuments(ctx, deps)
		}
	}
}

// compactDocuments runs the daily compaction of every document under documentCompactionLease, so
// that one process does that walk however many serve the database, once per compactInterval and
// not once per process: the period is this loop's own tick, so a process whose ticker fires hours
// after the other's finds the day's compaction already done and skips it.
func compactDocuments(ctx context.Context, deps Deps) {
	duty.Exclusive(ctx, deps.Leases, documentCompactionLease, compactInterval, func(ctx context.Context) {
		if err := deps.Docs.CompactAll(ctx); err != nil {
			slog.Error("dispatch outbox: compact documents", "error", err)
		}
	})
}

// fence is what a batch checks before each act it may make only while this process holds the
// publisher hold: the hold's loss deadline (duty.Hold.Deadline).
type fence interface {
	Deadline() time.Time
}

// errFenced is a send refused because the publisher hold's deadline had passed.
var errFenced = errors.New("the publisher hold's deadline has passed")

// fencedSender is a batch's window that sends nothing once the publisher hold's deadline has
// passed: every send checks it first, and one past it answers errFenced at once, so a process that
// froze past its deadline sends nothing when it resumes.
type fencedSender struct {
	bus.Sender
	hold fence
}

func (s fencedSender) PublishAsync(item contracts.Envelope) <-chan error {
	if !time.Now().Before(s.hold.Deadline()) {
		return failedAnswer(errFenced)
	}
	return s.Sender.PublishAsync(item)
}

func (s fencedSender) Request(subject string, item contracts.Envelope, opts bus.RequestOptions) <-chan error {
	if !time.Now().Before(s.hold.Deadline()) {
		return failedAnswer(errFenced)
	}
	return s.Sender.Request(subject, item, opts)
}

// failedAnswer is a send's answer already decided: err.
func failedAnswer(err error) <-chan error {
	answer := make(chan error, 1)
	answer <- err
	return answer
}

// scanBatch reads a batch (readBatch), resolves its events' destinations with the batch's
// followers and authors read in at most four statements (loadLookups) and its role routes' holders
// read once per role (resolveRoleHolders), and sends it through one window (bus.Client.Window) as
// chains: the sends of one owner's events to one destination key go one at a time, in id order,
// each once the one before it reached its destination, while the chains of different owners and
// destination keys proceed side by side (planChains, sendChains). That is the order a receiver
// needs and the only one the batch keeps: a session takes an owner's events in id order on each
// subject family it receives them on, while the sends to different owners or destination keys land
// in whatever order their answers allow. A send that fails holds back only its own chain, and an
// event that is not due holds back its owner's later events only from the destinations it has not
// reached. What the batch came to is written in one statement (recordBatch).
//
// A failure is classified once every answer is in. A send that met a lost or replaced connection
// (bus.ErrUnavailable) is no fault of its event, and its event is released with no attempt
// counted, to be sent again on the reconnect's wake. A send NATS denied (bus.ErrPublishDenied), did
// not acknowledge on a connection still current (bus.ErrNotAcknowledged: no acknowledgement in
// time, no responders, no receipt from a role's holder), a role route whose holder could not be
// resolved or is stale (roleForward), or a send that failed some other way backs its event off
// with the attempt counted and the error recorded. A batch with a send that failed for the
// connection either way then looks at the connection: a client that has reconnected since the
// batch began (its connection changed and it is connected again) releases the not-acknowledged
// events too and keeps the hold, since the resend follows on the reconnect's wake; otherwise the
// batch probes its window's connection, and a probe that fails - a silent cut on a connection still
// the window's, or a client not connected - reports NATS lost, which gives the publisher hold up
// (Outbox), and releases the not-acknowledged events rather than backing them off.
//
// The batch runs under hold, the publisher hold: every send checks its deadline first
// (fencedSender), and once it has passed the batch sends nothing more and is left unrecorded - the
// record is the last act checked against the deadline - so a process that froze past its deadline
// neither sends nor writes when it resumes. The record's back-offs, hold-backs and releases are
// written only while holder still holds the hold on the database's clock (recordBatch).
func scanBatch(ctx context.Context, deps Deps, hold fence, holder uuid.UUID) (batchResult, error) {
	pending, stopped, err := readBatch(ctx, deps)
	if err != nil {
		return batchResult{}, err
	}
	result := batchResult{}
	events := make([]*batchEvent, len(pending))
	for index := range pending {
		events[index] = &batchEvent{pendingEvent: pending[index], delivered: publishedDestinationSet(pending[index].destinations)}
		if pending[index].due {
			result.sendable++
		}
	}
	if err := resolveDestinations(ctx, deps, events); err != nil {
		return batchResult{}, err
	}
	resolveRoleHolders(deps, events)
	chains := planChains(events)

	changed := deps.Publisher.ConnectionChanged()
	window := deps.Publisher.Window()
	result.started = time.Now()
	sendChains(ctx, fencedSender{Sender: window, hold: hold}, chains)

	connectionFailed := false
	for _, event := range events {
		sent := false
		for _, send := range event.sends {
			switch {
			case errors.Is(send.err, errFenced):
				result.fenced = true
			case send.state == lost && errors.Is(send.err, bus.ErrUnavailable),
				send.state == failed && errors.Is(send.err, bus.ErrNotAcknowledged):
				connectionFailed = true
			}
			if send.state != unsent && send.state != held && !errors.Is(send.err, errFenced) {
				sent = true
			}
		}
		if sent {
			if result.sent == 0 || event.event.ID < result.firstID {
				result.firstID = event.event.ID
			}
			result.lastID = max(result.lastID, event.event.ID)
			result.sent++
		}
	}
	releaseUnacknowledged := false
	if connectionFailed && !result.fenced && ctx.Err() == nil {
		if connectionReplaced(changed) && deps.Publisher.Connected() {
			slog.Info("dispatch outbox: the batch's connection was replaced, so the sends it failed are released for the resend on the new one")
			releaseUnacknowledged = true
		} else if err := window.Probe(probeTimeout); err != nil {
			slog.Error("dispatch outbox: the batch's sends failed and its connection does not answer", "error", err)
			result.natsLost = true
			releaseUnacknowledged = true
		}
	}

	var outcomes []outcome
	anyReleased := false
	for _, event := range events {
		if !event.due {
			continue
		}
		event.outcome = settleEvent(event, releaseUnacknowledged)
		outcomes = append(outcomes, event.outcome)
		switch event.outcome.state {
		case published:
			result.published++
		case released:
			anyReleased = true
		}
	}
	if !stopped {
		outcomes = append(outcomes, deferPastWindow(events)...)
	}
	if anyReleased && !result.fenced && ctx.Err() == nil {
		slog.Error("dispatch outbox: NATS is unreachable, so the batch's released events wait for the next scan")
	}
	if result.fenced || !time.Now().Before(hold.Deadline()) {
		slog.Error("dispatch outbox: the publisher hold's deadline passed before the batch was recorded, so it is left unrecorded",
			"events", len(outcomes))
		result.fenced = true
		return result, nil
	}
	// Written even when ctx has ended: a process stopping mid-batch records what NATS already
	// accepted and releases the rest.
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	holds, err := recordBatch(record, deps, outcomes, holder)
	if err != nil {
		result.blocked = true
		return result, err
	}
	if !holds {
		slog.Error("dispatch outbox: the publisher hold is no longer this process's, so the batch's back-offs and releases were not written",
			"holder", holder)
		result.fenced = true
	}
	result.blocked = stopped || anyReleased
	return result, nil
}

// connectionReplaced reports whether changed, the client's connection-change channel taken before a
// batch's first send (Publisher.ConnectionChanged), has closed: the connection the batch's window
// pinned was lost or replaced since.
func connectionReplaced(changed <-chan struct{}) bool {
	select {
	case <-changed:
		return true
	default:
		return false
	}
}

// batchResult is what one batch (scanBatch) came to. sendable is how many of its events were due,
// sent how many of them it sent at least one envelope of, from firstID to lastID, the first send no
// earlier than started, and published how many it marked published. blocked is a batch after which
// the scan stops: a row it could not read, an event it released, or its record failed. fenced is
// the publisher hold's deadline passing during it, or its record finding the hold another's, and
// natsLost a probe of its connection failing after its sends failed for the connection.
type batchResult struct {
	sendable, sent, published int
	firstID, lastID           int64
	started                   time.Time
	blocked, fenced           bool
	natsLost                  bool
}

// outcomeState is what an event of a batch came to.
type outcomeState string

const (
	// published: every destination of the event is reached.
	published outcomeState = "published"
	// backedOff: the event failed, and waits retry.Delay of its attempts before it is tried again.
	backedOff outcomeState = "backed_off"
	// blocked: the event reached what it could, and a destination of it waits on an earlier event
	// of its owner that has not reached it. It falls due when the earliest of those does, with no
	// attempt counted, and its sends then follow theirs.
	blocked outcomeState = "blocked"
	// released: the event was not tried to the end, because NATS was unreachable, the
	// connection was replaced or the process is stopping, so it is due again at once, with no
	// attempt counted.
	released outcomeState = "released"
)

// outcome is what one event of a batch came to, as recordBatch writes it.
type outcome struct {
	id    int64
	state outcomeState
	// reached is every destination the batch reached, in the order of the event's destinations.
	reached []string
	// attempts and failure are the backed-off event's attempt count and why it failed.
	attempts int
	failure  string
	// delay is how long after the record a backed-off event falls due, or a blocked one whose
	// blocker was backed off by the same batch; until is when a blocked one whose blocker was not
	// due falls due, on the database's clock. A blocked event falls due at the earlier of the two.
	delay *time.Duration
	until *time.Time
}

// settleEvent returns what event, sent by its batch, came to. An event that could not be read is
// backed off. One with a send that met a lost connection, the hold's deadline or the process
// stopping is released; releaseUnacknowledged releases one whose sends NATS did not acknowledge
// too. One whose destinations could not all be resolved, or a send of which NATS answered with a
// failure, is backed off. Otherwise one with a destination held back waits on its blockers
// (holdBack), and one with none is published.
func settleEvent(event *batchEvent, releaseUnacknowledged bool) outcome {
	result := outcome{id: event.event.ID}
	if event.failure != nil {
		return backOff(result, event.pendingEvent, event.failureLog, event.failure)
	}
	var failures []error
	if event.resolveErr != nil {
		failures = append(failures, event.resolveErr)
	}
	release := false
	blockers := slices.Clone(event.held)
	for _, send := range event.sends {
		switch send.state {
		case reached:
			result.reached = append(result.reached, send.dest.subject)
		case failed:
			if releaseUnacknowledged && errors.Is(send.err, bus.ErrNotAcknowledged) {
				release = true
				continue
			}
			failures = append(failures, fmt.Errorf("%s %q: %w", send.dest.label, send.dest.id, send.err))
		case held:
			blockers = append(blockers, blocker{event: send.heldBy})
		default:
			release = true
		}
	}
	switch {
	case release:
		slog.Info("dispatch outbox: event released", "event_id", event.event.ID, "error", errors.Join(failures...))
		result.state = released
		return result
	case len(failures) > 0:
		return backOff(result, event.pendingEvent, "dispatch outbox: publish event", errors.Join(failures...))
	case len(blockers) > 0:
		return holdBack(result, blockers)
	}
	result.state = published
	return result
}

// backOff reports item's failure under what and returns result backed off by retry.Delay of the
// attempt it now counts, which the record adds to the database's clock.
func backOff(result outcome, item pendingEvent, what string, cause error) outcome {
	slog.Error(what, "event_id", item.event.ID, "error", cause)
	result.state = backedOff
	result.attempts = item.attempts + 1
	if result.attempts == retry.DeadLetterAttempts {
		slog.Error("dispatch outbox: event reached dead-letter threshold", "event_id", item.event.ID, "attempts", result.attempts)
	}
	result.delay = new(retry.Delay(result.attempts))
	result.failure = cause.Error()
	return result
}

// holdBack returns result blocked on blockers: due when the earliest of them is, so that it is read
// again beside its blocker and sent after it, rather than read at every wake while the blocker
// waits out a back-off. A blocker of the same batch that was released releases it too.
func holdBack(result outcome, blockers []blocker) outcome {
	result.state = blocked
	earlierDelay := func(delay *time.Duration) {
		if delay != nil && (result.delay == nil || *delay < *result.delay) {
			result.delay = delay
		}
	}
	earlierUntil := func(until *time.Time) {
		if until != nil && (result.until == nil || until.Before(*result.until)) {
			result.until = until
		}
	}
	for _, by := range blockers {
		if by.event == nil {
			earlierUntil(by.until)
			continue
		}
		switch by.event.outcome.state {
		case backedOff, blocked:
			earlierDelay(by.event.outcome.delay)
			earlierUntil(by.event.outcome.until)
		default:
			result.state, result.delay, result.until = released, nil, nil
			return result
		}
	}
	if result.delay == nil && result.until == nil {
		result.state = released
	}
	return result
}

// deferPastWindow returns the outcomes of the due events a batch chose but did not read: those of
// an owner with more than batchSize unpublished events whose first batchSize, every one read, are
// none of them due. Such an event waits on the earliest back-off of that window, the first moment
// the owner's window can move, rather than leave its owner chosen at every wake with nothing to
// send. An owner whose window the batch's cap cut short is left alone: the rest of its window is
// the next batch's.
func deferPastWindow(events []*batchEvent) []outcome {
	type window struct {
		read       map[int64]struct{}
		size       int
		due        bool
		earliest   *time.Time
		candidates []int64
	}
	windows := map[string]*window{}
	var owners []string
	for _, event := range events {
		if event.owner == "" {
			continue
		}
		w := windows[event.owner]
		if w == nil {
			w = &window{read: map[int64]struct{}{}, size: event.windowSize, candidates: event.candidates}
			windows[event.owner] = w
			owners = append(owners, event.owner)
		}
		w.read[event.event.ID] = struct{}{}
		if event.due {
			w.due = true
		} else if event.nextAttempt != nil && (w.earliest == nil || event.nextAttempt.Before(*w.earliest)) {
			w.earliest = event.nextAttempt
		}
	}
	var deferred []outcome
	for _, owner := range owners {
		w := windows[owner]
		if w.due || w.earliest == nil || len(w.read) < w.size {
			continue
		}
		for _, id := range w.candidates {
			if _, ok := w.read[id]; !ok {
				deferred = append(deferred, outcome{id: id, state: blocked, until: w.earliest})
			}
		}
	}
	return deferred
}

// recordBatch writes what each event of a batch came to, in one statement and so all of it or
// none, and reports whether holder still held the publisher hold when it did. What is true whoever
// holds the hold is written regardless: the destinations an event reached are added to its
// published_destinations, and an event that reached every destination is marked published, its
// publish_error cleared. A back-off (next_attempt_at on the database's clock, the attempt count and
// publish_error), a hold-back and a release are written only while the hold is holder's and has not
// lapsed, checked in the same statement, so a process that lost the hold mid-batch schedules
// nothing beside the holder that took it. Each row is guarded on published_at is null.
//
// A write that fails leaves the whole batch as it was read, which is what a process that dies
// between NATS's acknowledgements and this write leaves too: the batch is sent again by the next
// scan or the next holder. JetStream drops each repeat inside its duplicate window by its MsgId,
// a role forward's included (roleForward.send), and a role's holder drops a repeat forward by its
// dedupe key (internal/contracts/dedupe.go).
func recordBatch(ctx context.Context, deps Deps, outcomes []outcome, holder uuid.UUID) (bool, error) {
	if len(outcomes) == 0 {
		return true, nil
	}
	ids := make([]int64, 0, len(outcomes))
	states := make([]string, 0, len(outcomes))
	attempts := make([]*int32, 0, len(outcomes))
	delays := make([]*float64, 0, len(outcomes))
	until := make([]*time.Time, 0, len(outcomes))
	failures := make([]*string, 0, len(outcomes))
	var reachedIDs []int64
	var reachedTopics []string
	for _, result := range outcomes {
		ids = append(ids, result.id)
		states = append(states, string(result.state))
		var attempt *int32
		var failure *string
		if result.state == backedOff {
			attempt, failure = new(int32(result.attempts)), new(result.failure)
		}
		attempts = append(attempts, attempt)
		failures = append(failures, failure)
		var delay *float64
		if result.delay != nil {
			delay = new(result.delay.Seconds())
		}
		delays = append(delays, delay)
		until = append(until, result.until)
		for _, topic := range result.reached {
			reachedIDs = append(reachedIDs, result.id)
			reachedTopics = append(reachedTopics, topic)
		}
	}
	var holds bool
	if err := deps.Store.Pool.QueryRow(ctx, `
		with hold as (
			select exists (
				select 1 from duty_holds where name = $1 and holder = $2 and expires_at > now()
			) as mine
		), outcome as (
			select * from unnest($3::bigint[], $4::text[], $5::int[], $6::float8[], $7::timestamptz[], $8::text[])
				as o(id, state, attempts, delay, until, failure)
		), reached as (
			select id, array_agg(topic order by position) as topics
			from unnest($9::bigint[], $10::text[]) with ordinality as r(id, topic, position)
			group by id
		), recorded as (
			update events e set
				published_destinations = e.published_destinations || array(
					select t.topic from unnest(r.topics) with ordinality as t(topic, position)
					where t.topic <> all(e.published_destinations)
					order by t.position
				),
				published_at = case when o.state = 'published' then now() end,
				publish_error = case
					when o.state = 'published' then null
					when o.state = 'backed_off' and h.mine then o.failure
					else e.publish_error
				end,
				attempt_count = case when o.state = 'backed_off' and h.mine then o.attempts else e.attempt_count end,
				next_attempt_at = case
					when o.state = 'published' then null
					when not h.mine then e.next_attempt_at
					when o.state = 'backed_off' then now() + make_interval(secs => o.delay)
					when o.state = 'blocked' then least(now() + make_interval(secs => o.delay), o.until)
					else null
				end
			from outcome o
			cross join hold h
			left join reached r on r.id = o.id
			where e.id = o.id and e.published_at is null
			  and (o.state = 'published' or h.mine or r.id is not null)
		)
		select mine from hold
	`, publisherHold, holder, ids, states, attempts, delays, until, failures, reachedIDs, reachedTopics).Scan(&holds); err != nil {
		return false, fmt.Errorf("record the batch's outcome: %w", err)
	}
	return holds, nil
}

// resolveDestinations gives every event of a batch that has an owner and can be read its envelope
// and destinations (eventDestinations), reading the followers and authors all of them need at once
// (loadLookups). An event whose envelope cannot be built has no destination, and the error backs
// it off.
func resolveDestinations(ctx context.Context, deps Deps, events []*batchEvent) error {
	want := newWanted()
	for _, event := range events {
		if event.failure != nil || event.owner == "" {
			continue
		}
		item, err := envelope(event.event, event.slug)
		if err == nil {
			if err = item.Validate(); err != nil {
				err = fmt.Errorf("validate envelope: %w", err)
			}
		}
		if err != nil {
			event.resolveErr = err
			continue
		}
		event.item = item
		_, _ = eventDestinations(event.event, item.Topic, event.route, want)
	}
	found, err := loadLookups(ctx, deps.Store.Pool, want)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.failure != nil || event.owner == "" || event.resolveErr != nil {
			continue
		}
		event.destinationsOf, event.resolveErr = eventDestinations(event.event, event.item.Topic, event.route, found)
	}
	return nil
}

// destinations collects an event's destinations, each subject once: a subject an earlier route
// already reaches is not added again.
type destinations struct {
	list []destination
	seen map[string]struct{}
}

func (d *destinations) add(subject, label, id string) {
	d.addDestination(destination{subject: subject, label: label, id: id})
}

func (d *destinations) addDestination(dest destination) {
	if _, ok := d.seen[dest.subject]; ok {
		return
	}
	d.seen[dest.subject] = struct{}{}
	d.list = append(d.list, dest)
}

// addSessions adds each session's own subject, named by label and the session.
func (d *destinations) addSessions(sessionIDs []string, label string) {
	for _, sessionID := range sessionIDs {
		d.add(contracts.AgentSubject(sessionID), label, sessionID)
	}
}

// eventDestinations lists event's destinations in the order they are sent and recorded: its
// owner's topic (ownerTopic), then - for an event that notifies and is not a targeted message - its
// owner's route and its author routes, then its follower routes and the previous claimant's
// notice. A lookup that fails, or a route that does not parse, ends the list there: the
// destinations before it are returned with the error, which backs the event off once they are
// sent.
func eventDestinations(event model.Event, ownerTopic string, route *string, look lookup) ([]destination, error) {
	d := &destinations{seen: map[string]struct{}{}}
	d.add(ownerTopic, "publish owner topic", ownerTopic)
	targeted := (event.Type == "message.created" || event.Type == "message.answered") &&
		payloadString(event.Payload, "target") != ""
	if event.Notify && !targeted {
		if !suppressesCurrentRoute(event.Payload, route) {
			if err := addRoute(d, route); err != nil {
				return d.list, err
			}
		}
		if err := addAuthorRoutes(event, look, d); err != nil {
			return d.list, err
		}
	}
	if err := addFollowerRoutes(event, look, d); err != nil {
		return d.list, err
	}
	addPreviousClaimant(event, d)
	return d.list, nil
}

// addRoute adds the owner's route: a session route is the session's own subject, and a role route
// its role topic, which the batch forwards to the role's holder (resolveRoleHolders).
func addRoute(d *destinations, route *string) error {
	if route == nil || *route == "" {
		return nil
	}
	parsed, err := model.ParseRoute(*route)
	if err != nil {
		return fmt.Errorf("parse route %q: %w", *route, err)
	}
	switch parsed.Kind {
	case "role":
		d.addDestination(destination{subject: contracts.RoleTopicPrefix + parsed.ID, label: "publish route", id: *route, role: parsed.ID})
	case "session":
		d.add(contracts.AgentSubject(parsed.ID), "publish route", *route)
	default:
		return fmt.Errorf("unsupported route %q", *route)
	}
	return nil
}

// addPreviousClaimant tells a session that lost an issue's claim, on its own topic: a takeover
// (another agent claimed an issue this session's claim was on, because the listener no longer
// lists this session as live) and a release somebody else made. Like the follower routes this
// ignores Notify — the losing session must hear it whoever acted, and it would otherwise learn
// only by subscribing to the whole issue.
func addPreviousClaimant(event model.Event, d *destinations) {
	if event.Type != "issue.claimed" && event.Type != "issue.released" {
		return
	}
	sessionID := payloadPreviousClaimSession(event.Payload)
	if sessionID == "" || event.Actor.SameAs(model.Actor{Kind: "session", ID: sessionID}) {
		return
	}
	d.addSessions([]string{sessionID}, "publish claim change to")
}

// payloadPreviousClaimSession reads the session id out of an event payload's previous claim,
// or "" when there was none or a human held it.
func payloadPreviousClaimSession(payload any) string {
	values, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	claim, ok := values["previous_claim"].(map[string]any)
	if !ok {
		return ""
	}
	actor, ok := claim["actor"].(map[string]any)
	if !ok {
		return ""
	}
	if kind, _ := actor["kind"].(string); kind != "session" {
		return ""
	}
	id, _ := actor["id"].(string)
	return id
}

// addFollowerRoutes delivers an ask's answer, edit, hand-back, resolution, and every reply on it
// to each session following the ask (the asker, every session that replied, and any session a
// human added), on the session's own topic, in the order they followed it. Unlike the route and
// author routes this ignores Notify: an agent's reply on an ask must still reach the other
// followers, who otherwise learn of it only by subscribing to the whole issue. A quiet ask.edited
// (a version move of an approval request that an earlier version already moved since it was
// opened or last handed back, model.AskEditEventPayload.Quiet) reaches no follower. The event's own
// actor is skipped, and a topic the route already reaches is not added twice.
func addFollowerRoutes(event model.Event, look lookup, d *destinations) error {
	var askID string
	switch event.Type {
	case "ask.edited":
		if !payloadBool(event.Payload, "quiet") {
			askID = payloadString(event.Payload, "id")
		}
	case "ask.answered", "ask.handed_back", "ask.resolved":
		askID = payloadString(event.Payload, "id")
	case "comment.created", "comment.resolved", "comment.reopened", "comment.edited":
		askID = payloadString(event.Payload, "ask_id")
	}
	if askID == "" {
		return nil
	}
	followers, err := look.followers(askID)
	if err != nil {
		return err
	}
	var sessionIDs []string
	for _, follower := range followers {
		if !event.Actor.SameAs(model.Actor{Kind: "session", ID: follower}) {
			sessionIDs = append(sessionIDs, follower)
		}
	}
	d.addSessions(sessionIDs, "publish follower route to")
	return nil
}

// addAuthorRoutes delivers a human comment-thread action directly to the involved sessions' own
// topics, regardless of the issue's route (which may point at an entirely different reviewer) and
// regardless of whether the event's owner is an issue or a project document (which has no route at
// all). Without this, the agent that started the thread or is the thread's own root author never
// learns about the human's reply, resolution, reopening, or edit, and the agent that wrote a
// document never learns of a new comment on it. A reply reaches its thread's root author and its
// direct parent's author; a new comment on a document (no reply_to and no ask_id), and its edit,
// reach every session that document credits (loadDocumentSessionAuthors); a comment on an issue's
// conversation names no document and reaches no author. Ask threads are not walked here: their
// asker and repliers are the ask's followers (addFollowerRoutes). The event's own actor is
// skipped, and so is a session the comment's mentions already reach (suppressed_authors).
func addAuthorRoutes(event model.Event, look lookup, d *destinations) error {
	suppressedAuthors := payloadStringSet(event.Payload, "suppressed_authors")
	seen := map[string]bool{}
	var targets []string
	consider := func(author model.Actor, found bool) {
		if !found || author.Kind != "session" || seen[author.ID] {
			return
		}
		if event.Actor.SameAs(author) {
			return
		}
		seen[author.ID] = true
		if _, suppressed := suppressedAuthors[author.ID]; suppressed {
			return
		}
		targets = append(targets, author.ID)
	}
	considerLoaded := func(author model.Actor, found bool, err error) error {
		if err != nil {
			return err
		}
		consider(author, found)
		return nil
	}

	var inReplyTo, commentID, messageInReplyTo string
	switch event.Type {
	case "comment.created", "comment.resolved", "comment.reopened", "comment.edited":
		inReplyTo = payloadString(event.Payload, "reply_to")
		commentID = payloadString(event.Payload, "id")
	case "message.created", "message.answered":
		messageInReplyTo = payloadString(event.Payload, "in_reply_to")
	case "subscription.removed", "ask.follower_added", "ask.follower_removed":
		// The target is the unsubscribed, added, or removed session itself, carried directly
		// in the payload — there is no thread or ask to walk to find it.
		if sessionID := payloadString(event.Payload, "session_id"); sessionID != "" {
			consider(model.Actor{Kind: "session", ID: sessionID}, true)
		}
	default:
		return nil
	}

	if inReplyTo != "" {
		// New comment threads persist ReplyTo as their root ID. Walking still keeps
		// routing correct for legacy nested rows and finds the root author.
		if err := considerLoaded(look.rootCommentAuthor(inReplyTo)); err != nil {
			return err
		}
		if err := considerLoaded(look.commentAuthor(inReplyTo)); err != nil {
			return err
		}
	} else if commentID != "" && (event.Type == "comment.resolved" || event.Type == "comment.reopened") {
		if err := considerLoaded(look.rootCommentAuthor(commentID)); err != nil {
			return err
		}
	} else if (event.Type == "comment.created" || event.Type == "comment.edited") && payloadString(event.Payload, "ask_id") == "" {
		document := commentedDocument(event)
		if document != "" {
			authors, err := look.documentAuthors(document)
			if err != nil {
				return err
			}
			for _, author := range authors {
				consider(model.Actor{Kind: "session", ID: author}, true)
			}
		}
	}
	if messageInReplyTo != "" {
		if err := considerLoaded(look.messageAuthor(messageInReplyTo)); err != nil {
			return err
		}
	}

	d.addSessions(targets, "publish author route to")
	return nil
}

// commentedDocument names the artifact a top-level comment is on: its anchor's for a comment on a
// passage (of an issue's document or a project artifact), otherwise the event's own artifact for a
// comment on a whole project artifact. The author lookup decides whether that artifact is a document.
func commentedDocument(event model.Event) string {
	document := ""
	if values, ok := event.Payload.(map[string]any); ok {
		document = payloadString(values["anchor"], "artifact_id")
	}
	if document == "" && event.ArtifactID != nil {
		document = *event.ArtifactID
	}
	return document
}

func publishedDestinationSet(subjects []string) map[string]struct{} {
	delivered := make(map[string]struct{}, len(subjects))
	for _, subject := range subjects {
		delivered[subject] = struct{}{}
	}
	return delivered
}

func envelope(event model.Event, slug string) (contracts.Envelope, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return contracts.Envelope{}, fmt.Errorf("encode event: %w", err)
	}
	var topic string
	if event.ArtifactID != nil {
		if event.Project == "" || slug == "" {
			return contracts.Envelope{}, fmt.Errorf("document event requires project and slug")
		}
		topic = documentTopicPrefix + event.Project + "." + slug + "." + event.Type
	} else if event.IssueKey != nil {
		topic = "notifications.dispatch.issue." + *event.IssueKey + "." + event.Type
	} else if event.Project != "" {
		topic = "notifications.dispatch.project." + event.Project + "." + event.Type
	} else {
		return contracts.Envelope{}, fmt.Errorf("event requires an owner")
	}
	eventID := fmt.Sprintf("dispatch-%d", event.ID)
	item := contracts.Envelope{
		EventID:        eventID,
		Source:         "dispatch",
		SourceEventID:  fmt.Sprint(event.ID),
		Topic:          topic,
		DedupeKey:      eventID,
		IssuedAt:       event.CreatedAt.UnixMilli(),
		PayloadSummary: payloadSummary(event, slug),
		Payload:        string(payload),
		TraceID:        eventID,
	}
	if event.Actor.Kind == "session" {
		item.SourceSession = event.Actor.ID
	}
	if event.Type == "ask.answered" || event.Type == "ask.resolved" {
		item.InReplyTo = payloadString(event.Payload, "id")
	}
	if event.Type == "comment.created" || event.Type == "comment.answered" {
		if replyTo := payloadString(event.Payload, "reply_to"); replyTo != "" {
			item.InReplyTo = replyTo
		}
		// An ask reply is correlated to the question, not the thread root.
		if askID := payloadString(event.Payload, "ask_id"); askID != "" {
			item.InReplyTo = askID
		}
	}
	if event.Type == "message.created" || event.Type == "message.answered" {
		// A message reply correlates to the original message so the recipient's
		// TOON renders "re: <message ref>" and the payload carries reply_body.
		if inReplyTo := payloadString(event.Payload, "in_reply_to"); inReplyTo != "" {
			item.InReplyTo = inReplyTo
		}
	}
	if strings.HasPrefix(event.Type, "ask.") {
		item.Urgency = payloadString(event.Payload, "urgency")
	}
	return item, nil
}

func payloadSummary(event model.Event, slug string) string {
	kind := strings.ReplaceAll(event.Type, ".", " ")
	detail := ""
	switch {
	case event.Type == "ask.answered":
		// The answer, not the question: it is the first thing the asker should read.
		detail = text.HeadRunes(askAnswerText(event.Payload), 120)
	case event.Type == "subscription.removed", event.Type == "ask.follower_added", event.Type == "ask.follower_removed":
		detail = payloadString(event.Payload, "session_id")
	case event.Type == "issue.claimed", event.Type == "issue.released":
		// "claimed", "takeover", "forced", "released", or "closed": why the holder changed.
		detail = payloadString(event.Payload, "reason")
	case strings.HasPrefix(event.Type, "ask."):
		detail = text.HeadRunes(payloadString(event.Payload, "question"), 120)
	case event.Type == "message.created", event.Type == "message.answered":
		detail = payloadString(event.Payload, "body")
	case strings.HasPrefix(event.Type, "comment."), strings.HasPrefix(event.Type, "suggestion."):
		detail = payloadString(event.Payload, "body")
	}
	owner := ""
	if event.ArtifactID != nil {
		owner = event.Project + "/" + slug
	} else if event.IssueKey != nil {
		owner = *event.IssueKey
	}
	summary := owner + " " + kind
	if detail != "" {
		summary += ": " + detail
	}
	return text.HeadRunes(strings.Join(strings.Fields(summary), " "), 160)
}

func payloadString(payload any, key string) string {
	values, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	value, _ := values[key].(string)
	return value
}

func payloadBool(payload any, key string) bool {
	values, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	value, _ := values[key].(bool)
	return value
}

// suppressesCurrentRoute applies a create-time suppression to the route that was resolved
// alongside the mention, including a later direct route to that same resolved session. A route
// edit to any other target must still wake its new destination.
func suppressesCurrentRoute(payload any, current *string) bool {
	if !payloadBool(payload, "suppress_route") || current == nil || *current == "" {
		return false
	}
	if *current == payloadString(payload, "suppressed_route") {
		return true
	}
	sessionID := payloadString(payload, "suppressed_route_session_id")
	return sessionID != "" && *current == "session:"+sessionID
}

func payloadStringSet(payload any, key string) map[string]struct{} {
	values, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	result := map[string]struct{}{}
	switch entries := values[key].(type) {
	case []string:
		for _, entry := range entries {
			result[entry] = struct{}{}
		}
	case []any:
		for _, entry := range entries {
			if value, ok := entry.(string); ok {
				result[value] = struct{}{}
			}
		}
	}
	return result
}

// askAnswerText renders an ask's answer as one line: the text alone, the selected options
// alone, or "<options> - <text>" when a human did both (the same rendering the TS delivery uses).
func askAnswerText(payload any) string {
	values, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	answer, ok := values["answer"].(map[string]any)
	if !ok {
		return ""
	}
	var selected []string
	if options, ok := answer["selected"].([]any); ok {
		for _, option := range options {
			if label, ok := option.(string); ok {
				selected = append(selected, label)
			}
		}
	}
	text, _ := answer["text"].(string)
	joined := strings.Join(selected, ", ")
	switch {
	case text == "":
		return joined
	case joined == "":
		return text
	default:
		return joined + " - " + text
	}
}
