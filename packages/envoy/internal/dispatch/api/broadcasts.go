package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A broadcast is one human's message to many sessions. It is a grouping over the targeted
// messages Dispatch already sends, not a second delivery mechanism: every recipient gets an
// ordinary issue-less message aimed at its own session, carrying the broadcast's id, so each
// recipient's thread, retry and reply work exactly as they do for a message sent from one
// agent card. Nothing reads a broadcast to deliver it; the broadcast row only records what
// the recipients share, and GET /api/v1/broadcasts/{id} reads their messages back.
//
// A selected session that does not advertise the chosen mode is never switched to another
// one: it is excluded before anything is written, and named in the create response. The mode
// a human picked is part of what they said - a steer interrupts an agent and a btw waits for
// an answer - so silently downgrading it would deliver a different message than the one the
// sender composed. Exclusions are not stored: the browser excludes them at compose time from
// the same capability list, and the response covers the race where a session's capabilities
// or liveness changed in between. What persists is who was actually sent to.
//
// Broadcasting is human-only, like the one-session route it is built from.

// broadcastDeliveryWorkers is how many recipients are delivered to at once, once the create
// request has already answered. Each worker takes a tracking context of its own: the pool's
// one-connection guard is per context (store.WithTransactionTracking), so workers sharing one
// would both trip it and race on its flag. Four keeps a large send's wall time down without
// holding more than four of the shared pool's connections for one broadcast.
const broadcastDeliveryWorkers = 4

// broadcast is what a send's recipients share: who sent it, the body they were all sent, the
// mode it was sent in, and when.
type broadcast struct {
	ID        string      `json:"id"`
	Author    model.Actor `json:"author"`
	Body      string      `json:"body"`
	Delivery  string      `json:"delivery"`
	CreatedAt time.Time   `json:"created_at"`
}

// broadcastSummary is one row of the broadcast list: the send, how many sessions it reached,
// and how many of them have answered.
type broadcastSummary struct {
	broadcast
	Recipients int `json:"recipients"`
	Replies    int `json:"replies"`
}

// broadcastRecipient is one session's copy of a broadcast: the message it was sent, with its
// delivery attempts, and the replies threaded under it.
type broadcastRecipient struct {
	SessionID string          `json:"session_id"`
	Message   model.Message   `json:"message"`
	Replies   []model.Message `json:"replies"`
}

// broadcastRead is a broadcast with every recipient's state.
type broadcastRead struct {
	broadcast
	Recipients []broadcastRecipient `json:"recipients"`
}

// broadcastExclusion is a session the sender selected that was not sent to, and why.
type broadcastExclusion struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Reason    string `json:"reason"`
}

// broadcastCreated is the create response: what was sent, and what was left out of it.
type broadcastCreated struct {
	broadcastRead
	Excluded []broadcastExclusion `json:"excluded"`
}

// broadcastTarget is one selected session the send resolved: the live row it matched, or the
// reason it was excluded.
type broadcastTarget struct {
	sessionID string
	title     string
	reason    string
}

