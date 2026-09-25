package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

func (s *server) createDelivery(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	var input struct {
		Delivery string       `json:"delivery"`
		Actor    *model.Actor `json:"actor"`
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
		writeError(w, "MESSAGE_INPUT", http.StatusBadRequest, "delivery must be one of btw, aside, steer")
		return
	}
	message, err := s.loadMessage(r.Context(), s.deps.Store.Pool, "", r.PathValue("id"))
	if err != nil || message.Target == nil {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message not found")
		return
	}
	attempt, err := s.deliverMessage(r.Context(), message, input.Delivery, nil, actor, nil)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, attempt)
}

// pendingMessageDelivery is the attempt one targeted send owns: committed as pending under
// the message's lock, carrying the frame and the listener resolution it is sent with.
type pendingMessageDelivery struct {
	attemptClaim
	resolved ResolvedMention
	frame    []byte
}

// messageDeliveryRoute is what a targeted send resolves against the listener: the session a
// resumable pending attempt is already pointed at, or the message's own target when there is no
// such attempt or it was stranded before anything was resolved. A resumed attempt keeps its
// recipient - it is the attempt the listener has already deduplicated under its idempotency key,
// so re-pointing it at whoever holds the message's role now would deliver one attempt number to
// two sessions and leave the first unable to answer the frame it was sent. This runs with
// nothing held, before the resolution; recordPendingMessageDelivery re-reads the attempt under
// the message's lock and redoes the pair when the row it locked names a different recipient.
func (s *server) messageDeliveryRoute(ctx context.Context, message model.Message) (string, error) {
	var pinned string
	var lapsed bool
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select session_id, `+claimLapsed+`
		from message_deliveries
		where message_id = $1 and state = 'pending'
		order by attempt
		limit 1
	`, message.ID).Scan(&pinned, &lapsed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err != nil || !lapsed || pinned == "" {
		return messageTarget(message.Target), nil
	}
	return "session:" + pinned, nil
}

// recordPendingMessageDelivery is the claim transaction of a targeted delivery: it locks the
// message, settles which attempt this send owns and commits it as pending, all before the
// listener send, so the send holds no pooled connection. The locked rows re-verify the
// recipient the resolution was taken for: the attempt's own for a resumed attempt, the
// message's target for a new one.
func (s *server) recordPendingMessageDelivery(
	ctx context.Context,
	message model.Message,
	resolved ResolvedMention,
	replyThread *messageReplyThread,
) (pendingMessageDelivery, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return pendingMessageDelivery{}, err
	}
	defer tx.Rollback(ctx)
	// The issue is locked before the message row and the attempt rows, the order every other
	// message transaction takes them in, so the two halves of a delivery cannot deadlock
	// against each other.
	if message.IssueKey != nil {
		if _, err := s.requireOpenIssue(ctx, tx, *message.IssueKey); err != nil {
			return pendingMessageDelivery{}, err
		}
	}
	var target string
	if err := tx.QueryRow(ctx, `select target from messages where id = $1 for update`, message.ID).Scan(&target); err != nil {
		return pendingMessageDelivery{}, err
	}
	pending := pendingMessageDelivery{resolved: resolved}
	var pinned string
	var lapsed bool
	err = tx.QueryRow(ctx, `
		select attempt, session_id, `+claimLapsed+`
		from message_deliveries
		where message_id = $1 and state = 'pending'
		order by attempt
		limit 1
		for update
	`, message.ID).Scan(&pending.attempt, &pinned, &lapsed)
	switch {
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return pendingMessageDelivery{}, err
	case err == nil && lapsed:
		// Nobody holds this attempt, or whoever claimed it never came back, so this send takes
		// it under its original number - and so its original idempotency key, which the
		// listener deduplicates against a send that did land, rather than delivering the
		// message a second time. It keeps the recipient its row names; only an attempt
		// stranded before anything was resolved, whose row names none, goes to the message's
		// target. Either way the resolution has to have been taken for that recipient.
		recipient := target
		if pinned != "" {
			recipient = "session:" + pinned
		}
		if recipient != resolved.Target {
			return pendingMessageDelivery{}, errStaleResolution
		}
		// An attempt that already names a session keeps it; one stranded before anything was
		// resolved takes the recipient this resolution found, so the row names the session its
		// frame is going to either way and that session can answer it.
		if err := tx.QueryRow(ctx, `
			update message_deliveries
			set delivery = $3, session_id = coalesce(nullif(session_id, ''), $4), claimed_at = now()
			where message_id = $1 and attempt = $2
			returning claimed_at, session_id
		`, message.ID, pending.attempt, resolved.Delivery, resolved.attemptSessionID,
		).Scan(&pending.claimedAt, &pending.resolved.attemptSessionID); err != nil {
			return pendingMessageDelivery{}, err
		}
	default:
		// No attempt is pending, or a live sender holds the one that is, so this send opens an
		// attempt of its own rather than two senders driving the same one. The message row is
		// locked, so the highest attempt cannot move between reading it and inserting beside it.
		if target != resolved.Target {
			return pendingMessageDelivery{}, errStaleResolution
		}
		if err := tx.QueryRow(ctx, `
			insert into message_deliveries (message_id, attempt, delivery, session_id, state, claimed_at)
			values (
				$1,
				(select coalesce(max(attempt), 0) + 1 from message_deliveries where message_id = $1),
				$2, $3, 'pending', now()
			)
			returning attempt, claimed_at
		`, message.ID, resolved.Delivery, resolved.attemptSessionID,
		).Scan(&pending.attempt, &pending.claimedAt); err != nil {
			return pendingMessageDelivery{}, err
		}
	}
	thread := replyThread
	if thread == nil {
		derived, err := loadMessageReplyThread(ctx, tx, message)
		if err != nil {
			return pendingMessageDelivery{}, err
		}
		thread = &derived
	}
	pending.frame, err = json.Marshal(struct {
		Event    model.Event `json:"event"`
		Delivery any         `json:"delivery"`
	}{
		// A delivery frame replays the message to one session; the reference changes belong to
		// the appended event, which the stream already carried.
		Event: messageEvent(
			message, "message.created", message.Author, thread.payload(message, model.ReferenceChanges{}),
		),
		Delivery: map[string]any{"attempt": pending.attempt, "mode": resolved.Delivery},
	})
	if err != nil {
		return pendingMessageDelivery{}, fmt.Errorf("encode target delivery frame: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return pendingMessageDelivery{}, err
	}
	return pending, nil
}

// messageDeliveryReceipt is the message.delivery receipt an attempt row owes, read off that
// row: which attempt of which message went where, how it ended, and under which mode. Two
// fields the row does not carry come from either side of it - the target is the message's own,
// the conversation every attempt of it belongs to, and title is what the listener resolution
// called the session, empty where the session itself appends the receipt. Every producer of a
// message.delivery event builds it here.
func messageDeliveryReceipt(
	message model.Message, actor model.Actor, attempt model.MessageDelivery, title string,
) model.Event {
	return messageEvent(message, "message.delivery", actor,
		model.MessageDeliveryEventPayload{
			MessageID: attempt.MessageID, Attempt: attempt.Attempt, Delivery: attempt.Delivery,
			SessionID: attempt.SessionID, Target: messageTarget(message.Target), Title: title,
			State: attempt.State, Error: receiptError(attempt.Error),
		})
}

// completeMessageDelivery is the settle transaction of a targeted delivery: it records what the
// send did and appends the message.delivery receipt, holding no listener call.
func (s *server) completeMessageDelivery(
	ctx context.Context,
	message model.Message,
	actor model.Actor,
	pending pendingMessageDelivery,
	envelopeID *string,
	deliveryError string,
) (model.MessageDelivery, error) {
	state, failure := deliveryOutcome(deliveryError)
	// The receipt this send owes, read off the row it is about to write.
	receipt := messageDeliveryReceipt(message, actor, model.MessageDelivery{
		MessageID: message.ID, Attempt: pending.attempt, Delivery: pending.resolved.Delivery,
		SessionID: pending.resolved.attemptSessionID, State: state, Error: failure,
	}, pending.resolved.title)
	return settleDeliveryAttempt(ctx, s, receipt,
		func(ctx context.Context, tx pgx.Tx) (model.MessageDelivery, error) {
			return scanMessageDelivery(tx.QueryRow(ctx, `
				update message_deliveries set envelope_id = $4, state = $5, error = $6
				where message_id = $1 and attempt = $2 and state = 'pending' and claimed_at = $3
				returning `+messageDeliveryColumns,
				message.ID, pending.attempt, pending.claimedAt, envelopeID, state, failure,
			))
		},
		func(ctx context.Context, tx pgx.Tx) (model.MessageDelivery, bool, *string, error) {
			var mine bool
			answered, err := scanMessageDelivery(tx.QueryRow(ctx, `
				select `+messageDeliveryColumns+`, claimed_at is not distinct from $3
				from message_deliveries where message_id = $1 and attempt = $2
			`, message.ID, pending.attempt, pending.claimedAt), &mine)
			return answered, mine, answered.ReplyID, err
		},
		func(answered model.MessageDelivery) model.Event {
			return messageDeliveryReceipt(message, actor, answered, pending.resolved.title)
		},
	)
}

// deliverMessage records one delivery attempt of a targeted message and sends it to the
// resolved session: the listener resolution first with nothing held, the attempt committed as
// pending, the send, then the outcome. replyThread is what the delivery frame says about the
// thread the message joins; a caller that has not derived it passes nil and it is read from
// the stored parent.
func (s *server) deliverMessage(
	ctx context.Context,
	message model.Message,
	delivery string,
	urgency *string,
	actor model.Actor,
	replyThread *messageReplyThread,
) (model.MessageDelivery, error) {
	pending, err := resolveThenLock(ctx,
		func(ctx context.Context) (ResolvedMention, error) {
			route, err := s.messageDeliveryRoute(ctx, message)
			if err != nil {
				return ResolvedMention{}, err
			}
			return s.resolveMentionTargets(ctx, []string{route}, delivery)[0], nil
		},
		func(ctx context.Context, resolved ResolvedMention) (pendingMessageDelivery, error) {
			return s.recordPendingMessageDelivery(ctx, message, resolved, replyThread)
		},
		errorf(http.StatusConflict, "MESSAGE_TARGET_CHANGED",
			"the message's target changed while this delivery was being recorded"),
	)
	if err != nil {
		return model.MessageDelivery{}, err
	}
	envelopeID, deliveryError := s.sendResolvedDelivery(
		ctx, pending.resolved, message.Body, message.ID+":"+fmt.Sprint(pending.attempt), urgency, pending.frame,
	)
	return s.completeMessageDelivery(ctx, message, actor, pending, envelopeID, deliveryError)
}

func (s *server) replyMessage(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "reply requires an agent bearer token")
		return
	}
	var input struct {
		Actor   *model.Actor `json:"actor"`
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
		writeError(w, "MESSAGE_INPUT", http.StatusBadRequest, "message body is required")
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	message, err := scanMessage(tx.QueryRow(r.Context(), `
		select `+messageColumns+`
		from messages where id = $1
	`, r.PathValue("id")))
	if err != nil {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message not found")
		return
	}
	if message.IssueKey != nil {
		if _, err := s.requireOpenIssue(r.Context(), tx, *message.IssueKey); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	attempt, err := scanMessageDelivery(tx.QueryRow(r.Context(), `
		select `+messageDeliveryColumns+`
		from message_deliveries where message_id = $1 and attempt = $2 for update
	`, message.ID, input.Attempt))
	if err != nil {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message delivery not found")
		return
	}
	if attempt.SessionID != actor.ID {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "session may reply only to its own delivery")
		return
	}
	if attempt.ReplyID != nil {
		reply, err := s.loadMessage(r.Context(), tx, messageIssueKey(message), *attempt.ReplyID)
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
		updated, err := scanMessageDelivery(tx.QueryRow(r.Context(), `
			update message_deliveries set state = 'failed', error = $3 where message_id = $1 and attempt = $2
			returning `+messageDeliveryColumns,
			message.ID, input.Attempt, *input.Error,
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		attempt = updated
		event, err := s.appendEvent(r.Context(), tx, messageDeliveryReceipt(message, actor, attempt, ""))
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
	reply, err := scanMessage(tx.QueryRow(r.Context(), `
		insert into messages (issue_key, author, body, target, in_reply_to)
		values ($1, $2, $3, $4, $5)
		returning `+messageColumns+`
	`, message.IssueKey, author, *input.Body, message.Target, message.ID))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var referenceChanges model.ReferenceChanges
	if reply.IssueKey != nil {
		referenceChanges, err = s.replaceReferences(r.Context(), tx, "message", reply.ID, reply.Body)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The session answered, so the message reached it whatever the attempt recorded: a failed
	// attempt (a stale receipt, an error the session itself reported) reads as sent and answered.
	if _, err := tx.Exec(r.Context(), `
		update message_deliveries set reply_id = $3, state = 'sent', error = null
		where message_id = $1 and attempt = $2
	`, message.ID, input.Attempt, reply.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	thread, err := messageReplyParentOf(message).thread(r.Context(), tx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, messageEvent(reply, "message.answered", actor, thread.payload(reply, referenceChanges)))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if reply.IssueKey != nil {
		if err := refs.Stamp(r.Context(), tx, "message", reply.ID, event.ID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusCreated, reply)
}

// messageDeliveryColumns is the message_deliveries select list scanMessageDelivery reads, in
// scan order.
const messageDeliveryColumns = `message_id::text, attempt, delivery, session_id, envelope_id, state, error, reply_id::text, created_at`

// scanMessageDelivery decodes one messageDeliveryColumns row; extra receives any columns
// selected after them.
func scanMessageDelivery(row pgx.Row, extra ...any) (model.MessageDelivery, error) {
	var delivery model.MessageDelivery
	fields := []any{
		&delivery.MessageID, &delivery.Attempt, &delivery.Delivery, &delivery.SessionID, &delivery.EnvelopeID,
		&delivery.State, &delivery.Error, &delivery.ReplyID, &delivery.CreatedAt,
	}
	if err := row.Scan(append(fields, extra...)...); err != nil {
		return model.MessageDelivery{}, err
	}
	return delivery, nil
}

// loadMessageDeliveries reads every delivery attempt of the given messages in one query, keyed
// by message id and ordered by attempt within each; a message with no attempts maps to an empty
// (never nil) slice.
func (s *server) loadMessageDeliveries(ctx context.Context, q queryer, messageIDs []string) (map[string][]model.MessageDelivery, error) {
	deliveries := make(map[string][]model.MessageDelivery, len(messageIDs))
	for _, id := range messageIDs {
		deliveries[id] = []model.MessageDelivery{}
	}
	if len(messageIDs) == 0 {
		return deliveries, nil
	}
	rows, err := q.Query(ctx, `
		select `+messageDeliveryColumns+`
		from message_deliveries where message_id = any($1::uuid[]) order by message_id, attempt
	`, messageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		delivery, err := scanMessageDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries[delivery.MessageID] = append(deliveries[delivery.MessageID], delivery)
	}
	return deliveries, rows.Err()
}
