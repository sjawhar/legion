package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
)

// fallbackClaim is how long one fallback pass's claim keeps a comment_fallback_copies row out of
// every other pass: the claim is a next_attempt_at of its own (claimFallbackCopies), so a pass
// that dies mid-decision leaves its rows claimed for no longer than this before the next pass
// takes them. A pass decides at most batchSize rows, each a read, at most a short transaction and
// one publish, so a healthy one finishes well inside it.
const fallbackClaim = time.Minute

// mentionAbandonedError is the error the outbox records on a mention attempt it settles failed
// because its send was abandoned (mentionComment.settleAbandoned).
const mentionAbandonedError = "the outbox published the comment's own copy instead; the send was abandoned"

// mentionDeliveries is what a new comment's mentions did, read from comment_deliveries when the
// event is published (loadMentionDeliveries). A mention resolved at write time names only a
// candidate: its send runs after the comment commits and can fail - the listener does not know the
// session, the receipt times out, the handler is cancelled, the session lacks the delivery mode -
// and a session whose mention failed has no copy but the outbox's.
type mentionDeliveries struct {
	// sessions is keyed by the session each attempt counts under (model.MentionSessionSQL).
	sessions mentionOutcomes
	// targets is keyed by each attempt's target as written, for a route suppressed by that string
	// alone (payloadSuppressedRouteTarget).
	targets mentionOutcomes
	// fallbackKeys is keyed like sessions, and holds for each session with a failed attempt - or
	// an abandoned one, which the outbox settles failed before it publishes the copy - the dedupe
	// key that attempt's send went under (contracts.AgentSendDedupeKey of
	// model.CommentMentionIdempotencyKey), which the session's copy carries
	// (destinations.addSession, publishFallbackCopies). A send that landed before its attempt
	// failed - a receipt timeout, or a send handed the listener before a kill whose resend then
	// failed - has already given the host that key, so the host drops the copy; a session no
	// mention reached shows it. Every attempt of one mention sends under that one key, which
	// carries the mention's own mode and no attempt's, so which of a target's failed attempts is
	// the latest, and what mode it went in, do not change it. A session whose failed attempts
	// span several targets keys by the lowest target. One key is enough: each target is a send of
	// its own, so a comment naming one session under two targets gives it one copy per target
	// that lands, and a copy shown beside another target's landed mention leaves the session no
	// more copies than had every one of its mentions landed.
	fallbackKeys map[string]string
	// targetFallbackKeys is keyed like targets, and holds for each target with a failed or
	// abandoned attempt that names a session the dedupe key of that target's mention (of the
	// lowest session, should its attempts name several), which a fallback copy keyed by that
	// target carries (publishFallbackCopies).
	targetFallbackKeys map[string]string
	// held holds each destination of the comment a fallback row owns (comment_fallback_copies):
	// the fallback pass publishes or settles it, and the event's own publish skips it
	// (destinations.addDestination).
	held map[string]struct{}
}

// newMentionDeliveries is the empty reading of one comment's mentions, which every read fills.
func newMentionDeliveries() mentionDeliveries {
	return mentionDeliveries{
		sessions:           mentionOutcomes{kind: sessionMentions, states: map[string]mentionState{}},
		targets:            mentionOutcomes{kind: targetMentions, states: map[string]mentionState{}},
		fallbackKeys:       map[string]string{},
		targetFallbackKeys: map[string]string{},
		held:               map[string]struct{}{},
	}
}

// of is the outcomes of kind's keys and the dedupe keys a copy each of them decides carries.
func (d mentionDeliveries) of(kind mentionKind) (mentionOutcomes, map[string]string) {
	if kind == targetMentions {
		return d.targets, d.targetFallbackKeys
	}
	return d.sessions, d.fallbackKeys
}

// mentionComment is the comment whose mention attempts a mentionDeliveries read, the
// comment.created event that created it, and the context and store a settle or a hold of one of
// its keys runs on (settleAbandoned, holdFallbackCopy). It lives as long as the one batch or
// fallback pass that read it.
type mentionComment struct {
	ctx   context.Context
	deps  Deps
	event model.Event
	id    string
}

// mentionKind is the key a comment's mention attempts are grouped under: the session each counts
// under (model.MentionSession), or its target as written.
type mentionKind int

const (
	sessionMentions mentionKind = iota
	targetMentions
)

// attemptKey is the SQL expression of a comment_deliveries row's key of this kind.
func (k mentionKind) attemptKey() string {
	if k == targetMentions {
		return "target"
	}
	return model.MentionSessionSQL("comment_deliveries")
}

