package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// ResolvedMention binds a canonical mention to the session that owned it when the comment was
// created. SessionID is nil when no live holder was found; attemptSessionID remains internal so
// legacy message attempts retain their existing session-id representation.
type ResolvedMention struct {
	Target       string
	Delivery     string
	SessionID    *string
	ResolveError string

	title            string
	attemptSessionID string
}

// resolveMentionTargets performs the only registry lookup for a comment's original deliveries.
// The result is persisted in the comment-created event and handed to the post-commit sender, so a
// role transition cannot change the target after routing suppression has been decided.
func (s *server) resolveMentionTargets(ctx context.Context, targets []string, delivery string) []ResolvedMention {
	resolved := make([]ResolvedMention, 0, len(targets))
	for _, target := range targets {
		item := ResolvedMention{Target: target, Delivery: delivery}
		route, err := model.ParseRoute(target)
		if err != nil {
			item.ResolveError = err.Error()
			resolved = append(resolved, item)
			continue
		}
		sessionID, title, resolveError := s.resolveDeliveryTarget(ctx, route, delivery)
		item.title = title
		item.attemptSessionID = sessionID
		item.ResolveError = resolveError
		// A missing session is not a resolved target, even though the legacy message delivery
		// model retains its requested id in an attempt row. A live session that lacks a mode is
		// still resolved and participates in the event's session-level suppression calculation.
		if sessionID != "" && !strings.HasPrefix(resolveError, "no live session ") {
			item.SessionID = new(sessionID)
		}
		resolved = append(resolved, item)
	}
	return resolved
}

func (s *server) sendResolvedDelivery(ctx context.Context, target ResolvedMention, body, idempotencyKey string, urgency *string, frame []byte) (*string, string) {
	if target.ResolveError != "" {
		return nil, target.ResolveError
	}
	if target.SessionID == nil {
		return nil, "no live session"
	}
	expectsReply := "optional"
	if target.Delivery == "btw" {
		expectsReply = "required"
	}
	send, err := s.deps.Envoy.Send(ctx, dispatchenvoy.SendInput{
		TargetSession:  *target.SessionID,
		Message:        body,
		Payload:        frame,
		IdempotencyKey: idempotencyKey,
		Urgency:        urgencyValue(urgency),
		ExpectsReply:   expectsReply,
	})
	if err != nil {
		return nil, deliveryErrorText(err)
	}
	return &send.EnvelopeID, ""
}

// claimedCommentDelivery is the attempt one delivery call owns. settled is non-nil when that
// attempt already carries its outcome, or belongs to a sender that is still working, and
// nothing is to be sent.
type claimedCommentDelivery struct {
	comment model.Comment
	settled *model.CommentDelivery
	attemptClaim
	// pending is the attempt row an earlier call committed and this one resumed, under its
	// original number and idempotency key. Its pinned session, delivery mode and resolve error
	// are the resolution that attempt was opened with - both nil for an attempt stranded before
	// anything was resolved - and a resumed pin is only rechecked, never re-routed.
	pending *model.CommentDelivery
}

// openCommentDeliveryAttempt records a fresh attempt as pending and claimed, before anything
// is resolved or sent. The row is what makes the attempt durable: a process that dies before
// the send leaves it pending rather than losing the attempt entirely. The comment's mention is
// locked, so the highest attempt cannot move between reading it and inserting beside it.
func openCommentDeliveryAttempt(
	ctx context.Context, tx pgx.Tx, comment model.Comment, target ResolvedMention,
) (attemptClaim, error) {
	var claim attemptClaim
	if err := tx.QueryRow(ctx, `
		insert into comment_deliveries (comment_id, target, attempt, delivery, state, claimed_at)
		values (
			$1, $2,
			(select coalesce(max(attempt), 0) + 1 from comment_deliveries where comment_id = $1 and target = $2),
			$3, 'pending', now()
		)
		returning attempt, claimed_at
	`, comment.ID, target.Target, target.Delivery).Scan(&claim.attempt, &claim.claimedAt); err != nil {
		return attemptClaim{}, err
	}
	return claim, nil
}

