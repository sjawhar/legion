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
	parent, err := loadMessageReplyParent(ctx, tx, issueKey, input.Target, input.InReplyTo)
	if err != nil {
		return model.Message{}, nil, err
	}
	// A reply's conversation is its thread root's, so every reply event names the root's
	// target. A human's reply that names no target also continues the thread the way it was
	// last delivered: when the thread's root was targeted at a session, the reply reaches
	// that session too, which needs the thread's delivery mode as well as its target. A
	// session's own reply never inherits - the target would be itself - but the event still
	// has to name the conversation the reply lands in.
	thread := messageReplyThread{ReplyBody: parent.ReplyBody}
	if input.InReplyTo != nil {
		if input.Target == nil && actor.Kind == "user" {
			var rootTarget *string
			rootTarget, delivery, err = threadRootDelivery(ctx, tx, *input.InReplyTo)
			if err != nil {
				return model.Message{}, nil, err
			}
			input.Target = rootTarget
			thread = messageReplyThread{
				ReplyBody:    parent.ReplyBody,
				ThreadTarget: messageTarget(rootTarget),
			}
		} else {
			thread, err = parent.thread(ctx, tx)
			if err != nil {
				return model.Message{}, nil, err
			}
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
	var referenceChanges model.ReferenceChanges
	if message.IssueKey != nil {
		referenceChanges, err = s.replaceReferences(ctx, tx, "message", message.ID, message.Body)
		if err != nil {
			return model.Message{}, nil, err
		}
	}
	eventType := "message.created"
	if message.InReplyTo != nil {
		eventType = "message.answered"
	}
	event, err := s.appendEvent(ctx, tx, messageEvent(message, eventType, actor, thread.payload(message, referenceChanges)))
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
		attempt, err := s.deliverMessage(ctx, message, delivery, input.Urgency, actor, &thread)
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

// messageReplyParent is the message a reply names: its id, the preview its event carries, and
// the parent's own place in the thread, which is the thread root whenever the parent answers
// nothing itself.
type messageReplyParent struct {
	ID        string
	ReplyBody string
	Target    *string
	InReplyTo *string
}

// messageReplyPreview is how much of a parent's body a reply's event and delivery frame carry.
func messageReplyPreview(body string) string {
	return text.HeadRunes(body, maxMessageReplyPreview16)
}

// thread is what a reply to this parent says about the thread it joins.
func (p messageReplyParent) thread(ctx context.Context, tx pgx.Tx) (messageReplyThread, error) {
	threadTarget, err := replyThreadTarget(ctx, tx, p)
	if err != nil {
		return messageReplyThread{}, err
	}
	return messageReplyThread{ReplyBody: p.ReplyBody, ThreadTarget: threadTarget}, nil
}

// messageReplyParentOf is the reply parent an already-loaded message makes, for a caller that
// holds the row rather than an id a request supplied.
func messageReplyParentOf(message model.Message) messageReplyParent {
	return messageReplyParent{
		ID:        message.ID,
		ReplyBody: messageReplyPreview(message.Body),
		Target:    message.Target,
		InReplyTo: message.InReplyTo,
	}
}

// loadMessageReplyParent validates a reply's parent and returns it. A reply stays in its
// parent's conversation: on an issue, the parent is a message of that issue; in an issue-less
// agent conversation, the parent is issue-less and its thread root is targeted at the same
// session the reply is being sent to.
func loadMessageReplyParent(
	ctx context.Context, tx pgx.Tx, issueKey, target, inReplyTo *string,
) (messageReplyParent, error) {
	if inReplyTo == nil {
		return messageReplyParent{}, nil
	}
	invalid := errorf(http.StatusBadRequest, "MESSAGE_INPUT", "in_reply_to must identify a message on this issue")
	if issueKey == nil {
		invalid = errorf(http.StatusBadRequest, "MESSAGE_INPUT", "in_reply_to must identify a message of this session's conversation")
	}
	if strings.TrimSpace(*inReplyTo) == "" {
		return messageReplyParent{}, invalid
	}
	if _, err := uuid.Parse(*inReplyTo); err != nil {
		return messageReplyParent{}, invalid
	}
	var parent messageReplyParent
	var parentBody string
	var err error
	if issueKey != nil {
		err = tx.QueryRow(ctx, `
			select body, target, in_reply_to::text from messages where id = $1 and issue_key = $2
		`, *inReplyTo, *issueKey).Scan(&parentBody, &parent.Target, &parent.InReplyTo)
	} else {
		if target == nil {
			return messageReplyParent{}, invalid
		}
		err = tx.QueryRow(ctx, `
			with recursive thread as (
				select id, body, issue_key, target, in_reply_to, 0 as depth from messages where id = $1
				union all
				select m.id, m.body, m.issue_key, m.target, m.in_reply_to, t.depth + 1
				from messages m join thread t on m.id = t.in_reply_to
			)
			select
				(select body from thread where depth = 0),
				(select target from thread where depth = 0),
				(select in_reply_to::text from thread where depth = 0)
			from thread
			where in_reply_to is null and issue_key is null and target = $2
			  and not exists (select 1 from thread where issue_key is not null)
		`, *inReplyTo, *target).Scan(&parentBody, &parent.Target, &parent.InReplyTo)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return messageReplyParent{}, invalid
		}
		return messageReplyParent{}, err
	}
	parent.ID = *inReplyTo
	parent.ReplyBody = messageReplyPreview(parentBody)
	return parent, nil
}

// messageReplyThread is what a message's event and its delivery frame both say about the
// thread it joins: the preview of the parent it answers and the target of that thread's root.
// Both are empty for a root message, which answers nothing and is its own thread.
type messageReplyThread struct {
	ReplyBody    string
	ThreadTarget string
}

// payload is the event or delivery-frame payload for a message in this thread, so the two
// always say the same thing about it.
func (t messageReplyThread) payload(
	message model.Message,
	changes model.ReferenceChanges,
) model.MessageEventPayload {
	return model.MessageEventPayload{
		Message:                 message,
		ReferenceChangesPayload: model.NewReferenceChangesPayload(changes),
		ReplyBody:               t.ReplyBody,
		ThreadTarget:            t.ThreadTarget,
	}
}

// loadMessageReplyThread derives that for a stored message, for a caller that did not derive
// it while writing the message.
func loadMessageReplyThread(ctx context.Context, tx pgx.Tx, message model.Message) (messageReplyThread, error) {
	parent, err := loadMessageReplyParent(ctx, tx, message.IssueKey, message.Target, message.InReplyTo)
	if err != nil {
		return messageReplyThread{}, err
	}
	// A message that answers nothing has no parent to preview: loadMessageReplyParent returns
	// the zero value for a nil in_reply_to, so there is nothing to carry forward.
	if message.InReplyTo == nil {
		return messageReplyThread{}, nil
	}
	return parent.thread(ctx, tx)
}

// messageThreadCTE names `thread`: the message $1 and every ancestor it answers, up to the
// thread root. A message's in_reply_to is set once at insert and never updated, so the chain
// only ever points at older rows and the walk terminates.
const messageThreadCTE = `
		with recursive thread as (
			select id, target, in_reply_to from messages where id = $1
			union all
			select m.id, m.target, m.in_reply_to from messages m join thread t on m.id = t.in_reply_to
		)`

// threadRootDelivery walks a reply's ancestry to the thread root and returns that root's
// target - the conversation the whole thread belongs to - together with the mode of the
// thread's most recent delivery attempt, so a reply can be delivered like the thread was.
// An untargeted thread yields nothing.
func threadRootDelivery(ctx context.Context, tx pgx.Tx, inReplyTo string) (*string, string, error) {
	var target *string
	var delivery *string
	err := tx.QueryRow(ctx, messageThreadCTE+`
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

// replyThreadTarget is the target of the thread root a reply to parent lands under: the
// conversation the reply joins, empty under an untargeted thread. The parent is that root
// unless it answers something itself, so only a deeper reply walks.
func replyThreadTarget(ctx context.Context, tx pgx.Tx, parent messageReplyParent) (string, error) {
	if parent.InReplyTo == nil {
		return messageTarget(parent.Target), nil
	}
	var rootTarget *string
	if err := tx.QueryRow(ctx, messageThreadCTE+`
		select (select target from thread where in_reply_to is null)
	`, parent.ID).Scan(&rootTarget); err != nil {
		return "", err
	}
	return messageTarget(rootTarget), nil
}

func urgencyValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func messageIssueKey(message model.Message) string {
	if message.IssueKey == nil {
		return ""
	}
	return *message.IssueKey
}

func messageTarget(target *string) string {
	if target == nil {
		return ""
	}
	return *target
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
