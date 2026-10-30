// Package outbox publishes durable Dispatch events to NATS.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/metrics"
)

const (
	batchSize           = 100
	retryInterval       = 5 * time.Second
	compactInterval     = 24 * time.Hour
	documentTopicPrefix = "notifications.dispatch.document."
	// claimWindow is how long scanPendingEvents' claim keeps an event out of every other
	// process's batch. Two envoy-dispatch processes serve one Postgres and one NATS, and a
	// plain select would hand both the same rows and publish every one of them twice. The
	// claim is one statement (select ... for update skip locked, then push next_attempt_at
	// out by this), never a transaction held open across the publishes that follow: an open
	// cursor or transaction holds its pooled connection, which is exactly what pendingEvent's
	// comment above scanBatch's drain exists to avoid.
	//
	// One minute: a batch is one window of publishes (scanBatch) whose answers each come within
	// the bus's five-second publish window, plus a handful of statements, so even a batch whose
	// every acknowledgement times out finishes well inside it, and a process that dies
	// mid-batch leaves its rows claimed for no longer than that before the other process
	// picks them up. A publish that fails is backed off (recordBatch), a write that
	// legitimately overwrites the claim with the retry's own (usually shorter) wait;
	// published_at, not this, is what marks an event done.
	//
	// What the claim gives, for a batch that finishes inside the window: two healthy processes
	// no longer publish one event twice, and each events row still reaches published_at exactly
	// once - the terminal write is guarded on published_at is null, so a second one changes no
	// row. A batch that overruns the window gets no such guarantee: claimPendingEvents commits
	// the claim, and scanBatch then publishes up to batchSize events outside any transaction,
	// re-checking nothing, so once the window lapses the other process claims the rows this one
	// has not reached yet - legitimately, by the same statement, their next_attempt_at being due
	// again - and publishes them a second time; the guarded write discards RowsAffected, so
	// neither process notices. Nor does the claim cover a process that dies between a successful
	// publish and its published_at write: it leaves the row claimed but unmarked, and the other
	// process republishes it once the window lapses. Either repeat is the same at-least-once on
	// the role lanes (notifications.role.*), which go over core NATS with no nats.MsgId on the
	// envelope (internal/bus/nats.go) and so carry no deduplication of their own; their receivers
	// already drop the repeat by its dedupe key (internal/contracts/dedupe.go). A JetStream
	// destination suppresses the repeat inside the stream's duplicate window. Neither is fixed
	// here.
	claimWindow = time.Minute
	// mentionPendingDelay is how long an event waits when a session route held its copy back
	// for a mention whose send is still undecided (errMentionPending): long enough for the
	// handler that holds the send to settle it, short enough that the copy follows a failed send
	// promptly. The retry ticker (retryInterval) is what picks the event up again, so in practice
	// it waits until the next tick after this delay. The wait is bounded: a pending attempt
	// counts as undecided only for model.DeliveryClaimLapse, the minute its sender's claim holds
	// (mentionDeliveryQuery).
	mentionPendingDelay = 2 * time.Second
	// maxCommentThreadDepth bounds the reply_to walks (loadRootCommentAuthor and
	// loadCommentRecipients) so a malformed cycle (comments.reply_to has no acyclicity
	// constraint) cannot spin the recursive query and stall the outbox scan.
	maxCommentThreadDepth = 32
	// recordTimeout bounds the write of a batch's outcome (recordBatch), which a stopping process
	// still makes after its context has ended: one statement, which a healthy database answers in
	// milliseconds, so a write that takes longer is a database the batch's claim is left to.
	recordTimeout = 5 * time.Second
)

// errMentionPending is what an attempt reports (attempt.err) when it failed nowhere but held back
// a session's copy of a new comment because the comment's mention of that session is still being
// sent: the mention may deliver it, or may fail and leave the copy to the outbox. Every other
// destination was sent. It is a deferral, not a failure: settle records the event deferred, to be
// tried again shortly without counting an attempt.
//
// The deferral holds back this one event, not the events after it. The same comment's
// comment.edited, comment.resolved and comment.reopened are events of their own and publish at
// once, so the held-back session's topic - as claimant, follower, document author or route - can
// receive the edit before the creation: for as long as the mention's send stays undecided,
// normally the listener's receipt timeout and at most model.DeliveryClaimLapse for an attempt
// nobody finishes, plus mentionPendingDelay. A host that receives an edit, resolution or
// reopening for a comment it has not seen is seeing this window, not a bug; the creation follows.
var errMentionPending = errors.New("a mention's delivery is still undecided")

// Publisher is the NATS publication boundary used by the Dispatch outbox.
type Publisher interface {
	// Window starts one batch's sends: each is sent without waiting for NATS's answer, all on one
	// connection and in the order they were made, or not at all (bus.Client.Window). A send the
	// window has to make again leaves after the window's later sends, and a copy it sends without
	// its id is stored by the stream as a message of its own.
	Window() bus.Sender
}

// documentCompactionLease names the lease the daily document compaction runs under. It walks
// every document, so two envoy-dispatch processes compacting at the same moment is twice the work
// for one result.
const documentCompactionLease = "document-compaction"

// Deps are the durable state and delivery dependencies for the outbox.
type Deps struct {
	// Store is what the outbox reads and records events through. cmd/dispatch gives it a store on
	// a pool of its own (store.Pool.Outbox), so the requests queued on the shared pool never hold
	// a publish back.
	Store     *store.Store
	Publisher Publisher
	Broker    *events.Broker
	Docs      docs.API
	// Leases is where the compaction tick takes documentCompactionLease (compactDaily). It
	// is required of a Run that ticks: there is no nil-means-unleased path, and cmd/dispatch
	// exits at boot if the lease pool cannot be opened.
	Leases duty.Leases
	// PublishFailures counts a destination of a publish attempt that did not reach NATS, by
	// reason (countPublishFailure). cmd/dispatch registers it on the process's metrics
	// registry, so a denied or refused destination shows on /metrics rather than only in the
	// log.
	PublishFailures *metrics.Counter
}

// countPublishFailure counts one destination of a publish attempt that did not reach NATS, under
// reason ("denied", "refused" or "failed"). Every increment goes through here: a Deps built
// without a counter - the package's own tests - then counts nothing instead of dereferencing nil.
func (d Deps) countPublishFailure(reason string) {
	if d.PublishFailures == nil {
		return
	}
	d.PublishFailures.Inc([2]string{"reason", reason})
}