// claimCommentDeliveryAttempt is the first of the two transactions a delivery runs: it locks
// the comment and its mention, settles which attempt this call owns, and commits - all before
// the listener is called at all, so no connection is held across the send.
func (s *server) claimCommentDeliveryAttempt(
	ctx context.Context, comment model.Comment, target ResolvedMention, initial bool,
) (claimedCommentDelivery, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return claimedCommentDelivery{}, err
	}
	defer tx.Rollback(ctx)
	stored, err := s.lockedComment(ctx, tx, comment.ID)
	if err != nil {
		return claimedCommentDelivery{}, err
	}
	if err := tx.QueryRow(ctx, `
		select target from comment_mentions where comment_id = $1 and target = $2 for update
	`, stored.ID, target.Target).Scan(new(string)); err != nil {
		return claimedCommentDelivery{}, err
	}
	claimed := claimedCommentDelivery{comment: stored}
	var lapsed bool
	pending, err := scanCommentDelivery(tx.QueryRow(ctx, `
		select `+commentDeliveryColumns+`, `+claimLapsed+`
		from comment_deliveries
		where comment_id = $1 and target = $2 and state = 'pending'
		order by attempt
		limit 1
		for update
	`, stored.ID, target.Target), &lapsed)
	switch {
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return claimedCommentDelivery{}, err
	case err == nil && lapsed:
		// Nobody holds this attempt, or whoever claimed it never came back, so this call takes
		// it under its original number and the listener deduplicates a send that did land.
		if err := tx.QueryRow(ctx, `
			update comment_deliveries set claimed_at = now()
			where comment_id = $1 and target = $2 and attempt = $3
			returning claimed_at
		`, stored.ID, target.Target, pending.Attempt).Scan(&claimed.claimedAt); err != nil {
			return claimedCommentDelivery{}, err
		}
		claimed.attempt = pending.Attempt
		claimed.pending = &pending
	case err == nil && initial:
		// A retry took the comment's original attempt in the moment between the creating
		// transaction's commit and this completion of it; that sender owns the outcome.
		claimed.settled = &pending
	case err == nil:
		// A live sender holds the pending attempt, so this retry gets one of its own rather
		// than two senders driving the same attempt.
		if claimed.attemptClaim, err = openCommentDeliveryAttempt(ctx, tx, stored, target); err != nil {
			return claimedCommentDelivery{}, err
		}
	case initial:
		// The original attempt is already answered: a reply consumed it while this completion
		// was on its way, and that terminal row is what the creating request reports.
		answered, err := loadCommentDelivery(ctx, tx, stored.ID, target.Target, 1)
		if err != nil {
			return claimedCommentDelivery{}, err
		}
		if answered.State != "sent" && answered.State != "failed" {
			return claimedCommentDelivery{}, fmt.Errorf(
				"initial comment delivery %s/%s is no longer pending", stored.ID, target.Target,
			)
		}
		claimed.settled = &answered
	default:
		if claimed.attemptClaim, err = openCommentDeliveryAttempt(ctx, tx, stored, target); err != nil {
			return claimedCommentDelivery{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return claimedCommentDelivery{}, err
	}
	return claimed, nil
}

// commentDeliveryReceipt is the comment.delivery receipt an attempt row owes, read off that
// row: what was delivered, to whom, how it ended, and the reply that ended it. Both the sender
// settling an attempt a reply already answered and replyComment's own error branch append
// exactly this.
func commentDeliveryReceipt(
	comment model.Comment, actor model.Actor, attempt model.CommentDelivery,
) model.Event {
	return ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.delivery",
		actor,
		model.CommentDeliveryEventPayload{
			CommentID: comment.ID, AskID: comment.AskID, Target: attempt.Target,
			Attempt: attempt.Attempt, Delivery: attempt.Delivery, SessionID: attempt.SessionID,
			State: attempt.State, Error: receiptError(attempt.Error), ReplyID: attempt.ReplyID,
		},
	)
}

