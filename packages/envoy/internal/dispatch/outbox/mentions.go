package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

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
	// fallbackKeys is keyed like sessions, and holds for each session with a failed attempt - or
	// an abandoned one, which the outbox settles failed before it publishes the copy - the dedupe
	// key that attempt's send went under (contracts.AgentSendDedupeKey of
	// model.CommentMentionIdempotencyKey), which the session's copy carries (attempt.sessionCopy,
	// publishFallbackCopies). A send that landed before its attempt failed - a receipt timeout, or
	// a send handed the listener before a kill whose resend then failed - has already given the
	// host that key, so the host drops the copy; a session no mention reached shows it. A session
	// whose failed attempts span several targets keys by the lowest target, and a target with
	// several failed attempts by the latest one's mode. One key is enough: each target is a send
	// of its own, so a comment naming one session under two targets gives it one copy per target
	// that lands, and a copy shown beside another target's landed mention leaves the session no
	// more copies than had every one of its mentions landed.
	fallbackKeys map[string]string
	// targetFallbackKeys is keyed like targets, and holds for each target with a failed or
	// abandoned attempt that names a session the dedupe key of that target's latest such attempt
	// (of the lowest session, should its attempts name several), which a fallback copy keyed by
	// that target carries (publishFallbackCopies).
	targetFallbackKeys map[string]string
	// held holds each destination of the comment a fallback row owns (comment_fallback_copies):
	// the fallback pass publishes or settles it, and the event's own publish skips it
	// (publishDestination).
	held map[string]struct{}
}

// mentionState is what the attempts under one key of mentionDeliveries came to. The states are
// in the order a key whose attempts came to several counts as the first of them.
type mentionState int

