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
	"github.com/sjawhar/envoy/internal/metrics"
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
	// maxCommentThreadDepth bounds the reply_to walks in loadRootCommentAuthors and
	// loadCommentRecipients so a malformed cycle (comments.reply_to has no acyclicity constraint)
	// cannot spin the recursive query and stall the outbox scan.
	maxCommentThreadDepth = 32
	// mentionPendingDelay is how long an event waits when a session route held its copy back for
	// a mention whose send is still undecided (destinations.leftToMention): long enough for the
	// handler that holds the send to settle it, short enough that the copy follows a failed send
	// promptly. The wait is bounded: a pending attempt counts as undecided only for
	// model.DeliveryClaimLapse, the minute its sender's claim holds (mentionDeliveryQuery).
	mentionPendingDelay = 2 * time.Second
	// recordTimeout bounds the write of a batch's outcome (recordBatch), which a stopping process
	// still makes after its context has ended: one statement, which a healthy database answers in
	// milliseconds, so a write that takes longer is left undone, and the batch is read again.
	recordTimeout = 5 * time.Second
)

// Publisher is the NATS client the outbox publishes through (bus.Client).
type Publisher interface {
	// WindowUntil starts one batch's sends: each is sent without waiting for NATS's answer, all on
	// one connection or not at all, in no order of its own, and none written at or past deadline,
	// which a send past it answers bus.ErrWindowDeadline (bus.Client.WindowUntil); sendChains keeps
	// the order per owner and destination. A nil deadline is none.
	WindowUntil(deadline func() time.Time) bus.Sender
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
	// Leases is where the compaction tick takes documentCompactionLease (compactDaily). It
	// is required of a Run that ticks: there is no nil-means-unleased path, and cmd/dispatch
	// exits at boot if the lease pool cannot be opened.
	Leases duty.Leases
	// RoleClaims and Sessions are where a role route's holder is resolved (roles.ResolveHolder,
	// resolveRoleHolders): role claims read from the role bucket, released there when their holder
	// is gone, and holders read from the session bucket. cmd/dispatch passes one roles.Buckets for
	// both, reading on Publisher's connection.
	RoleClaims roles.ClaimStore
	Sessions   roles.SessionLookup
	// PublishFailures counts a destination of a publish attempt that did not reach it, by
	// reason (countPublishFailure). cmd/dispatch registers it on the process's metrics
	// registry, so a denied or refused destination shows on /metrics rather than only in the
	// log.
	PublishFailures *metrics.Counter
	// lanes is what the batches of one scan share: the owners they hold in flight, the outbox
	// pool's one statement at a time, and the wake a released owner gives the scan (Outbox.scan).
	// A batch run with none - one by itself, as this package's tests run it - holds no owner,
	// leaves none out of its read and takes no lock.
	lanes *lanes
}

// countPublishFailure counts one destination of a publish attempt that did not reach it, under
// reason: "denied" for a send the server refused under this connection's own publish permissions,
// "refused" for one NATS refuses however often it is retried, "holder" for a role forward its
// holder's resolution held pending with nothing sent, "unanswered" for a forward NATS took whose
// answer never came - the holder's receipt, or the stream's acknowledgement of the stored copy
// for a holder with a serving port (roleForward.send) - and "failed" for every other failure
// (sendChains). Every increment goes through here: a Deps built without a counter - the package's
// own tests - then counts nothing instead of dereferencing nil.
func (d Deps) countPublishFailure(reason string) {
	if d.PublishFailures == nil {
		return
	}
	d.PublishFailures.Inc([2]string{"reason", reason})
}

// compactDaily runs compactDocuments once per compactInterval until ctx ends, on a goroutine of its
// own: a compaction walks every document for minutes, and on the outbox's loop nothing would
// publish meanwhile. It goes through the document service's store, not the outbox's pool.
//
// Each compaction runs under documentCompactionLease, so that one process does that walk however
// many serve the database, once per compactInterval and not once per process: the period is this
// loop's own interval, so a process whose tick comes hours after the other's finds the day's
// compaction already done and skips it. The ticks are duty.RunLeased's, a ticker's: the first one
// interval after the call, then one each interval; the first tick a compaction overran comes at
// once with the moment it was due, and only the ones after it are dropped.
func compactDaily(ctx context.Context, deps Deps) {
	ctx = store.WithTransactionTracking(ctx)
	duty.RunLeased(ctx, deps.Leases, documentCompactionLease, compactInterval, duty.TickAfterInterval, func(ctx context.Context) {
		compactDocuments(ctx, deps)
	})
}