// Run drains unpublished events at startup, after committed local events, and
// periodically so a dropped in-process notification cannot strand an event. It
// returns when ctx is cancelled.
func Run(ctx context.Context, deps Deps) {
	// Started before the publisher marks its context, so the compaction carries a marker of its
	// own: a connection it holds must never count as one the publisher holds.
	go compactDaily(ctx, deps)
	// The publisher is one of the process's transaction openers, so its context carries the
	// pool's marker: a read it ever makes while one of its transactions is open is refused
	// rather than left to deadlock the pool (store.ErrNestedAcquire).
	ctx = store.WithTransactionTracking(ctx)
	events, unsubscribe := deps.Broker.Subscribe()
	defer func() { unsubscribe() }()

	scan(ctx, deps)
	retry := time.NewTicker(retryInterval)
	defer retry.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-events:
			if !open {
				unsubscribe()
				events, unsubscribe = deps.Broker.Subscribe()
				continue
			}
			scan(ctx, deps)
		case <-retry.C:
			scan(ctx, deps)
		}
	}
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

func scan(ctx context.Context, deps Deps) {
	for {
		count, blocked, err := scanBatch(ctx, deps)
		if err != nil {
			slog.Error("dispatch outbox: scan", "error", err)
			return
		}
		if count < batchSize || blocked {
			return
		}
	}
}

// pendingEvent is one row of a scan, read out before anything is published: an open cursor
// holds its pooled connection until it closes, and publishing a batch reads followers and
// authors through the same pool, so the rows are drained first and the work done after.
type pendingEvent struct {
	event        model.Event
	attempts     int
	destinations []string
	slug         string
	routing      routing
	// threadSessions are the sessions a person's reply reaches through its thread
	// (loadCommentRecipients), read for the whole batch by resolveCommentRecipients and filtered by
	// publishAuthorRoutes.
	threadSessions recipientList
	// primaryAuthors are the sessions the primary document of the event's issue credits, which a
	// person's comment reaches when the issue has no route and no session claimant, read for the
	// whole batch by resolveCommentRecipients and filtered by publishPrimaryDocumentAuthors.
	primaryAuthors recipientList
	// failure is why the row cannot be published as it was read - it did not scan, or its
	// actor or payload JSON did not decode - and failureLog is the predicate it reports under.
	// The batch backs such an event off instead of publishing it. A recipient query that failed
	// is no failure of the row's (recipientList).
	failure    error
	failureLog string
}

// routing is what the scan read off the event's issue when it publishes, not what it was when the
// event was written: the issue's route, and the session holding its claim ("" when the issue is
// unclaimed or a person holds it, since a person has no Envoy topic). Every scan reads them again,
// so a retry after the route or the claim moved reaches the new destination.
type routing struct {
	route    *string
	claimant string
}

// scanBatch reads a batch and publishes it as one window (bus.Client.Window). Every event's
// envelope is sent to each of its destinations, in id order and each event's destinations in
// turn, before any answer is waited for, so a batch costs one NATS round trip rather than one per
// destination, and NATS takes the envelopes in the order they were sent: on the stream, on the role
// lanes and on each issue's subject, a batch's events arrive in id order. A send the window has to
// make again is the exception: it leaves after the window's later sends (bus.Client.Window). Then
// every answer is read, and what the batch came to is written in one statement (recordBatch). An
// event is marked published only once NATS accepted each of its destinations; one that failed is
// backed off, so the events queued behind it are not stuck behind it; and one that held a
// session's copy back for a mention still being sent (errMentionPending) is deferred, its other
// destinations recorded.
//
// A failure of the connection rather than the envelope (bus.ErrUnavailable: no connection, one
// lost or reconnected mid-batch, no answer at all) is no fault of its event. The window sends
// nothing on a connection that came back, so when the connection fails, what of the batch reached
// NATS is a prefix of it: no event after the first send that met the lost connection is sent, and
// every event that met the failure, or was not sent, is released rather than backed off, so the
// next scan sends them again in id order, and the stream's MsgId drops the copies of those that
// had landed, except a copy the window sent again without its id, which the stream stored as a
// message of its own. An event backed off instead would reach its issue's subject after the later
// events of that issue, which the Legion daemon then drops as stale by their sequence. The
// guarantee covers a failed connection only: an answer that times out, or no responders, on a
// connection still current stops no later send, so the released event can land after a later
// event of its issue that NATS did store.
func scanBatch(ctx context.Context, deps Deps) (int, bool, error) {
	pending, blocked, err := scanPendingEvents(ctx, deps)
	if err != nil {
		return 0, false, err
	}
	unavailable, err := publishBatch(ctx, deps, pending)
	if err != nil {
		return len(pending), true, err
	}
	return len(pending), blocked || unavailable, nil
}

// publishBatch is scanBatch's work after the read: it reads the batch's comment recipients
// (resolveCommentRecipients), sends every event through one window, settles each and records what
// the batch came to (recordBatch). It reports whether NATS was unreachable, which holds the rest of
// the backlog for the next scan, and returns the record's error.
func publishBatch(ctx context.Context, deps Deps, pending []pendingEvent) (bool, error) {
	resolveCommentRecipients(ctx, deps, pending)

	sender := deps.Publisher.Window()
	attempts := make([]*attempt, len(pending))
	for index, item := range pending {
		if ctx.Err() != nil {
			break
		}
		if item.failure != nil {
			continue
		}
		attempts[index] = send(ctx, deps, sender, item, publishedDestinationSet(item.destinations))
		if errors.Is(attempts[index].ended, bus.ErrUnavailable) {
			break
		}
	}
	outcomes := make([]outcome, len(pending))
	unavailable := false
	for index, item := range pending {
		outcomes[index] = settle(ctx, item, attempts[index])
		if outcomes[index].state == released && !unavailable && ctx.Err() == nil {
			slog.Error("dispatch outbox: NATS is unreachable, so the batch from this event on waits for the next scan",
				"event_id", item.event.ID)
			unavailable = true
		}
	}
	// Written even when ctx has ended: a process stopping mid-batch records what NATS already
	// accepted and releases the rest, rather than leave the batch claimed for claimWindow.
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := recordBatch(record, deps, outcomes); err != nil {
		return false, err
	}
	return unavailable, nil
}

// outcomeState is what an event of a batch came to.
type outcomeState string