// fallbackColumn is the comment_fallback_copies column that names a row's key of this kind.
func (k mentionKind) fallbackColumn() string {
	if k == targetMentions {
		return "target"
	}
	return "session_id"
}

// mentionState is what the attempts under one key of mentionDeliveries came to. The states are
// in the order a key whose attempts came to several counts as the first of them.
type mentionState int

const (
	// mentionSent: an attempt reached the session, so the mention delivered the comment.
	mentionSent mentionState = iota
	// mentionResending: an attempt past the lease the outbox waits a send out for
	// (model.MentionResumedSQL): the delivery sweep's to resend, or a queued send's to make, or
	// one a later attempt overtook, which its sender sends when its lane reaches it, or settles
	// superseded once a later attempt has delivered the comment.
	mentionResending
	// mentionSending: an attempt whose send is live (model.MentionLiveSQL).
	mentionSending
	// mentionAbandoned: an attempt whose send was abandoned (model.MentionAbandonedSQL).
	mentionAbandoned
	// mentionFailed: every attempt failed, or none was recorded.
	mentionFailed
)

// mentionStateOf is the state of a key whose attempts hold a sent one, one being resent, one
// being sent and an abandoned one, as each is true.
func mentionStateOf(sent, resending, sending, abandoned bool) mentionState {
	switch {
	case sent:
		return mentionSent
	case resending:
		return mentionResending
	case sending:
		return mentionSending
	case abandoned:
		return mentionAbandoned
	}
	return mentionFailed
}

// mentionOutcomes is what the attempts under each key of one kind came to.
type mentionOutcomes struct {
	kind mentionKind
	// states holds the state of each key with an attempt; a key with none is mentionFailed.
	states map[string]mentionState
}

// state is the state of key's attempts.
func (o mentionOutcomes) state(key string) mentionState {
	if state, ok := o.states[key]; ok {
		return state
	}
	return mentionFailed
}

// record adds what the attempts of one row of mentionDeliveryQuery came to under key: the first
// state of those key's rows came to.
func (o mentionOutcomes) record(key string, state mentionState) {
	if current, ok := o.states[key]; !ok || state < current {
		o.states[key] = state
	}
}

// leftToMention is the one rule every session route of a new comment applies to a session the
// comment's mentions resolved to: it reports whether the route leaves that session's copy, on its
// own topic, to the mention (destinations.leftTo). Callers ask only about a session their payload
// names as mentioned; a session no mention delivers to has no attempt, so it is never skipped.
func (d *destinations) leftToMention(sessionID string) bool {
	return d.leftTo(d.mentions.sessions, sessionID, contracts.AgentSubject(sessionID))
}

// leftTo reports whether the copy of the comment at destination, which key's attempts decide, is
// left to the mention. A destination a fallback row owns is the fallback pass's, so the copy is
// skipped. A mention that reached the session delivered the comment, so the copy is skipped. A
// mention still being sent may yet, so the copy is held back and the event marked mentionPending,
// to be decided again when it is next read (settleEvent blocks it for mentionPendingDelay). A
// mention past the lease that wait is bounded by (model.MentionResumedSQL: the delivery sweep
// resends it, a sender holds it queued still, or a later attempt overtook it) is that send's: the
// copy is skipped and held as a fallback row (mentionComment.holdFallbackCopy), which the fallback
// pass publishes should the send fail (publishFallbackCopies); an insert that fails leaves the
// copy out of the list and backs the event off, so it is read again. A destination this event
// already reached gave the session a copy, so it needs no row. A mention whose send was abandoned
// is settled by the outbox first (mentionComment.settleAbandoned), and the copy follows what key's
// attempts are once that settle commits: one a sender claimed before the settle took its row is
// pending, so the copy is held back and the sender owns the outcome; one the settle took is
// failed, and the copy is the outbox's. Only one of the two acts on an attempt, so the session
// gets the mention or the copy, not both. A settle that fails holds the copy back and backs the
// event off, so it is read again. Any other key - every attempt failed, or none was recorded - is
// published to as if it had not been mentioned, except that a session copy for a failed attempt
// carries that attempt's dedupe key (destinations.addSession): a send that landed before its
// attempt failed has given the host the comment already, and the host drops the copy.
func (d *destinations) leftTo(outcomes mentionOutcomes, key, destination string) bool {
	if _, ok := d.mentions.held[destination]; ok {
		return true
	}
	state := outcomes.state(key)
	if state == mentionAbandoned {
		settled, err := d.comment.settleAbandoned(outcomes.kind, key)
		if err != nil {
			d.failed = append(d.failed, err)
			outcomes.states[key] = mentionSending
			return true
		}
		state = settled
		outcomes.states[key] = settled
	}
	switch state {
	case mentionSent:
		return true
	case mentionSending:
		d.mentionPending = true
		return true
	case mentionResending:
		if _, ok := d.delivered[destination]; ok {
			return true
		}
		if err := d.comment.holdFallbackCopy(outcomes.kind, key, destination); err != nil {
			d.failed = append(d.failed, err)
			return true
		}
		d.mentions.held[destination] = struct{}{}
		return true
	}
	return false
}

