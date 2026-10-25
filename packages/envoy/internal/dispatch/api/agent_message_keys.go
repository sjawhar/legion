package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A single-session send (POST /api/v1/agents/{session_id}/messages) names itself with an
// idempotency key, as a broadcast does and under the same rule (idempotency_keys.go). The page
// sends it on every send and retries a send under the same key after a 5xx, a 429 or a request
// that timed out, none of which says whether the first one committed; the key is what makes that
// retry safe. Its agent_message_idempotency_keys row (migration 0116) is written in the message's
// own transaction (createStoredMessage), so a message and its key commit together or not at all.
// A send with no key - a tab opened before the deploy - is accepted and delivered as before the
// key existed, with no guarantee against a second send, and logged at WARN (createAgentMessage).
//
// A request whose key is held is answered from the database before anything else, so it never
// fails because the listener is down: the same request is answered 200 with the message the key
// made, and another request under the key is refused 409 MESSAGE_KEY_REUSED. Before a repeat is
// answered, the message is put on its way when nothing else will (sendHeldMessageOn).

// errAgentMessageKeyHeld is createStoredMessage's answer when the key row already exists: another
// request of this person's committed the same key first, its own transaction rolled back, and the
// caller answers with the other request's message.
var errAgentMessageKeyHeld = errors.New("message idempotency key already held")

// newAgentMessageKey is what a single-session send's key is judged by (newIdempotencyKey): what the
// send asked for is its session, body, mode and the message it answers, after validation.
func newAgentMessageKey(actor model.Actor, key, sessionID, body, delivery string, inReplyTo *string) idempotencyKey {
	return newIdempotencyKey(actor, key, struct {
		SessionID string  `json:"session_id"`
		Body      string  `json:"body"`
		Delivery  string  `json:"delivery"`
		InReplyTo *string `json:"in_reply_to"`
	}{sessionID, body, delivery, inReplyTo})
}

// insertAgentMessageKey is the send's claim on its key, written in the transaction that writes its
// message. A concurrent request carrying the same key waits here on this transaction's uncommitted
// row and, once this commits, is refused it (23505): that is errAgentMessageKeyHeld, and the
// refused transaction rolls back with its caller's deferred Rollback, which also releases its
// connection before the caller reads the winner's message - the pool refuses a second connection
// to a holder (store.ErrNestedAcquire).
func insertAgentMessageKey(ctx context.Context, tx pgx.Tx, key idempotencyKey, messageID string) error {
	if _, err := tx.Exec(ctx, `
		insert into agent_message_idempotency_keys (login, idempotency_key, message_id, request_digest)
		values ($1, $2, $3, $4)
	`, key.login, key.key, messageID, key.digest); err != nil {
		if isUniqueViolation(err) {
			return errAgentMessageKeyHeld
		}
		return err
	}
	return nil
}

// answerHeldAgentMessageKey answers a send whose key this person has already used, and reports
// whether it did: the message that key made, with its deliveries read as
// GET /api/v1/agents/{session_id}/messages reads them, when the request is the same one (200), once
// sendHeldMessageOn has put it on its way; 409 MESSAGE_KEY_REUSED, naming the message that used the
// key and saying this request was not sent, when the key was reused for a different session,
// message, mode or reply. A refusal writes nothing. It reports false, having written nothing, when
// the key is not held.
func (s *server) answerHeldAgentMessageKey(
	ctx context.Context, w http.ResponseWriter, key idempotencyKey, delivery string, actor model.Actor,
) bool {
	var messageID, digest string
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select message_id::text, request_digest from agent_message_idempotency_keys
		where login = $1 and idempotency_key = $2
	`, key.login, key.key).Scan(&messageID, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	if digest != key.digest {
		// The STATE_STALE shape (state.go): the code, the text, and the field a script acts on.
		WriteJSON(w, http.StatusConflict, map[string]any{
			"code":       "MESSAGE_KEY_REUSED",
			"error":      "idempotency_key was already used by message " + messageID + " for a different session, message, mode or reply; this request was not sent: a changed send needs a new idempotency_key",
			"message_id": messageID,
		})
		return true
	}
	message, err := s.loadMessage(ctx, s.deps.Store.Pool, "", messageID)
	if err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	if err := s.sendHeldMessageOn(ctx, message, delivery, actor); err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	deliveries, err := s.loadMessageDeliveries(ctx, s.deps.Store.Pool, []string{message.ID})
	if err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	message.Deliveries = deliveries[message.ID]
	WriteJSON(w, http.StatusOK, message)
	return true
}

// sendHeldMessageOn makes sure a held send's message is on its way before its repeat is answered,
// in the message's own mode (the repeat's, which its digest makes the original's). A message with
// no attempt at all - its process died between the commit and the attempt, or its own request is
// between them right now - gets its first attempt; one whose only attempt is pending with a lapsed
// claim has it resumed under its original number, as a same-mode retry from the agent card does.
// Either way the send is bound to that attempt (deliverMessageResuming), so of two requests that
// both find it owed, one sends and the other finds the attempt taken and sends nothing. A pending
// attempt whose claim is live is in flight and left to its sender; an attempt that settled, and a
// message with more than one attempt, which a person has already retried from the agent card, owe
// nothing. The send runs on past the request, as the original's does, and a listener it cannot
// reach is recorded on the attempt, never returned.
func (s *server) sendHeldMessageOn(ctx context.Context, message model.Message, delivery string, actor model.Actor) error {
	attempt, err := s.attemptOwed(ctx, message.ID)
	if err != nil || attempt == 0 {
		return err
	}
	sending := context.WithoutCancel(ctx)
	if _, err := s.deliverMessageResuming(sending, message, delivery, nil, actor, nil, attempt); err != nil &&
		!errors.Is(err, errAttemptNotPending) {
		return err
	}
	return nil
}

// attemptOwed is the attempt sendHeldMessageOn sends, or 0 for none: the first of a message with no
// attempt, or the only one when it is pending and its claim has lapsed, which Postgres judges
// against the claimed_at it wrote (claimLapsed).
func (s *server) attemptOwed(ctx context.Context, messageID string) (int, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select attempt, state = 'pending' and `+claimLapsed+`
		from message_deliveries where message_id = $1 order by attempt
	`, messageID)
	if err != nil {
		return 0, err
	}
	type attemptRow struct {
		Attempt  int
		Stranded bool
	}
	attempts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[attemptRow])
	if err != nil {
		return 0, err
	}
	switch {
	case len(attempts) == 0:
		return 1, nil
	case len(attempts) == 1 && attempts[0].Stranded:
		return attempts[0].Attempt, nil
	default:
		return 0, nil
	}
}