// completeCommentDelivery is the settle transaction of a comment mention's delivery: it records
// what the send did on the attempt this sender claimed and appends the comment.delivery
// receipt, holding no listener call. recorded is the row recordCommentDeliveryResolution left
// before the send, so this receipt states what that row says - the session holding the frame
// included - rather than what the resolution behind the send happened to find.
func (s *server) completeCommentDelivery(
	ctx context.Context,
	comment model.Comment,
	recorded model.CommentDelivery,
	actor model.Actor,
	claim attemptClaim,
	envelopeID *string,
	deliveryError string,
) (model.CommentDelivery, error) {
	sent := recorded
	sent.State, sent.Error = deliveryOutcome(deliveryError)
	return settleDeliveryAttempt(ctx, s, commentDeliveryReceipt(comment, actor, sent),
		func(ctx context.Context, tx pgx.Tx) (model.CommentDelivery, error) {
			return scanCommentDelivery(tx.QueryRow(ctx, `
				update comment_deliveries set envelope_id = $5, state = $6, error = $7
				where comment_id = $1 and target = $2 and attempt = $3
				  and state = 'pending' and claimed_at = $4
				returning `+commentDeliveryColumns,
				comment.ID, sent.Target, claim.attempt, claim.claimedAt, envelopeID, sent.State, sent.Error,
			))
		},
		func(ctx context.Context, tx pgx.Tx) (model.CommentDelivery, bool, *string, error) {
			var mine bool
			answered, err := scanCommentDelivery(tx.QueryRow(ctx, `
				select `+commentDeliveryColumns+`, claimed_at is not distinct from $4
				from comment_deliveries
				where comment_id = $1 and target = $2 and attempt = $3
			`, comment.ID, sent.Target, claim.attempt, claim.claimedAt), &mine)
			return answered, mine, answered.ReplyID, err
		},
		func(answered model.CommentDelivery) model.Event {
			return commentDeliveryReceipt(comment, actor, answered)
		},
	)
}

// mentionResolutionFor is the resolution one mention delivery is sent with, and the resolve
// error its attempt row records.
//
// A resumed attempt carries the resolution it was opened with, in its pinned session or its
// resolve error, and keeps it: the routing decision - which session this attempt targets -
// stays fixed, exactly as resolveMentionTargets already guarantees against a role transition
// redirecting an in-flight delivery. An attempt stranded before either was recorded - the claim
// transaction commits the row before anything is resolved, so a sender that never returns
// leaves both NULL - has no resolution to keep: it is resolved now, under its own attempt
// number and idempotency key, rather than reported "no live session" without the listener ever
// being called.
//
// A pinned session is rechecked. The resolution behind the attempt was made before the
// comment-creation transaction committed (or, for a genuine retry, at an even earlier
// comment-creation moment) while the send always happens after that commit, whether this is the
// synchronous post-commit completion of a fresh comment or a much later retry, and a session's
// advertised capabilities and liveness are live state that changes on every registration. So
// both callers recheck, not just retries - only whether that pinned session can still receive
// this delivery mode right now, through the same resolveMentionTargets/resolveDeliveryTarget
// every other send uses, resolving it as a direct session target so no role lookup (and no
// chance of re-picking a different holder) is involved.
func (s *server) mentionResolutionFor(
	ctx context.Context, target ResolvedMention, pending *model.CommentDelivery,
) (ResolvedMention, *string) {
	if pending == nil || (pending.SessionID == nil && pending.ResolveError == nil) {
		resolved := s.resolveMentionTargets(ctx, []string{target.Target}, target.Delivery)[0]
		if resolved.ResolveError == "" {
			return resolved, nil
		}
		return resolved, &resolved.ResolveError
	}
	resumed := ResolvedMention{Target: target.Target, Delivery: pending.Delivery, SessionID: pending.SessionID}
	if pending.ResolveError != nil {
		resumed.ResolveError = *pending.ResolveError
	}
	if pending.SessionID != nil {
		recheck := s.resolveMentionTargets(ctx, []string{"session:" + *pending.SessionID}, pending.Delivery)[0]
		resumed.SessionID = recheck.SessionID
		resumed.ResolveError = recheck.ResolveError
		resumed.attemptSessionID = recheck.attemptSessionID
		resumed.title = recheck.title
	}
	return resumed, pending.ResolveError
}

