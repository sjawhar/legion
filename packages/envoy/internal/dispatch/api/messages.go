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
	"github.com/sjawhar/envoy/internal/dispatch/text"
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

type messageRead struct {
	Message model.Message   `json:"message"`
	Replies []model.Message `json:"replies"`
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
		if actor, err = bearerSessionActor(actor, input.Actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	delivery, err := validateCreateMessage(&input)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issueKey := r.PathValue("key")
	message, advice, err := s.createStoredMessage(r.Context(), &issueKey, input, delivery, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, withAdvice(message, advice))
}

func (s *server) createAgentMessage(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		Body      string  `json:"body"`
		Delivery  string  `json:"delivery"`
		InReplyTo *string `json:"in_reply_to"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	target := "session:" + r.PathValue("session_id")
	messageInput := createMessageInput{
		Body: input.Body, InReplyTo: input.InReplyTo, Target: &target, Delivery: &input.Delivery,
	}
	delivery, err := validateCreateMessage(&messageInput)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	message, _, err := s.createStoredMessage(r.Context(), nil, messageInput, delivery, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, message)
}

func (s *server) createStoredMessage(
	ctx context.Context,
	issueKey *string,
	input createMessageInput,
	delivery string,
	actor model.Actor,
) (model.Message, *writeAdvice, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Message{}, nil, err
	}
	defer tx.Rollback(ctx)
	var status string
	if issueKey != nil {
		status, err = s.requireOpenIssue(ctx, tx, *issueKey)
		if err != nil {
			return model.Message{}, nil, err
		}
	}
	replyBody, err := messageReplyBody(ctx, tx, issueKey, input.Target, input.InReplyTo)
	if err != nil {
		return model.Message{}, nil, err
	}
	// A human's reply that names no target continues the thread the way it was last delivered:
	// when the thread's root was targeted at a session, the reply reaches that session too. A
	// session's own reply never inherits - the target would be itself.
	if input.Target == nil && input.InReplyTo != nil && actor.Kind == "user" {
		input.Target, delivery, err = inheritedThreadDelivery(ctx, tx, *input.InReplyTo)
		if err != nil {
			return model.Message{}, nil, err
		}
	}
	author, err := json.Marshal(actor)
	if err != nil {
		return model.Message{}, nil, err
	}
	message, err := scanMessage(tx.QueryRow(ctx, `
		insert into messages (issue_key, author, body, target, in_reply_to)
		values ($1, $2, $3, $4, $5)
		returning `+messageColumns+`
	`, issueKey, author, input.Body, input.Target, input.InReplyTo))
	if err != nil {
		return model.Message{}, nil, err
	}
	if message.IssueKey != nil {
		if err := refs.Replace(ctx, tx, "message", message.ID, message.Body, s.deps.ServerURL); err != nil {
			return model.Message{}, nil, err
		}
	}
	eventType := "message.created"
	if message.InReplyTo != nil {
		eventType = "message.answered"
	}
	event, err := s.appendEvent(ctx, tx, messageEvent(
		message, eventType, actor, model.MessageEventPayload{Message: message, ReplyBody: replyBody},
	))
	if err != nil {
		return model.Message{}, nil, err
	}
	if message.IssueKey != nil {
		if err := refs.Stamp(ctx, tx, "message", message.ID, event.ID); err != nil {
			return model.Message{}, nil, err
		}
	}
	var advice *writeAdvice
	if issueKey != nil {
		advice = s.writeAdvice(
			ctx, tx, "POST /api/v1/issues/{key}/messages", *issueKey, actor, "", status,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Message{}, nil, err
	}
	s.publish(event)
	if message.Target != nil {
		attempt, err := s.deliverMessage(ctx, message, delivery, input.Urgency, actor, &replyBody)
		if err != nil {
			return model.Message{}, nil, err
		}
		message.Deliveries = []model.MessageDelivery{attempt}
	}
	return message, advice, nil
}

func messageEvent(message model.Message, eventType string, actor model.Actor, payload any) model.Event {
	if message.IssueKey != nil {
		return issueOwner(*message.IssueKey).event(eventType, actor, payload)
	}
	return model.Event{Type: eventType, Actor: actor, Payload: payload}
}

func validateCreateMessage(input *createMessageInput) (string, error) {
	if strings.TrimSpace(input.Body) == "" {
		return "", errorf(http.StatusBadRequest, "INVALID_MESSAGE", "message body is required")
	}
	if length := len16(input.Body); length > maxMessageBody16 {
		return "", capExceededError("body", length, maxMessageBody16)
	}
	target, delivery, err := validateMessageDelivery(input.Target, input.Delivery)
	if err != nil {
		return "", errorf(http.StatusBadRequest, "MESSAGE_INPUT", "%s", err)
	}
	input.Target = target
	if input.Urgency != nil && !validMessageUrgency(*input.Urgency) {
		return "", errorf(http.StatusBadRequest, "MESSAGE_INPUT", "urgency must be one of low, med, high, blocking")
	}
	return delivery, nil
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

// messageReplyBody validates a reply's parent and returns its preview. A reply stays in its
// parent's conversation: on an issue, the parent is a message of that issue; in an issue-less
// agent conversation, the parent is issue-less and its thread root is targeted at the same
// session the reply is being sent to.
func messageReplyBody(ctx context.Context, tx pgx.Tx, issueKey, target, inReplyTo *string) (string, error) {
	if inReplyTo == nil {
		return "", nil
	}
	invalid := errorf(http.StatusBadRequest, "MESSAGE_INPUT", "in_reply_to must identify a message on this issue")
	if issueKey == nil {
		invalid = errorf(http.StatusBadRequest, "MESSAGE_INPUT", "in_reply_to must identify a message of this session's conversation")
	}
	if strings.TrimSpace(*inReplyTo) == "" {
		return "", invalid
	}
	if _, err := uuid.Parse(*inReplyTo); err != nil {
		return "", invalid
	}
	var parentBody string
	var err error
	if issueKey != nil {
		err = tx.QueryRow(ctx, `select body from messages where id = $1 and issue_key = $2`, *inReplyTo, *issueKey).Scan(&parentBody)
	} else {
		if target == nil {
			return "", invalid
		}
		err = tx.QueryRow(ctx, `
			with recursive thread as (
				select id, body, issue_key, target, in_reply_to, 0 as depth from messages where id = $1
				union all
				select m.id, m.body, m.issue_key, m.target, m.in_reply_to, t.depth + 1
				from messages m join thread t on m.id = t.in_reply_to
			)
			select (select body from thread where depth = 0)
			from thread
			where in_reply_to is null and issue_key is null and target = $2
			  and not exists (select 1 from thread where issue_key is not null)
		`, *inReplyTo, *target).Scan(&parentBody)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", invalid
		}
		return "", err
	}
	return text.HeadRunes(parentBody, maxMessageReplyPreview16), nil
}

// inheritedThreadDelivery walks a reply's ancestry to the thread root; when that root was
// targeted, it returns the root's target and the mode of the thread's most recent delivery
// attempt, so the reply is delivered like the thread was. An untargeted thread yields nothing.
func inheritedThreadDelivery(ctx context.Context, tx pgx.Tx, inReplyTo string) (*string, string, error) {
	var target *string
	var delivery *string
	err := tx.QueryRow(ctx, `
		with recursive thread as (
			select id, target, in_reply_to from messages where id = $1
			union all
			select m.id, m.target, m.in_reply_to from messages m join thread t on m.id = t.in_reply_to
		)
		select
			(select target from thread where in_reply_to is null),
			(select d.delivery from message_deliveries d join thread t on d.message_id = t.id
			 order by d.created_at desc, d.attempt desc limit 1)
	`, inReplyTo).Scan(&target, &delivery)
	if err != nil {
		return nil, "", err
	}
	if target == nil {
		return nil, "", nil
	}
	if delivery == nil {
		return target, "steer", nil
	}
	return target, *delivery, nil
}

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

// deliverMessage records one delivery attempt of a targeted message and sends it to the resolved
// session. replyBody is the parent preview a reply's delivery frame carries; a caller that has
// not derived it passes nil and it is read from the stored parent.
func (s *server) deliverMessage(
	ctx context.Context,
	message model.Message,
	delivery string,
	urgency *string,
	actor model.Actor,
	replyBody *string,
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
	if message.IssueKey != nil {
		if _, err := s.requireOpenIssue(ctx, tx, *message.IssueKey); err != nil {
			return model.MessageDelivery{}, err
		}
	}
	var attemptNumber int
	if err := tx.QueryRow(ctx, `select coalesce(max(attempt), 0) + 1 from message_deliveries where message_id = $1`, message.ID).Scan(&attemptNumber); err != nil {
		return model.MessageDelivery{}, err
	}

	resolved := s.resolveMentionTargets(ctx, []string{target}, delivery)[0]
	targetSession := resolved.attemptSessionID
	var preview string
	if replyBody != nil {
		preview = *replyBody
	} else {
		preview, err = messageReplyBody(ctx, tx, message.IssueKey, message.Target, message.InReplyTo)
		if err != nil {
			return model.MessageDelivery{}, err
		}
	}
	frame, err := json.Marshal(struct {
		Event    model.Event `json:"event"`
		Delivery any         `json:"delivery"`
	}{
		Event: messageEvent(message, "message.created", message.Author,
			model.MessageEventPayload{Message: message, ReplyBody: preview}),
		Delivery: map[string]any{"attempt": attemptNumber, "mode": delivery},
	})
	if err != nil {
		return model.MessageDelivery{}, fmt.Errorf("encode target delivery frame: %w", err)
	}
	envelopeID, deliveryError := s.sendResolvedDelivery(
		ctx, resolved, message.Body, message.ID+":"+fmt.Sprint(attemptNumber), urgency, frame,
	)
	state := "sent"
	if deliveryError != "" {
		state = "failed"
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
	event, err := s.appendEvent(ctx, tx, messageEvent(message, "message.delivery", actor,
		model.MessageDeliveryEventPayload{
			MessageID: message.ID, Attempt: attemptNumber, Delivery: delivery, SessionID: targetSession,
			Target: target, Title: resolved.title, State: state, Error: deliveryError,
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
	if !hasCapability(target.Capabilities, delivery) {
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
		event, err := s.appendEvent(r.Context(), tx, messageEvent(message, "message.delivery", actor,
			model.MessageDeliveryEventPayload{
				MessageID: message.ID, Attempt: input.Attempt, Delivery: attempt.Delivery, SessionID: attempt.SessionID,
				Target: messageTarget(message), State: "failed", Error: *attempt.Error,
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
	if reply.IssueKey != nil {
		if err := refs.Replace(r.Context(), tx, "message", reply.ID, reply.Body, s.deps.ServerURL); err != nil {
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
	event, err := s.appendEvent(r.Context(), tx, messageEvent(
		reply,
		"message.answered",
		actor,
		model.MessageEventPayload{Message: reply, ReplyBody: text.HeadRunes(message.Body, maxMessageReplyPreview16)},
	))
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

func messageIssueKey(message model.Message) string {
	if message.IssueKey == nil {
		return ""
	}
	return *message.IssueKey
}

func messageTarget(message model.Message) string {
	if message.Target == nil {
		return ""
	}
	return *message.Target
}

func (s *server) getMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !requireUUIDPath(w, r, "message") {
		return
	}
	message, err := s.loadMessage(r.Context(), s.deps.Store.Pool, r.PathValue("key"), r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	deliveries, err := s.loadMessageDeliveries(r.Context(), s.deps.Store.Pool, []string{message.ID})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	message.Deliveries = deliveries[message.ID]
	replies, err := s.loadMessageReplyChains(r.Context(), s.deps.Store.Pool, []string{message.ID})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, messageRead{Message: message, Replies: replies[message.ID]})
}

func (s *server) listAgentMessages(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	if sessionID == "" {
		writeError(w, "MESSAGE_INPUT", http.StatusBadRequest, "session id is required")
		return
	}
	target := "session:" + sessionID
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select m.id::text, m.issue_key, m.author, m.body, m.target, m.in_reply_to::text, m.created_at
		from messages m
		where m.in_reply_to is null
		  and (
			m.target = $1
			or exists (
				select 1 from message_deliveries d
				where d.message_id = m.id and d.session_id = $2
			)
		  )
		order by m.created_at desc, m.id desc
		limit 50
	`, target, sessionID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	roots := []model.Message{}
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		roots = append(roots, message)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows.Close()
	rootIDs := make([]string, len(roots))
	for index, root := range roots {
		rootIDs[index] = root.ID
	}
	deliveries, err := s.loadMessageDeliveries(r.Context(), s.deps.Store.Pool, rootIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	replies, err := s.loadMessageReplyChains(r.Context(), s.deps.Store.Pool, rootIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	result := make([]messageRead, 0, len(roots))
	for _, root := range roots {
		root.Deliveries = deliveries[root.ID]
		result = append(result, messageRead{Message: root, Replies: replies[root.ID]})
	}
	WriteJSON(w, http.StatusOK, result)
}

func (s *server) loadMessage(ctx context.Context, q queryer, issueKey, id string) (model.Message, error) {
	query := `
		select ` + messageColumns + `
		from messages where id = $1`
	args := []any{id}
	if issueKey != "" {
		query += " and issue_key = $2"
		args = append(args, issueKey)
	}
	return scanMessage(q.QueryRow(ctx, query, args...))
}

// messageColumns is the messages select list scanMessage reads, in scan order.
const messageColumns = `id::text, issue_key, author, body, target, in_reply_to::text, created_at`

// scanMessage decodes one messageColumns row; extra receives any columns selected after them.
func scanMessage(row pgx.Row, extra ...any) (model.Message, error) {
	var message model.Message
	var author []byte
	dest := append([]any{
		&message.ID, &message.IssueKey, &author, &message.Body, &message.Target, &message.InReplyTo, &message.CreatedAt,
	}, extra...)
	if err := row.Scan(dest...); err != nil {
		return model.Message{}, err
	}
	if err := json.Unmarshal(author, &message.Author); err != nil {
		return model.Message{}, fmt.Errorf("decode message author: %w", err)
	}
	message.Deliveries = []model.MessageDelivery{}
	return message, nil
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
		select message_id::text, attempt, delivery, session_id, envelope_id, state, error, reply_id::text, created_at
		from message_deliveries where message_id = any($1::uuid[]) order by message_id, attempt
	`, messageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var delivery model.MessageDelivery
		if err := rows.Scan(
			&delivery.MessageID, &delivery.Attempt, &delivery.Delivery, &delivery.SessionID, &delivery.EnvelopeID,
			&delivery.State, &delivery.Error, &delivery.ReplyID, &delivery.CreatedAt,
		); err != nil {
			return nil, err
		}
		deliveries[delivery.MessageID] = append(deliveries[delivery.MessageID], delivery)
	}
	return deliveries, rows.Err()
}

// loadMessageReplyChains reads, for each seed message, every message transitively replying to
// it, oldest first, in one recursive query; each reply carries its own deliveries. A seed with
// no replies maps to an empty (never nil) slice. Seeds must not be replies of one another: a
// reply is grouped under exactly one seed.
func (s *server) loadMessageReplyChains(ctx context.Context, q queryer, seedIDs []string) (map[string][]model.Message, error) {
	chains := make(map[string][]model.Message, len(seedIDs))
	for _, id := range seedIDs {
		chains[id] = []model.Message{}
	}
	if len(seedIDs) == 0 {
		return chains, nil
	}
	rows, err := q.Query(ctx, `
		with recursive replies as (
			select id, issue_key, author, body, target, in_reply_to, created_at, in_reply_to as seed_id
			from messages where in_reply_to = any($1::uuid[])
			union all
			select m.id, m.issue_key, m.author, m.body, m.target, m.in_reply_to, m.created_at, r.seed_id
			from messages m join replies r on m.in_reply_to = r.id
		)
		select `+messageColumns+`, seed_id::text
		from replies
		order by created_at, id
	`, seedIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var replyIDs []string
	for rows.Next() {
		var seedID string
		reply, err := scanMessage(rows, &seedID)
		if err != nil {
			return nil, err
		}
		chains[seedID] = append(chains[seedID], reply)
		replyIDs = append(replyIDs, reply.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// A reply in the thread may itself have been delivered (a human's follow-up on a targeted
	// thread); its attempts belong to the chain a reader sees.
	deliveries, err := s.loadMessageDeliveries(ctx, q, replyIDs)
	if err != nil {
		return nil, err
	}
	for _, chain := range chains {
		for index := range chain {
			chain[index].Deliveries = deliveries[chain[index].ID]
		}
	}
	return chains, nil
}
