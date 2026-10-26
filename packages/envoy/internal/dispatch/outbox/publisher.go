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

	// maxCommentThreadDepth bounds the reply_to walk in loadRootCommentAuthor so a
	// malformed cycle (comments.reply_to has no acyclicity constraint) cannot spin the
	// recursive query and stall the outbox scan.
	maxCommentThreadDepth = 32
	// recordTimeout bounds the write of a batch's outcome (recordBatch), which a stopping process
	// still makes after its context has ended: one statement, which a healthy database answers in
	// milliseconds, so a write that takes longer is a database the batch's claim is left to.
	recordTimeout = 5 * time.Second
)

// Publisher is the NATS publication boundary used by the Dispatch outbox.
type Publisher interface {
	// PublishAsync sends item without waiting for NATS's answer, and returns a channel that
	// receives the outcome once: nil once NATS accepted item, or why it did not. Items sent one
	// after another reach NATS in the order they were sent (bus.Client.PublishAsync).
	PublishAsync(contracts.Envelope) <-chan error
}

// Deps are the durable state and delivery dependencies for the outbox.
type Deps struct {
	// Store is what the outbox reads and records events through. cmd/dispatch gives it a store on
	// a pool of its own (store.Pool.Outbox), so the requests queued on the shared pool never hold
	// a publish back.
	Store     *store.Store
	Publisher Publisher
	Broker    *events.Broker
	Docs      docs.API
}

// Run drains unpublished events at startup, after committed local events, and
// periodically so a dropped in-process notification cannot strand an event. It
// returns when ctx is cancelled.
func Run(ctx context.Context, deps Deps) {
	// The publisher is one of the process's transaction openers, so its context carries the
	// pool's marker: a read it ever makes while one of its transactions is open is refused
	// rather than left to deadlock the pool (store.ErrNestedAcquire).
	ctx = store.WithTransactionTracking(ctx)
	events, unsubscribe := deps.Broker.Subscribe()
	defer func() { unsubscribe() }()

	scan(ctx, deps)
	retry := time.NewTicker(retryInterval)
	defer retry.Stop()
	compact := time.NewTicker(compactInterval)
	defer compact.Stop()

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
		case <-compact.C:
			if err := deps.Docs.CompactAll(ctx); err != nil {
				slog.Error("dispatch outbox: compact documents", "error", err)
			}
		}
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
	route        *string
	// failure is why the row cannot be published as it was read - it did not scan, or its
	// actor or payload JSON did not decode - and failureLog is the predicate it reports
	// under. The batch backs such an event off instead of publishing it.
	failure    error
	failureLog string
}

// scanBatch claims a batch and publishes it as one window. Every event's envelope is sent to each
// of its destinations, in id order and each event's destinations in turn, before any answer is
// waited for, so a batch costs one NATS round trip rather than one per destination, and
// NATS takes the envelopes in the order they were sent: on the stream, on the role lanes and on
// each issue's subject, a batch's events arrive in id order. Then every answer is read, and what
// the batch came to is written in one statement (recordBatch). An event is marked published only
// once NATS accepted each of its destinations; one that failed is backed off, so the events
// queued behind it are not stuck behind it.
//
// A send NATS could not take at all because the client has no connection (bus.ErrUnavailable)
// is no fault of its event: nothing after it in the batch is sent, since it would meet the same
// connection, and that event and every one after it are released rather than backed off, so the
// next scan takes them again in id order. An event backed off for an outage would reach its
// issue's subject after the later events of that issue, which the Legion daemon then drops as
// stale by their sequence.
func scanBatch(ctx context.Context, deps Deps) (int, bool, error) {
	pending, blocked, err := scanPendingEvents(ctx, deps)
	if err != nil {
		return 0, false, err
	}

	attempts := make([]*attempt, len(pending))
	unavailable := false
	for index, item := range pending {
		if item.failure != nil {
			continue
		}
		attempts[index] = send(ctx, deps, item.event, item.slug, item.route, publishedDestinationSet(item.destinations))
		if errors.Is(attempts[index].ended, bus.ErrUnavailable) {
			slog.Error("dispatch outbox: NATS is unreachable, so the rest of the batch waits for the next scan",
				"event_id", item.event.ID, "error", attempts[index].ended)
			unavailable = true
			break
		}
	}
	outcomes := make([]outcome, len(pending))
	for index, item := range pending {
		outcomes[index] = settle(ctx, item, attempts[index])
	}
	// Written even when ctx has ended: a process stopping mid-batch records what NATS already
	// accepted, so the next process does not send it again.
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
)

