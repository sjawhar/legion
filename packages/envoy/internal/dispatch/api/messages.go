package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

const maxMessageBody16 = 2000

const maxMessageReplyPreview16 = 160

type createMessageInput struct {
	Body      string       `json:"body"`
	InReplyTo *string      `json:"in_reply_to"`
	Target    *string      `json:"target"`
	Delivery  *string      `json:"delivery"`
	Urgency   *string      `json:"urgency"`
	Actor     *model.Actor `json:"actor"`
}

func (s *server) createMessage(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	var input createMessageInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !human {
		if input.Target != nil || input.Delivery != nil || input.Urgency != nil {
			writeError(w, "ACTOR_KIND", http.StatusBadRequest, "only users may target message delivery")
			return
		}
		if input.Actor == nil || input.Actor.Kind != "session" || strings.TrimSpace(input.Actor.ID) == "" {
			writeError(w, "ACTOR_KIND", http.StatusBadRequest, "bearer callers require actor.kind session")
			return
		}
		actor = *input.Actor
	}
	if strings.TrimSpace(input.Body) == "" {
		writeError(w, "INVALID_MESSAGE", http.StatusBadRequest, "message body is required")
		return
	}
	if length := len16(input.Body); length > maxMessageBody16 {
		capExceeded(w, "body", length, maxMessageBody16)
		return
	}
	target, delivery, err := validateMessageDelivery(input.Target, input.Delivery)
	if err != nil {
		writeError(w, "MESSAGE_INPUT", http.StatusBadRequest, err.Error())
		return
	}
	if input.Urgency != nil && !validMessageUrgency(*input.Urgency) {
		writeError(w, "MESSAGE_INPUT", http.StatusBadRequest, "urgency must be one of low, med, high, blocking")
		return
	}
	if !human && (target != nil || delivery != "") {
		writeError(w, "ACTOR_KIND", http.StatusBadRequest, "only users may target message delivery")
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	issueKey := r.PathValue("key")
	if err := s.requireOpenIssue(r.Context(), tx, issueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	replyBody, err := messageReplyBody(r.Context(), tx, issueKey, input.InReplyTo)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	author, err := json.Marshal(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var message model.Message
	var authorRaw []byte
	if err := tx.QueryRow(r.Context(), `
		insert into messages (issue_key, author, body, target, in_reply_to)
		values ($1, $2, $3, $4, $5)
		returning id::text, issue_key, author, body, target, in_reply_to::text, created_at
	`, issueKey, author, input.Body, target, input.InReplyTo).Scan(
		&message.ID, &message.IssueKey, &authorRaw, &message.Body, &message.Target, &message.InReplyTo, &message.CreatedAt,
	); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := json.Unmarshal(authorRaw, &message.Author); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	message.Deliveries = []model.MessageDelivery{}
	if err := refs.Replace(r.Context(), tx, "message", message.ID, message.Body, s.deps.ServerURL); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	eventType := "message.created"
	if message.InReplyTo != nil {
		eventType = "message.answered"
	}
	event, err := s.appendEvent(r.Context(), tx, issueOwner(issueKey).event(
		eventType,
		actor,
		model.MessageEventPayload{Message: message, ReplyBody: replyBody},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)

	if target != nil {
		attempt, err := s.deliverMessage(r.Context(), message, delivery, input.Urgency, actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		message.Deliveries = []model.MessageDelivery{attempt}
	}
	writeJSON(w, http.StatusCreated, message)
}

func (s *server) messageActor(
	w http.ResponseWriter,
	r *http.Request,
	supplied *model.Actor,
	hasDeliveryInput bool,
) (model.Actor, bool, bool) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return model.Actor{}, false, false
	}
	if human {
		return actor, true, true
	}
	if hasDeliveryInput {
		writeError(w, "ACTOR_KIND", http.StatusBadRequest, "only users may target message delivery")
		return model.Actor{}, false, false
	}
	if supplied == nil || supplied.Kind != "session" || strings.TrimSpace(supplied.ID) == "" {
		writeError(w, "ACTOR_KIND", http.StatusBadRequest, "bearer callers require actor.kind session")
		return model.Actor{}, false, false
	}
	return *supplied, false, true
}

func validMessageUrgency(value string) bool {
	return value == "low" || value == "med" || value == "high" || value == "blocking"
}

func validDelivery(value string) bool {
	return value == "btw" || value == "aside" || value == "steer"
}

func validateMessageDelivery(target *string, delivery *string) (*string, string, error) {
	if target == nil && delivery == nil {
		return nil, "", nil
	}
	if target == nil || strings.TrimSpace(*target) == "" {
		return nil, "", errors.New("delivery requires target")
	}
	if delivery == nil || !validDelivery(*delivery) {
		return nil, "", errors.New("delivery must be one of btw, aside, steer")
	}
	parsed, err := model.ParseRoute(*target)
	if err != nil || (parsed.Kind != "role" && parsed.Kind != "session") {
		return nil, "", errors.New("target must be role:<name> or session:<id>")
	}
	canonical := parsed.Kind + ":" + parsed.ID
	return &canonical, *delivery, nil
}

func messageReplyBody(ctx context.Context, tx pgx.Tx, issueKey string, inReplyTo *string) (string, error) {
	if inReplyTo == nil {
		return "", nil
	}
	if strings.TrimSpace(*inReplyTo) == "" {
		return "", errorf(http.StatusBadRequest, "MESSAGE_INPUT", "in_reply_to must identify a message on this issue")
	}
	if _, err := uuid.Parse(*inReplyTo); err != nil {
		return "", errorf(http.StatusBadRequest, "MESSAGE_INPUT", "in_reply_to must identify a message on this issue")
	}
	var parentBody string
	if err := tx.QueryRow(ctx, `select body from messages where id = $1 and issue_key = $2`, *inReplyTo, issueKey).Scan(&parentBody); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errorf(http.StatusBadRequest, "MESSAGE_INPUT", "in_reply_to must identify a message on this issue")
		}
		return "", err
	}
	return truncateRunes(parentBody, maxMessageReplyPreview16), nil
}

func (s *server) createDelivery(w http.ResponseWriter, r *http.Request) {
	if _, human, err := s.optionalActor(r); err != nil {
		s.writeAuthenticationError(w, err)
		return
	} else if !human {
		writeError(w, "ACTOR_KIND", http.StatusBadRequest, "only users may target message delivery")
		return
	}
	var input struct {
		Delivery string `json:"delivery"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
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
	actor, _, _ := s.optionalActor(r)
	attempt, err := s.deliverMessage(r.Context(), message, input.Delivery, nil, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, attempt)
}

func (s *server) deliverMessage(
	ctx context.Context,
	message model.Message,
	delivery string,
	urgency *string,
	actor model.Actor,
) (model.MessageDelivery, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.MessageDelivery{}, err
	}
	defer tx.Rollback(ctx)
	var target string
	if err := tx.QueryRow(ctx, `select target from messages where id = $1 for update`, message.ID).Scan(&target); err != nil {
		return model.MessageDelivery{}, err
	}
	if err := s.requireOpenIssue(ctx, tx, message.IssueKey); err != nil {
		return model.MessageDelivery{}, err
	}
	route, err := model.ParseRoute(target)
	if err != nil {
		return model.MessageDelivery{}, err
	}
	var attemptNumber int
	if err := tx.QueryRow(ctx, `select coalesce(max(attempt), 0) + 1 from message_deliveries where message_id = $1`, message.ID).Scan(&attemptNumber); err != nil {
		return model.MessageDelivery{}, err
	}

	targetSession, title, deliveryError := s.resolveDeliveryTarget(ctx, route, delivery)
	var envelopeID *string
	state := "failed"
	if deliveryError == "" {
		frame, err := json.Marshal(struct {
			Event    model.Event `json:"event"`
			Delivery any         `json:"delivery"`
		}{
			Event:    issueOwner(message.IssueKey).event("message.created", message.Author, model.MessageEventPayload{Message: message}),
			Delivery: map[string]any{"attempt": attemptNumber, "mode": delivery},
		})
		if err != nil {
			return model.MessageDelivery{}, fmt.Errorf("encode target delivery frame: %w", err)
		}
		expectsReply := "optional"
		if delivery == "btw" {
			expectsReply = "required"
		}
		send, err := s.deps.Envoy.Send(ctx, dispatchenvoy.SendInput{
			TargetSession:  targetSession,
			Message:        message.Body,
			Payload:        frame,
			IdempotencyKey: message.ID + ":" + fmt.Sprint(attemptNumber),
			Urgency:        urgencyValue(urgency),
			ExpectsReply:   expectsReply,
		})
		if err != nil {
			deliveryError = deliveryErrorText(err)
		} else {
			state = "sent"
			envelopeID = &send.EnvelopeID
		}
	}
	attempt := model.MessageDelivery{
		MessageID: message.ID, Attempt: attemptNumber, Delivery: delivery, SessionID: targetSession,
		EnvelopeID: envelopeID, State: state, CreatedAt: message.CreatedAt,
	}
	if deliveryError != "" {
		attempt.Error = &deliveryError
	}
	if err := tx.QueryRow(ctx, `
		insert into message_deliveries (message_id, attempt, delivery, session_id, envelope_id, state, error)
		values ($1, $2, $3, $4, $5, $6, $7)
		returning created_at
	`, message.ID, attemptNumber, delivery, targetSession, envelopeID, state, attempt.Error).Scan(&attempt.CreatedAt); err != nil {
		return model.MessageDelivery{}, err
	}
	event, err := s.appendEvent(ctx, tx, issueOwner(message.IssueKey).event("message.delivery", actor,
		model.MessageDeliveryEventPayload{
			MessageID: message.ID, Attempt: attemptNumber, Delivery: delivery, SessionID: targetSession,
			Title: title, State: state, Error: deliveryError,
		}))
	if err != nil {
		return model.MessageDelivery{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.MessageDelivery{}, err
	}
	s.publish(event)
	return attempt, nil
}

func (s *server) resolveDeliveryTarget(ctx context.Context, route model.Route, delivery string) (string, string, string) {
	if s.deps.Envoy == nil {
		return "", "", "envoy listener: ENVOY_URL is not configured"
	}
	var target dispatchenvoy.Session
	var err error
	if route.Kind == "role" {
		target, err = s.deps.Envoy.Role(ctx, route.ID)
	} else {
		var sessions []dispatchenvoy.Session
		sessions, err = s.deps.Envoy.Sessions(ctx)
		if err == nil {
			for _, candidate := range sessions {
				if candidate.SessionID == route.ID {
					target = candidate
					break
				}
			}
			if target.SessionID == "" {
				return route.ID, "", "no live session " + route.ID
			}
		}
	}
	if err != nil {
		return "", "", deliveryErrorText(err)
	}
	if delivery != "steer" && !hasCapability(target.Capabilities, delivery) {
		return target.SessionID, target.Title,
			fmt.Sprintf("session %s (%s) does not advertise %s", target.SessionID, target.Title, delivery)
	}
	return target.SessionID, target.Title, ""
}

func hasCapability(capabilities []string, value string) bool {
	for _, capability := range capabilities {
		if capability == value {
			return true
		}
	}
	return false
}

func urgencyValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func deliveryErrorText(err error) string {
	if errors.Is(err, dispatchenvoy.ErrUnavailable) {
		return "envoy listener: " + strings.TrimPrefix(err.Error(), "envoy listener unavailable: ")
	}
	return err.Error()
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
		select id::text, issue_key, author, body, target, in_reply_to::text, created_at
		from messages where id = $1
	`, r.PathValue("id")))
	if err != nil {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message not found")
		return
	}
	if err := s.requireOpenIssue(r.Context(), tx, message.IssueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var attempt model.MessageDelivery
	if err := tx.QueryRow(r.Context(), `
		select message_id::text, attempt, delivery, session_id, envelope_id, state, error, reply_id::text, created_at
		from message_deliveries where message_id = $1 and attempt = $2 for update
	`, message.ID, input.Attempt).Scan(
		&attempt.MessageID, &attempt.Attempt, &attempt.Delivery, &attempt.SessionID, &attempt.EnvelopeID,
		&attempt.State, &attempt.Error, &attempt.ReplyID, &attempt.CreatedAt,
	); err != nil {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message delivery not found")
		return
	}
	if attempt.SessionID != actor.ID {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "session may reply only to its own delivery")
		return
	}
	if attempt.ReplyID != nil {
		reply, err := s.loadMessage(r.Context(), tx, message.IssueKey, *attempt.ReplyID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, reply)
		return
	}
	if attempt.State == "failed" && attempt.Error != nil {
		if input.Error != nil {
			if err := tx.Commit(r.Context()); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, attempt)
			return
		}
		writeError(w, "ATTEMPT_FAILED", http.StatusConflict, "delivery attempt has already failed")
		return
	}
	if input.Error != nil {
		if err := tx.QueryRow(r.Context(), `
			update message_deliveries set state = 'failed', error = $3 where message_id = $1 and attempt = $2
			returning message_id::text, attempt, delivery, session_id, envelope_id, state, error, reply_id::text, created_at
		`, message.ID, input.Attempt, *input.Error).Scan(
			&attempt.MessageID, &attempt.Attempt, &attempt.Delivery, &attempt.SessionID, &attempt.EnvelopeID,
			&attempt.State, &attempt.Error, &attempt.ReplyID, &attempt.CreatedAt,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		event, err := s.appendEvent(r.Context(), tx, issueOwner(message.IssueKey).event("message.delivery", actor,
			model.MessageDeliveryEventPayload{
				MessageID: message.ID, Attempt: input.Attempt, Delivery: attempt.Delivery, SessionID: attempt.SessionID,
				State: "failed", Error: *attempt.Error,
			}))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		s.publish(event)
		writeJSON(w, http.StatusOK, attempt)
		return
	}
	author, err := json.Marshal(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var reply model.Message
	var authorRaw []byte
	if err := tx.QueryRow(r.Context(), `
		insert into messages (issue_key, author, body, in_reply_to)
		values ($1, $2, $3, $4)
		returning id::text, issue_key, author, body, target, in_reply_to::text, created_at
	`, message.IssueKey, author, *input.Body, message.ID).Scan(
		&reply.ID, &reply.IssueKey, &authorRaw, &reply.Body, &reply.Target, &reply.InReplyTo, &reply.CreatedAt,
	); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := json.Unmarshal(authorRaw, &reply.Author); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reply.Deliveries = []model.MessageDelivery{}
	if err := refs.Replace(r.Context(), tx, "message", reply.ID, reply.Body, s.deps.ServerURL); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update message_deliveries set reply_id = $3 where message_id = $1 and attempt = $2
	`, message.ID, input.Attempt, reply.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, issueOwner(message.IssueKey).event(
		"message.answered", actor, model.MessageEventPayload{Message: reply, ReplyBody: truncateRunes(message.Body, maxMessageReplyPreview16)},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	writeJSON(w, http.StatusCreated, reply)
}

func (s *server) getMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	message, err := s.loadMessage(r.Context(), s.deps.Store.Pool, r.PathValue("key"), r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	message.Deliveries, err = s.loadMessageDeliveries(r.Context(), s.deps.Store.Pool, message.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	replies, err := s.loadMessageReplyChain(r.Context(), s.deps.Store.Pool, message.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Message model.Message   `json:"message"`
		Replies []model.Message `json:"replies"`
	}{Message: message, Replies: replies})
}

func (s *server) loadMessage(ctx context.Context, q queryer, issueKey, id string) (model.Message, error) {
	query := `
		select id::text, issue_key, author, body, target, in_reply_to::text, created_at
		from messages where id = $1`
	args := []any{id}
	if issueKey != "" {
		query += " and issue_key = $2"
		args = append(args, issueKey)
	}
	return scanMessage(q.QueryRow(ctx, query, args...))
}

func scanMessage(row pgx.Row, into ...*model.Message) (model.Message, error) {
	var message model.Message
	if len(into) > 0 {
		message = *into[0]
	}
	var author []byte
	if err := row.Scan(
		&message.ID, &message.IssueKey, &author, &message.Body, &message.Target, &message.InReplyTo, &message.CreatedAt,
	); err != nil {
		return model.Message{}, err
	}
	if err := json.Unmarshal(author, &message.Author); err != nil {
		return model.Message{}, fmt.Errorf("decode message author: %w", err)
	}
	message.Deliveries = []model.MessageDelivery{}
	return message, nil
}

func (s *server) loadMessageDeliveries(ctx context.Context, q queryer, messageID string) ([]model.MessageDelivery, error) {
	rows, err := q.Query(ctx, `
		select message_id::text, attempt, delivery, session_id, envelope_id, state, error, reply_id::text, created_at
		from message_deliveries where message_id = $1 order by attempt
	`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deliveries := []model.MessageDelivery{}
	for rows.Next() {
		var delivery model.MessageDelivery
		if err := rows.Scan(
			&delivery.MessageID, &delivery.Attempt, &delivery.Delivery, &delivery.SessionID, &delivery.EnvelopeID,
			&delivery.State, &delivery.Error, &delivery.ReplyID, &delivery.CreatedAt,
		); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

func (s *server) loadMessageReplyChain(ctx context.Context, q queryer, seedID string) ([]model.Message, error) {
	rows, err := q.Query(ctx, `
		with recursive replies as (
			select id, issue_key, author, body, target, in_reply_to, created_at
			from messages where in_reply_to = $1
			union all
			select m.id, m.issue_key, m.author, m.body, m.target, m.in_reply_to, m.created_at
			from messages m join replies r on m.in_reply_to = r.id
		)
		select id::text, issue_key, author, body, target, in_reply_to::text, created_at
		from replies
		order by created_at, id
	`, seedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	replies := []model.Message{}
	for rows.Next() {
		reply, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	return replies, rows.Err()
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