// recordCommentDeliveryResolution writes onto the claimed attempt row, before the send, the
// resolution that send is made with, and returns the row that write left. A pending attempt
// names the session its frame is going to, so the mentioned session can answer that frame while
// it is still in flight and can still answer it after the sender dies without settling - the
// same rule the message path gets by resolving before it claims. The comment path cannot
// resolve before it claims, because a resumed attempt's pin is only known under the mention's
// lock, so it records the resolution in a statement of its own. That statement is scoped to
// this sender's claim, like both statements of the settle transaction: an attempt another
// sender has since resumed keeps what that sender recorded, and this sender reports the
// resolution it made without writing it anywhere.
//
// A session already pinned is kept, the coalesce recordPendingMessageDelivery makes on the
// message twin, spelled once here in SQL too. The recheck behind a resumed
// attempt asks only whether that same session can still receive this mode, so it can return
// that session or nothing at all: a listener that cannot answer - the outage this shape exists
// for - must not un-name the session holding the frame, or that session's reply is refused for
// the life of the comment.
func (s *server) recordCommentDeliveryResolution(
	ctx context.Context,
	comment model.Comment,
	target ResolvedMention,
	claim attemptClaim,
	resolveError *string,
) (model.CommentDelivery, error) {
	recorded, err := scanCommentDelivery(s.deps.Store.Pool.QueryRow(ctx, `
		update comment_deliveries
		set delivery = $5, session_id = coalesce(session_id, $6), resolve_error = $7
		where comment_id = $1 and target = $2 and attempt = $3
		  and state = 'pending' and claimed_at = $4
		returning `+commentDeliveryColumns,
		comment.ID, target.Target, claim.attempt, claim.claimedAt, target.Delivery, target.SessionID, resolveError,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.CommentDelivery{
			CommentID: comment.ID, Target: target.Target, Attempt: claim.attempt,
			Delivery: target.Delivery, SessionID: target.SessionID,
		}, nil
	}
	return recorded, err
}

// deliverResolvedCommentMention delivers one mention of a comment: it claims an attempt in one
// short transaction, resolves with nothing held, records that resolution on the claimed row,
// sends, then records the outcome in a second transaction. The listener never runs inside a
// transaction.
func (s *server) deliverResolvedCommentMention(
	ctx context.Context,
	comment model.Comment,
	created model.Event,
	target ResolvedMention,
	actor model.Actor,
	initial bool,
) (model.CommentDelivery, error) {
	claimed, err := s.claimCommentDeliveryAttempt(ctx, comment, target, initial)
	if err != nil {
		return model.CommentDelivery{}, err
	}
	if claimed.settled != nil {
		return *claimed.settled, nil
	}
	stored := claimed.comment
	target, resolveError := s.mentionResolutionFor(ctx, target, claimed.pending)
	recorded, err := s.recordCommentDeliveryResolution(ctx, stored, target, claimed.attemptClaim, resolveError)
	if err != nil {
		return model.CommentDelivery{}, err
	}
	frame, err := json.Marshal(struct {
		Event    model.Event `json:"event"`
		Delivery any         `json:"delivery"`
	}{
		Event: created,
		Delivery: map[string]any{
			"attempt":    claimed.attempt,
			"mode":       target.Delivery,
			"comment_id": stored.ID,
			"target":     target.Target,
		},
	})
	if err != nil {
		return model.CommentDelivery{}, fmt.Errorf("encode comment mention delivery frame: %w", err)
	}
	envelopeID, deliveryError := s.sendResolvedDelivery(
		ctx, target, stored.Body, stored.ID+":"+target.Target+":"+fmt.Sprint(claimed.attempt), nil, frame,
	)
	return s.completeCommentDelivery(
		ctx, stored, recorded, actor, claimed.attemptClaim, envelopeID, deliveryError,
	)
}

func (s *server) loadCommentCreatedEvent(ctx context.Context, q queryer, commentID string) (model.Event, error) {
	var event model.Event
	var actor, payload []byte
	if err := q.QueryRow(ctx, `
		select id, issue_key, artifact_id::text, coalesce(project_key, ''), seq, type, actor, notify, created_at, payload
		from events
		where type = 'comment.created' and payload ->> 'id' = $1
		order by id
		limit 1
	`, commentID).Scan(
		&event.ID, &event.IssueKey, &event.ArtifactID, &event.Project, &event.Seq, &event.Type,
		&actor, &event.Notify, &event.CreatedAt, &payload,
	); err != nil {
		return model.Event{}, err
	}
	if err := json.Unmarshal(actor, &event.Actor); err != nil {
		return model.Event{}, fmt.Errorf("decode comment-created event actor: %w", err)
	}
	if err := json.Unmarshal(payload, &event.Payload); err != nil {
		return model.Event{}, fmt.Errorf("decode comment-created event payload: %w", err)
	}
	return event, nil
}

// commentDeliveryColumns is the comment_deliveries select list scanCommentDelivery reads, in
// scan order.
const commentDeliveryColumns = `comment_id::text, target, attempt, delivery, session_id, envelope_id, state, error, resolve_error, reply_id::text, created_at`

// scanCommentDelivery decodes one commentDeliveryColumns row; extra receives any columns
// selected after them.
func scanCommentDelivery(row pgx.Row, extra ...any) (model.CommentDelivery, error) {
	var attempt model.CommentDelivery
	fields := []any{
		&attempt.CommentID, &attempt.Target, &attempt.Attempt, &attempt.Delivery, &attempt.SessionID,
		&attempt.EnvelopeID, &attempt.State, &attempt.Error, &attempt.ResolveError, &attempt.ReplyID,
		&attempt.CreatedAt,
	}
	if err := row.Scan(append(fields, extra...)...); err != nil {
		return model.CommentDelivery{}, err
	}
	return attempt, nil
}

func loadCommentDelivery(ctx context.Context, q queryer, commentID, target string, attemptNumber int) (model.CommentDelivery, error) {
	return scanCommentDelivery(q.QueryRow(ctx, `
		select `+commentDeliveryColumns+`
		from comment_deliveries
		where comment_id = $1 and target = $2 and attempt = $3
	`, commentID, target, attemptNumber))
}

func (s *server) createCommentDelivery(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	var input struct {
		Actor    *model.Actor `json:"actor"`
		Target   *string      `json:"target"`
		Delivery string       `json:"delivery"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !human {
		if actor, err = bearerSessionActor(actor, input.Actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if !validDelivery(input.Delivery) {
		writeError(w, "MENTION_INPUT", http.StatusBadRequest, "delivery must be one of btw, aside, steer")
		return
	}
	comment, err := s.loadComment(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment not found")
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select target from comment_mentions where comment_id = $1 order by target
	`, comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	targets := []string{}
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows.Close()
	if len(targets) == 0 {
		writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment has no mentions")
		return
	}
	target := ""
	if input.Target == nil {
		if len(targets) != 1 {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target is required when a comment has multiple mentions")
			return
		}
		target = targets[0]
	} else {
		target = *input.Target
		found := false
		for _, candidate := range targets {
			if candidate == target {
				found = true
				break
			}
		}
		if !found {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target must name one of the comment mentions")
			return
		}
	}
	created, err := s.loadCommentCreatedEvent(r.Context(), s.deps.Store.Pool, comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	resolved := ResolvedMention{Target: target, Delivery: input.Delivery}
	attempt, err := s.deliverResolvedCommentMention(r.Context(), comment, created, resolved, actor, false)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, attempt)
}

func (s *server) replyComment(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "reply requires an agent bearer token")
		return
	}
	var input struct {
		Actor   *model.Actor `json:"actor"`
		Target  *string      `json:"target"`
		Attempt int          `json:"attempt"`
		Body    *string      `json:"body"`
		Error   *string      `json:"error"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if actor.Kind != "session" || input.Attempt < 1 || (input.Body == nil) == (input.Error == nil) {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "reply requires one body or error for a delivery attempt")
		return
	}
	if input.Body != nil && strings.TrimSpace(*input.Body) == "" {
		writeError(w, "MENTION_INPUT", http.StatusBadRequest, "comment body is required")
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	comment, err := s.lockedComment(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment not found")
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	rows, err := tx.Query(r.Context(), `
		select target from comment_mentions where comment_id = $1 order by target for update
	`, comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	targets := []string{}
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			rows.Close()
			s.writeHandlerError(w, err)
			return
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.writeHandlerError(w, err)
		return
	}
	rows.Close()
	target := ""
	if input.Target == nil {
		if len(targets) != 1 {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target is required when a comment has multiple mentions")
			return
		}
		target = targets[0]
	} else {
		target = *input.Target
		found := false
		for _, candidate := range targets {
			if candidate == target {
				found = true
				break
			}
		}
		if !found {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target must name one of the comment mentions")
			return
		}
	}

	attempt, err := scanCommentDelivery(tx.QueryRow(r.Context(), `
		select `+commentDeliveryColumns+`
		from comment_deliveries where comment_id = $1 and target = $2 and attempt = $3 for update
	`, comment.ID, target, input.Attempt))
	if err != nil {
		writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment delivery not found")
		return
	}
	if attempt.SessionID == nil || *attempt.SessionID != actor.ID {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "session may reply only to its own delivery")
		return
	}
	if attempt.ReplyID != nil {
		reply, err := s.loadComment(r.Context(), tx, *attempt.ReplyID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, reply)
		return
	}
	if attempt.State == "failed" && input.Error != nil {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, attempt)
		return
	}
	if input.Error != nil {
		updated, err := scanCommentDelivery(tx.QueryRow(r.Context(), `
			update comment_deliveries
			set state = 'failed', error = $4
			where comment_id = $1 and target = $2 and attempt = $3
			returning `+commentDeliveryColumns,
			comment.ID, attempt.Target, input.Attempt, *input.Error,
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		attempt = updated
		event, err := s.appendEvent(r.Context(), tx, commentDeliveryReceipt(comment, actor, attempt))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		s.publish(event)
		WriteJSON(w, http.StatusOK, attempt)
		return
	}
	author, err := json.Marshal(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A callback reply joins its thread exactly as `POST .../comments` would: the shared climb
	// walks `reply_to` up to the head of the thread, so a reply anywhere under an ask stores
	// that ask's `ask_id` - the column every ask read, the Inbox row and the ask card select
	// on - instead of a bare `reply_to` none of them can see. The comment it answers is
	// already loaded and locked, so the climb starts there rather than re-reading it.
	thread, err := s.threadHeadOf(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID), comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A callback reply requests no turn, so it hands the turn back the way any session reply
	// with no `turn` does.
	turn := thread.replyTurn(actor, nil)
	reply, err := scanComment(tx.QueryRow(r.Context(), `
		insert into comments (issue_key, artifact_id, author, body, reply_to, ask_id, turn)
		values ($1, $2, $3, $4, $5, $6, $7)
		returning `+commentColumns+`
	`, comment.IssueKey, comment.ArtifactID, author, *input.Body, thread.ReplyTo, thread.AskID, turn))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if thread.AskID != nil {
		if err := asks.FollowAuthor(r.Context(), tx, *thread.AskID, actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	referenceChanges, err := s.replaceReferences(r.Context(), tx, "comment", reply.ID, reply.Body)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update comment_deliveries set reply_id = $4, state = 'sent', error = null
		where comment_id = $1 and target = $2 and attempt = $3
	`, comment.ID, attempt.Target, input.Attempt, reply.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifactName, err := s.commentArtifactName(r.Context(), tx, reply)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload, err := s.commentEventPayload(
		r.Context(), tx, reply, artifactName, thread.eventThread(turn), referenceChanges,
	)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerOf(reply.IssueKey, reply.ArtifactID).event(
		"comment.answered", actor, payload,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Stamp(r.Context(), tx, "comment", reply.ID, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusCreated, reply)
}