const (
	// published: NATS accepted every destination of the event.
	published outcomeState = "published"
	// backedOff: the event failed, and waits retry.Delay of its attempts before it is tried again.
	backedOff outcomeState = "backed_off"
	// released: the event was not tried to the end, because NATS was unreachable or the process
	// is stopping, so its claim is cleared and no attempt is counted.
	released outcomeState = "released"
	// deferred: the event reached every destination but a session's copy held back for a mention
	// still being sent (errMentionPending), and waits mentionPendingDelay before it is tried
	// again, published_at null and no attempt counted: nothing failed, and an undecided send is
	// bounded by its sender's claim, so the deferral cannot run the event to its dead-letter count.
	deferred outcomeState = "deferred"
)

// outcome is what one event of a batch came to, as recordBatch writes it.
type outcome struct {
	id    int64
	state outcomeState
	// reached is every destination the attempt reached, in the order it sent them.
	reached []string
	// attempts is the backed-off event's attempt count; retryAt is the backed-off or deferred
	// event's next attempt.
	attempts int
	retryAt  time.Time
}

// settle waits for the answers to item's attempt (nil when it was never sent) and returns what the
// event came to.
func settle(ctx context.Context, item pendingEvent, a *attempt) outcome {
	result := outcome{id: item.event.ID}
	if item.failure != nil {
		return backOff(result, item, item.failureLog, item.failure)
	}
	if a == nil {
		result.state = released
		return result
	}
	a.wait(ctx)
	result.reached = a.reached
	err := a.err()
	switch {
	case err == nil:
		result.state = published
		return result
	// A held-back copy defers the event only when it is the whole of why the attempt did not
	// finish: err is errMentionPending itself, not joined with a denial, a failure or the error
	// that ended the attempt. An attempt that also met one of those is that error's, as any
	// failed attempt is - released below, or backed off with its attempt counted - so a denied
	// destination is never retried every mentionPendingDelay with no attempt counted, and the
	// destinations it did reach are recorded either way.
	case err == errMentionPending:
		slog.Debug("dispatch outbox: event waits on a mention's delivery", "event_id", item.event.ID)
		result.state = deferred
		result.retryAt = time.Now().Add(mentionPendingDelay)
		return result
	// The connection failing, or the process stopping before every answer came, is no failure of
	// the event's own.
	case errors.Is(err, bus.ErrUnavailable) || ctx.Err() != nil:
		slog.Info("dispatch outbox: event released", "event_id", item.event.ID, "error", err)
		result.state = released
		return result
	default:
		return backOff(result, item, "dispatch outbox: publish event", err)
	}
}

// backOff reports item's failure under what and returns result backed off by retry.Delay of the
// attempt it now counts.
func backOff(result outcome, item pendingEvent, what string, cause error) outcome {
	slog.Error(what, "event_id", item.event.ID, "error", cause)
	result.state = backedOff
	result.attempts = item.attempts + 1
	if result.attempts == retry.DeadLetterAttempts {
		slog.Error("dispatch outbox: event reached dead-letter threshold", "event_id", item.event.ID, "attempts", result.attempts)
	}
	result.retryAt = time.Now().Add(retry.Delay(result.attempts))
	return result
}

// recordBatch writes what each event of a batch came to, in one statement and so all of it or
// none: the destinations an event reached are added to its published_destinations, and it is
// marked published, backed off, deferred, or released (its claim cleared). Each row is guarded on
// published_at is null, like every terminal write here. A write that fails leaves the whole batch
// claimed and unmarked, which is what a process that dies between NATS's acknowledgements and this
// write leaves too: the batch is published again once claimWindow lapses. JetStream drops each
// repeat inside its duplicate window by its MsgId; a role lane, over core NATS, carries the repeat,
// and its receivers drop it by its dedupe key (internal/contracts/dedupe.go).
func recordBatch(ctx context.Context, deps Deps, outcomes []outcome) error {
	if len(outcomes) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(outcomes))
	states := make([]string, 0, len(outcomes))
	attempts := make([]*int32, 0, len(outcomes))
	retryAt := make([]*time.Time, 0, len(outcomes))
	var reachedIDs []int64
	var reachedTopics []string
	for _, result := range outcomes {
		ids = append(ids, result.id)
		states = append(states, string(result.state))
		switch result.state {
		case backedOff:
			attempts = append(attempts, new(int32(result.attempts)))
			retryAt = append(retryAt, new(result.retryAt))
		case deferred:
			attempts = append(attempts, nil)
			retryAt = append(retryAt, new(result.retryAt))
		default:
			attempts = append(attempts, nil)
			retryAt = append(retryAt, nil)
		}
		for _, topic := range result.reached {
			reachedIDs = append(reachedIDs, result.id)
			reachedTopics = append(reachedTopics, topic)
		}
	}
	if _, err := deps.Store.Pool.Exec(ctx, `
		with outcome as (
			select * from unnest($1::bigint[], $2::text[], $3::int[], $4::timestamptz[])
				as o(id, state, attempts, retry_at)
		), reached as (
			select id, array_agg(topic order by position) as topics
			from unnest($5::bigint[], $6::text[]) with ordinality as r(id, topic, position)
			group by id
		)
		update events e set
			published_destinations = e.published_destinations || array(
				select t.topic from unnest(r.topics) with ordinality as t(topic, position)
				where t.topic <> all(e.published_destinations)
				order by t.position
			),
			published_at = case when o.state = 'published' then now() end,
			attempt_count = coalesce(o.attempts, e.attempt_count),
			next_attempt_at = o.retry_at
		from outcome o
		left join reached r on r.id = o.id
		where e.id = o.id and e.published_at is null
	`, ids, states, attempts, retryAt, reachedIDs, reachedTopics); err != nil {
		return fmt.Errorf("record the batch's outcome: %w", err)
	}
	return nil
}

// claimPendingEvents claims at most batchSize eligible events, lowest id first, and returns their
// ids: one statement takes them under for update skip locked, so a second process's concurrent
// scan skips them rather than waiting on them, and pushes their next_attempt_at out by
// claimWindow in the same breath. It locks rows of events alone - the issue and artifact columns
// a publish needs are read by scanPendingEvents afterwards, from the ids this returns, because a
// statement that both locks `for update` and names issues or artifacts is refused by the room
// lock's rule (TestNoForUpdateOnOwnerTables in internal/dispatch/api). The claimed rows are this
// process's for the window whether or not that read succeeds, so the split costs a round trip
// and nothing else: a read that fails leaves them claimed until the window lapses, exactly as a
// process dying mid-batch does.
func claimPendingEvents(ctx context.Context, deps Deps) ([]int64, error) {
	rows, err := deps.Store.Pool.Query(ctx, `
		update events set next_attempt_at = now() + $2
		where id in (
			select id from events
			where published_at is null
			  and (next_attempt_at is null or next_attempt_at <= now())
			  and coalesce(payload ->> 'pending', 'false') <> 'true'
			order by id
			limit $1
			for update skip locked
		)
		returning id
	`, batchSize, claimWindow)
	if err != nil {
		return nil, fmt.Errorf("claim unpublished events: %w", err)
	}
	// CollectRows closes the cursor, which the read below needs: a pooled connection held open
	// by one query is a second connection this caller may not ask for (store.ErrNestedAcquire).
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("claim unpublished events: %w", err)
	}
	return ids, nil
}