// compactDocuments is one daily compaction of every document, the pass compactDaily runs under
// documentCompactionLease.
func compactDocuments(ctx context.Context, deps Deps) {
	if err := deps.Docs.CompactAll(ctx); err != nil {
		slog.Error("dispatch outbox: compact documents", "error", err)
	}
}

// fence is what a batch checks before each act it may make only while this process holds the
// publisher hold: the hold's loss deadline (duty.Hold.Deadline).
type fence interface {
	Deadline() time.Time
}

// failedAnswer is a send's answer already decided: err.
func failedAnswer(err error) <-chan error {
	answer := make(chan error, 1)
	answer <- err
	return answer
}

// scanBatch reads a batch (readBatch) and publishes it (publishBatch): it resolves its events'
// destinations with the batch's followers, authors, recipients and mention deliveries read
// in at most seven statements (loadLookups) and its role routes' holders read once per role
// (resolveRoleHolders), and sends it through one window (Publisher.WindowUntil) as
// chains: the sends of one owner's events to one destination key go one at a time, in id order,
// each once the one before it reached its destination, while the chains of different owners and
// destination keys proceed side by side (planChains, sendChains). That is the order a receiver
// needs and the only one the batch keeps: a session takes an owner's events in id order on each
// subject family it receives them on, while the sends to different owners or destination keys land
// in whatever order their answers allow. A send that fails holds back only its own chain, and an
// event that is not due holds back its owner's later events only from the destinations it has not
// reached. An event whose recipient read failed holds them back too, from every session key its
// list neither names nor it already reached, until its retry. What an owner came to is written in
// one statement, and what the owners left at the batch's end came to in one more (recordBatch).
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
// The batch runs under hold, the publisher hold: its window is the hold's (Publisher.WindowUntil
// with the hold's deadline), which checks the deadline before each send and writes nothing past
// it, answering bus.ErrWindowDeadline, and once it has passed the batch sends nothing more and is
// left unrecorded - the record is the last act checked against the deadline - so a process that
// froze past its deadline neither sends nor writes when it resumes. The record's back-offs,
// hold-backs and releases are written only while holder still holds the hold on the database's
// clock (recordBatch).
func scanBatch(ctx context.Context, deps Deps, hold fence, holder uuid.UUID) (batchResult, error) {
	pending, stopped, excluded, err := readBatch(ctx, deps)
	if err != nil {
		return batchResult{}, err
	}
	result, err := publishBatch(ctx, deps, hold, holder, pending, stopped)
	result.excluded = excluded
	return result, err
}

