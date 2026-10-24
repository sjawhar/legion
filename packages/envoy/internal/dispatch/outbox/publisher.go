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
	// One minute: a batch is batchSize (100) events, each a NATS round trip plus the write
	// that records it, so a healthy batch finishes well inside it, and a process that dies
	// mid-batch leaves its rows claimed for no longer than that before the other process
	// picks them up. A publish that fails is backed off by scheduleRetry, whose write
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
	// maxCommentThreadDepth bounds the reply_to walk in loadRootCommentAuthor so a
	// malformed cycle (comments.reply_to has no acyclicity constraint) cannot spin the
	// recursive query and stall the outbox scan.
	maxCommentThreadDepth = 32
)

// Publisher is the NATS publication boundary used by the Dispatch outbox.
type Publisher interface {
	Publish(contracts.Envelope) error
}

// documentCompactionLease names the lease the daily document compaction runs under. It walks
// every document, so two envoy-dispatch processes compacting at the same moment is twice the work
// for one result.
const documentCompactionLease = "document-compaction"

// Deps are the durable state and delivery dependencies for the outbox.
type Deps struct {
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
// holds its pooled connection until it closes, so publishing - a network round trip, a
// follower lookup, and a write per event - inside the loop would hold one of the pool's four
// connections across all of it. The rows are drained first and the work done after.
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

func scanBatch(ctx context.Context, deps Deps) (int, bool, error) {
	pending, blocked, err := scanPendingEvents(ctx, deps)
	if err != nil {
		return 0, false, err
	}

	for _, item := range pending {
		if item.failure != nil {
			if retryEvent(ctx, deps, item, item.failureLog, item.failure) {
				blocked = true
			}
			continue
		}
		if err := publish(
			ctx, deps, item.event, item.slug, item.route, publishedDestinationSet(item.destinations),
		); err != nil {
			if retryEvent(ctx, deps, item, "dispatch outbox: publish event", err) {
				blocked = true
			}
			continue
		}
		if _, err := deps.Store.Pool.Exec(ctx, `
			update events set published_at = now(), next_attempt_at = null where id = $1 and published_at is null
		`, item.event.ID); err != nil {
			retryEvent(ctx, deps, item, "dispatch outbox: mark event published", err)
			blocked = true
		}
	}
	return len(pending), blocked, nil
}

// retryEvent reports a failed event under what and backs it off, so the events queued behind
// one that cannot be published are not stuck behind it. It reports whether the batch is
// blocked: an event whose retry could not be written stays eligible immediately, and a scan
// that carried on would select the same batch forever.
func retryEvent(ctx context.Context, deps Deps, item pendingEvent, what string, cause error) bool {
	slog.Error(what, "event_id", item.event.ID, "error", cause)
	if err := scheduleRetry(ctx, deps, item.event.ID, item.attempts); err != nil {
		slog.Error("dispatch outbox: schedule retry", "event_id", item.event.ID, "error", err)
		return true
	}
	return false
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
// issue and artifact columns a publish needs, in the order by id every batch has always had.
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
		       coalesce(ar.slug, ''), coalesce(i.route, ai.route)
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
			&item.route,
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
	// here: a row the batch reached and backed off keeps the backoff retryEvent wrote for it,
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

func scheduleRetry(ctx context.Context, deps Deps, eventID int64, attempts int) error {
	attempts++
	if attempts == retry.DeadLetterAttempts {
		slog.Error("dispatch outbox: event reached dead-letter threshold", "event_id", eventID, "attempts", attempts)
	}
	_, err := deps.Store.Pool.Exec(ctx, `
		update events
		set attempt_count = $2, next_attempt_at = $3
		where id = $1 and published_at is null
	`, eventID, attempts, time.Now().Add(retry.Delay(attempts)))
	if err != nil {
		return fmt.Errorf("schedule event retry: %w", err)
	}
	return nil
}

// publish publishes event to each of its destinations. A destination NATS denies (a role route
// outside Dispatch's grant, say) holds back none of the others: it is left unrecorded, and the
// denials are returned joined once every destination was tried, so the event is retried. Any other
// failure, of the connection or of the store, ends the attempt at once, since every destination
// after it would fail the same way, and is returned joined with the denials before it. Each
// destination that succeeded is recorded (publishDestination), so the retry publishes only the
// rest.
func publish(ctx context.Context, deps Deps, event model.Event, slug string, route *string, delivered map[string]struct{}) error {
	if event.IssueKey == nil && event.ArtifactID == nil && event.Project == "" {
		return nil
	}
	item, err := envelope(event, slug)
	if err != nil {
		return err
	}
	if err := item.Validate(); err != nil {
		return fmt.Errorf("validate envelope: %w", err)
	}
	a := &attempt{delivered: delivered}
	err = publishDestinations(ctx, deps, event, item, route, a)
	return errors.Join(append(a.denied, err)...)
}

// publishDestinations publishes item to each of event's destinations in turn, and returns the
// first error that ends the attempt; the denials it goes on past are in a.
func publishDestinations(ctx context.Context, deps Deps, event model.Event, item contracts.Envelope, route *string, a *attempt) error {
	if err := publishDestination(ctx, deps, event.ID, item, a, "publish owner topic", item.Topic); err != nil {
		return err
	}
	targeted := (event.Type == "message.created" || event.Type == "message.answered") &&
		payloadString(event.Payload, "target") != ""
	if event.Notify && !targeted {
		if !suppressesCurrentRoute(event.Payload, route) {
			if err := publishRoute(ctx, deps, event.ID, item, a, route); err != nil {
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
	return publishPreviousClaimant(ctx, deps, event.ID, item, event, a)
}

// attempt is one pass over an event's destinations: the ones already reached, and the denials
// (bus.ErrPublishDenied) the pass went on past.
type attempt struct {
	delivered map[string]struct{}
	denied    []error
}

// publishSessionRoutes publishes item to each session's own topic, naming a failure or a denial by
// label and the session.
func publishSessionRoutes(ctx context.Context, deps Deps, eventID int64, item contracts.Envelope, a *attempt, sessionIDs []string, label string) error {
	for _, sessionID := range sessionIDs {
		routed := item
		routed.Topic = contracts.AgentSubject(sessionID)
		if err := publishDestination(ctx, deps, eventID, routed, a, label, sessionID); err != nil {
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
func publishPreviousClaimant(ctx context.Context, deps Deps, eventID int64, item contracts.Envelope, event model.Event, a *attempt) error {
	if event.Type != "issue.claimed" && event.Type != "issue.released" {
		return nil
	}
	sessionID := payloadPreviousClaimSession(event.Payload)
	if sessionID == "" || event.Actor.SameAs(model.Actor{Kind: "session", ID: sessionID}) {
		return nil
	}
	return publishSessionRoutes(ctx, deps, eventID, item, a, []string{sessionID}, "publish claim change to")
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
	return publishSessionRoutes(ctx, deps, eventID, item, a, sessionIDs, "publish follower route to")
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
// event's own actor is skipped, and so is a session the comment's mentions already reach
// (suppressed_authors).
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

	return publishSessionRoutes(ctx, deps, eventID, item, a, targets, "publish author route to")
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

// publishDestination publishes item to its topic once per event: a destination the event already
// reached is skipped, and one it reaches is recorded. A destination NATS refuses (bus.ErrRefused:
// the envelope is past the server's max payload, or its subject is past NATS's limit or holds
// whitespace, as an unbounded document slug or a bearer's session id can make it) is refused the
// same way however often it is retried, so it is logged, once, and recorded like a publication, and
// the event goes on to its other destinations. A destination NATS denies (bus.ErrPublishDenied) is
// left unrecorded and its denial added to a, and the attempt goes on; every other error ends it.
// Either is named by label and id (`publish route "role:reviewer": …`), since a subject alone does
// not say which of the event's destinations it was.
func publishDestination(ctx context.Context, deps Deps, eventID int64, item contracts.Envelope, a *attempt, label, id string) error {
	if _, ok := a.delivered[item.Topic]; ok {
		return nil
	}
	if err := deps.Publisher.Publish(item); err != nil {
		switch {
		case errors.Is(err, bus.ErrPublishDenied):
			a.denied = append(a.denied, fmt.Errorf("%s %q: %w", label, id, err))
			return nil
		case !errors.Is(err, bus.ErrRefused):
			return fmt.Errorf("%s %q: %w", label, id, err)
		}
		slog.Error("dispatch outbox: destination refused", "event_id", eventID, "topic", item.Topic, "error", err)
	}
	if _, err := deps.Store.Pool.Exec(ctx, `
		update events
		set published_destinations = array_append(published_destinations, $2)
		where id = $1 and published_at is null and not ($2 = any(published_destinations))
	`, eventID, item.Topic); err != nil {
		return fmt.Errorf("%s %q: record published destination: %w", label, id, err)
	}
	a.delivered[item.Topic] = struct{}{}
	return nil
}

func publishRoute(ctx context.Context, deps Deps, eventID int64, item contracts.Envelope, a *attempt, route *string) error {
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
	return publishDestination(ctx, deps, eventID, item, a, "publish route", *route)
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