// scanPendingEvents claims a batch (claimPendingEvents) and reads each claimed event with the
// issue and artifact columns a publish needs, in the order by id every batch has always had: the
// issue's route and claimant among them (routing), read here on every attempt.
func scanPendingEvents(ctx context.Context, deps Deps) ([]pendingEvent, bool, error) {
	claimed, err := claimPendingEvents(ctx, deps)
	if err != nil {
		return nil, false, err
	}
	if len(claimed) == 0 {
		return nil, false, nil
	}
	rows, err := deps.Store.Pool.Query(ctx, `
		select e.id, e.issue_key, e.artifact_id::text, coalesce(e.project_key, i.project_key, ar.project_key, ''), e.seq,
		       e.type, e.actor, e.notify, e.created_at, e.payload, e.attempt_count, e.published_destinations,
		       coalesce(ar.slug, ''), coalesce(i.route, ai.route),
		       coalesce(case when i.claimed_by ->> 'kind' = 'session' then i.claimed_by ->> 'id' end, '')
		from events e
		left join issues i on i.key = e.issue_key
		left join artifacts ar on ar.id = e.artifact_id
		left join issues ai on ai.key = ar.issue_key
		where e.id = any($1)
		order by e.id
	`, claimed)
	if err != nil {
		return nil, false, fmt.Errorf("read claimed events: %w", err)
	}
	defer rows.Close()

	var pending []pendingEvent
	unreadable := false
	for rows.Next() {
		var item pendingEvent
		var actor, payload []byte
		if err := rows.Scan(
			&item.event.ID,
			&item.event.IssueKey,
			&item.event.ArtifactID,
			&item.event.Project,
			&item.event.Seq,
			&item.event.Type,
			&actor,
			&item.event.Notify,
			&item.event.CreatedAt,
			&payload,
			&item.attempts,
			&item.destinations,
			&item.slug,
			&item.routing.route,
			&item.routing.claimant,
		); err != nil {
			// pgx scans positionally and treats a failure as fatal to the cursor, so this
			// row ends the batch and rows.Err() below repeats the same error. Carrying the
			// row on as a failure backs it off like any other unpublishable event, which is
			// what lets the events queued behind it be read at all; e.id is the first
			// destination, so the row scanned that far before it failed. The rest of the
			// batch is claimed (claimPendingEvents) and is never read here, so its claim
			// is cleared below instead of left to lapse, and this row is backed off
			// rather than left to poison every batch after it too.
			item.failure = fmt.Errorf("read event: %w", err)
			item.failureLog = "dispatch outbox: read event"
			unreadable = true
			pending = append(pending, item)
			continue
		}
		if err := json.Unmarshal(actor, &item.event.Actor); err != nil {
			item.failure = fmt.Errorf("decode event actor: %w", err)
			item.failureLog = "dispatch outbox: decode event actor"
		} else if err := json.Unmarshal(payload, &item.event.Payload); err != nil {
			item.failure = fmt.Errorf("decode event payload: %w", err)
			item.failureLog = "dispatch outbox: decode event payload"
		}
		pending = append(pending, item)
	}
	// Closed before the statement below rather than at the return: a cursor still open holds
	// one of the pool's connections, and a caller asking for a second is refused
	// (store.ErrNestedAcquire). Close is idempotent, so the deferred one above still stands.
	rows.Close()
	if err := rows.Err(); err != nil && !unreadable {
		return nil, false, fmt.Errorf("iterate unpublished events: %w", err)
	}
	// A row that fails to scan kills the cursor, so the claimed ids behind it are never read,
	// never published and never backed off: without this they would sit out the whole
	// claimWindow, a minute argued from how long a healthy batch of 100 takes (claimWindow),
	// where before the claim existed the next scan tick re-selected them retryInterval later.
	// Driven by the ids actually read, because scanBatch's loop reaches every item returned
	// here: a row the batch reached and backed off keeps the backoff recordBatch wrote for it,
	// and a row it published keeps its published_at. The paths above that return an error
	// release nothing and leave the claim standing, as claimPendingEvents describes.
	if len(pending) < len(claimed) {
		releaseClaim(ctx, deps, unreachedIDs(claimed, pending))
	}
	return pending, unreadable, nil
}

// unreachedIDs returns the claimed ids missing from pending: those a cursor that died mid-scan
// never produced, and so the ones no part of the batch touched.
func unreachedIDs(claimed []int64, pending []pendingEvent) []int64 {
	read := make(map[int64]struct{}, len(pending))
	for _, item := range pending {
		read[item.event.ID] = struct{}{}
	}
	unreached := make([]int64, 0, len(claimed)-len(pending))
	for _, id := range claimed {
		if _, ok := read[id]; !ok {
			unreached = append(unreached, id)
		}
	}
	return unreached
}

// releaseClaim clears the claim claimPendingEvents wrote on ids, so the next scan tick selects
// them again. Guarded on published_at is null like every other terminal write here, so a row
// another process published once the window lapsed keeps its mark. A failure is logged and
// nothing more: those rows then wait the claim out, which is what they did before this existed.
func releaseClaim(ctx context.Context, deps Deps, ids []int64) {
	if _, err := deps.Store.Pool.Exec(ctx, `
		update events set next_attempt_at = null where id = any($1) and published_at is null
	`, ids); err != nil {
		slog.Error("dispatch outbox: release unread claim", "event_ids", ids, "error", err)
	}
}

