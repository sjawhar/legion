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
	// row. The claim is also the token the batch records its outcome under: claimPendingEvents
	// returns the next_attempt_at it wrote on each row, and recordBatch marks a row published, or
	// overwrites its next_attempt_at and attempt_count, only while the row still holds that
	// value. A row reopened after the batch claimed it (the API reopens a comment.created when a
	// mention's send fails, so the session's copy follows: reopenCommentCreated in
	// internal/dispatch/api) or claimed again by another process no longer holds it, so the batch
	// records only the destinations it reached and leaves the row to whichever scan holds it now:
	// a batch never marks published a row reopened or re-claimed after it claimed it.
	//
	// What the claim cannot give is one publish for a batch that overruns the window:
	// claimPendingEvents commits the claim, and scanBatch then publishes up to batchSize events
	// outside any transaction, re-checking nothing, so once the window lapses the other process
	// claims the rows this one has not reached yet - legitimately, by the same statement, their
	// next_attempt_at being due again - and publishes them a second time; the token keeps this
	// process from marking or rescheduling a row the other now holds, but the repeat is already
	// sent. Nor does the claim cover a process that dies between a successful publish and its
	// published_at write: it leaves the row claimed but unmarked, and the other process
	// republishes it once the window lapses. Either repeat is the same at-least-once on the role
	// lanes (notifications.role.*), which go over core NATS with no nats.MsgId on the envelope
	// (internal/bus/nats.go) and so carry no deduplication of their own; their receivers already
	// drop the repeat by its dedupe key (internal/contracts/dedupe.go). A JetStream destination
	// suppresses the repeat inside the stream's duplicate window. Neither is fixed here.
	claimWindow = time.Minute
	// mentionPendingDelay is how long an event waits when a session route held its copy back
	// for a mention whose send is still undecided (errMentionPending): long enough for the
	// handler that holds the send to settle it, short enough that the copy follows a failed send
	// promptly. The retry ticker (retryInterval) is what picks the event up again, so in practice
	// it waits until the next tick after this delay. The wait is bounded: a pending attempt
	// counts as undecided only for model.DeliveryClaimLapse, the minute its sender's claim holds
	// (mentionSendLapsed); past it the outbox settles the attempt failed and publishes the copy
	// (attempt.leftTo).
	mentionPendingDelay = 2 * time.Second
	// maxCommentThreadDepth bounds the reply_to walk in loadRootCommentAuthor so a
	// malformed cycle (comments.reply_to has no acyclicity constraint) cannot spin the
	// recursive query and stall the outbox scan.
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
	// connection and in the order they were made, or not at all (bus.Client.Window).
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
	// Leases is where the compaction tick takes documentCompactionLease (compactDocuments). It
	// is required of a Run that ticks: there is no nil-means-unleased path, and cmd/dispatch
	// exits at boot if the lease pool cannot be opened.
	Leases duty.Leases
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
	// claim is the next_attempt_at claimPendingEvents wrote on the row when it claimed it: the
	// token recordBatch records the event's outcome under (claimWindow).
	claim time.Time
	// failure is why the row cannot be published as it was read - it did not scan, or its
	// actor or payload JSON did not decode - and failureLog is the predicate it reports
	// under. The batch backs such an event off instead of publishing it.
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
// lanes and on each issue's subject, a batch's events arrive in id order. Then every answer is
// read, and what the batch came to is written in one statement (recordBatch). An event is marked
// published only once NATS accepted each of its destinations; one that failed is backed off, so
// the events queued behind it are not stuck behind it; and one that held a session's copy back for
// a mention still being sent (errMentionPending) is deferred, its other destinations recorded.
//
// A failure of the connection rather than the envelope (bus.ErrUnavailable: no connection, one
// lost or reconnected mid-batch, no answer at all) is no fault of its event. The window sends
// nothing on a connection that came back, so when the connection fails, what of the batch reached
// NATS is a prefix of it: no event after the first send that met the lost connection is sent, and
// every event that met the failure, or was not sent, is released rather than backed off, so the
// next scan sends them again in id order, and the stream's MsgId drops the copies of those that
// had landed. An event backed off instead would reach its issue's subject after the later events
// of that issue, which the Legion daemon then drops as stale by their sequence. The guarantee
// covers a failed connection only: an answer that times out, or no responders, on a connection
// still current stops no later send, so the released event can land after a later event of its
// issue that NATS did store.
func scanBatch(ctx context.Context, deps Deps) (int, bool, error) {
	pending, blocked, err := scanPendingEvents(ctx, deps)
	if err != nil {
		return 0, false, err
	}

	sender := deps.Publisher.Window()
	attempts := make([]*attempt, len(pending))
	for index, item := range pending {
		if ctx.Err() != nil {
			break
		}
		if item.failure != nil {
			continue
		}
		attempts[index] = send(ctx, deps, sender, item.event, item.slug, item.routing, publishedDestinationSet(item.destinations))
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
		return len(pending), true, err
	}
	return len(pending), blocked || unavailable, nil
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
	// claim is the event's claim token (pendingEvent.claim): the outcome is written only while
	// the row still holds it.
	claim time.Time
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
	result := outcome{id: item.event.ID, claim: item.claim}
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
// published_at is null, like every terminal write here, and the mark, the attempt count and the
// next attempt are written only while the row still holds the batch's claim token (outcome.claim,
// claimWindow): a row reopened or claimed again since this batch claimed it gets the destinations
// the batch reached and nothing more, so it stays unpublished and keeps the next_attempt_at the
// reopen or the newer claim wrote, and the scan that holds it now publishes what is left. A write
// that fails leaves the whole batch claimed and unmarked, which is what a process that dies between
// NATS's acknowledgements and this write leaves too: the batch is published again once claimWindow
// lapses. JetStream drops each repeat inside its duplicate window by its MsgId; a role lane, over
// core NATS, carries the repeat, and its receivers drop it by its dedupe key
// (internal/contracts/dedupe.go).
func recordBatch(ctx context.Context, deps Deps, outcomes []outcome) error {
	if len(outcomes) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(outcomes))
	states := make([]string, 0, len(outcomes))
	claims := make([]time.Time, 0, len(outcomes))
	attempts := make([]*int32, 0, len(outcomes))
	retryAt := make([]*time.Time, 0, len(outcomes))
	var reachedIDs []int64
	var reachedTopics []string
	for _, result := range outcomes {
		ids = append(ids, result.id)
		states = append(states, string(result.state))
		claims = append(claims, result.claim)
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
			select * from unnest($1::bigint[], $2::text[], $3::int[], $4::timestamptz[], $7::timestamptz[])
				as o(id, state, attempts, retry_at, claim)
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
			published_at = case when o.state = 'published' and e.next_attempt_at = o.claim then now() end,
			attempt_count = case when e.next_attempt_at = o.claim
				then coalesce(o.attempts, e.attempt_count) else e.attempt_count end,
			next_attempt_at = case when e.next_attempt_at = o.claim then o.retry_at else e.next_attempt_at end
		from outcome o
		left join reached r on r.id = o.id
		where e.id = o.id and e.published_at is null
	`, ids, states, attempts, retryAt, reachedIDs, reachedTopics, claims); err != nil {
		return fmt.Errorf("record the batch's outcome: %w", err)
	}
	return nil
}

// claimPendingEvents claims at most batchSize eligible events, lowest id first, and returns the
// next_attempt_at it wrote on each, by id - the claim token recordBatch writes the event's outcome
// under (claimWindow): one statement takes them under for update skip locked, so a second
// process's concurrent scan skips them rather than waiting on them, and pushes their
// next_attempt_at out by claimWindow in the same breath. It locks rows of events alone - the
// issue and artifact columns a publish needs are read by scanPendingEvents afterwards, from the
// ids this returns, because a statement that both locks `for update` and names issues or
// artifacts is refused by the room lock's rule (TestNoForUpdateOnOwnerTables in
// internal/dispatch/api). The claimed rows are this process's for the window whether or not that
// read succeeds, so the split costs a round trip and nothing else: a read that fails leaves them
// claimed until the window lapses, exactly as a process dying mid-batch does.
func claimPendingEvents(ctx context.Context, deps Deps) (map[int64]time.Time, error) {
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
		returning id, next_attempt_at
	`, batchSize, claimWindow)
	if err != nil {
		return nil, fmt.Errorf("claim unpublished events: %w", err)
	}
	// The loop drains and closes the cursor, which the read below needs: a pooled connection
	// held open by one query is a second connection this caller may not ask for
	// (store.ErrNestedAcquire).
	defer rows.Close()
	claimed := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var claim time.Time
		if err := rows.Scan(&id, &claim); err != nil {
			return nil, fmt.Errorf("claim unpublished events: %w", err)
		}
		claimed[id] = claim
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim unpublished events: %w", err)
	}
	return claimed, nil
}