// suppressesCurrentRoute applies a create-time suppression to the route that was resolved
// alongside the mention, including a later direct route to that same resolved session. A route
// edit to any other target must still wake its new destination. A suppression that names the
// session the route resolved to when the comment was written (suppressed_route_session_id) holds
// only once the mention of that session delivered the comment, by the rule destinations.leftTo
// applies. One that names the route string alone, the target of a mention whose lookup went
// unanswered (payloadSuppressedRouteTarget), holds while the route is still that string and only
// once the mention of that target delivered the comment, the same rule keyed by the target. Either
// way the copy leftTo holds back is the route's own topic (routeTopic). A route whose mention
// failed is published to.
func suppressesCurrentRoute(payload any, current *string, d *destinations) bool {
	if !payloadBool(payload, "suppress_route") || current == nil || *current == "" {
		return false
	}
	topic := routeTopic(*current)
	if topic == "" {
		return false
	}
	sessionID := payloadString(payload, "suppressed_route_session_id")
	if sessionID == "" {
		target := payloadSuppressedRouteTarget(payload)
		return target != "" && *current == target && d.leftTo(d.mentions.targets, target, topic)
	}
	if *current != payloadString(payload, "suppressed_route") && *current != "session:"+sessionID {
		return false
	}
	return d.leftTo(d.mentions.sessions, sessionID, topic)
}

// routeTopic is the subject addRoute gives route's copy, "" for a route that does not parse or
// names neither a role nor a session.
func routeTopic(route string) string {
	parsed, err := model.ParseRoute(route)
	if err != nil {
		return ""
	}
	switch parsed.Kind {
	case "role":
		return contracts.RoleTopicPrefix + parsed.ID
	case "session":
		return contracts.AgentSubject(parsed.ID)
	}
	return ""
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

// payloadMentionedSessions reads the sessions a comment payload's mentions count under
// (model.MentionSession), as the batch decodes the payload from JSON: each mentions[].session_id,
// and the <id> of a session:<id> mention that names none (its lookup went unanswered, so the
// sender resolves it after the commit). These are candidates only: a session route skips one once
// the mention delivered to it (destinations.leftToMention).
func payloadMentionedSessions(payload any) map[string]struct{} {
	values, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	mentions, _ := values["mentions"].([]any)
	result := map[string]struct{}{}
	for _, entry := range mentions {
		if sessionID, ok := model.MentionSession(payloadString(entry, "session_id"), payloadString(entry, "target")); ok {
			result[sessionID] = struct{}{}
		}
	}
	return result
}

// holdFallbackCopy records the fallback row of the copy of the comment at destination, decided by
// the attempts under key of kind, unless the destination has one already (publishFallbackCopies).
// It commits before the batch that holds the copy back can mark the event published.
func (c mentionComment) holdFallbackCopy(kind mentionKind, key, destination string) error {
	if _, err := c.deps.Store.Pool.Exec(c.ctx, `
		insert into comment_fallback_copies (comment_id, destination, event_id, `+kind.fallbackColumn()+`)
		values ($1, $2, $3, $4)
		on conflict (comment_id, destination) do nothing
	`, c.id, destination, c.event.ID, key); err != nil {
		return fmt.Errorf("hold the fallback copy of comment %q at %s: %w", c.id, destination, err)
	}
	return nil
}

// settleAbandoned settles failed, with mentionAbandonedError, every attempt of the comment whose
// key of kind is key and whose send was abandoned (model.MentionAbandonedSQL), appends each one's
// comment.delivery receipt as the API's sender would (the comment's author its actor), and reports
// the state key's attempts are in then: sent, past the lease a send is waited out for, being
// sent, or failed. It is one transaction, and it takes the comment's owner lock before any
// attempt row, the order the API's claim and settle transactions take them in
// (claimCommentDeliveryAttempt, settleDeliveryAttempt in internal/dispatch/api), so it and a
// sender's claim of the same attempt run one after the other. A claim that commits first leaves
// the row claimed just now, so the update's guard no longer matches it and the attempt reads
// pending; an attempt this settles first is no longer pending when the claim reads it, and the
// claim takes it as settled and sends nothing. The receipts reach the event log in the same
// commit, so a reader sees the attempt fail, and the outbox publishes them like any event.
func (c mentionComment) settleAbandoned(kind mentionKind, key string) (state mentionState, err error) {
	ctx, deps, event, commentID := c.ctx, c.deps, c.event, c.id
	defer func() {
		if err != nil {
			err = fmt.Errorf("settle the abandoned mention attempts of comment %q for %q: %w", commentID, key, err)
		}
	}()
	tx, err := deps.Store.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	owner := model.Event{IssueKey: event.IssueKey, ArtifactID: event.ArtifactID}
	if err := deps.Broker.LockOwners(ctx, tx, owner); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `
		update comment_deliveries set state = 'failed', error = $3
		where comment_id = $1 and `+kind.attemptKey()+` = $2 and state = 'pending' and `+model.MentionAbandonedSQL("comment_deliveries")+`
		returning target, attempt, delivery, session_id, reply_id::text
	`, commentID, key, mentionAbandonedError)
	if err != nil {
		return 0, err
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
		return 0, err
	}
	receipts := make([]model.Event, 0, len(settled))
	for _, payload := range settled {
		receipt := owner
		receipt.Type, receipt.Actor, receipt.Payload = "comment.delivery", event.Actor, payload
		appended, err := deps.Broker.Append(ctx, tx, receipt)
		if err != nil {
			return 0, err
		}
		receipts = append(receipts, appended)
	}
	var sent, resending, pending bool
	if err := tx.QueryRow(ctx, `
		select coalesce(bool_or(state = 'sent'), false),
		       coalesce(bool_or(state = 'pending' and `+model.MentionResumedSQL("comment_deliveries")+`), false),
		       coalesce(bool_or(state = 'pending'), false)
		from comment_deliveries
		where comment_id = $1 and `+kind.attemptKey()+` = $2
	`, commentID, key).Scan(&sent, &resending, &pending); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	for _, receipt := range receipts {
		deps.Broker.Publish(receipt)
	}
	return mentionStateOf(sent, resending, pending, false), nil
}

