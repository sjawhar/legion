package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
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

// maxBroadcastIdempotencyKey bounds the caller's key. The dashboard sends a UUID (36); the cap
// keeps a runaway key out of the index. broadcastKeyPattern keeps a key printable wherever it is
// echoed - a refusal, a log line, a psql session - and out of it whitespace and control
// characters; the pattern makes a key ASCII, so its length is its byte length.
const maxBroadcastIdempotencyKey = 128

var broadcastKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// errBroadcastKeyHeld is writeBroadcast's answer when the key row already exists: another request
// of this human's committed the same key first, and the caller answers with its broadcast.
var errBroadcastKeyHeld = errors.New("broadcast idempotency key already held")

// broadcastKey is what a send's idempotency key is judged by: the human it belongs to, the key,
// and the digest of the request it was first used for.
type broadcastKey struct{ login, key, digest string }

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
		Body           string   `json:"body"`
		Delivery       string   `json:"delivery"`
		SessionIDs     []string `json:"session_ids"`
		IdempotencyKey string   `json:"idempotency_key"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	requested, err := validateBroadcastInput(input.Body, input.Delivery, input.IdempotencyKey, input.SessionIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	key := newBroadcastKey(actor, input.IdempotencyKey, input.Body, input.Delivery, requested)
	// A key this human has already used answers its broadcast before anything else is read: a
	// retry of a send that landed owes nothing to the registry and must not fail because the
	// listener is down.
	if s.answerHeldBroadcastKey(r.Context(), w, key, requested) {
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

	created, events, err := s.writeBroadcast(r.Context(), actor, key, input.Body, input.Delivery, recipients)
	if errors.Is(err, errBroadcastKeyHeld) {
		// Two requests with one key reached the write together; the other committed first, and
		// this one's transaction rolled back - broadcast, messages, attempts and events with it.
		if !s.answerHeldBroadcastKey(r.Context(), w, key, requested) {
			s.writeHandlerError(w, fmt.Errorf("broadcast key %q was held and then not found", key.key))
		}
		return
	}
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
func validateBroadcastInput(body, delivery, key string, sessionIDs []string) ([]string, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errorf(http.StatusBadRequest, "INVALID_MESSAGE", "message body is required")
	}
	if length := len16(body); length > maxMessageBody16 {
		return nil, capExceededError("body", length, maxMessageBody16)
	}
	if !validDelivery(delivery) {
		return nil, errorf(http.StatusBadRequest, "BROADCAST_INPUT", "delivery must be one of btw, aside, steer")
	}
	// The rule comes first, since it is the answer for every keyless caller; the second sentence is
	// for a page loaded before the field shipped, whose failed row is the only copy of the message
	// and which a reload drops, so it says what to do, in order.
	if key == "" {
		return nil, errorf(http.StatusBadRequest, "BROADCAST_INPUT",
			"idempotency_key is required, one per send. On a page loaded before it was required: press Restore draft and copy the message, then reload the page and send it again")
	}
	if !broadcastKeyPattern.MatchString(key) {
		return nil, errorf(http.StatusBadRequest, "BROADCAST_INPUT",
			"idempotency_key may use only letters, digits, '.', '_', ':' and '-'")
	}
	if len(key) > maxBroadcastIdempotencyKey {
		return nil, capExceededError("idempotency_key", len(key), maxBroadcastIdempotencyKey)
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

// newBroadcastKey is what a send's key is judged by: the sender's canonical login, the key, and a
// fingerprint of what the send asked for - the body, the mode and the requested sessions in order,
// after validateBroadcastInput trimmed and de-duplicated them - so a repeat of a key can be told
// from a reuse of it.
func newBroadcastKey(actor model.Actor, key, body, delivery string, requested []string) broadcastKey {
	encoded, err := json.Marshal(struct {
		Body       string   `json:"body"`
		Delivery   string   `json:"delivery"`
		SessionIDs []string `json:"session_ids"`
	}{body, delivery, requested})
	if err != nil {
		// A struct of strings always encodes.
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return broadcastKey{login: canonicalLogin(actor.ID), key: key, digest: hex.EncodeToString(sum[:])}
}

// answerHeldBroadcastKey answers a send whose key this human has already used, and reports
// whether it did: the broadcast that key made, as GET /api/v1/broadcasts/{id} reads it, when the
// request is the same one (200); 409 BROADCAST_KEY_REUSED, naming the broadcast that used the
// key and saying this request was not sent, when the key was reused for a different message,
// mode or selection. It writes nothing, and reports false, when the key is not held. The replay
// reads the broadcast as it stands: a recipient whose attempt was stranded pending by a process
// death before delivery comes back pending, for the broadcast view's same-mode retry to resume
// (deliverBroadcast's comment), so a 200 says the broadcast exists, not that delivery is under
// way.
func (s *server) answerHeldBroadcastKey(ctx context.Context, w http.ResponseWriter, key broadcastKey, requested []string) bool {
	var broadcastID, digest string
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select broadcast_id::text, request_digest from broadcast_idempotency_keys
		where login = $1 and idempotency_key = $2
	`, key.login, key.key).Scan(&broadcastID, &digest)
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
			"code":         "BROADCAST_KEY_REUSED",
			"error":        "idempotency_key was already used by broadcast " + broadcastID + " for a different message, mode or recipients; this request was not sent: a changed send needs a new idempotency_key",
			"broadcast_id": broadcastID,
		})
		return true
	}
	read, err := s.readBroadcast(ctx, broadcastID)
	if err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	WriteJSON(w, http.StatusOK, broadcastCreated{
		broadcastRead: read, Excluded: replayedExclusions(requested, read.Recipients),
	})
	return true
}