const (
	// mentionSent: an attempt reached the session, so the mention delivered the comment.
	mentionSent mentionState = iota
	// mentionResending: an attempt the API's stranded pass resends (mentionResumed).
	mentionResending
	// mentionSending: an attempt whose send is live: pending, and claimed within
	// model.DeliveryClaimLapse, or unclaimed and created within it.
	mentionSending
	// mentionAbandoned: a pending attempt mentionSendLapsed takes as abandoned.
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

// mentionOutcomes is what the attempts under each key of mentionDeliveries came to.
type mentionOutcomes struct {
	// states holds the state of each key with an attempt; a key with none is mentionFailed.
	states map[string]mentionState
	// settle settles the abandoned attempts under key failed, and reports the state key's
	// attempts are in once that commits (settleAbandonedMentions).
	settle func(key string) (mentionState, error)
	// hold records the fallback row of the copy of the comment at destination that key's
	// attempts decide (holdFallbackCopy).
	hold func(key, destination string) error
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
// own topic, to the mention (attempt.leftTo). Callers ask only about a session their payload
// names as mentioned; a session no mention delivers to has no attempt, so it is never skipped.
func (a *attempt) leftToMention(sessionID string) bool {
	return a.leftTo(a.mentions.sessions, sessionID, contracts.AgentSubject(sessionID))
}

// leftTo reports whether the copy of the comment at destination, which key's attempts decide, is
// left to the mention. A destination a fallback row owns is the fallback pass's, so the copy is
// skipped. A mention that reached the session delivered the comment, so the copy is skipped. A
// mention still being sent may yet, so the copy is held back and the attempt marked deferred, to
// be decided again on the next scan. A mention the stranded pass resends is that resend's: the
// copy is skipped and held as a fallback row (outcomes.hold), which the fallback pass publishes
// should the resend fail (publishFallbackCopies); an insert that fails holds the copy back and
// fails the attempt, so the event is retried. A destination this event already reached gave the
// session a copy, so it needs no row. A mention whose send was abandoned is settled by the outbox
// first (outcomes.settle), and the copy follows what key's attempts are once that settle commits:
// one a sender claimed before the settle took its row is pending, so the copy is held back and
// the sender owns the outcome; one the settle took is failed, and the copy is the outbox's. Only
// one of the two acts on an attempt, so the session gets the mention or the copy, not both. A
// settle that fails holds the copy back and fails the attempt, so the event is retried. Any other
// key - every attempt failed, or none was recorded - is published to as if it had not been
// mentioned, except that a session copy for a failed attempt carries that attempt's dedupe key
// (attempt.sessionCopy): a send that landed before its attempt failed has given the host the
// comment already, and the host drops the copy.
func (a *attempt) leftTo(outcomes mentionOutcomes, key, destination string) bool {
	if _, ok := a.mentions.held[destination]; ok {
		return true
	}
	state := outcomes.state(key)
	if state == mentionAbandoned {
		settled, err := outcomes.settle(key)
		if err != nil {
			a.failed = append(a.failed, err)
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
		a.deferred = true
		return true
	case mentionResending:
		if _, ok := a.delivered[destination]; ok {
			return true
		}
		if err := outcomes.hold(key, destination); err != nil {
			a.failed = append(a.failed, err)
			return true
		}
		a.mentions.held[destination] = struct{}{}
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
// stranded pass (mentionResumed).
var mentionSendLapsed = `((claimed_at is null and created_at < now() - ` + model.DeliveryClaimLapseInterval +
	`) or created_at < now() - ` + model.StrandedMentionWindowInterval + `)`

// mentionResumed is true of a pending attempt a sender claimed and never settled, whose claim
// lapsed a lease ago, within model.StrandedMentionWindow: its sender died with the send in flight,
// perhaps after the listener took it, and the API's stranded pass resends it under its own
// idempotency key, which the listener drops as a repeat when the first send landed. The outbox
// cannot tell a send that landed before the kill from one that did not, so it publishes no copy
// for it and does not wait on it: it holds the copy as a fallback row, which the fallback pass
// publishes only once the resend failed (publishFallbackCopies).
var mentionResumed = `(claimed_at is not null and claimed_at < now() - ` + model.DeliveryClaimLapseInterval +
	` and created_at >= now() - ` + model.StrandedMentionWindowInterval + `)`

// mentionDeliveryQuery groups a comment's mention attempts by the session each counts under
// (mentionSessionKey) and by its target, and reports of each group whether an attempt was sent,
// is resent by the stranded pass (mentionResumed), is pending and live, and is pending and
// abandoned (mentionSendLapsed), and the mode of its latest attempt that failed or is abandoned,
// null when none did (mentionDeliveries.fallbackKeys).
var mentionDeliveryQuery = `
	select ` + mentionSessionKey + `,
	       target,
	       bool_or(state = 'sent'),
	       bool_or(state = 'pending' and ` + mentionResumed + `),
	       bool_or(state = 'pending' and not (` + mentionSendLapsed + `) and not (` + mentionResumed + `)),
	       bool_or(state = 'pending' and ` + mentionSendLapsed + `),
	       (array_agg(delivery order by attempt desc)
	           filter (where state = 'failed' or (state = 'pending' and ` + mentionSendLapsed + `)))[1]
	from comment_deliveries
	where comment_id = $1
	group by 1, 2
`

// mentionAbandonedError is the error the outbox records on a mention attempt it settles failed
// because its send was abandoned (settleAbandonedMentions).
const mentionAbandonedError = "the outbox published the comment's own copy instead; the send was abandoned"

// loadMentionDeliveries reads what a new comment's mentions delivered and which of its
// destinations fallback rows own (readMentionDeliveries). Only a comment.created that names a
// mentioned session (payloadMentionedSessions) or suppresses its route by a mention's target
// (payloadSuppressedRouteTarget) asks: every other event, edits, resolutions and reopenings
// included, suppresses nothing, and costs no query.
func loadMentionDeliveries(ctx context.Context, deps Deps, event model.Event) (mentionDeliveries, error) {
	if event.Type != "comment.created" ||
		(len(payloadMentionedSessions(event.Payload)) == 0 && payloadSuppressedRouteTarget(event.Payload) == "") {
		return mentionDeliveries{}, nil
	}
	return readMentionDeliveries(ctx, deps, event, payloadString(event.Payload, "id"))
}

// readMentionDeliveries reads what the mentions of commentID, which event created, delivered
// (mentionDeliveries) and the destinations its fallback rows own, in two queries. A key that
// reads abandoned costs a settle transaction of its own, only when a copy asks about it
// (attempt.leftTo) or the fallback pass decides that key's row; a key whose mention is resent
// costs the insert of a fallback row, only when a copy asks about it.
func readMentionDeliveries(ctx context.Context, deps Deps, event model.Event, commentID string) (mentionDeliveries, error) {
	failed := func(err error) (mentionDeliveries, error) {
		return mentionDeliveries{}, fmt.Errorf("load mention deliveries of comment %q: %w", commentID, err)
	}
	rows, err := deps.Store.Pool.Query(ctx, mentionDeliveryQuery, commentID)
	if err != nil {
		return failed(err)
	}
	defer rows.Close()
	outcomes := func(keyColumn, rowColumn string) mentionOutcomes {
		return mentionOutcomes{
			states: map[string]mentionState{},
			settle: func(key string) (mentionState, error) {
				return settleAbandonedMentions(ctx, deps, event, commentID, keyColumn, key)
			},
			hold: func(key, destination string) error {
				return holdFallbackCopy(ctx, deps, event.ID, commentID, rowColumn, key, destination)
			},
		}
	}
	result := mentionDeliveries{
		sessions: outcomes(mentionSessionKey, "session_id"), targets: outcomes("target", "target"),
		fallbackKeys: map[string]string{}, targetFallbackKeys: map[string]string{},
	}
	fallbackTargets, fallbackSessions := map[string]string{}, map[string]string{}
	for rows.Next() {
		var sessionID, failedDelivery *string
		var target string
		var sent, resending, sending, abandoned bool
		if err := rows.Scan(&sessionID, &target, &sent, &resending, &sending, &abandoned, &failedDelivery); err != nil {
			return failed(err)
		}
		state := mentionStateOf(sent, resending, sending, abandoned)
		if sessionID != nil {
			result.sessions.record(*sessionID, state)
			if failedDelivery != nil {
				key := contracts.AgentSendDedupeKey(*sessionID, model.CommentMentionIdempotencyKey(commentID, target, *failedDelivery))
				if chosen, ok := fallbackTargets[*sessionID]; !ok || target < chosen {
					fallbackTargets[*sessionID] = target
					result.fallbackKeys[*sessionID] = key
				}
				if chosen, ok := fallbackSessions[target]; !ok || *sessionID < chosen {
					fallbackSessions[target] = *sessionID
					result.targetFallbackKeys[target] = key
				}
			}
		}
		result.targets.record(target, state)
	}
	// Closed before the read below: a cursor still open holds one of the pool's connections, and
	// a caller asking for a second is refused (store.ErrNestedAcquire).
	rows.Close()
	if err := rows.Err(); err != nil {
		return failed(err)
	}
	heldRows, err := deps.Store.Pool.Query(ctx, `select destination from comment_fallback_copies where comment_id = $1`, commentID)
	if err != nil {
		return failed(err)
	}
	held, err := pgx.CollectRows(heldRows, pgx.RowTo[string])
	if err != nil {
		return failed(err)
	}
	result.held = make(map[string]struct{}, len(held))
	for _, destination := range held {
		result.held[destination] = struct{}{}
	}
	return result, nil
}

// holdFallbackCopy records the fallback row of the copy of commentID, which the event eventID
// created, at destination, decided by the attempts under key in keyColumn (session_id or target),
// unless the destination has one already (publishFallbackCopies). It commits before the attempt
// that holds the copy back can mark the event published.
func holdFallbackCopy(ctx context.Context, deps Deps, eventID int64, commentID, keyColumn, key, destination string) error {
	if _, err := deps.Store.Pool.Exec(ctx, `
		insert into comment_fallback_copies (comment_id, destination, event_id, `+keyColumn+`)
		values ($1, $2, $3, $4)
		on conflict (comment_id, destination) do nothing
	`, commentID, destination, eventID, key); err != nil {
		return fmt.Errorf("hold the fallback copy of comment %q at %s: %w", commentID, destination, err)
	}
	return nil
}

// settleAbandonedMentions settles failed, with mentionAbandonedError, every attempt of the comment
// that event created whose keyColumn (mentionSessionKey or target) is key and whose send is
// abandoned (mentionSendLapsed), appends each one's comment.delivery receipt as the API's sender
// would (the comment's author its actor), and reports the state key's attempts are in then: sent,
// resent by the stranded pass, being sent, or failed. It is one transaction, and it takes the
// comment's owner lock before any attempt row, the order the API's claim and settle transactions
// take them in (claimCommentDeliveryAttempt, settleDeliveryAttempt in internal/dispatch/api), so
// it and a sender's claim of the same attempt run one after the other. A claim that commits first
// leaves the row claimed just now, so the update's guard no longer matches it and the attempt
// reads pending; an attempt this settles first is no longer pending when the claim reads it, and
// the claim takes it as settled and sends nothing. The receipts reach the event log in the same
// commit, so a reader sees the attempt fail, and the outbox publishes them like any event.
func settleAbandonedMentions(
	ctx context.Context, deps Deps, event model.Event, commentID, keyColumn, key string,
) (state mentionState, err error) {
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
		where comment_id = $1 and `+keyColumn+` = $2 and state = 'pending' and `+mentionSendLapsed+`
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
		       coalesce(bool_or(state = 'pending' and `+mentionResumed+`), false),
		       coalesce(bool_or(state = 'pending'), false)
		from comment_deliveries
		where comment_id = $1 and `+keyColumn+` = $2
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
// at destination, the comment.created event eventID's envelope, decided by the attempts under key,
// a target when byTarget and a session otherwise.
type fallbackCopy struct {
	commentID   string
	destination string
	eventID     int64
	key         string
	byTarget    bool
}

// fallbackEvent is the comment.created event a fallback copy is the envelope of, and the slug of
// its artifact (envelope).
type fallbackEvent struct {
	event model.Event
	slug  string
}

// fallbackRecord is what a fallback pass came to for one row: settled, and published when it
// published the copy. A record of a row not settled releases the row's claim for a later pass.
type fallbackRecord struct {
	held      fallbackCopy
	settled   bool
	published bool
}

// publishFallbackCopies is the fallback pass. This is the one description of how a mentioned
// session still gets its copy of a new comment when its mention was in flight at a kill.
//
// A comment's mention is sent by the API after the comment commits (mentionSender in
// internal/dispatch/api), and every destination of the comment.created event that would reach a
// mentioned session - the route, the claimant, an author, a follower - leaves that session's copy
// to the mention by the rule attempt.leftTo applies. A mention attempt a sender claimed and never
// settled, whose claim lapsed within model.StrandedMentionWindow (mentionResumed), is resent by
// the API's stranded pass under the attempt's own idempotency key: its sender died with the send
// in flight, perhaps after the listener took it, and the listener drops the resend as a repeat
// when it did. The outbox cannot tell which, so it neither publishes that copy nor waits on the
// resend. It records the copy it held back as a comment_fallback_copies row instead, keyed by the
// comment and the exact destination topic the copy would have gone to and naming the key whose
// attempts decide it (the session, or the target of a route suppressed by that string alone),
// inserted idempotently before the event can be marked published; an insert that fails fails the
// attempt, so the event is retried. From then on the row owns that destination: the event's own
// publish skips it on every retry (mentionDeliveries.held, publishDestination), so the event never
// sends it too, and the event is published and recorded as it would be had the mention been sent.
//
// The pass runs at startup and on every retry tick of Run, beside the event scan. It claims a
// bounded batch of unsettled rows, oldest first (next_attempt_at pushed out by claimWindow under
// for update skip locked, so a second process skips them), and decides each by its key's
// attempts, read as leftTo reads them (readMentionDeliveries): a sent attempt delivered the
// comment, so the row is settled with nothing sent; an attempt still being sent or resent leaves
// the row, its claim released, to a later pass; an abandoned attempt is settled failed first
// (settleAbandonedMentions, which a sender's claim of it runs after or before under the owner
// lock) and the row decided again; and when no attempt was sent and none is pending, the
// comment.created event's envelope is published to the row's destination alone, carrying the
// dedupe key of the failed attempt's send (mentionDeliveries.fallbackKeys for a session,
// targetFallbackKeys for a target), and the row is marked published. The pass never writes the
// events row: the event's published_at, published_destinations and payload stay as its own
// publish left them, and no destination but the row's is reached, whatever the issue's route,
// claimant, followers or authors are by then. A decision or a publish that fails keeps the row's
// claim, so it is decided again once claimWindow lapses, except one cut short by the process
// stopping or by NATS being unreachable, which releases the claim for the next pass; a process
// that dies between the publish and the mark leaves the claim too, and the repeat carries the same
// dedupe key, which the host, and the stream within its duplicate window, drop.
//
// Races. A person's Retry of the mention opens or resumes an attempt the pass reads as being
// sent, so the row waits for it, and a Retry that is sent settles the row with nothing sent. A
// Retry that starts once the pass has decided to publish can deliver the mention and the copy
// both, under one dedupe key when it is sent in the failed attempt's mode, so the host shows one.
// A send the killed process had handed the listener before it died, whose resend then fails for
// good (the listener down after the restart), gives the host that mention and then the copy under
// the mention's own dedupe key, which the host drops. The API's failed settle appends a
// comment.delivery receipt, an event insert that wakes the event scan; the pass runs on the next
// retry tick, within retryInterval.
func publishFallbackCopies(ctx context.Context, deps Deps) {
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
	// releases what it did not decide, rather than leave those rows claimed for claimWindow.
	defer func() {
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		defer cancel()
		if err := recordFallbackCopies(record, deps, records); err != nil {
			slog.Error("dispatch outbox: record fallback copies", "error", err)
		}
	}()
	release := func(held fallbackCopy) {
		records = append(records, fallbackRecord{held: held})
	}
	events, err := loadFallbackEvents(ctx, deps, copies)
	if err != nil {
		slog.Error("dispatch outbox: read the events of fallback copies", "error", err)
		if ctx.Err() != nil {
			for _, held := range copies {
				release(held)
			}
		}
		return
	}
	type sentCopy struct {
		held   fallbackCopy
		answer <-chan error
	}
	sender := deps.Publisher.Window()
	var sent []sentCopy
	for _, held := range copies {
		if ctx.Err() != nil {
			release(held)
			continue
		}
		event, ok := events[held.eventID]
		if !ok {
			slog.Error("dispatch outbox: fallback copy names no event", "comment_id", held.commentID, "event_id", held.eventID)
			continue
		}
		decision, item, err := decideFallbackCopy(ctx, deps, held, event)
		switch {
		case err != nil:
			slog.Error("dispatch outbox: decide fallback copy", "comment_id", held.commentID, "destination", held.destination, "error", err)
			if ctx.Err() != nil {
				release(held)
			}
		case decision == fallbackPublishes:
			sent = append(sent, sentCopy{held: held, answer: sender.PublishAsync(item)})
		default:
			records = append(records, fallbackRecord{held: held, settled: decision == fallbackSettles})
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
			slog.Error("dispatch outbox: fallback copy refused", "comment_id", item.held.commentID, "destination", item.held.destination, "error", err)
		default:
			slog.Error("dispatch outbox: publish fallback copy", "comment_id", item.held.commentID, "destination", item.held.destination, "error", err)
			if errors.Is(err, bus.ErrUnavailable) || ctx.Err() != nil {
				release(item.held)
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
	deliveries, err := readMentionDeliveries(ctx, deps, event.event, held.commentID)
	if err != nil {
		return 0, contracts.Envelope{}, err
	}
	outcomes, keys := deliveries.sessions, deliveries.fallbackKeys
	if held.byTarget {
		outcomes, keys = deliveries.targets, deliveries.targetFallbackKeys
	}
	state := outcomes.state(held.key)
	if state == mentionAbandoned {
		if state, err = outcomes.settle(held.key); err != nil {
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

// claimFallbackCopies claims at most batchSize unsettled fallback rows whose claim is not held,
// oldest first, in one statement: for update skip locked, so a second process's pass skips them,
// and next_attempt_at pushed out by claimWindow in the same breath. It reads the partial index of
// unsettled rows, so a pass with nothing to decide reads no row.
func claimFallbackCopies(ctx context.Context, deps Deps) ([]fallbackCopy, error) {
	rows, err := deps.Store.Pool.Query(ctx, `
		update comment_fallback_copies set next_attempt_at = now() + $2
		where (comment_id, destination) in (
			select comment_id, destination from comment_fallback_copies
			where settled_at is null and (next_attempt_at is null or next_attempt_at <= now())
			order by created_at
			limit $1
			for update skip locked
		)
		returning comment_id::text, destination, event_id, session_id, target
	`, batchSize, claimWindow)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (fallbackCopy, error) {
		var held fallbackCopy
		var sessionID, target *string
		if err := row.Scan(&held.commentID, &held.destination, &held.eventID, &sessionID, &target); err != nil {
			return fallbackCopy{}, err
		}
		if target != nil {
			held.key, held.byTarget = *target, true
		} else if sessionID != nil {
			held.key = *sessionID
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
// marked so, published when it published the copy, and each row left for a later pass has its
// claim released. A row a pass did not decide, or whose publish failed, is not written and keeps
// its claim until claimWindow lapses.
func recordFallbackCopies(ctx context.Context, deps Deps, records []fallbackRecord) error {
	if len(records) == 0 {
		return nil
	}
	commentIDs := make([]string, 0, len(records))
	destinations := make([]string, 0, len(records))
	settled := make([]bool, 0, len(records))
	published := make([]bool, 0, len(records))
	for _, record := range records {
		commentIDs = append(commentIDs, record.held.commentID)
		destinations = append(destinations, record.held.destination)
		settled = append(settled, record.settled)
		published = append(published, record.published)
	}
	_, err := deps.Store.Pool.Exec(ctx, `
		update comment_fallback_copies c set
			settled_at = case when o.settled then now() end,
			published = o.published,
			next_attempt_at = null
		from unnest($1::uuid[], $2::text[], $3::bool[], $4::bool[]) as o(comment_id, destination, settled, published)
		where c.comment_id = o.comment_id and c.destination = o.destination and c.settled_at is null
	`, commentIDs, destinations, settled, published)
	return err
}