// fallbackCopy is one comment_fallback_copies row a fallback pass claimed: the copy of the comment
// at destination, the comment.created event eventID's envelope, decided by the attempts under key
// of kind. attempts is how many of the row's passes have failed so far (attempt_count), and claim
// the next_attempt_at the pass's claim set, which every write of the pass's record is guarded on
// (recordFallbackCopies).
type fallbackCopy struct {
	commentID   string
	destination string
	eventID     int64
	key         string
	kind        mentionKind
	attempts    int
	claim       time.Time
}

// fallbackEvent is the comment.created event a fallback copy is the envelope of, and the slug of
// its artifact (envelope).
type fallbackEvent struct {
	event model.Event
	slug  string
}

// fallbackRecord is what a fallback pass came to for one row: settled, and published when it
// published the copy; failed when its decision or publish failed, which counts an attempt on the
// row and backs it off (failedFallbackCopy). A record neither settled nor failed releases the
// row's claim for a later pass, counting no attempt, and delay is how long the row waits before
// it is due again: mentionPendingDelay while its mention is still being sent, retry.BaseDelay
// while NATS is unreachable, and none at all when the publisher hold's deadline fenced the send
// or the process is stopping, where whatever pass runs next takes the row at once.
type fallbackRecord struct {
	held      fallbackCopy
	settled   bool
	published bool
	failed    bool
	delay     time.Duration
}

// failedFallbackCopy is the record of a row whose decision or publish failed: one more attempt
// counted on it, and its next attempt retry.Delay of that count away, the schedule an event backs
// off by (backOff). Nothing about a publish failing is permanent, so the row is never given up:
// the attempt that brings its count to retry.DeadLetterAttempts, the threshold the event scan logs
// at, is logged at Error, and the row is retried on the same schedule after it.
func failedFallbackCopy(held fallbackCopy) fallbackRecord {
	if attempts := held.attempts + 1; attempts == retry.DeadLetterAttempts {
		slog.Error("dispatch outbox: fallback copy reached dead-letter threshold",
			"comment_id", held.commentID, "destination", held.destination, "attempts", attempts)
	}
	return fallbackRecord{held: held, failed: true}
}