// publishBatch is scanBatch's work after the read: it resolves pending's destinations, sends them,
// settles each event and records what the batch came to, under hold and as holder. stopped is
// readBatch's, a row that failed to scan and ended the read: the batch then defers no event past
// an owner's window (deferPastWindow) and is reported blocked.
//
// An owner is settled, recorded and released as soon as the last chain of its own has finished
// (sendChains), so its next events and the owners this batch left behind are read while the rest
// of the batch is still sending: a chain waiting for a role holder's receipt then holds back its
// own owner's later events alone. An owner with a send that failed for the connection, one with a
// send the hold's deadline fenced, one with a send the batch's stop left unmade, one read past
// the hold's deadline, and every owner of a batch whose context has ended wait for the batch's
// end instead, where the connection check below decides what their sends came to: each of those
// is an owner an early settle would release or leave unrecorded, so it is settled once, there.
// Whichever way an owner settles, every lane the read took is given back before the batch
// returns.
func publishBatch(ctx context.Context, deps Deps, hold fence, holder uuid.UUID, pending []pendingEvent, stopped bool) (batchResult, error) {
	result := batchResult{}
	events := make([]*batchEvent, len(pending))
	for index := range pending {
		events[index] = &batchEvent{pendingEvent: pending[index], delivered: publishedDestinationSet(pending[index].destinations)}
		if pending[index].due {
			result.sendable++
		}
	}
	var owners []string
	byOwner := map[string][]*batchEvent{}
	lanesHeld := map[string]struct{}{}
	for _, event := range events {
		if event.owner == "" {
			continue
		}
		if _, ok := byOwner[event.owner]; !ok {
			owners = append(owners, event.owner)
			lanesHeld[event.owner] = struct{}{}
		}
		byOwner[event.owner] = append(byOwner[event.owner], event)
	}
	defer func() {
		for owner := range lanesHeld {
			deps.lanes.release(owner)
		}
	}()
	if err := resolveDestinations(ctx, deps, events); err != nil {
		return batchResult{}, err
	}
	resolveRoleHolders(ctx, deps, events)
	chains := planChains(events)

	changed := deps.Publisher.ConnectionChanged()
	window := deps.Publisher.WindowUntil(hold.Deadline)
	result.started = time.Now()
	settled := map[string]struct{}{}
	var recordErr error
	lostHold := false
	// settleAndRecord settles each due event of owned (settleEvent), defers the due events their
	// owners' windows left unread (deferPastWindow) and writes what they all came to in one
	// statement. Every event the batch settles goes through here once, so each is logged once:
	// an owner settled as its own chains finish, and at the batch's end every event no owner's
	// settle recorded. Nothing is written once a send met the publisher hold's deadline or the
	// deadline has passed, the record being the last act checked against it; the batch is then
	// left unrecorded and reported fenced.
	settleAndRecord := func(owned []*batchEvent, releaseUnacknowledged bool) (settlement, error) {
		var outcomes []outcome
		var done settlement
		for _, event := range owned {
			if !event.due {
				continue
			}
			event.outcome = settleEvent(event, releaseUnacknowledged)
			outcomes = append(outcomes, event.outcome)
			switch event.outcome.state {
			case published:
				done.published++
			case released:
				done.released = true
			}
		}
		if !stopped {
			outcomes = append(outcomes, deferPastWindow(owned)...)
		}
		if done.released && !result.fenced && ctx.Err() == nil {
			slog.Error("dispatch outbox: NATS is unreachable, so the batch's released events wait for the next scan")
		}
		if result.fenced || !time.Now().Before(hold.Deadline()) {
			slog.Error("dispatch outbox: the publisher hold's deadline passed before the batch was recorded, so it is left unrecorded",
				"events", len(outcomes))
			result.fenced = true
			return done, nil
		}
		// Written even when ctx has ended: a process stopping mid-batch records what NATS already
		// accepted and releases the rest.
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		defer cancel()
		holds, err := recordBatch(record, deps, outcomes, holder)
		done.wrote, done.holds = true, holds
		return done, err
	}
	settleOwner := func(owner string) {
		if ctx.Err() != nil || !time.Now().Before(hold.Deadline()) {
			return
		}
		for _, event := range byOwner[owner] {
			for _, send := range event.sends {
				if send.connection || send.fenced || send.state == unsent {
					return
				}
			}
		}
		done, err := settleAndRecord(byOwner[owner], false)
		settled[owner] = struct{}{}
		switch {
		case err != nil:
			recordErr = err
		case !done.wrote:
			return
		case !done.holds:
			slog.Error("dispatch outbox: the publisher hold is no longer this process's, so the owner's back-offs and releases were not written",
				"holder", holder, "owner", owner)
			lostHold = true
		default:
			result.published += done.published
			delete(lanesHeld, owner)
			deps.lanes.releaseEarly(owner)
		}
	}
	// An owner with no chain has no send to wait for, so it settles before the window opens.
	sending := map[string]struct{}{}
	for _, c := range chains {
		sending[c.owner] = struct{}{}
	}
	for _, owner := range owners {
		if _, ok := sending[owner]; !ok {
			settleOwner(owner)
		}
	}
	sendChains(ctx, deps, window, chains, settleOwner)

	connectionFailed := false
	for _, event := range events {
		sent := false
		for _, send := range event.sends {
			switch {
			case send.fenced:
				result.fenced = true
			case send.connection:
				connectionFailed = true
			}
			if send.sent() {
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

	var rest []*batchEvent
	for _, event := range events {
		if _, ok := settled[event.owner]; !ok {
			rest = append(rest, event)
		}
	}
	done, err := settleAndRecord(rest, releaseUnacknowledged)
	result.published += done.published
	if err != nil {
		result.blocked = true
		return result, err
	}
	if !done.wrote {
		return result, nil
	}
	if !done.holds {
		slog.Error("dispatch outbox: the publisher hold is no longer this process's, so the batch's back-offs and releases were not written",
			"holder", holder)
		lostHold = true
	}
	result.fenced = result.fenced || lostHold
	result.blocked = stopped || done.released
	if recordErr != nil {
		result.blocked = true
		return result, recordErr
	}
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
// natsLost a probe of its connection failing after its sends failed for the connection. excluded
// is a read that left the owners the batches beside it held out (lanes.reserve): it may have read
// past their events, so the scan reads again once every batch has given its owners back.
type batchResult struct {
	sendable, sent, published int
	firstID, lastID           int64
	started                   time.Time
	blocked, fenced           bool
	natsLost                  bool
	excluded                  bool
}

// settlement is what one settle-and-record (publishBatch's settleAndRecord) came to: how many of
// its events it marked published, whether any of them was released, whether it wrote them at all
// - a batch past the publisher hold's deadline is left unrecorded - and, when it wrote, whether
// the hold was still this process's.
type settlement struct {
	published    int
	released     bool
	wrote, holds bool
}

// outcomeState is what an event of a batch came to.
type outcomeState string

const (
	// published: every destination of the event is reached.
	published outcomeState = "published"
	// backedOff: the event failed, and waits retry.Delay of its attempts before it is tried again.
	backedOff outcomeState = "backed_off"
	// blocked: the event reached what it could, and a destination of it waits on an earlier event
	// of its owner that has not reached it, or a session's copy of it was held back for a mention
	// still being sent (destinations.leftToMention), or both. It falls due when the earliest of
	// its blockers does, and mentionPendingDelay after the record at the latest when a copy waits
	// on a mention, with no attempt counted, and its sends then follow its blockers'. Nothing
	// failed, and an undecided send is bounded by its sender's claim, so waiting on a mention
	// cannot run the event to its dead-letter count.
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
	// blocker was backed off by the same batch or whose mention is still being sent; until is when
	// a blocked one whose blocker was not due falls due, on the database's clock. A blocked event
	// falls due at the earlier of the two.
	delay *time.Duration
	until *time.Time
}

// settleEvent returns what event, sent by its batch, came to. An event that could not be read is
// backed off. One with a send that met a lost connection or the process stopping is released, and
// so is one whose destinations could not all be resolved because the process is stopping
// (batchEvent.resolveLost); releaseUnacknowledged releases one whose sends NATS did not acknowledge
// too. One with a send that met the hold's deadline settles as released as well, but its batch is
// left unrecorded, not released (scanBatch): that outcome is never written. One whose destinations
// could not all be resolved otherwise, or a send of which NATS answered with a failure, is backed
// off. Otherwise one with a destination held back, or a session's copy held back for a mention
// still being sent (batchEvent.mentionPending), is blocked: on its blockers (holdBack), and for
// mentionPendingDelay at most when its copy waits on a mention; and one with neither is published.
// A held-back copy so blocks the event only when no send of it failed or was lost: a denied or
// failed send backs the event off with its attempt counted, and a lost one releases it, as they
// would without the mention.
func settleEvent(event *batchEvent, releaseUnacknowledged bool) outcome {
	result := outcome{id: event.event.ID}
	if event.failure != nil {
		return backOff(result, event.pendingEvent, event.failureLog, event.failure)
	}
	var failures []error
	if event.resolveErr != nil {
		failures = append(failures, event.resolveErr)
	}
	release := event.resolveLost
	blockers := slices.Clone(event.held)
	for _, send := range event.sends {
		switch send.state {
		case reached:
			result.reached = append(result.reached, send.dest.subject)
		case failed:
			if releaseUnacknowledged && send.connection {
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
	case len(blockers) > 0 || event.mentionPending:
		if len(blockers) > 0 {
			result = holdBack(result, blockers)
		} else {
			slog.Debug("dispatch outbox: event waits on a mention's delivery", "event_id", event.event.ID)
			result.state = blocked
		}
		if event.mentionPending && result.state == blocked && (result.delay == nil || mentionPendingDelay < *result.delay) {
			result.delay = new(mentionPendingDelay)
		}
		return result
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
	var past []outcome
	for _, owner := range owners {
		w := windows[owner]
		if w.due || w.earliest == nil || len(w.read) < w.size {
			continue
		}
		for _, id := range w.candidates {
			if _, ok := w.read[id]; !ok {
				past = append(past, outcome{id: id, state: blocked, until: w.earliest})
			}
		}
	}
	return past
}

// recordBatch writes what each event of a batch came to, in one statement and so all of it or
// none, and reports whether holder still held the publisher hold when it did. What is true whoever
// holds the hold is written regardless: the destinations an event reached are added to its
// published_destinations, and an event that reached every destination is marked published, its
// publish_error cleared. A back-off (next_attempt_at on the database's clock, the attempt count and
// publish_error), a hold-back (next_attempt_at alone, the earlier of its delay out and its until)
// and a release are written only while the hold is holder's and has not lapsed, checked in the
// same statement, so a process that lost the hold mid-batch schedules nothing beside the holder
// that took it. Each row is guarded on published_at is null.
//
// A write that fails leaves the events it covered (one owner's, or the batch end's remainder) as
// they were read, which is what a process that dies between NATS's acknowledgements and this write
// leaves too: they are sent again by the next scan or the next holder. JetStream drops each repeat
// inside its duplicate window by its MsgId, a role forward's included (roleForward.send), and a
// role's holder drops a repeat forward by its dedupe key (internal/contracts/dedupe.go).
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
	if err := deps.lanes.statement(func() error {
		return deps.Store.Pool.QueryRow(ctx, `
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
	`, publisherHold, holder, ids, states, attempts, delays, until, failures, reachedIDs, reachedTopics).Scan(&holds)
	}); err != nil {
		return false, fmt.Errorf("record the batch's outcome: %w", err)
	}
	return holds, nil
}

// resolveDestinations gives every event of a batch that has an owner and can be read its envelope
// and destinations (eventDestinations), reading the followers, authors, recipients and mention
// deliveries all of them need at once (loadLookups), a not-due event's as a due one's: its
// destinations are what it holds its owner's later events back from (planChains). An event whose
// envelope cannot be built has no destination, and the error backs it off. A lookup that failed
// because ctx ended - a recipient read the process's stop cancelled (loadLookups) - marks its event
// resolveLost, which releases it as a send the stop cut off does (settleEvent). An event whose list
// ends at a recipient read that failed is incomplete (unansweredRecipients, batchEvent.incomplete).
func resolveDestinations(ctx context.Context, deps Deps, events []*batchEvent) error {
	var resolvable []*batchEvent
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
		resolvable = append(resolvable, event)
		_, _, _ = eventDestinations(event.event, item.Topic, event.routing, want)
	}
	// The lookups' statements run one after another, and the gate is held from the first to the
	// last (lanes.statement), so no other batch's statement is run between two of them.
	var found *lookups
	if err := deps.lanes.statement(func() error {
		var err error
		found, err = loadLookups(ctx, deps.Store.Pool, want)
		return err
	}); err != nil {
		return err
	}
	for _, event := range resolvable {
		event.destinationsOf, event.mentionPending, event.resolveErr = eventDestinations(event.event, event.item.Topic, event.routing, found)
		event.resolveLost = event.resolveErr != nil && ctx.Err() != nil && errors.Is(event.resolveErr, ctx.Err())
		var unanswered *unansweredRecipients
		event.incomplete = errors.As(event.resolveErr, &unanswered)
	}
	return nil
}

// destinations collects an event's destinations, each subject once: a subject an earlier route
// already reaches is not added again. mentions is what the event's mentions delivered (a new
// comment's alone), and mentionPending whether a session's copy was held back for a mention still
// being sent (leftToMention).
type destinations struct {
	list           []destination
	seen           map[string]struct{}
	mentions       mentionDeliveries
	mentionPending bool
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

// leftToMention is the one rule every session route of a new comment applies to a session the
// comment's mentions resolved to: it reports whether the route leaves that session's copy to the
// mention. A mention that reached the session delivered the comment, so the copy is skipped. A
// mention still being sent may yet, so the copy is held back and the event marked mentionPending,
// to be decided again when it is next read (settleEvent blocks it). Any other session - every
// attempt failed or was abandoned, or none was recorded - is published to as if it had not been
// mentioned. Callers ask only about a session their payload names as mentioned; a session no
// mention delivers to is in neither set, so it is never skipped.
func (d *destinations) leftToMention(sessionID string) bool {
	if _, ok := d.mentions.sent[sessionID]; ok {
		return true
	}
	if _, ok := d.mentions.pending[sessionID]; ok {
		d.mentionPending = true
		return true
	}
	return false
}

// eventDestinations lists event's destinations in the order they are sent and recorded: its
// owner's topic (ownerTopic), then - for an event that notifies and is not a targeted message - its
// owner's route, its claimant route, its primary document's sessions and its author routes, then
// its follower routes and the previous claimant's notice. mentionPending reports a session's copy
// held back for a mention still being sent (destinations.leftToMention). The mention deliveries of
// a new comment whose mentions resolved to a session are read before anything else, so a lookup of
// them that fails leaves the event with no destination. Any other lookup that fails, or a route
// that does not parse, ends the list there: the destinations before it are returned with the
// error, which backs the event off once they are sent. So a recipient read the batch has no answer
// for (unansweredRecipients) still lets the owner topic, the route and the claimant go first.
func eventDestinations(event model.Event, ownerTopic string, routing routing, look lookup) (list []destination, mentionPending bool, err error) {
	d := &destinations{seen: map[string]struct{}{}}
	if event.Type == "comment.created" && len(payloadMentionedSessions(event.Payload)) > 0 {
		if d.mentions, err = look.mentionDeliveries(payloadString(event.Payload, "id")); err != nil {
			return nil, false, err
		}
	}
	d.add(ownerTopic, "publish owner topic", ownerTopic)
	targeted := (event.Type == "message.created" || event.Type == "message.answered") &&
		payloadString(event.Payload, "target") != ""
	if event.Notify && !targeted {
		if !suppressesCurrentRoute(event.Payload, routing.route, d) {
			if err := addRoute(d, routing.route); err != nil {
				return d.list, d.mentionPending, err
			}
		}
		addClaimantRoute(event, routing.claimant, d)
		if err := addPrimaryDocumentAuthors(event, routing, look, d); err != nil {
			return d.list, d.mentionPending, err
		}
		if err := addAuthorRoutes(event, look, d); err != nil {
			return d.list, d.mentionPending, err
		}
	}
	if err := addFollowerRoutes(event, look, d); err != nil {
		return d.list, d.mentionPending, err
	}
	addPreviousClaimant(event, d)
	return d.list, d.mentionPending, nil
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

// addClaimantRoute delivers a person's answer, comment or untargeted message on an issue to
// claimant, the session that holds the issue's claim, on its own topic. Otherwise it reaches the
// claimant only when the claimant subscribed to the whole issue, or happens to be the route, a
// follower of the ask, or an author the thread or document names, and the session working the
// issue misses what a person told it there. Only a person's event qualifies (Actor.Kind "user"):
// an agent's events reach nobody new through here, even the ones that notify. A new comment whose
// mentions resolved to the claimant is left to the mention once the mention delivered it
// (destinations.leftToMention); a claimant whose mention failed gets this copy instead, and the
// comment's edit, resolution and reopening are never left to it. The claim is the one the batch
// read when the event is published (readBatch, routing), not when it was written, so a retry after
// the claim moved reaches the new claimant. A claimant that is also the route, a follower or an
// author gets one copy: destinations adds a subject once per event. The claimant's subject is an
// ordinary agent subject, so its sends share the chain of every other route to that session
// (destinationKey).
func addClaimantRoute(event model.Event, claimant string, d *destinations) {
	// A project document's event names no issue, so there is no claim to read.
	if event.IssueKey == nil || event.Actor.Kind != "user" || claimant == "" {
		return
	}
	switch event.Type {
	case "ask.answered", "ask.handed_back", "ask.resolved",
		"comment.created", "comment.edited", "comment.resolved", "comment.reopened",
		"message.created", "message.answered":
	default:
		return
	}
	if event.Type == "comment.created" {
		if _, mentioned := payloadMentionedSessions(event.Payload)[claimant]; mentioned && d.leftToMention(claimant) {
			return
		}
	}
	d.addSessions([]string{claimant}, "publish claimant route to")
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
// actor is skipped, and so is a follower a new reply's mentions resolved to
// (payloadMentionedSessions) once the mention delivered the reply to it
// (destinations.leftToMention); a follower whose mention failed gets this copy instead. Only the
// reply's creation is left to the mention; its edit, resolution and reopening reach every
// follower. A topic the route already reaches is not added twice.
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
	var mentioned map[string]struct{}
	if event.Type == "comment.created" {
		mentioned = payloadMentionedSessions(event.Payload)
	}
	var sessionIDs []string
	for _, follower := range followers {
		if event.Actor.SameAs(model.Actor{Kind: "session", ID: follower}) {
			continue
		}
		if _, ok := mentioned[follower]; ok && d.leftToMention(follower) {
			continue
		}
		sessionIDs = append(sessionIDs, follower)
	}
	d.addSessions(sessionIDs, "publish follower route to")
	return nil
}

// addPrimaryDocumentAuthors delivers a person's comment, or its edit, on an issue with no route and
// no session claimant (takesPrimaryAuthors) to every session the issue's primary document credits,
// each on its own topic: a session that writes an issue's spec without a claim still hears the
// comments on it. The comment's own thread does not matter: a comment in an ask's thread, or a
// reply, reaches those sessions too. The batch reads them for all such events in one query
// (loadLookups, loadPrimaryDocumentAuthors); when that query failed, its error ends the event's
// destinations here. The actor is skipped, and so is a session the comment's mentions resolved to
// (payloadMentionedSessions) once its mention delivered the comment (recipientFilter).
func addPrimaryDocumentAuthors(event model.Event, routing routing, look lookup, d *destinations) error {
	if !takesPrimaryAuthors(event, routing) {
		return nil
	}
	authors, err := look.primaryDocumentAuthors(*event.IssueKey)
	if err != nil {
		return err
	}
	filter := newRecipientFilter(event, d)
	var targets []string
	for _, sessionID := range authors {
		if filter.keep(sessionID) {
			targets = append(targets, sessionID)
		}
	}
	d.addSessions(targets, "publish primary document author route to")
	return nil
}

// addAuthorRoutes delivers a human comment-thread action, or a person's settled edit of a
// document, directly to the involved sessions' own topics, regardless of the issue's route (which
// may point at an entirely different reviewer) and regardless of whether the event's owner is an
// issue or a project document (which has no route at all). Without this, the sessions in a thread
// never learn of the human's reply, resolution, reopening, or edit, and the agent that wrote a
// document never learns of a new comment on it or of a person's edit to it.
//
// A reply (reply_to set), and its edit, reach every session that wrote a comment in the thread,
// its root and each reply, then every session the thread's document credits, which the batch reads
// for all its replies in one query (loadLookups, loadCommentRecipients); when that query failed,
// its error ends the event's destinations here. A new comment on a document (no reply_to and no
// ask_id), and its edit, reach every session that document credits (loadDocumentSessionAuthors,
// whose failed query ends the event's destinations here too); a new comment on an issue's
// conversation names no document and reaches no author. A resolution or a reopening reaches the
// thread root's author only, so the reopening a reply makes tells no new session twice.
// artifact.human_edited, which the notice sweep appends once a person's quiet window on a document
// ends (docs.sendEditNotice), reaches every session the document its payload's artifact_id names
// credits, read the same way. Ask threads are not walked here: their asker and repliers are the
// ask's followers (addFollowerRoutes). The event's own actor is skipped, and so is a session the
// comment's mentions resolved to (payloadMentionedSessions) once its mention delivered the comment
// (recipientFilter); an author whose mention failed is added instead.
func addAuthorRoutes(event model.Event, look lookup, d *destinations) error {
	filter := newRecipientFilter(event, d)
	var targets []string
	consider := func(sessionID string) {
		if filter.keep(sessionID) {
			targets = append(targets, sessionID)
		}
	}
	considerLoaded := func(author model.Actor, found bool, err error) error {
		if err != nil {
			return err
		}
		if found && author.Kind == "session" {
			consider(author.ID)
		}
		return nil
	}
	considerSessions := func(sessionIDs []string, err error) error {
		if err != nil {
			return err
		}
		for _, sessionID := range sessionIDs {
			consider(sessionID)
		}
		return nil
	}

	switch event.Type {
	case "comment.created", "comment.edited":
		switch replyTo := payloadString(event.Payload, "reply_to"); {
		case payloadString(event.Payload, "ask_id") != "":
			// An ask's thread reaches the ask's followers (addFollowerRoutes).
		case replyTo != "":
			if err := considerSessions(look.commentRecipients(replyTo)); err != nil {
				return err
			}
		default:
			if document := commentedDocument(event); document != "" {
				if err := considerSessions(look.documentAuthors(document)); err != nil {
					return err
				}
			}
		}
	case "comment.resolved", "comment.reopened":
		// New threads persist a reply's reply_to as the thread root. Walking from the comment
		// still finds the root for legacy nested rows.
		if commentID := payloadString(event.Payload, "id"); commentID != "" {
			if err := considerLoaded(look.rootCommentAuthor(commentID)); err != nil {
				return err
			}
		}
	case "message.created", "message.answered":
		if inReplyTo := payloadString(event.Payload, "in_reply_to"); inReplyTo != "" {
			if err := considerLoaded(look.messageAuthor(inReplyTo)); err != nil {
				return err
			}
		}
	case "subscription.removed", "ask.follower_added", "ask.follower_removed":
		// The target is the unsubscribed, added, or removed session itself, carried directly
		// in the payload — there is no thread or ask to walk to find it.
		if sessionID := payloadString(event.Payload, "session_id"); sessionID != "" {
			consider(sessionID)
		}
	case "artifact.human_edited":
		if document := payloadString(event.Payload, "artifact_id"); document != "" {
			if err := considerSessions(look.documentAuthors(document)); err != nil {
				return err
			}
		}
	default:
		return nil
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

// envelope builds the bus envelope of one event. Its payload is the event with one field the
// event log does not hold: `quiet` (events.Quiet), whether the event carries nothing for a
// reader following its owner topic. A reader takes it as the whole answer, where `notify` says
// only whether the event wakes the route, so an agent's comment on an issue reaches the sessions
// that subscribed to that issue's topic.
func envelope(event model.Event, slug string) (contracts.Envelope, error) {
	payload, err := json.Marshal(struct {
		model.Event
		Quiet bool `json:"quiet"`
	}{Event: event, Quiet: events.Quiet(event)})
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
// edit to any other target must still wake its new destination. The suppression names the session
// the route resolved to when the comment was written (suppressed_route_session_id), and holds only
// once the mention delivered the comment to it (destinations.leftToMention): a route whose
// session's mention failed is published to.
func suppressesCurrentRoute(payload any, current *string, d *destinations) bool {
	if !payloadBool(payload, "suppress_route") || current == nil || *current == "" {
		return false
	}
	sessionID := payloadString(payload, "suppressed_route_session_id")
	if sessionID == "" {
		return false
	}
	if *current != payloadString(payload, "suppressed_route") && *current != "session:"+sessionID {
		return false
	}
	return d.leftToMention(sessionID)
}

// payloadMentionedSessions reads the sessions a comment payload's mentions resolved to (each
// mentions[].session_id), as the batch decodes the payload from JSON. A mention that resolved to
// no session (session_id null) names none. These are candidates only: a session route skips one
// once the mention delivered to it (destinations.leftToMention).
func payloadMentionedSessions(payload any) map[string]struct{} {
	values, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	mentions, _ := values["mentions"].([]any)
	result := map[string]struct{}{}
	for _, entry := range mentions {
		if sessionID := payloadString(entry, "session_id"); sessionID != "" {
			result[sessionID] = struct{}{}
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