// POST /api/v1/broadcasts
func (s *server) createBroadcast(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		Body       string   `json:"body"`
		Delivery   string   `json:"delivery"`
		SessionIDs []string `json:"session_ids"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	requested, err := validateBroadcastInput(input.Body, input.Delivery, input.SessionIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if s.deps.Envoy == nil {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, "ENVOY_URL is not configured")
		return
	}
	// One listener read decides every recipient, so a broadcast judges its whole selection
	// against one view of the registry rather than a different one per session.
	sessions, err := s.deps.Envoy.Sessions(r.Context())
	if err != nil {
		if errors.Is(err, dispatchenvoy.ErrUnavailable) {
			writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, err.Error())
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	targets := resolveBroadcastTargets(requested, input.Delivery, sessions)
	recipients := make([]broadcastTarget, 0, len(targets))
	excluded := make([]broadcastExclusion, 0)
	for _, target := range targets {
		if target.reason != "" {
			excluded = append(excluded, broadcastExclusion{
				SessionID: target.sessionID, Title: target.title, Reason: target.reason,
			})
			continue
		}
		recipients = append(recipients, target)
	}
	if len(recipients) == 0 {
		writeError(w, "BROADCAST_EMPTY", http.StatusBadRequest,
			"no selected session can receive this message: "+excludedSummary(excluded))
		return
	}

	created, events, err := s.writeBroadcast(r.Context(), actor, input.Body, input.Delivery, recipients)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for _, event := range events {
		s.publish(event)
	}
	// The send answers as soon as the messages are committed, and delivery runs behind it.
	// Tying delivery to the request meant a request cut off partway - a closed tab, a deploy's
	// five-second shutdown, the load balancer's idle timeout - left one attempt stranded
	// pending and every later recipient with a message and no attempt at all, which nothing
	// recovers. Every recipient's attempt, receipt and reply reaches an open broadcast view
	// over the event stream, so nothing is lost by answering first.
	messages := make([]model.Message, 0, len(created.Recipients))
	for _, recipient := range created.Recipients {
		messages = append(messages, recipient.Message)
	}
	go s.deliverBroadcast(messages, input.Delivery, actor)
	WriteJSON(w, http.StatusCreated, broadcastCreated{broadcastRead: created, Excluded: excluded})
}

// validateBroadcastInput checks one send's shape and returns its recipients, in the order the
// caller listed them and with the duplicates a multi-select can produce removed: a session
// named twice is one recipient, not two messages.
func validateBroadcastInput(body, delivery string, sessionIDs []string) ([]string, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errorf(http.StatusBadRequest, "INVALID_MESSAGE", "message body is required")
	}
	if length := len16(body); length > maxMessageBody16 {
		return nil, capExceededError("body", length, maxMessageBody16)
	}
	if !validDelivery(delivery) {
		return nil, errorf(http.StatusBadRequest, "BROADCAST_INPUT", "delivery must be one of btw, aside, steer")
	}
	seen := make(map[string]struct{}, len(sessionIDs))
	requested := make([]string, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		trimmed := strings.TrimSpace(sessionID)
		if trimmed == "" {
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		requested = append(requested, trimmed)
	}
	if len(requested) == 0 {
		return nil, errorf(http.StatusBadRequest, "BROADCAST_INPUT", "session_ids must name at least one session")
	}
	// contracts.MaxBroadcastRecipients bounds one send, and the dashboard refuses the same number
	// before it asks. A runaway guard rather than a product limit.
	if len(requested) > contracts.MaxBroadcastRecipients {
		return nil, errorf(http.StatusBadRequest, "BROADCAST_INPUT",
			"a broadcast reaches at most %d sessions (%d selected)", contracts.MaxBroadcastRecipients, len(requested))
	}
	return requested, nil
}

// resolveBroadcastTargets judges each selected session against one registry read: it is a
// recipient when it is live and advertises the chosen mode, and otherwise carries the reason
// it was left out.
func resolveBroadcastTargets(
	requested []string, delivery string, sessions []dispatchenvoy.Session,
) []broadcastTarget {
	live := make(map[string]dispatchenvoy.Session, len(sessions))
	for _, session := range sessions {
		live[session.SessionID] = session
	}
	targets := make([]broadcastTarget, 0, len(requested))
	for _, sessionID := range requested {
		session, ok := live[sessionID]
		switch {
		case !ok:
			targets = append(targets, broadcastTarget{sessionID: sessionID, reason: "no live session"})
		case !hasCapability(session.Capabilities, delivery):
			targets = append(targets, broadcastTarget{
				sessionID: sessionID, title: session.Title, reason: "does not advertise " + delivery,
			})
		default:
			targets = append(targets, broadcastTarget{sessionID: sessionID, title: session.Title})
		}
	}
	return targets
}

// excludedSummary names every excluded session and its reason, for the error a send with no
// reachable recipient answers with.
func excludedSummary(excluded []broadcastExclusion) string {
	reasons := make([]string, 0, len(excluded))
	for _, item := range excluded {
		name := item.Title
		if name == "" {
			name = item.SessionID
		}
		reasons = append(reasons, name+" "+item.Reason)
	}
	return strings.Join(reasons, "; ")
}

// writeBroadcast commits the broadcast and one targeted message per recipient in a single
// transaction, so a send is never half-written, and returns the events its messages owe the
// stream. Nothing is delivered here: a listener call never runs with a transaction open
// (envoy_resolve.go).
func (s *server) writeBroadcast(
	ctx context.Context,
	actor model.Actor,
	body, delivery string,
	recipients []broadcastTarget,
) (broadcastRead, []model.Event, error) {
	author, err := json.Marshal(actor)
	if err != nil {
		return broadcastRead{}, nil, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return broadcastRead{}, nil, err
	}
	defer tx.Rollback(ctx)
	sent := broadcastRead{broadcast: broadcast{Author: actor, Body: body, Delivery: delivery}}
	if err := tx.QueryRow(ctx, `
		insert into broadcasts (author, body, delivery)
		values ($1, $2, $3)
		returning id::text, created_at
	`, author, body, delivery).Scan(&sent.ID, &sent.CreatedAt); err != nil {
		return broadcastRead{}, nil, err
	}
	events := make([]model.Event, 0, len(recipients))
	sent.Recipients = make([]broadcastRecipient, 0, len(recipients))
	for position, recipient := range recipients {
		target := "session:" + recipient.sessionID
		message, err := scanMessage(tx.QueryRow(ctx, `
			insert into messages (issue_key, author, body, target, broadcast_id, broadcast_position)
			values (null, $1, $2, $3, $4, $5)
			returning `+messageColumns+`
		`, author, body, target, sent.ID, position))
		if err != nil {
			return broadcastRead{}, nil, err
		}
		// A broadcast message is a thread root that answers nothing, so its event says what
		// every other root's does; the broadcast it belongs to is read from the message row.
		event, err := s.appendEvent(ctx, tx, messageEvent(
			message, "message.created", actor, messageReplyThread{}.payload(message, model.ReferenceChanges{}),
		))
		if err != nil {
			return broadcastRead{}, nil, err
		}
		events = append(events, event)
		// The recipient's first delivery attempt is opened here, unclaimed, rather than by the
		// worker that will send it. A recipient then always has an attempt to show - "Sending"
		// while a worker holds it, and, if the process dies before the send, a lapsed pending
		// row the ordinary same-mode retry resumes under its own idempotency key. Opening it
		// later left a stranded recipient with no attempt at all, which only a delivery in a
		// DIFFERENT mode could move, and that is a genuine second frame where the first landed.
		// claimed_at stays null, so recordPendingMessageDelivery reads the row as free and
		// resumes it instead of opening a second attempt beside it.
		attempt, err := scanMessageDelivery(tx.QueryRow(ctx, `
			insert into message_deliveries (message_id, attempt, delivery, session_id, state)
			values ($1, 1, $2, $3, 'pending')
			returning `+messageDeliveryColumns, message.ID, delivery, recipient.sessionID))
		if err != nil {
			return broadcastRead{}, nil, err
		}
		message.Deliveries = []model.MessageDelivery{attempt}
		sent.Recipients = append(sent.Recipients, broadcastRecipient{
			SessionID: recipient.sessionID, Message: message, Replies: []model.Message{},
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return broadcastRead{}, nil, err
	}
	return sent, events, nil
}

// deliverBroadcast sends each recipient's message after the create request has answered,
// broadcastDeliveryWorkers at a time. It runs on the server's lifetime rather than the
// request's, so a browser that goes away cannot strand a recipient, and a shutdown cancels it
// rather than leaving the goroutine behind. One recipient's failure never stops another's: a
// send the listener refused settles that recipient's attempt as failed, a recipient whose
// attempt was taken over from the agent card is skipped, and a delivery this
// cannot record at all leaves the attempt pending and unclaimed, which the broadcast view
// shows as sending and, once the claim lease has passed, offers a same-mode retry for.
func (s *server) deliverBroadcast(messages []model.Message, delivery string, actor model.Actor) {
	workers := min(broadcastDeliveryWorkers, len(messages))
	pending := make(chan model.Message)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for message := range pending {
				ctx := store.WithTransactionTracking(s.lifetime())
				// Bound to the attempt the write opened. A human who changes the mode from the
				// agent card while this worker is queued settles that attempt and sends their
				// own; the worker must then send nothing, because an attempt of its own would
				// put a second frame on the session's subject.
				_, err := s.deliverMessageResuming(ctx, message, delivery, nil, actor, nil, 1)
				if errors.Is(err, errAttemptNotPending) {
					continue
				}
				if err != nil {
					slog.Error("dispatch: broadcast delivery",
						"message", message.ID, "target", messageTarget(message.Target), "error", err)
				}
			}
		}()
	}
	for _, message := range messages {
		pending <- message
	}
	close(pending)
	wait.Wait()
}

// lifetime is the process context background work runs on: cancelled when the server shuts
// down, unbounded in a test that configured none.
func (s *server) lifetime() context.Context {
	if s.deps.Lifetime == nil {
		return context.Background()
	}
	return s.deps.Lifetime
}

// GET /api/v1/broadcasts
func (s *server) listBroadcasts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select b.id::text, b.author, b.body, b.delivery, b.created_at,
		       count(distinct m.id),
		       -- Recipients who answered, not answered attempts: one recipient that answered a
		       -- retry as well as the original attempt is one answer, never two.
		       count(distinct m.id) filter (where d.reply_id is not null)
		from broadcasts b
		left join messages m on m.broadcast_id = b.id
		left join message_deliveries d on d.message_id = m.id and d.reply_id is not null
		group by b.id
		order by b.created_at desc, b.id desc
		limit 50
	`)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	summaries := []broadcastSummary{}
	for rows.Next() {
		var summary broadcastSummary
		var author []byte
		if err := rows.Scan(
			&summary.ID, &author, &summary.Body, &summary.Delivery, &summary.CreatedAt,
			&summary.Recipients, &summary.Replies,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := json.Unmarshal(author, &summary.Author); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, summaries)
}