// publishFallbackCopies is the fallback pass. This is the one description of how a mentioned
// session gets a new comment once - by its mention, or by the outbox's copy - however the
// mention's send ends; every other description points here.
//
// A comment's mention is sent by the API after the comment commits (mentionSender in
// internal/dispatch/api), and every destination of the comment.created event that would reach a
// mentioned session - the route, the claimant, an author, a follower - leaves that session's copy
// to the mention by the rule destinations.leftTo applies to the attempts the session counts under
// (model.MentionSession). A sent attempt delivered the comment, so the copy is skipped; a live
// one (model.MentionLiveSQL) holds the copy back and blocks the event for mentionPendingDelay, for
// the model.DeliveryClaimLapse after its comment committed and no longer; a failed one leaves the
// copy to the outbox, under the failed send's dedupe key. An abandoned attempt
// (model.MentionAbandonedSQL) is settled failed by the outbox, with its comment.delivery receipt,
// before it publishes the copy (mentionComment.settleAbandoned); a sender's claim of it runs
// before or after that settle under the comment's owner lock, so whichever commits first acts on
// the attempt and the other finds it taken.
//
// A mention attempt past that lease (model.MentionResumedSQL) is a send the outbox waits no
// longer on: the delivery sweep resends it (resumeOwedDeliveries in internal/dispatch/api) under
// the attempt's own idempotency key once no sender holds it - the process that queued it died
// with the send in flight, or never started it, perhaps after the listener took it, and the
// listener drops the resend as a repeat when it did - or a live sender holds it, queued behind a
// lane its recipient has held longer than a lease, and sends it when that lane reaches it, or
// settles it superseded without sending when a Retry from the card has delivered the comment
// meanwhile (supersedeCommentDelivery). The outbox cannot tell which, and holding the comment's
// event for a lane's minutes would be no bound at all, so it neither publishes that copy nor
// waits on the send. It records the copy it held back as a comment_fallback_copies row instead,
// keyed by the comment and the exact destination topic the copy would have gone to and naming the
// key whose attempts decide it (the session, or the target of a route suppressed by that string
// alone), inserted idempotently before the batch can mark the event published; an insert that
// fails backs the event off, so it is read again. From then on the row owns that destination: the
// event's own publish skips it on every retry (mentionDeliveries.held,
// destinations.addDestination), so the event never sends it too, and the event is published and
// recorded as it would be had the mention been sent.
//
// The pass runs on every scan the publisher hold's holder makes, beside the event scan
// (Outbox.serve), and under that hold: it claims a bounded batch of unsettled rows, oldest first
// (next_attempt_at pushed out by fallbackClaim under for update skip locked, so a second process
// skips them), and decides each by its key's attempts, read as leftTo reads them
// (mentionDeliveriesOf): a sent attempt delivered the comment, so the row is settled with nothing
// sent; an attempt still being sent or resent leaves the row to a later pass, its claim released
// for mentionPendingDelay, so the pass of every wake does not read it again; an abandoned attempt
// is settled failed first (mentionComment.settleAbandoned, which a sender's claim of it runs after
// or before under the owner lock) and the row decided again; and when no attempt was sent and none
// is pending, the comment.created event's envelope is published to the row's destination alone,
// carrying the dedupe key of the failed attempt's send (mentionDeliveries.fallbackKeys for a
// session, targetFallbackKeys for a target), and the row is marked published. The pass never
// writes the events row: the event's published_at, published_destinations and payload stay as its
// own publish left them, and no destination but the row's is reached, whatever the issue's route,
// claimant, followers or authors are by then. A decision or a publish that fails counts an attempt
// on the row (attempt_count) and moves its next_attempt_at out by retry.Delay of that count, the
// schedule an event backs off by, so it is decided again once that passes; nothing about a publish
// failing is permanent, so the row is never given up, and the attempt that brings the count to
// retry.DeadLetterAttempts is logged at Error (failedFallbackCopy). One cut short by NATS being
// unreachable counts nothing - the outage is no fault of the row, as it is none of an event whose
// send met the connection (settleEvent) - and releases the claim for retry.BaseDelay, the wait a
// scan that failed is tried again after: the holder gives the hold up for NATS lost, and the
// first pass of whichever holder follows, armed to that wait (Outbox.nextRetry), takes the row.
// One cut short by the process stopping counts nothing and releases it at once too, for whatever
// process runs next, and a pass that starts once the process is already stopping claims nothing
// at all; a process that dies between the publish
// and the mark leaves the claim, and the repeat carries the same dedupe key, which the host, and
// the stream within its duplicate window, drop.
//
// The pass is fenced by the publisher hold as a batch is: it claims nothing once the hold's loss
// deadline has passed, sends through the hold's window (Publisher.WindowUntil), and leaves its
// record unwritten when that deadline passed during it, the record being the last act checked
// against it - the rows it claimed are then left claimed until fallbackClaim lapses, which the
// next holder's pass waits out. Every write of a pass's record is guarded on the claim the pass
// took (recordFallbackCopies), so a pass that outlasts fallbackClaim writes nothing to a row a
// later pass has claimed since.
//
// Races. A person's Retry of the mention opens or resumes an attempt the pass reads as being
// sent, so the row waits for it, and a Retry that is sent settles the row with nothing sent. A
// Retry that starts once the pass has decided to publish can deliver the mention and the copy
// both, under one dedupe key when it is sent in the failed attempt's mode, so the host shows one.
// A send the killed process had handed the listener before it died, whose resend then fails for
// good (the listener down after the restart), gives the host that mention and then the copy under
// the mention's own dedupe key, which the host drops. The API's failed settle appends a
// comment.delivery receipt, an event insert that wakes the holder, and the pass runs on the scan
// that wake starts.
func publishFallbackCopies(ctx context.Context, deps Deps, hold fence) {
	if ctx.Err() != nil {
		// Nothing this pass claimed could be decided: every row would be read and handed straight
		// back, and the events behind them read on a context that is already done. The rows are
		// left for whatever process runs next, as a pass that claimed them would leave them.
		return
	}
	if !time.Now().Before(hold.Deadline()) {
		// Past the publisher hold's loss deadline this process publishes nothing: the hold is
		// another's to take, and its pass decides these rows.
		return
	}
	copies, err := claimFallbackCopies(ctx, deps)
	if err != nil {
		slog.Error("dispatch outbox: claim fallback copies", "error", err)
		return
	}
	if len(copies) == 0 {
		return
	}
	var records []fallbackRecord
	// Written even when ctx has ended: a process stopping mid-pass records what NATS accepted and
	// releases what it did not decide, rather than leave those rows claimed for fallbackClaim.
	defer func() {
		if !time.Now().Before(hold.Deadline()) {
			slog.Error("dispatch outbox: the publisher hold's deadline passed before the fallback pass was recorded, so it is left unrecorded",
				"copies", len(records))
			return
		}
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		defer cancel()
		if err := recordFallbackCopies(record, deps, records); err != nil {
			slog.Error("dispatch outbox: record fallback copies", "error", err)
		}
	}()
	// release leaves the row to a later pass, counting no attempt: it is due again once delay has
	// passed, or at once when delay is none.
	release := func(held fallbackCopy, delay time.Duration) {
		records = append(records, fallbackRecord{held: held, delay: delay})
	}
	fail := func(held fallbackCopy) {
		if ctx.Err() != nil {
			release(held, 0)
			return
		}
		records = append(records, failedFallbackCopy(held))
	}
	events, err := loadFallbackEvents(ctx, deps, copies)
	if err != nil {
		slog.Error("dispatch outbox: read the events of fallback copies", "error", err)
		for _, held := range copies {
			fail(held)
		}
		return
	}
	type sentCopy struct {
		held   fallbackCopy
		answer <-chan error
	}
	sender := deps.Publisher.WindowUntil(hold.Deadline)
	var sent []sentCopy
	for _, held := range copies {
		if ctx.Err() != nil {
			release(held, 0)
			continue
		}
		event, ok := events[held.eventID]
		if !ok {
			slog.Error("dispatch outbox: fallback copy names no event", "comment_id", held.commentID, "event_id", held.eventID)
			fail(held)
			continue
		}
		decision, item, err := decideFallbackCopy(ctx, deps, held, event)
		switch {
		case err != nil:
			slog.Error("dispatch outbox: decide fallback copy", "comment_id", held.commentID, "destination", held.destination, "error", err)
			fail(held)
		case decision == fallbackPublishes:
			sent = append(sent, sentCopy{held: held, answer: sender.PublishAsync(item)})
		case decision == fallbackSettles:
			records = append(records, fallbackRecord{held: held, settled: true})
		default:
			// The key's mention is still being sent or resent: the row waits for it as an event
			// whose copy that mention holds back waits, rather than being claimed and read again
			// by every pass until the mention ends.
			release(held, mentionPendingDelay)
		}
	}
	for _, item := range sent {
		var err error
		// An answer already in is read before ctx is: a stopping process records what NATS
		// accepted.
		select {
		case err = <-item.answer:
		default:
			select {
			case err = <-item.answer:
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, bus.ErrRefused):
			deps.countPublishFailure("refused")
			slog.Error("dispatch outbox: fallback copy refused", "comment_id", item.held.commentID, "destination", item.held.destination, "error", err)
		case errors.Is(err, bus.ErrWindowDeadline):
			// The hold's deadline refused the send: it never left this process, so it is no
			// failure of the row and counts nothing. The record below is left unwritten too, so
			// the row keeps its claim until it lapses.
			release(item.held, 0)
			continue
		default:
			// Counted as a batch counts a destination that did not reach NATS (sendChains), except
			// an answer a stopping process stopped waiting for, which says nothing of NATS.
			switch {
			case errors.Is(err, bus.ErrPublishDenied):
				deps.countPublishFailure("denied")
			case ctx.Err() == nil || !errors.Is(err, ctx.Err()):
				deps.countPublishFailure("failed")
			}
			slog.Error("dispatch outbox: publish fallback copy", "comment_id", item.held.commentID, "destination", item.held.destination, "error", err)
			if errors.Is(err, bus.ErrUnavailable) {
				release(item.held, retry.BaseDelay)
			} else {
				fail(item.held)
			}
			continue
		}
		records = append(records, fallbackRecord{held: item.held, settled: true, published: true})
	}
}