// send starts the attempt of pending's event: its envelope is sent to each of its destinations not
// in delivered (publishDestinations), and the attempt returned holds the answers still to come
// (attempt.wait). A destination NATS denies (a role route outside Dispatch's grant, say) holds back
// none of the others: it is left unreached, and the denial fails the attempt once every destination
// was tried, so the event is retried. Any other failure that comes back before anything is sent, of
// the connection or of the store (a recipient query of the batch that failed, recipientList, among
// them), ends the attempt at once, since every destination after it would fail the same way. A
// session's copy of a new comment held back for a mention still being sent
// (attempt.leftToMention) holds back none of the others either: the attempt is marked deferred and
// reports errMentionPending once every answer is in (attempt.err). The destinations the attempt
// reached are recorded with the batch (recordBatch), so a retry publishes only the rest.
func send(ctx context.Context, deps Deps, sender bus.Sender, pending pendingEvent, delivered map[string]struct{}) *attempt {
	a := &attempt{deps: deps, delivered: delivered, sender: sender}
	event := pending.event
	if event.IssueKey == nil && event.ArtifactID == nil && event.Project == "" {
		return a
	}
	item, err := envelope(event, pending.slug)
	if err != nil {
		a.ended = err
		return a
	}
	if err := item.Validate(); err != nil {
		a.ended = fmt.Errorf("validate envelope: %w", err)
		return a
	}
	mentions, err := loadMentionDeliveries(ctx, deps, event)
	if err != nil {
		a.ended = err
		return a
	}
	a.mentions = mentions
	a.ended = publishDestinations(ctx, deps, pending, item, a)
	return a
}

// publishDestinations sends item to each of the destinations of pending's event in turn, and
// returns the first error that ends the attempt. The order is fixed: the owner topic, the route,
// the claimant (publishClaimantRoute), the sessions of the issue's primary document when the issue
// has no route and no session claimant (publishPrimaryDocumentAuthors), the author routes, the
// ask's followers, and the session that lost the issue's claim. The route, the claimant, the
// primary document's sessions and the author routes are for an event that notifies
// (events.Broker.Notify) and is no targeted message. A topic is sent once, in its first place
// (publishDestination). A recipient query that failed for the batch ends the attempt at the step
// that needed it, once the owner topic, the route and the claimant are sent.
func publishDestinations(ctx context.Context, deps Deps, pending pendingEvent, item contracts.Envelope, a *attempt) error {
	event := pending.event
	if err := publishDestination(deps, event.ID, item, a, "publish owner topic", item.Topic); err != nil {
		return err
	}
	targeted := (event.Type == "message.created" || event.Type == "message.answered") &&
		payloadString(event.Payload, "target") != ""
	if event.Notify && !targeted {
		if !suppressesCurrentRoute(event.Payload, pending.routing.route, a) {
			if err := publishRoute(deps, event.ID, item, a, pending.routing.route); err != nil {
				return err
			}
		}
		if err := publishClaimantRoute(deps, event.ID, item, event, pending.routing.claimant, a); err != nil {
			return err
		}
		if err := publishPrimaryDocumentAuthors(deps, item, event, pending.primaryAuthors, a); err != nil {
			return err
		}
		if err := publishAuthorRoutes(ctx, deps, event.ID, item, event, pending.threadSessions, a); err != nil {
			return err
		}
	}
	if err := publishFollowerRoutes(ctx, deps, event.ID, item, event, a); err != nil {
		return err
	}
	return publishPreviousClaimant(deps, event.ID, item, event, a)
}

// publishPrimaryDocumentAuthors sends a person's comment, or its edit, on an issue with no route
// and no session claimant to every session the issue's primary document credits, each on its own
// topic: a session that writes an issue's spec without a claim still hears the comments on it. The
// comment's own thread does not matter: a comment in an ask's thread, or a reply, reaches those
// sessions too. resolveCommentRecipients chose the events and read the sessions (primaryAuthors);
// when that query failed, its error ends the attempt here. The actor is skipped, and so is a
// session the comment's mentions resolved to (payloadMentionedSessions) once its mention delivered
// the comment (recipientFilter).
func publishPrimaryDocumentAuthors(deps Deps, item contracts.Envelope, event model.Event, authors recipientList, a *attempt) error {
	if authors.err != nil {
		return authors.err
	}
	filter := newRecipientFilter(event, a)
	var targets []string
	for _, sessionID := range authors.sessions {
		if filter.keep(sessionID) {
			targets = append(targets, sessionID)
		}
	}
	return publishSessionRoutes(deps, event.ID, item, a, targets, "publish primary document author route to")
}

// attempt is one pass over an event's destinations, sent through sender, its batch's window
// (Publisher.Window), for the run deps describes. delivered holds the destinations the event
// reached before this pass and those this pass sent, so that none is sent twice; sent is what this
// pass sent, in order, with each one's answer to come. wait reads those answers into reached, the
// destinations NATS accepted or refused (bus.ErrRefused), denied, the denials
// (bus.ErrPublishDenied) the pass went on past, and failed, every other failure. ended is the
// error that ended the pass before it sent everything. mentions is what the event's mentions
// delivered (a new comment's alone), and deferred whether a session's copy was held back for a
// mention still being sent.
type attempt struct {
	deps      Deps
	sender    bus.Sender
	delivered map[string]struct{}
	sent      []sentDestination
	reached   []string
	denied    []error
	failed    []error
	ended     error
	mentions  mentionDeliveries
	deferred  bool
}

// sentDestination is one destination an attempt sent, named by label and id in what it reports
// (`publish route "role:reviewer": …`), since a subject alone does not say which of the event's
// destinations it was, and the channel its answer comes on.
type sentDestination struct {
	eventID int64
	topic   string
	label   string
	id      string
	answer  <-chan error
}