// replayedExclusions names the requested sessions the original send did not reach. The request is
// the original's (its digest matched), so requested minus recipients is exactly the set it
// excluded; the reasons were answered once, to the original request, and are not stored.
func replayedExclusions(requested []string, recipients []broadcastRecipient) []broadcastExclusion {
	sent := make(map[string]struct{}, len(recipients))
	for _, recipient := range recipients {
		sent[recipient.SessionID] = struct{}{}
	}
	excluded := make([]broadcastExclusion, 0)
	for _, sessionID := range requested {
		if _, ok := sent[sessionID]; !ok {
			excluded = append(excluded, broadcastExclusion{SessionID: sessionID, Reason: "excluded by the original send"})
		}
	}
	return excluded
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

// writeBroadcast commits the broadcast, its key, and one targeted message per recipient in a
// single transaction, so a send is never half-written, and returns the events its messages owe
// the stream. Nothing is delivered here: a listener call never runs with a transaction open
// (envoy_resolve.go). A key another request of this human's committed first is
// errBroadcastKeyHeld, with nothing written.
func (s *server) writeBroadcast(
	ctx context.Context,
	actor model.Actor,
	key broadcastKey,
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
	// The key row is the send's claim on its key: a concurrent request carrying the same key waits
	// here on this transaction's uncommitted row and, once this commits, is refused it. Its
	// transaction then rolls back with the deferred Rollback as writeBroadcast returns, which
	// also releases the connection before the caller reads the winner's broadcast: the pool
	// refuses a second connection to a holder (store.ErrNestedAcquire).
	if _, err := tx.Exec(ctx, `
		insert into broadcast_idempotency_keys (login, idempotency_key, broadcast_id, request_digest)
		values ($1, $2, $3, $4)
	`, key.login, key.key, sent.ID, key.digest); err != nil {
		if isUniqueViolation(err) {
			return broadcastRead{}, nil, errBroadcastKeyHeld
		}
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
		// resumes it instead of opening a second attempt beside it. The author asked for it, and
		// the resume keeps that.
		attempt, err := scanMessageDelivery(tx.QueryRow(ctx, `
			insert into message_deliveries (message_id, attempt, delivery, session_id, state, requested_by)
			values ($1, 1, $2, $3, 'pending', $4)
			returning `+messageDeliveryColumns, message.ID, delivery, recipient.sessionID, author))
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
	read, err := s.readBroadcast(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, "BROADCAST_NOT_FOUND", http.StatusNotFound, "broadcast not found")
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, read)
}

// readBroadcast reads one broadcast with every recipient's state, as GET /api/v1/broadcasts/{id}
// answers it; an unknown id is pgx.ErrNoRows.
func (s *server) readBroadcast(ctx context.Context, id string) (broadcastRead, error) {
	var read broadcastRead
	var author []byte
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select id::text, author, body, delivery, created_at from broadcasts where id = $1
	`, id).Scan(&read.ID, &author, &read.Body, &read.Delivery, &read.CreatedAt); err != nil {
		return broadcastRead{}, err
	}
	if err := json.Unmarshal(author, &read.Author); err != nil {
		return broadcastRead{}, err
	}
	recipients, err := s.loadBroadcastRecipients(ctx, read.ID)
	if err != nil {
		return broadcastRead{}, err
	}
	read.Recipients = recipients
	return read, nil
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