// fallbackDecision is what a fallback pass does with one row (decideFallbackCopy).
type fallbackDecision int

const (
	// fallbackWaits: the key's mention is still being sent or resent; the row waits for a later
	// pass.
	fallbackWaits fallbackDecision = iota
	// fallbackSettles: the key's mention was sent; the row is settled with nothing sent.
	fallbackSettles
	// fallbackPublishes: no attempt of the key was sent and none is pending; the copy is published.
	fallbackPublishes
)

// decideFallbackCopy decides one claimed row by its key's attempts (publishFallbackCopies), and
// returns, when the copy is to be published, the envelope to publish.
func decideFallbackCopy(
	ctx context.Context, deps Deps, held fallbackCopy, event fallbackEvent,
) (fallbackDecision, contracts.Envelope, error) {
	deliveries, err := mentionDeliveriesOf(ctx, deps.Store.Pool, held.commentID)
	if err != nil {
		return 0, contracts.Envelope{}, err
	}
	outcomes, keys := deliveries.of(held.kind)
	state := outcomes.state(held.key)
	if state == mentionAbandoned {
		comment := mentionComment{ctx: ctx, deps: deps, event: event.event, id: held.commentID}
		if state, err = comment.settleAbandoned(held.kind, held.key); err != nil {
			return 0, contracts.Envelope{}, err
		}
	}
	switch state {
	case mentionSent:
		return fallbackSettles, contracts.Envelope{}, nil
	case mentionResending, mentionSending:
		return fallbackWaits, contracts.Envelope{}, nil
	}
	item, err := envelope(event.event, event.slug)
	if err != nil {
		return 0, contracts.Envelope{}, err
	}
	item.Topic = held.destination
	if key, ok := keys[held.key]; ok {
		item.DedupeKey = key
	}
	if err := item.Validate(); err != nil {
		return 0, contracts.Envelope{}, fmt.Errorf("validate envelope: %w", err)
	}
	return fallbackPublishes, item, nil
}