// outcome is what one event of a batch came to, as recordBatch writes it.
type outcome struct {
	id    int64
	state outcomeState
	// reached is every destination the attempt reached, in the order it sent them.
	reached []string
	// attempts and retryAt are the backed-off event's attempt count and next attempt.
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
	if err := a.err(); err != nil {
		// NATS unreachable, or the process stopping before every answer came, is no failure of
		// the event's own.
		if errors.Is(a.ended, bus.ErrUnavailable) || ctx.Err() != nil {
			result.state = released
			return result
		}
		return backOff(result, item, "dispatch outbox: publish event", err)
	}
	result.state = published
	return result
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
// marked published, backed off, or released (left due at once). Each row is guarded on
// published_at is null, like every terminal write here. A write that fails leaves the whole batch
// unmarked, which is what a process that dies between NATS's acknowledgements and this write
// leaves too: the next scan publishes the batch again, and JetStream drops each repeat inside its
// duplicate window by its MsgId.
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
		if result.state == backedOff {
			attempts = append(attempts, new(int32(result.attempts)))
			retryAt = append(retryAt, new(result.retryAt))
		} else {
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

func scanPendingEvents(ctx context.Context, deps Deps) ([]pendingEvent, bool, error) {
	rows, err := deps.Store.Pool.Query(ctx, `
		select e.id, e.issue_key, e.artifact_id::text, coalesce(e.project_key, i.project_key, ar.project_key, ''), e.seq,
		       e.type, e.actor, e.notify, e.created_at, e.payload, e.attempt_count, e.published_destinations,
		       coalesce(ar.slug, ''), coalesce(i.route, ai.route)
		from events e
		left join issues i on i.key = e.issue_key
		left join artifacts ar on ar.id = e.artifact_id
		left join issues ai on ai.key = ar.issue_key
		where e.published_at is null
		  and (e.next_attempt_at is null or e.next_attempt_at <= now())
		  and coalesce(e.payload ->> 'pending', 'false') <> 'true'
		order by e.id
		limit $1
	`, batchSize)
	if err != nil {
		return nil, false, fmt.Errorf("select unpublished events: %w", err)
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
			&item.route,
		); err != nil {
			// pgx scans positionally and treats a failure as fatal to the cursor, so this
			// row ends the batch and rows.Err() below repeats the same error. Carrying the
			// row on as a failure backs it off like any other unpublishable event, which is
			// what lets the events queued behind it be read at all; e.id is the first
			// destination, so the row scanned that far before it failed.
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
	if err := rows.Err(); err != nil && !unreadable {
		return nil, false, fmt.Errorf("iterate unpublished events: %w", err)
	}
	return pending, unreadable, nil
}

// send starts event's attempt: its envelope is sent to each of its destinations not in delivered
// (publishDestinations), and the attempt returned holds the answers still to come (attempt.wait).
// A destination NATS denies (a role route outside Dispatch's grant, say) holds back none of the
// others: it is left unreached, and the denial fails the attempt once every destination was
// tried, so the event is retried. Any other failure that comes back before anything is sent, of
// the connection or of the store, ends the attempt at once, since every destination after it would
// fail the same way. The destinations the attempt reached are recorded with the batch
// (recordBatch), so a retry publishes only the rest.
func send(ctx context.Context, deps Deps, event model.Event, slug string, route *string, delivered map[string]struct{}) *attempt {
	a := &attempt{delivered: delivered}
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
	a.ended = publishDestinations(ctx, deps, event, item, route, a)
	return a
}

// publishDestinations sends item to each of event's destinations in turn, and returns the first
// error that ends the attempt.
func publishDestinations(ctx context.Context, deps Deps, event model.Event, item contracts.Envelope, route *string, a *attempt) error {
	if err := publishDestination(deps, event.ID, item, a, "publish owner topic", item.Topic); err != nil {
		return err
	}
	targeted := (event.Type == "message.created" || event.Type == "message.answered") &&
		payloadString(event.Payload, "target") != ""
	if event.Notify && !targeted {
		if !suppressesCurrentRoute(event.Payload, route) {
			if err := publishRoute(deps, event.ID, item, a, route); err != nil {
				return err
			}
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

// attempt is one pass over an event's destinations. delivered holds the destinations the event
// reached before this pass and those this pass sent, so that none is sent twice; sent is what this
// pass sent, in order, with each one's answer to come. wait reads those answers into reached, the
// destinations NATS accepted or refused (bus.ErrRefused), denied, the denials
// (bus.ErrPublishDenied) the pass went on past, and failed, every other failure. ended is the
// error that ended the pass before it sent everything.
type attempt struct {
	delivered map[string]struct{}
	sent      []sentDestination
	reached   []string
	denied    []error
	failed    []error
	ended     error
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
		select {
		case err = <-sent.answer:
		case <-ctx.Done():
			err = ctx.Err()
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

// err is why the attempt failed - its denials, its other failures and the error that ended it,
// joined - or nil when every destination it sent was reached.
func (a *attempt) err() error {
	return errors.Join(append(append(a.denied, a.failed...), a.ended)...)
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
// skipped, and a topic the route already reached is not published twice.
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
	var sessionIDs []string
	for _, follower := range followers {
		if !event.Actor.SameAs(model.Actor{Kind: "session", ID: follower.SessionID}) {
			sessionIDs = append(sessionIDs, follower.SessionID)
		}
	}
	return publishSessionRoutes(deps, eventID, item, a, sessionIDs, "publish follower route to")
}

// publishAuthorRoutes delivers a human comment-thread action directly to the involved
// sessions' own topics, regardless of the issue's route (which may point at an entirely
// different reviewer) and regardless of whether the event's owner is an issue or a project
// document (which has no route at all). Without this, the agent that started the thread
// or is the thread's own root author never learns about the human's reply, resolution,
// reopening, or edit, and the agent that wrote a document never learns of a new comment on it.
// A reply reaches its thread's root author and its direct parent's author; a new comment on a
// document (no reply_to and no ask_id), and its edit, reach every session that document credits
// (loadDocumentSessionAuthors); a comment on an issue's conversation names no document and reaches
// no author. Ask threads are not walked here: their asker and repliers are the ask's followers
// (publishFollowerRoutes). The event's own actor is skipped, and so is a session the comment's
// mentions already reach (suppressed_authors).
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
	sent := sentDestination{eventID: eventID, topic: item.Topic, label: label, id: id, answer: deps.Publisher.PublishAsync(item)}
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