// scanPendingEvents claims a batch (claimPendingEvents) and reads each claimed event with the
// issue and artifact columns a publish needs, in the order by id every batch has always had: the
// issue's route and claimant among them (routing), read here on every attempt. Each event carries
// the claim token its claim wrote (pendingEvent.claim).
func scanPendingEvents(ctx context.Context, deps Deps) ([]pendingEvent, bool, error) {
	claimed, err := claimPendingEvents(ctx, deps)
	if err != nil {
		return nil, false, err
	}
	if len(claimed) == 0 {
		return nil, false, nil
	}
	ids := make([]int64, 0, len(claimed))
	for id := range claimed {
		ids = append(ids, id)
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
	`, ids)
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
			item.claim = claimed[item.event.ID]
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
		item.claim = claimed[item.event.ID]
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
func unreachedIDs(claimed map[int64]time.Time, pending []pendingEvent) []int64 {
	read := make(map[int64]struct{}, len(pending))
	for _, item := range pending {
		read[item.event.ID] = struct{}{}
	}
	unreached := make([]int64, 0, len(claimed)-len(pending))
	for id := range claimed {
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

// send starts event's attempt: its envelope is sent to each of its destinations not in delivered
// (publishDestinations), and the attempt returned holds the answers still to come (attempt.wait).
// A destination NATS denies (a role route outside Dispatch's grant, say) holds back none of the
// others: it is left unreached, and the denial fails the attempt once every destination was
// tried, so the event is retried. Any other failure that comes back before anything is sent, of
// the connection or of the store, ends the attempt at once, since every destination after it would
// fail the same way. A session's copy of a new comment held back for a mention still being sent
// (attempt.leftToMention) holds back none of the others either: the attempt is marked deferred and
// reports errMentionPending once every answer is in (attempt.err). The destinations the attempt
// reached are recorded with the batch (recordBatch), so a retry publishes only the rest.
func send(ctx context.Context, deps Deps, sender bus.Sender, event model.Event, slug string, routing routing, delivered map[string]struct{}) *attempt {
	a := &attempt{delivered: delivered, sender: sender}
	if event.IssueKey == nil && event.ArtifactID == nil && event.Project == "" {
		return a
	}
	item, err := envelope(event, slug)
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
	a.ended = publishDestinations(ctx, deps, event, item, routing, a)
	return a
}

// publishDestinations sends item to each of event's destinations in turn, and returns the first
// error that ends the attempt.
func publishDestinations(ctx context.Context, deps Deps, event model.Event, item contracts.Envelope, routing routing, a *attempt) error {
	if err := publishDestination(deps, event.ID, item, a, "publish owner topic", item.Topic); err != nil {
		return err
	}
	targeted := (event.Type == "message.created" || event.Type == "message.answered") &&
		payloadString(event.Payload, "target") != ""
	if event.Notify && !targeted {
		if !suppressesCurrentRoute(event.Payload, routing.route, a) {
			if err := publishRoute(deps, event.ID, item, a, routing.route); err != nil {
				return err
			}
		}
		if err := publishClaimantRoute(deps, event.ID, item, event, routing.claimant, a); err != nil {
			return err
		}
		if err := publishAuthorRoutes(ctx, deps, event.ID, item, event, a); err != nil {
			return err
		}
	}
	if err := publishFollowerRoutes(ctx, deps, event.ID, item, event, a); err != nil {
		return err
	}
	return publishPreviousClaimant(deps, event.ID, item, event, a)
}

// attempt is one pass over an event's destinations, sent through sender, its batch's window
// (Publisher.Window). delivered holds the destinations the event
// reached before this pass and those this pass sent, so that none is sent twice; sent is what this
// pass sent, in order, with each one's answer to come. wait reads those answers into reached, the
// destinations NATS accepted or refused (bus.ErrRefused), denied, the denials
// (bus.ErrPublishDenied) the pass went on past, and failed, every other failure. ended is the
// error that ended the pass before it sent everything. mentions is what the event's mentions
// delivered (a new comment's alone), and deferred whether a session's copy was held back for a
// mention still being sent.
type attempt struct {
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
		a.denied = append(a.denied, fmt.Errorf("%s %q: %w", sent.label, sent.id, err))
		return nil
	case !errors.Is(err, bus.ErrRefused):
		return fmt.Errorf("%s %q: %w", sent.label, sent.id, err)
	default:
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

// mentionDeliveries is what a new comment's mentions did, read from comment_deliveries when the
// event is published (loadMentionDeliveries). A mention resolved at write time names only a
// candidate: its send runs after the comment commits and can fail - the listener does not know the
// session, the receipt times out, the handler is cancelled, the session lacks the delivery mode -
// and a session whose mention failed has no copy but the outbox's.
type mentionDeliveries struct {
	// sessions is keyed by the session each attempt went to: its session_id, or, while a
	// session:<id> mention is unpinned (its lookup went unanswered and no sender has resolved it
	// yet), the <id> its target names.
	sessions mentionOutcomes
	// targets is keyed by each attempt's target as written, for a route suppressed by that string
	// alone (payloadSuppressedRouteTarget).
	targets mentionOutcomes
}

// mentionOutcomes is what the attempts under each key of mentionDeliveries came to. A key in more
// than one set counts as the first of sent, pending and abandoned it is in (attempt.leftTo).
type mentionOutcomes struct {
	// sent holds each key some attempt reached, or whose attempt the API's stranded pass resends
	// (mentionResumed): either way the mention, not the copy, delivers the comment - and should
	// that resend fail, the API reopens the event and the next scan publishes the copy.
	sent map[string]struct{}
	// pending holds each key with an attempt whose send is live: pending, and claimed within
	// model.DeliveryClaimLapse, or unclaimed and created within it.
	pending map[string]struct{}
	// abandoned holds each key with a pending attempt mentionSendLapsed takes as abandoned.
	abandoned map[string]struct{}
	// settle settles the abandoned attempts under key failed, and reports whether key's attempts
	// then hold a sent one and a pending one (settleAbandonedMentions).
	settle func(key string) (sent, pending bool, err error)
}

// record adds what the attempts of one row of mentionDeliveryQuery came to under key.
func (o mentionOutcomes) record(key string, sent, live, abandoned bool) {
	switch {
	case sent:
		o.sent[key] = struct{}{}
	case live:
		o.pending[key] = struct{}{}
	case abandoned:
		o.abandoned[key] = struct{}{}
	}
}

// leftToMention is the one rule every session route of a new comment applies to a session the
// comment's mentions resolved to: it reports whether the route leaves that session's copy to the
// mention (attempt.leftTo). Callers ask only about a session their payload names as mentioned; a
// session no mention delivers to is in no set, so it is never skipped.
func (a *attempt) leftToMention(sessionID string) bool {
	return a.leftTo(a.mentions.sessions, sessionID)
}

// leftToMentionOf is the same rule for a route suppressed by its string alone, keyed by the
// mention target that string is (payloadSuppressedRouteTarget), since no session is known for it.
func (a *attempt) leftToMentionOf(target string) bool {
	return a.leftTo(a.mentions.targets, target)
}

// leftTo reports whether the copy of key is left to the mention. A mention that reached it
// delivered the comment, so the copy is skipped. A mention still being sent may yet, so the copy
// is held back and the attempt marked deferred, to be decided again on the next scan. A mention
// whose send was abandoned is settled by the outbox first (outcomes.settle), and the copy follows
// what key's attempts are once that settle commits: one a sender claimed before the settle took
// its row is pending, so the copy is held back and the sender owns the outcome; one the settle
// took is failed, and the copy is the outbox's. Only one of the two acts on an attempt, so the
// session gets the mention or the copy, not both. A settle that fails holds the copy back and
// fails the attempt, so the event is retried. Any other key - every attempt failed, or none was
// recorded - is published to as if it had not been mentioned.
//
// The copy also follows a send that fails after this decision: the API's settle of a failed
// mention send reopens the comment's comment.created (reopenCommentCreated in
// internal/dispatch/api) when its payload leaves that session's copy to a mention, and the next
// scan republishes it to the destinations it has not reached, reading the attempt failed. A
// batch publishing the event when the reopen commits records only the destinations it reached
// (recordBatch), so the decision it made before the reopen never marks the event published.
func (a *attempt) leftTo(outcomes mentionOutcomes, key string) bool {
	if _, ok := outcomes.sent[key]; ok {
		return true
	}
	if _, ok := outcomes.pending[key]; ok {
		a.deferred = true
		return true
	}
	if _, ok := outcomes.abandoned[key]; !ok {
		return false
	}
	delete(outcomes.abandoned, key)
	sent, pending, err := outcomes.settle(key)
	switch {
	case err != nil:
		a.failed = append(a.failed, err)
		outcomes.pending[key] = struct{}{}
		return true
	case sent:
		outcomes.sent[key] = struct{}{}
		return true
	case pending:
		outcomes.pending[key] = struct{}{}
		a.deferred = true
		return true
	}
	return false
}

// mentionSessionKey is the session a comment_deliveries row's attempt counts under: its
// session_id, or, for an attempt with none whose target is session:<id> - an unpinned mention its
// sender has not resolved yet, or one that failed before it was - that <id>, the one session that
// target can reach (substr drops the 8 bytes of "session:"). An unpinned role:X counts under no
// session, only under its target.
const mentionSessionKey = `coalesce(session_id, case when target like 'session:%' then substr(target, 9) end)`

// mentionSendLapsed is true of a pending attempt whose send the outbox takes as abandoned: no
// sender ever claimed it and it was created more than model.DeliveryClaimLapse ago (the mention
// sender leaves an attempt unclaimed until its lane starts the send, so a send queued behind its
// lane for longer than that lease reads abandoned too), or it is older than
// model.StrandedMentionWindow, past which no sender resumes it. An attempt a sender claimed within
// that window may have reached the listener before its sender died, so it is left to the API's
// stranded pass, which resends it under the attempt's own idempotency key and the listener drops
// the repeat; publishing the session's own copy for it too would give that session two copies.
// The copy follows only once the resend fails (mentionResumed).
var mentionSendLapsed = `((claimed_at is null and created_at < now() - ` + model.DeliveryClaimLapseInterval +
	`) or created_at < now() - ` + model.StrandedMentionWindowInterval + `)`

// mentionResumed is true of a pending attempt a sender claimed and never settled, whose claim
// lapsed a lease ago, within model.StrandedMentionWindow: its sender died with the send in flight,
// perhaps after the listener took it, and the API's stranded pass resends it under its own
// idempotency key, which the listener drops as a repeat when the first send landed. The outbox
// leaves the session's copy to that resend, as to a sent mention, and does not wait on it: it
// cannot tell a send that landed before the kill from one that did not. A resend that fails is
// settled failed by the API, whose settle reopens the comment.created (reopenCommentCreated in
// internal/dispatch/api) when the comment mentions the session and the event has not reached its
// topic, and the next scan reads the attempt failed and publishes whichever copy reaches the
// session (attempt.leftTo), so a mentioned session loses no comment through a kill. One duplicate
// remains: a send the killed process had handed the listener before it
// died, whose resend then fails for good (the listener down after the restart), gives the session
// that mention and then the copy, under different dedupe keys
// (agent.<session>.<comment>:<target>:<mode> for the mention, dispatch-<event id> for the copy).
var mentionResumed = `(claimed_at is not null and claimed_at < now() - ` + model.DeliveryClaimLapseInterval +
	` and created_at >= now() - ` + model.StrandedMentionWindowInterval + `)`

// mentionLeftToMention is true of an attempt whose mention, not the outbox's copy, delivers the
// comment: sent, or resumed by the stranded pass (mentionResumed).
var mentionLeftToMention = `(state = 'sent' or (state = 'pending' and ` + mentionResumed + `))`

// mentionDeliveryQuery groups a comment's mention attempts by the session each counts under
// (mentionSessionKey) and by its target, and reports of each group whether an attempt was sent
// or is resumed by the stranded pass (mentionLeftToMention), is pending and live, and is pending
// and abandoned (mentionSendLapsed).
var mentionDeliveryQuery = `
	select ` + mentionSessionKey + `,
	       target,
	       bool_or(` + mentionLeftToMention + `),
	       bool_or(state = 'pending' and not (` + mentionSendLapsed + `) and not (` + mentionResumed + `)),
	       bool_or(state = 'pending' and ` + mentionSendLapsed + `)
	from comment_deliveries
	where comment_id = $1
	group by 1, 2
`

// mentionAbandonedError is the error the outbox records on a mention attempt it settles failed
// because its send was abandoned (settleAbandonedMentions).
const mentionAbandonedError = "the outbox published the comment's own copy instead; the send was abandoned"

// loadMentionDeliveries reads what a new comment's mentions delivered (mentionDeliveries), in one
// query per attempt. Only a comment.created that names a mentioned session
// (payloadMentionedSessions) or suppresses its route by a mention's target
// (payloadSuppressedRouteTarget) asks: every other event, edits, resolutions and reopenings
// included, suppresses nothing, and costs no query. A key that reads abandoned costs a settle
// transaction of its own, and only when a copy asks about it (attempt.leftTo).
func loadMentionDeliveries(ctx context.Context, deps Deps, event model.Event) (mentionDeliveries, error) {
	if event.Type != "comment.created" ||
		(len(payloadMentionedSessions(event.Payload)) == 0 && payloadSuppressedRouteTarget(event.Payload) == "") {
		return mentionDeliveries{}, nil
	}
	commentID := payloadString(event.Payload, "id")
	rows, err := deps.Store.Pool.Query(ctx, mentionDeliveryQuery, commentID)
	if err != nil {
		return mentionDeliveries{}, fmt.Errorf("load mention deliveries of comment %q: %w", commentID, err)
	}
	defer rows.Close()
	outcomes := func(keyColumn string) mentionOutcomes {
		return mentionOutcomes{
			sent: map[string]struct{}{}, pending: map[string]struct{}{}, abandoned: map[string]struct{}{},
			settle: func(key string) (bool, bool, error) {
				return settleAbandonedMentions(ctx, deps, event, commentID, keyColumn, key)
			},
		}
	}
	result := mentionDeliveries{sessions: outcomes(mentionSessionKey), targets: outcomes("target")}
	for rows.Next() {
		var sessionID *string
		var target string
		var sent, live, abandoned bool
		if err := rows.Scan(&sessionID, &target, &sent, &live, &abandoned); err != nil {
			return mentionDeliveries{}, fmt.Errorf("load mention deliveries of comment %q: %w", commentID, err)
		}
		if sessionID != nil {
			result.sessions.record(*sessionID, sent, live, abandoned)
		}
		result.targets.record(target, sent, live, abandoned)
	}
	if err := rows.Err(); err != nil {
		return mentionDeliveries{}, fmt.Errorf("load mention deliveries of comment %q: %w", commentID, err)
	}
	return result, nil
}

// settleAbandonedMentions settles failed, with mentionAbandonedError, every attempt of the comment
// that event created whose keyColumn (mentionSessionKey or target) is key and whose send is
// abandoned (mentionSendLapsed), appends each one's comment.delivery receipt as the API's sender
// would (the comment's author its actor), and reports whether key's attempts then hold a sent one
// and a pending one. It is one transaction, and it takes the comment's owner lock before any
// attempt row, the order the API's claim and settle transactions take them in
// (claimCommentDeliveryAttempt, settleDeliveryAttempt in internal/dispatch/api), so it and a
// sender's claim of the same attempt run one after the other. A claim that commits first leaves
// the row claimed just now, so the update's guard no longer matches it and the attempt reads
// pending; an attempt this settles first is no longer pending when the claim reads it, and the
// claim takes it as settled and sends nothing. The receipts reach the event log in the same
// commit, so a reader sees the attempt fail, and the outbox publishes them like any event.
func settleAbandonedMentions(
	ctx context.Context, deps Deps, event model.Event, commentID, keyColumn, key string,
) (sent, pending bool, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("settle the abandoned mention attempts of comment %q for %q: %w", commentID, key, err)
		}
	}()
	tx, err := deps.Store.Pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	owner := model.Event{IssueKey: event.IssueKey, ArtifactID: event.ArtifactID}
	if err := deps.Broker.LockOwners(ctx, tx, owner); err != nil {
		return false, false, err
	}
	rows, err := tx.Query(ctx, `
		update comment_deliveries set state = 'failed', error = $3
		where comment_id = $1 and `+keyColumn+` = $2 and state = 'pending' and `+mentionSendLapsed+`
		returning target, attempt, delivery, session_id, reply_id::text
	`, commentID, key, mentionAbandonedError)
	if err != nil {
		return false, false, err
	}
	var askID *string
	if id := payloadString(event.Payload, "ask_id"); id != "" {
		askID = &id
	}
	settled, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.CommentDeliveryEventPayload, error) {
		receipt := model.CommentDeliveryEventPayload{
			CommentID: commentID, AskID: askID, State: "failed", Error: mentionAbandonedError,
		}
		err := row.Scan(&receipt.Target, &receipt.Attempt, &receipt.Delivery, &receipt.SessionID, &receipt.ReplyID)
		return receipt, err
	})
	if err != nil {
		return false, false, err
	}
	receipts := make([]model.Event, 0, len(settled))
	for _, payload := range settled {
		receipt := owner
		receipt.Type, receipt.Actor, receipt.Payload = "comment.delivery", event.Actor, payload
		appended, err := deps.Broker.Append(ctx, tx, receipt)
		if err != nil {
			return false, false, err
		}
		receipts = append(receipts, appended)
	}
	if err := tx.QueryRow(ctx, `
		select coalesce(bool_or(`+mentionLeftToMention+`), false), coalesce(bool_or(state = 'pending'), false)
		from comment_deliveries
		where comment_id = $1 and `+keyColumn+` = $2
	`, commentID, key).Scan(&sent, &pending); err != nil {
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, err
	}
	for _, receipt := range receipts {
		deps.Broker.Publish(receipt)
	}
	return sent, pending, nil
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
// issue or a project document (which has no route at all). Without this, the agent that started
// the thread or is the thread's own root author never learns about the human's reply, resolution,
// reopening, or edit, and the agent that wrote a document never learns of a new comment on it or
// of a person's edit to it. A reply reaches its thread's root author and its direct parent's
// author; a new comment on a document (no reply_to and no ask_id), and its edit, reach every
// session that document credits (loadDocumentSessionAuthors); a comment on an issue's
// conversation names no document and reaches no author; and artifact.human_edited, which the
// notice sweep appends once a person's quiet window on a document ends (docs.sendEditNotice),
// reaches every session the document its payload's artifact_id names credits. Ask threads are not
// walked here: their asker and repliers are the ask's followers (publishFollowerRoutes). The
// event's own actor is skipped, and so is a session the comment's mentions resolved to
// (suppressed_authors) once its mention delivered the comment (attempt.leftToMention); an author
// whose mention failed is published to instead.
func publishAuthorRoutes(ctx context.Context, deps Deps, eventID int64, item contracts.Envelope, event model.Event, a *attempt) error {
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
		if _, suppressed := suppressedAuthors[author.ID]; suppressed && a.leftToMention(author.ID) {
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
	case "artifact.human_edited":
		// The edited document is named in the payload (editedDocument); its writers are read
		// below.
	default:
		return nil
	}

	if inReplyTo != "" {
		// New comment threads persist ReplyTo as their root ID. Walking still keeps
		// routing correct for legacy nested rows and finds the root author.
		if err := considerLoaded(loadRootCommentAuthor(ctx, deps, inReplyTo)); err != nil {
			return err
		}
		if err := considerLoaded(loadCommentAuthor(ctx, deps, inReplyTo)); err != nil {
			return err
		}
	} else if commentID != "" && (event.Type == "comment.resolved" || event.Type == "comment.reopened") {
		if err := considerLoaded(loadRootCommentAuthor(ctx, deps, commentID)); err != nil {
			return err
		}
	} else if document := editedDocument(event); document != "" {
		authors, err := loadDocumentSessionAuthors(ctx, deps, document)
		if err != nil {
			return err
		}
		for _, author := range authors {
			consider(model.Actor{Kind: "session", ID: author}, true)
		}
	} else if (event.Type == "comment.created" || event.Type == "comment.edited") && payloadString(event.Payload, "ask_id") == "" {
		document := commentedDocument(event)
		if document != "" {
			authors, err := loadDocumentSessionAuthors(ctx, deps, document)
			if err != nil {
				return err
			}
			for _, author := range authors {
				consider(model.Actor{Kind: "session", ID: author}, true)
			}
		}
	}
	if messageInReplyTo != "" {
		if err := considerLoaded(loadMessageAuthor(ctx, deps, messageInReplyTo)); err != nil {
			return err
		}
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

func loadCommentAuthor(ctx context.Context, deps Deps, commentID string) (model.Actor, bool, error) {
	var authorJSON []byte
	if err := deps.Store.Pool.QueryRow(ctx, `select author from comments where id = $1`, commentID).Scan(&authorJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Actor{}, false, nil
		}
		return model.Actor{}, false, fmt.Errorf("load comment author %q: %w", commentID, err)
	}
	return decodeAuthor(authorJSON, "comment", commentID)
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

// editedDocument names the document a person's artifact.human_edited is about, its payload's
// artifact_id, or "" for every other event.
func editedDocument(event model.Event) string {
	if event.Type != "artifact.human_edited" {
		return ""
	}
	return payloadString(event.Payload, "artifact_id")
}

// loadDocumentSessionAuthors lists, in one query, the sessions a document credits: the one that
// created it (artifacts.created_by) and every one a version lists (artifact_versions.authors), each
// once and in the order the document first credits it, its creator first. A person has no Envoy
// topic, so people are left out, and so is a session credit naming no id, which has none either.
// A version's authors are a JSON array, or JSON null where a writer encoded a nil list, which
// every Go reader of the column decodes as no authors; the query reads null the same way rather
// than failing the event's every publish on jsonb_array_elements' refusal of a scalar.
func loadDocumentSessionAuthors(ctx context.Context, deps Deps, artifactID string) ([]string, error) {
	rows, err := deps.Store.Pool.Query(ctx, `
		with credited as (
			select a.created_by as author, 0 as number, 0::bigint as position
			from artifacts a
			where a.id = $1 and a.kind = 'doc'
			union all
			select credit.author, v.number, credit.position
			from artifact_versions v
			join artifacts a on a.id = v.artifact_id and a.kind = 'doc'
			cross join lateral jsonb_array_elements(nullif(v.authors, 'null'::jsonb))
				with ordinality as credit(author, position)
			where v.artifact_id = $1
		)
		select author->>'id'
		from credited
		where author->>'kind' = 'session' and coalesce(author->>'id', '') <> ''
		group by author->>'id'
		order by min(array[number, position])
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
// named by label and id.
func publishDestination(deps Deps, eventID int64, item contracts.Envelope, a *attempt, label, id string) error {
	if _, ok := a.delivered[item.Topic]; ok {
		return nil
	}
	a.delivered[item.Topic] = struct{}{}
	sent := sentDestination{eventID: eventID, topic: item.Topic, label: label, id: id, answer: a.sender.PublishAsync(item)}
	select {
	case err := <-sent.answer:
		if err != nil && !errors.Is(err, bus.ErrPublishDenied) && !errors.Is(err, bus.ErrRefused) {
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
// edit to any other target must still wake its new destination. A suppression that names the
// session the route resolved to when the comment was written (suppressed_route_session_id) holds
// only once the mention delivered the comment to that session (attempt.leftToMention). One that
// names the route string alone, the target of a mention whose lookup went unanswered
// (payloadSuppressedRouteTarget), holds while the route is still that string and only once the
// mention of that target delivered the comment (attempt.leftToMentionOf). A route whose mention
// failed is published to.
func suppressesCurrentRoute(payload any, current *string, a *attempt) bool {
	if !payloadBool(payload, "suppress_route") || current == nil || *current == "" {
		return false
	}
	sessionID := payloadString(payload, "suppressed_route_session_id")
	if sessionID == "" {
		target := payloadSuppressedRouteTarget(payload)
		return target != "" && *current == target && a.leftToMentionOf(target)
	}
	if *current != payloadString(payload, "suppressed_route") && *current != "session:"+sessionID {
		return false
	}
	return a.leftToMention(sessionID)
}

// payloadSuppressedRouteTarget is the mention target a comment payload suppresses the route copy
// by, string alone: suppressed_route, when suppress_route is set, no suppressed_route_session_id
// names a session, and one of the mentions' targets (mentions[].target) is that string - the route
// was that mention's target and its lookup went unanswered. It is "" for every other payload.
func payloadSuppressedRouteTarget(payload any) string {
	route := payloadString(payload, "suppressed_route")
	if !payloadBool(payload, "suppress_route") || route == "" || payloadString(payload, "suppressed_route_session_id") != "" {
		return ""
	}
	values, _ := payload.(map[string]any)
	mentions, _ := values["mentions"].([]any)
	for _, entry := range mentions {
		if payloadString(entry, "target") == route {
			return route
		}
	}
	return ""
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

// payloadMentionedSessions reads the sessions a comment payload's mentions resolved to, as the
// scan decodes the payload from JSON: each mentions[].session_id, and, for a session:<id> mention
// that names none (its lookup went unanswered, so the sender resolves it after the commit), the
// <id> its target names. A role:X mention that resolved to no session names none. These are
// candidates only: a session route skips one once the mention delivered to it
// (attempt.leftToMention).
func payloadMentionedSessions(payload any) map[string]struct{} {
	values, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	mentions, _ := values["mentions"].([]any)
	result := map[string]struct{}{}
	for _, entry := range mentions {
		sessionID := payloadString(entry, "session_id")
		if named, unpinned := strings.CutPrefix(payloadString(entry, "target"), "session:"); sessionID == "" && unpinned {
			sessionID = named
		}
		if sessionID != "" {
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