// claimFallbackCopies claims at most batchSize unsettled fallback rows that are due - never
// claimed, or past the claim, the wait or the backoff their next_attempt_at holds - oldest first,
// in one statement: for update skip locked, so a second process's pass skips them, and
// next_attempt_at pushed out by fallbackClaim in the same breath, which each row returns as its
// claim. It reads the partial index of unsettled rows, so a pass with nothing to decide reads no
// row.
//
// The statement runs outside the pass's context, under claimTimeout of its own, as the record
// step does: a context that ends once it has committed would answer context canceled and lose the
// rows it claimed, which would then sit claimed for fallbackClaim with no pass deciding them. The
// pass reads its own ctx instead and releases every row it claimed at once (publishFallbackCopies).
func claimFallbackCopies(ctx context.Context, deps Deps) ([]fallbackCopy, error) {
	claim, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimTimeout)
	defer cancel()
	rows, err := deps.Store.Pool.Query(claim, `
		update comment_fallback_copies set next_attempt_at = now() + $2
		where (comment_id, destination) in (
			select comment_id, destination from comment_fallback_copies
			where settled_at is null and (next_attempt_at is null or next_attempt_at <= now())
			order by created_at
			limit $1
			for update skip locked
		)
		returning comment_id::text, destination, event_id, session_id, target, attempt_count, next_attempt_at
	`, batchSize, fallbackClaim)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (fallbackCopy, error) {
		var held fallbackCopy
		var sessionID, target *string
		if err := row.Scan(
			&held.commentID, &held.destination, &held.eventID, &sessionID, &target, &held.attempts, &held.claim,
		); err != nil {
			return fallbackCopy{}, err
		}
		if target != nil {
			held.key, held.kind = *target, targetMentions
		} else if sessionID != nil {
			held.key, held.kind = *sessionID, sessionMentions
		}
		return held, nil
	})
}