// GET /api/v1/broadcasts/{id}
func (s *server) getBroadcast(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, "BROADCAST_NOT_FOUND", http.StatusNotFound, "broadcast not found")
		return
	}
	var read broadcastRead
	var author []byte
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		select id::text, author, body, delivery, created_at from broadcasts where id = $1
	`, id).Scan(&read.ID, &author, &read.Body, &read.Delivery, &read.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "BROADCAST_NOT_FOUND", http.StatusNotFound, "broadcast not found")
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	if err := json.Unmarshal(author, &read.Author); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	recipients, err := s.loadBroadcastRecipients(r.Context(), read.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read.Recipients = recipients
	WriteJSON(w, http.StatusOK, read)
}

// loadBroadcastRecipients reads every message the broadcast sent in the sender's requested
// order. Messages created before positions existed, or by an old server during a rolling deploy,
// have no position; their request order was never stored, so they retain the created_at and id
// fallback order. created_at cannot order one current broadcast because all of its rows share
// the one transaction's now().
func (s *server) loadBroadcastRecipients(ctx context.Context, broadcastID string) ([]broadcastRecipient, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select `+messageColumns+`
		from messages
		where broadcast_id = $1
		order by broadcast_position nulls last, created_at, id
	`, broadcastID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []model.Message{}
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	ids := make([]string, len(messages))
	for index, message := range messages {
		ids[index] = message.ID
	}
	deliveries, err := s.loadMessageDeliveries(ctx, s.deps.Store.Pool, ids)
	if err != nil {
		return nil, err
	}
	replies, err := s.loadMessageReplyChains(ctx, s.deps.Store.Pool, ids)
	if err != nil {
		return nil, err
	}
	recipients := make([]broadcastRecipient, 0, len(messages))
	for _, message := range messages {
		message.Deliveries = deliveries[message.ID]
		recipients = append(recipients, broadcastRecipient{
			SessionID: strings.TrimPrefix(messageTarget(message.Target), "session:"),
			Message:   message,
			Replies:   replies[message.ID],
		})
	}
	return recipients, nil
}