// wait reads the answer to each destination the attempt sent, in the order it sent them, until ctx
// ends. An answer that does not come before then counts as a failure: the destination is not
// known to be reached.
func (a *attempt) wait(ctx context.Context) {
	for _, sent := range a.sent {
		var err error
		// An answer already in is read before ctx is: a stopping process records what NATS
		// accepted.
		select {
		case err = <-sent.answer:
		default:
			select {
			case err = <-sent.answer:
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		if err := a.answer(sent, err); err != nil {
			a.failed = append(a.failed, err)
		}
	}
	a.sent = nil
}

// answer records NATS's answer err to sent. A destination NATS accepted is reached. One NATS
// refuses (bus.ErrRefused: the envelope is past the server's max payload, or its subject is past
// NATS's limit or holds whitespace, as an unbounded document slug or a bearer's session id can make
// it) is refused the same way however often it is retried, so it is logged, once, and reached like
// a publication. A denial (bus.ErrPublishDenied) is added to denied. Any other failure is returned,
// named by the destination's label and id.
func (a *attempt) answer(sent sentDestination, err error) error {
	switch {
	case err == nil:
	case errors.Is(err, bus.ErrPublishDenied):
		a.deps.countPublishFailure("denied")
		a.denied = append(a.denied, fmt.Errorf("%s %q: %w", sent.label, sent.id, err))
		return nil
	case !errors.Is(err, bus.ErrRefused):
		a.deps.countPublishFailure("failed")
		return fmt.Errorf("%s %q: %w", sent.label, sent.id, err)
	default:
		a.deps.countPublishFailure("refused")
		slog.Error("dispatch outbox: destination refused", "event_id", sent.eventID, "topic", sent.topic, "error", err)
	}
	a.reached = append(a.reached, sent.topic)
	return nil
}

// err is why the attempt did not finish - its denials, its other failures and the error that ended
// it, joined - or, when none of those happened but a session's copy was held back for a mention
// still being sent, errMentionPending; nil when every destination it sent was reached.
func (a *attempt) err() error {
	if err := errors.Join(append(append(a.denied, a.failed...), a.ended)...); err != nil {
		return err
	}
	if a.deferred {
		return errMentionPending
	}
	return nil
}

// mentionDeliveries is what a new comment's mentions did for the sessions they resolved to, read
// from comment_deliveries when the event is published (loadMentionDeliveries). A mention resolved
// at write time names only a candidate: its send runs after the comment commits and can fail - the
// listener does not know the session, the receipt times out, the handler is cancelled, the session
// lacks the delivery mode - and a session whose mention failed has no copy but the outbox's.
type mentionDeliveries struct {
	// sent holds each session some attempt of a mention reached.
	sent map[string]struct{}
	// pending holds each session no attempt reached yet whose send is still live: an attempt is
	// pending and its claim, or its creation when no sender has claimed it yet, is younger than
	// model.DeliveryClaimLapse.
	pending map[string]struct{}
}

// leftToMention is the one rule every session route of a new comment applies to a session the
// comment's mentions resolved to: it reports whether the route leaves that session's copy to the
// mention. A mention that reached the session delivered the comment, so the copy is skipped. A
// mention still being sent may yet, so the copy is held back and the attempt marked deferred,
// to be decided again on the next scan. Any other session - every attempt failed or was
// abandoned, or none was recorded - is published to as if it had not been mentioned. Callers ask
// only about a session their payload names as mentioned; a session no mention delivers to is in
// neither set, so it is never skipped.
func (a *attempt) leftToMention(sessionID string) bool {
	if _, ok := a.mentions.sent[sessionID]; ok {
		return true
	}
	if _, ok := a.mentions.pending[sessionID]; ok {
		a.deferred = true
		return true
	}
	return false
}

// mentionDeliveryQuery groups a comment's mention attempts by the session each was sent to. An
// attempt pending for longer than model.DeliveryClaimLapse - by its claim, or by its creation when
// no sender ever claimed it - is abandoned, the same lease the API's claimLapsed gives a sender
// (internal/dispatch/api), so it no longer holds the outbox's copy back.
var mentionDeliveryQuery = `
	select session_id,
	       bool_or(state = 'sent'),
	       bool_or(state = 'pending' and coalesce(claimed_at, created_at) >= now() - ` + model.DeliveryClaimLapseInterval + `)
	from comment_deliveries
	where comment_id = $1 and session_id is not null
	group by session_id
`

// loadMentionDeliveries reads what a new comment's mentions delivered (mentionDeliveries), once per
// attempt. Only a comment.created whose mentions resolved to a session asks: every other event,
// edits, resolutions and reopenings included, suppresses nothing, and costs no query.
func loadMentionDeliveries(ctx context.Context, deps Deps, event model.Event) (mentionDeliveries, error) {
	if event.Type != "comment.created" || len(payloadMentionedSessions(event.Payload)) == 0 {
		return mentionDeliveries{}, nil
	}
	commentID := payloadString(event.Payload, "id")
	rows, err := deps.Store.Pool.Query(ctx, mentionDeliveryQuery, commentID)
	if err != nil {
		return mentionDeliveries{}, fmt.Errorf("load mention deliveries of comment %q: %w", commentID, err)
	}
	defer rows.Close()
	result := mentionDeliveries{sent: map[string]struct{}{}, pending: map[string]struct{}{}}
	for rows.Next() {
		var sessionID string
		var sent, pending bool
		if err := rows.Scan(&sessionID, &sent, &pending); err != nil {
			return mentionDeliveries{}, fmt.Errorf("load mention deliveries of comment %q: %w", commentID, err)
		}
		switch {
		case sent:
			result.sent[sessionID] = struct{}{}
		case pending:
			result.pending[sessionID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return mentionDeliveries{}, fmt.Errorf("load mention deliveries of comment %q: %w", commentID, err)
	}
	return result, nil
}

// publishSessionRoutes sends item to each session's own topic, naming a failure or a denial by
// label and the session.
func publishSessionRoutes(deps Deps, eventID int64, item contracts.Envelope, a *attempt, sessionIDs []string, label string) error {
	for _, sessionID := range sessionIDs {
		routed := item
		routed.Topic = contracts.AgentSubject(sessionID)
		if err := publishDestination(deps, eventID, routed, a, label, sessionID); err != nil {
			return err
		}
	}
	return nil
}

// publishPreviousClaimant tells a session that lost an issue's claim, on its own topic: a
// takeover (another agent claimed an issue this session's claim was on, because the listener
// no longer lists this session as live) and a release somebody else made. Like the follower
// routes this ignores Notify — the losing session must hear it whoever acted, and it would
// otherwise learn only by subscribing to the whole issue.
func publishPreviousClaimant(deps Deps, eventID int64, item contracts.Envelope, event model.Event, a *attempt) error {
	if event.Type != "issue.claimed" && event.Type != "issue.released" {
		return nil
	}
	sessionID := payloadPreviousClaimSession(event.Payload)
	if sessionID == "" || event.Actor.SameAs(model.Actor{Kind: "session", ID: sessionID}) {
		return nil
	}
	return publishSessionRoutes(deps, eventID, item, a, []string{sessionID}, "publish claim change to")
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

// publishFollowerRoutes delivers an ask's answer, edit, hand-back, resolution, and every reply on it
// to each session following the ask (the asker, every session that replied, and any
// session a human added), on the session's own topic. Unlike the route and author routes
// this ignores Notify: an agent's reply on an ask must still reach the other followers,
// who otherwise learn of it only by subscribing to the whole issue. A quiet ask.edited (a version
// move of an approval request that an earlier version already moved since it was opened or last
// handed back, model.AskEditEventPayload.Quiet) reaches no follower. The event's own actor is
// skipped, and so is a follower a new reply's mentions resolved to (payloadMentionedSessions)
// once the mention delivered the reply to it (attempt.leftToMention); a follower whose mention
// failed gets this copy instead. Only the reply's creation is left to the mention; its edit,
// resolution and reopening reach every follower. A topic the route already reached is not
// published twice.
func publishFollowerRoutes(ctx context.Context, deps Deps, eventID int64, item contracts.Envelope, event model.Event, a *attempt) error {
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
	followers, err := asks.Followers(ctx, deps.Store.Pool, askID)
	if err != nil {
		return err
	}
	var mentioned map[string]struct{}
	if event.Type == "comment.created" {
		mentioned = payloadMentionedSessions(event.Payload)
	}
	var sessionIDs []string
	for _, follower := range followers {
		if event.Actor.SameAs(model.Actor{Kind: "session", ID: follower.SessionID}) {
			continue
		}
		if _, ok := mentioned[follower.SessionID]; ok && a.leftToMention(follower.SessionID) {
			continue
		}
		sessionIDs = append(sessionIDs, follower.SessionID)
	}
	return publishSessionRoutes(deps, eventID, item, a, sessionIDs, "publish follower route to")
}

// publishClaimantRoute delivers a person's answer, comment or untargeted message on an issue to
// claimant, the session that holds the issue's claim, on its own topic. Otherwise it reaches the
// claimant only when the claimant subscribed to the whole issue, or happens to be the route, a
// follower of the ask, or an author the thread or document names, and the session working the
// issue misses what a person told it there. Only a person's event qualifies (Actor.Kind "user"):
// an agent's events reach nobody new through here, even the ones that notify. A new comment whose
// mentions resolved to the claimant is left to the mention once the mention delivered it
// (attempt.leftToMention); a claimant whose mention failed gets this copy instead, and the
// comment's edit, resolution and reopening are never left to it. The claim is the one the scan
// read when the event is published (routing), not when it was written, so a retry after the
// claim moved reaches the new claimant. A claimant that is also the route, a follower or an author
// gets one copy: publishDestination sends a topic once per event.
func publishClaimantRoute(deps Deps, eventID int64, item contracts.Envelope, event model.Event, claimant string, a *attempt) error {
	// A project document's event names no issue, so there is no claim to read.
	if event.IssueKey == nil || event.Actor.Kind != "user" || claimant == "" {
		return nil
	}
	switch event.Type {
	case "ask.answered", "ask.handed_back", "ask.resolved",
		"comment.created", "comment.edited", "comment.resolved", "comment.reopened",
		"message.created", "message.answered":
	default:
		return nil
	}
	if event.Type == "comment.created" {
		if _, mentioned := payloadMentionedSessions(event.Payload)[claimant]; mentioned && a.leftToMention(claimant) {
			return nil
		}
	}
	return publishSessionRoutes(deps, eventID, item, a, []string{claimant}, "publish claimant route to")
}

// publishAuthorRoutes delivers a human comment-thread action, or a person's settled edit of a
// document, directly to the involved sessions' own topics, regardless of the issue's route (which
// may point at an entirely different reviewer) and regardless of whether the event's owner is an
// issue or a project document (which has no route at all). Without this, the sessions in a thread
// never learn of the human's reply, resolution, reopening, or edit, and the agent that wrote a
// document never learns of a new comment on it or of a person's edit to it.
//
// A reply (reply_to set), and its edit, reach threadSessions, which resolveCommentRecipients read
// for the batch (loadCommentRecipients): every session that wrote a comment in the thread, its root
// and each reply, then every session the thread's document credits; when that query failed, its
// error ends the attempt here. A new comment on a document (no reply_to and no ask_id), and its
// edit, reach every session that document credits (loadDocumentSessionAuthors); a new comment on an
// issue's conversation names no document and reaches no author. A resolution or a reopening reaches
// the thread root's author only, so the reopening a reply makes tells no new session twice.
// artifact.human_edited, which the notice sweep appends once a person's quiet window on a document
// ends (docs.sendEditNotice), reaches every session the document its payload's artifact_id names
// credits. Ask threads are not walked here: their asker and repliers are the ask's followers
// (publishFollowerRoutes). The event's own actor is skipped, and so is a session the comment's
// mentions resolved to (payloadMentionedSessions) once its mention delivered the comment
// (recipientFilter); an author whose mention failed is published to instead.
func publishAuthorRoutes(ctx context.Context, deps Deps, eventID int64, item contracts.Envelope, event model.Event, threadSessions recipientList, a *attempt) error {
	filter := newRecipientFilter(event, a)
	var targets []string
	consider := func(author model.Actor, found bool) {
		if found && author.Kind == "session" && filter.keep(author.ID) {
			targets = append(targets, author.ID)
		}
	}
	considerLoaded := func(author model.Actor, found bool, err error) error {
		if err != nil {
			return err
		}
		consider(author, found)
		return nil
	}
	considerDocument := func(document string) error {
		if document == "" {
			return nil
		}
		authors, err := loadDocumentSessionAuthors(ctx, deps, document)
		if err != nil {
			return err
		}
		for _, author := range authors {
			consider(model.Actor{Kind: "session", ID: author}, true)
		}
		return nil
	}

	switch event.Type {
	case "comment.created", "comment.edited":
		switch {
		case payloadString(event.Payload, "ask_id") != "":
			// An ask's thread reaches the ask's followers (publishFollowerRoutes).
		case payloadString(event.Payload, "reply_to") != "":
			if threadSessions.err != nil {
				return threadSessions.err
			}
			for _, sessionID := range threadSessions.sessions {
				consider(model.Actor{Kind: "session", ID: sessionID}, true)
			}
		default:
			if err := considerDocument(commentedDocument(event)); err != nil {
				return err
			}
		}
	case "comment.resolved", "comment.reopened":
		// New threads persist a reply's reply_to as the thread root. Walking from the comment
		// still finds the root for legacy nested rows.
		if commentID := payloadString(event.Payload, "id"); commentID != "" {
			if err := considerLoaded(loadRootCommentAuthor(ctx, deps, commentID)); err != nil {
				return err
			}
		}
	case "message.created", "message.answered":
		if inReplyTo := payloadString(event.Payload, "in_reply_to"); inReplyTo != "" {
			if err := considerLoaded(loadMessageAuthor(ctx, deps, inReplyTo)); err != nil {
				return err
			}
		}
	case "subscription.removed", "ask.follower_added", "ask.follower_removed":
		// The target is the unsubscribed, added, or removed session itself, carried directly
		// in the payload — there is no thread or ask to walk to find it.
		if sessionID := payloadString(event.Payload, "session_id"); sessionID != "" {
			consider(model.Actor{Kind: "session", ID: sessionID}, true)
		}
	case "artifact.human_edited":
		if err := considerDocument(payloadString(event.Payload, "artifact_id")); err != nil {
			return err
		}
	default:
		return nil
	}

	return publishSessionRoutes(deps, eventID, item, a, targets, "publish author route to")
}

func loadMessageAuthor(ctx context.Context, deps Deps, messageID string) (model.Actor, bool, error) {
	var authorJSON []byte
	if err := deps.Store.Pool.QueryRow(ctx, `select author from messages where id = $1`, messageID).Scan(&authorJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Actor{}, false, nil
		}
		return model.Actor{}, false, fmt.Errorf("load message author %q: %w", messageID, err)
	}
	return decodeAuthor(authorJSON, "message", messageID)
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

// loadDocumentSessionAuthors lists, in one query, the sessions a document credits
// (documentCreditsSQL): the one that created it and every one a version lists, each once and in
// the order the document first credits it, its creator first. People, and session credits with no
// id, are left out (sessionCreditSQL).
func loadDocumentSessionAuthors(ctx context.Context, deps Deps, artifactID string) ([]string, error) {
	rows, err := deps.Store.Pool.Query(ctx, `
		with documents as (
			select $1::uuid as key, $1::uuid as artifact_id
		),
		credited as (`+documentCreditsSQL+`)
		select author->>'id'
		from credited
		where `+sessionCreditSQL+`
		group by author->>'id'
		order by min(rank)
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("load document %q session authors: %w", artifactID, err)
	}
	authors, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("load document %q session authors: %w", artifactID, err)
	}
	return authors, nil
}

// loadRootCommentAuthor walks a comment's reply_to chain up to its thread root - the comment
// humans reply under - and returns that root's author. comments.reply_to has no acyclicity
// constraint, so the walk unions on the visited row and stops at maxCommentThreadDepth: a
// cycle then finds no reply_to-is-null row and reports no root rather than spinning.
func loadRootCommentAuthor(ctx context.Context, deps Deps, commentID string) (model.Actor, bool, error) {
	var authorJSON []byte
	if err := deps.Store.Pool.QueryRow(ctx, `
		with recursive chain as (
			select id, reply_to, author, 0 as depth from comments where id = $1
			union
			select c.id, c.reply_to, c.author, h.depth + 1
			from comments c join chain h on c.id = h.reply_to
			where h.depth < $2
		)
		select author from chain where reply_to is null limit 1
	`, commentID, maxCommentThreadDepth).Scan(&authorJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Actor{}, false, nil
		}
		return model.Actor{}, false, fmt.Errorf("load root comment author %q: %w", commentID, err)
	}
	return decodeAuthor(authorJSON, "comment", commentID)
}

func decodeAuthor(raw []byte, kind, id string) (model.Actor, bool, error) {
	var author model.Actor
	if err := json.Unmarshal(raw, &author); err != nil {
		return model.Actor{}, false, fmt.Errorf("decode %s author %q: %w", kind, id, err)
	}
	return author, true, nil
}

func publishedDestinationSet(subjects []string) map[string]struct{} {
	delivered := make(map[string]struct{}, len(subjects))
	for _, subject := range subjects {
		delivered[subject] = struct{}{}
	}
	return delivered
}

// publishDestination sends item to its topic once per event: a destination the event already
// reached, or this attempt already sent, is skipped. The answer is read later (attempt.wait),
// except one that came back before anything was sent: a refusal or a denial is kept for wait to
// read, and any other failure - NATS unreachable (bus.ErrUnavailable), say - ends the attempt,
// named by label and id. Such a failure is counted here, as attempt.answer counts the ones it
// reads, and counted once: a destination that ends the attempt is never added to a.sent, so wait
// never reads its answer.
func publishDestination(deps Deps, eventID int64, item contracts.Envelope, a *attempt, label, id string) error {
	if _, ok := a.delivered[item.Topic]; ok {
		return nil
	}
	a.delivered[item.Topic] = struct{}{}
	sent := sentDestination{eventID: eventID, topic: item.Topic, label: label, id: id, answer: a.sender.PublishAsync(item)}
	select {
	case err := <-sent.answer:
		if err != nil && !errors.Is(err, bus.ErrPublishDenied) && !errors.Is(err, bus.ErrRefused) {
			deps.countPublishFailure("failed")
			return fmt.Errorf("%s %q: %w", label, id, err)
		}
		answered := make(chan error, 1)
		answered <- err
		sent.answer = answered
	default:
	}
	a.sent = append(a.sent, sent)
	return nil
}

func publishRoute(deps Deps, eventID int64, item contracts.Envelope, a *attempt, route *string) error {
	if route == nil || *route == "" {
		return nil
	}
	parsed, err := model.ParseRoute(*route)
	if err != nil {
		return fmt.Errorf("parse route %q: %w", *route, err)
	}
	switch parsed.Kind {
	case "role":
		item.Topic = contracts.RoleTopicPrefix + parsed.ID
	case "session":
		item.Topic = contracts.AgentSubject(parsed.ID)
	default:
		return fmt.Errorf("unsupported route %q", *route)
	}
	return publishDestination(deps, eventID, item, a, "publish route", *route)
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
	case event.Type == "handoff.recorded":
		// The phase and version (a JSON number decodes to float64, which %v prints as 2), never a
		// body: the event carries none.
		values, _ := event.Payload.(map[string]any)
		detail = fmt.Sprintf("%v version %v", values["phase"], values["version"])
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
// once the mention delivered the comment to it (attempt.leftToMention): a route whose session's
// mention failed is published to.
func suppressesCurrentRoute(payload any, current *string, a *attempt) bool {
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
	return a.leftToMention(sessionID)
}

// payloadMentionedSessions reads the sessions a comment payload's mentions resolved to (each
// mentions[].session_id), as the scan decodes the payload from JSON. A mention that resolved to no
// session (session_id null) names none. These are candidates only: a session route skips one
// once the mention delivered to it (attempt.leftToMention).
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