// loadFallbackEvents reads, by id, the comment.created events copies are the envelopes of, with
// the artifact slug a document's envelope needs.
func loadFallbackEvents(ctx context.Context, deps Deps, copies []fallbackCopy) (map[int64]fallbackEvent, error) {
	ids := make([]int64, 0, len(copies))
	for _, held := range copies {
		ids = append(ids, held.eventID)
	}
	rows, err := deps.Store.Pool.Query(ctx, `
		select e.id, e.issue_key, e.artifact_id::text, coalesce(e.project_key, i.project_key, ar.project_key, ''), e.seq,
		       e.type, e.actor, e.notify, e.created_at, e.payload, coalesce(ar.slug, '')
		from events e
		left join issues i on i.key = e.issue_key
		left join artifacts ar on ar.id = e.artifact_id
		where e.id = any($1)
	`, ids)
	if err != nil {
		return nil, err
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (fallbackEvent, error) {
		var item fallbackEvent
		var actor, payload []byte
		if err := row.Scan(
			&item.event.ID, &item.event.IssueKey, &item.event.ArtifactID, &item.event.Project, &item.event.Seq,
			&item.event.Type, &actor, &item.event.Notify, &item.event.CreatedAt, &payload, &item.slug,
		); err != nil {
			return fallbackEvent{}, err
		}
		if err := json.Unmarshal(actor, &item.event.Actor); err != nil {
			return fallbackEvent{}, fmt.Errorf("decode event %d actor: %w", item.event.ID, err)
		}
		if err := json.Unmarshal(payload, &item.event.Payload); err != nil {
			return fallbackEvent{}, fmt.Errorf("decode event %d payload: %w", item.event.ID, err)
		}
		return item, nil
	})
	if err != nil {
		return nil, err
	}
	events := make(map[int64]fallbackEvent, len(read))
	for _, item := range read {
		events[item.event.ID] = item
	}
	return events, nil
}

// recordFallbackCopies writes what a fallback pass came to, in one statement: each row settled is
// marked so, published when it published the copy; each failed row counts one more attempt and
// waits retry.Delay of its new count from now (failedFallbackCopy); and each row left for a later
// pass has its claim replaced by the wait it was released with, which counts no attempt, or
// cleared when that wait is none (fallbackRecord). Every write is guarded on the claim the pass
// took (fallbackCopy's claim): a row whose next_attempt_at is no longer that claim was claimed
// again by a later pass, once the claim lapsed, and is that pass's, so it is not written. A row a
// pass did not reach is not written either and keeps its claim.
func recordFallbackCopies(ctx context.Context, deps Deps, records []fallbackRecord) error {
	if len(records) == 0 {
		return nil
	}
	commentIDs := make([]string, 0, len(records))
	destinations := make([]string, 0, len(records))
	claims := make([]time.Time, 0, len(records))
	settled := make([]bool, 0, len(records))
	published := make([]bool, 0, len(records))
	counted := make([]bool, 0, len(records))
	retryAfter := make([]*time.Duration, 0, len(records))
	for _, record := range records {
		commentIDs = append(commentIDs, record.held.commentID)
		destinations = append(destinations, record.held.destination)
		claims = append(claims, record.held.claim)
		settled = append(settled, record.settled)
		published = append(published, record.published)
		counted = append(counted, record.failed)
		switch {
		case record.failed:
			retryAfter = append(retryAfter, new(retry.Delay(record.held.attempts+1)))
		case record.delay > 0:
			retryAfter = append(retryAfter, new(record.delay))
		default:
			retryAfter = append(retryAfter, nil)
		}
	}
	_, err := deps.Store.Pool.Exec(ctx, `
		update comment_fallback_copies c set
			settled_at = case when o.settled then now() end,
			published = o.published,
			attempt_count = c.attempt_count + case when o.counted then 1 else 0 end,
			next_attempt_at = now() + o.retry_after
		from unnest($1::uuid[], $2::text[], $3::timestamptz[], $4::bool[], $5::bool[], $6::interval[], $7::bool[])
			as o(comment_id, destination, claim, settled, published, retry_after, counted)
		where c.comment_id = o.comment_id and c.destination = o.destination and c.settled_at is null
			and c.next_attempt_at = o.claim
	`, commentIDs, destinations, claims, settled, published, retryAfter, counted)
	return err
}
