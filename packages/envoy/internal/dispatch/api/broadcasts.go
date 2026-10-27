package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

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
// request has already answered. Each worker takes a tracking context of its own
// (store.ForConcurrentRead): the pool's one-connection guard is per context, so workers sharing
// one would both trip it and race on its flag. Four keeps a large send's wall time down without
// holding more than four of the shared pool's connections for one broadcast.
const broadcastDeliveryWorkers = 4

// broadcastSend is one recipient's message and the attempt the sender owes it: the attempt the
// write opened for a new send, and the attempt a repeat found owed (owedMessageAttempts).
// Every send is bound to an attempt, so two senders that both find a recipient owed deliver one
// frame between them rather than one each.
type broadcastSend struct {
	message model.Message
	attempt int
}

// broadcastResolveConcurrency is how many per-session listener reads a send has in flight
// while it resolves its recipients, and broadcastResolveTimeout is how long the whole
// resolution may take. See resolveBroadcastSessions for why a send needs both.
const broadcastResolveConcurrency = 8

const broadcastResolveTimeout = 5 * time.Second

// broadcastResolveAllowance is the budget a send's recipient resolution runs under:
// broadcastResolveTimeout on every server the deployment builds, since register sets no
// broadcastResolveBudget, and whatever a test set otherwise.
func (s *server) broadcastResolveAllowance() time.Duration {
	return cmp.Or(s.broadcastResolveBudget, broadcastResolveTimeout)
}

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

// POST /api/v1/broadcasts. A send under an Idempotency-Key commits its broadcast as the key's
// recovery point, and a repeat is answered from it (replayBroadcast) before the registry is read:
// a retry of a send that landed owes nothing to the registry and must not fail because the
// listener is down.
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
	// Each selected session is judged against the session store by its own id, one read each.
	// The listener's live list is one process's in-memory cache of the registry, so a session
	// that is working while that cache refills is simply absent from it, and judging a whole
	// selection against that one view excluded such a session with "no live session" - a
	// message the human composed for it, dropped because Dispatch asked the wrong question.
	// A per-session read asks the store the question the exclusion claims to answer, and the
	// selection is the sender's, not a set the list has to be scanned for, so there is nothing
	// a single list read buys that is worth that.
	sessions, err := s.resolveBroadcastSessions(r.Context(), requested)
	if err != nil {
		// Nothing is written when a read did not answer, or the budget ended before every
		// selected session was read, not even the recipients whose reads answered:
		// a broadcast commits under its idempotency key, whose recovery point keeps the send's
		// exclusions and answers every repeat of that key with them, so a transient refusal
		// turned into a recipient's absence would be frozen into that key forever - the sender's
		// retry would be told the session was left out on purpose, and no retry of theirs could
		// ever reach it.
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

	created, events, err := s.writeBroadcast(r.Context(), actor, input.Body, input.Delivery, recipients, excluded)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for _, event := range events {
		s.publish(event)
	}
	// The send answers as soon as the messages are committed, and delivery runs behind it: a
	// fan-out to many sessions, each bounded by the listener client's receipt timeout, is too
	// long to hold an answer for. A single send to one session answers once its own attempt has
	// settled instead (createStoredMessage), which is what keeps a person's sends to one session
	// in the order they pressed them; a broadcast is not ordered against a later single send to
	// one of its recipients. Every recipient's attempt, receipt and reply reaches an open
	// broadcast view over the live event feed, so nothing is lost by answering first.
	//
	// The delivery runs on a context detached from the request, and the server's background
	// wait holds shutdown for it (waitBackground): SIGTERM cancels Deps.Lifetime before
	// http.Server.Shutdown drains, so a delivery bound to the lifetime was cut off by a deploy
	// with recipients it had not reached. A deploy that kills the process before they all settle
	// leaves owed recipients the delivery sweep resumes (resumeOwedDeliveries), and a repeat under
	// the key resumes them too (replayBroadcast).
	sends := make([]broadcastSend, 0, len(created.Recipients))
	for _, recipient := range created.Recipients {
		// The write opened each recipient's first attempt, unclaimed, so every worker is bound
		// to that attempt rather than opening one of its own.
		sends = append(sends, broadcastSend{message: recipient.Message, attempt: 1})
	}
	sending := context.WithoutCancel(r.Context())
	s.detached.goDetached(func() { s.deliverBroadcast(sending, sends, input.Delivery, actor) })
	WriteJSON(w, http.StatusCreated, broadcastCreated{broadcastRead: created, Excluded: excluded})
}

// validateBroadcastInput checks one send's shape and returns its recipients, in the order the
// caller listed them and with the duplicates a multi-select can produce removed: a session
// named twice is one recipient, not two messages.
func validateBroadcastInput(body, delivery string, sessionIDs []string) ([]string, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errorf(http.StatusBadRequest, "INVALID_MESSAGE", "message body is required")
	}
	if length := len16(body); length > contracts.DispatchBodyMax {
		return nil, capExceededError("body", length, contracts.DispatchBodyMax)
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

// broadcastRecovery is the recovery point a keyed broadcast commits with its broadcast
// (bindRequestKeyRecovery): the broadcast, and the sessions the send left out and why, which a
// repeat answers as the send did.
type broadcastRecovery struct {
	BroadcastID string               `json:"broadcast_id"`
	Excluded    []broadcastExclusion `json:"excluded"`
}

// replayBroadcast answers a repeat of a keyed broadcast whose broadcast committed: the broadcast as
// GET /api/v1/broadcasts/{id} reads it, every recipient's attempts as they stand, with the send's
// own exclusions, under the send's own 201 - answered at once, as the send was, while
// sendOwedBroadcastRecipients puts every recipient it still owes on its way behind the answer.
func (s *server) replayBroadcast(w http.ResponseWriter, r *http.Request, point broadcastRecovery, _ []byte) {
	ctx := r.Context()
	read, err := s.readBroadcast(ctx, point.BroadcastID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.sendOwedBroadcastRecipients(ctx, read); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	excluded := point.Excluded
	if excluded == nil {
		excluded = []broadcastExclusion{}
	}
	WriteJSON(w, http.StatusCreated, broadcastCreated{broadcastRead: read, Excluded: excluded})
}

// resolveBroadcastSessions reads each selected session's own record from the session store,
// through the listener's per-session route, and returns the ones it holds a record for; an id
// the store has no record of is simply missing from the map, which is the one absence a send
// is allowed to turn into an exclusion. It answers only once it has decided every requested
// id - found its record, or had the store say it holds none - and any other outcome ends the
// whole resolution with an error, because a read that did not answer, or was never made, says
// nothing about whether the session is working.
//
// The reads run broadcastResolveConcurrency at a time under one budget taken from the
// request's context (broadcastResolveAllowance), and those two together are what keep a send's
// resolution short: one request may name contracts.MaxBroadcastRecipients sessions, so a
// hundred reads that each wait out a five-second listener timeout, run one after another,
// would hold the request for the better part of ten minutes. Bounded at eight in flight and
// cut off at five seconds altogether, the resolution costs a send five seconds at worst,
// whatever the listener does.
func (s *server) resolveBroadcastSessions(
	ctx context.Context, requested []string,
) (map[string]dispatchenvoy.Session, error) {
	budget := s.broadcastResolveAllowance()
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var mu sync.Mutex
	live := make(map[string]dispatchenvoy.Session, len(requested))
	decided := 0
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(broadcastResolveConcurrency)
	for _, sessionID := range requested {
		if groupCtx.Err() != nil {
			break
		}
		group.Go(func() error {
			session, err := s.deps.Envoy.Session(groupCtx, sessionID)
			if err != nil && !errors.Is(err, dispatchenvoy.ErrNotFound) {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			decided++
			if err == nil {
				live[sessionID] = session
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	// Wait reports only the errors the reads returned, never the budget's: a budget that ends
	// after every launched read has answered, and before the loop launches the next, leaves
	// the rest of the selection unread with no error to show for it. Answering then would let
	// resolveBroadcastTargets exclude each unread session as "no live session", and a send
	// with one recipient left would commit that exclusion under its idempotency key, whose
	// recovery point answers every retry of that key with it - a session dropped because time
	// ran out, recorded as left out on purpose. The count
	// is the invariant the caller depends on, that every selected session was judged against
	// its own record, so it is checked rather than the budget's context: it also refuses an id
	// a future change drops for some other reason, which a context test would let through.
	// The reason names the budget's own context, not groupCtx, which Wait cancels even when
	// every read succeeded.
	if decided != len(requested) {
		return nil, fmt.Errorf(
			"%w: decided %d of %d selected sessions within the %s resolution budget (%v)",
			dispatchenvoy.ErrUnavailable, decided, len(requested), budget, ctx.Err(),
		)
	}
	return live, nil
}

// resolveBroadcastTargets judges each selected session against the record the session store
// holds for it: it is a recipient when the store holds its session and that session advertises
// the chosen mode, and otherwise carries the reason it was left out.
func resolveBroadcastTargets(
	requested []string, delivery string, live map[string]dispatchenvoy.Session,
) []broadcastTarget {
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

// writeBroadcast commits the broadcast, its keyed request's recovery point, and one targeted
// message per recipient in a single transaction, so a send is never half-written, and returns the
// events its messages owe the stream. Nothing is delivered here: a listener call never runs with a
// transaction open (envoy_resolve.go). excluded is what the send left out, which the recovery point
// keeps for a repeat's answer.
func (s *server) writeBroadcast(
	ctx context.Context,
	actor model.Actor,
	body, delivery string,
	recipients []broadcastTarget,
	excluded []broadcastExclusion,
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
	// The recovery point right after its broadcast: a repeat that took the key over meanwhile
	// refuses it here, and this transaction rolls back before it writes a message.
	if err := bindRequestKeyRecovery(ctx, tx, broadcastRecovery{BroadcastID: sent.ID, Excluded: excluded}); err != nil {
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

// deliverBroadcast sends each recipient's owed attempt, broadcastDeliveryWorkers at a time, and
// returns once every one of them has settled. It runs on ctx, which its callers detach from the
// request (context.WithoutCancel): a browser that goes away, and a deploy's signal, must not cut
// a recipient off mid-flight. One recipient's failure never stops another's: a send the listener
// refused settles that recipient's attempt as failed, a recipient whose attempt was taken over
// from the agent card is skipped, and a delivery this cannot record at all leaves the attempt
// pending and unclaimed, which the broadcast view shows as sending and which the delivery sweep
// (resumeOwedDeliveries), or a repeat under the send's key (sendOwedBroadcastRecipients), resumes
// once the claim lease has passed.
func (s *server) deliverBroadcast(ctx context.Context, sends []broadcastSend, delivery string, actor model.Actor) {
	workers := min(broadcastDeliveryWorkers, len(sends))
	pending := make(chan broadcastSend)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for send := range pending {
				// A mark of this worker's own: workers sharing one would refuse each other a
				// connection (store.ErrNestedAcquire), and the caller holds none by now - the
				// create's transaction committed before it detached this delivery, and a
				// repeat's reads are closed before it resumes anyone.
				sending, err := store.ForConcurrentRead(ctx)
				if err != nil {
					slog.Error("dispatch: broadcast delivery",
						"message", send.message.ID, "target", messageTarget(send.message.Target), "error", err)
					continue
				}
				// Bound to the attempt this send owes. A human who changes the mode from the
				// agent card while this worker is queued settles that attempt and sends their
				// own; the worker must then send nothing, because an attempt of its own would
				// put a second frame on the session's subject. Of two senders that both found
				// the attempt owed, the one that claims it sends and the other stops here.
				_, err = s.deliverMessageResuming(sending, send.message, delivery, nil, actor, nil, send.attempt)
				if errors.Is(err, errAttemptNotPending) {
					continue
				}
				if err != nil {
					slog.Error("dispatch: broadcast delivery",
						"message", send.message.ID, "target", messageTarget(send.message.Target), "error", err)
				}
			}
		}()
	}
	for _, send := range sends {
		pending <- send
	}
	close(pending)
	wait.Wait()
}

// sendOwedBroadcastRecipients puts every recipient the broadcast still owes on its way
// (owedMessageAttempts), as the original send did, behind the repeat's answer: the repeat is
// answered as soon as it has read the broadcast, as the original was answered as soon as it
// committed. The process that answered the original send may have been stopped, or have died,
// with recipients it had not reached; the delivery sweep resumes those too (resumeOwedDeliveries),
// and a repeat only puts them on their way sooner. Only the read of what is owed can fail here:
// a listener the delivery cannot reach is recorded on the attempt.
func (s *server) sendOwedBroadcastRecipients(ctx context.Context, read broadcastRead) error {
	messageIDs := make([]string, 0, len(read.Recipients))
	for _, recipient := range read.Recipients {
		messageIDs = append(messageIDs, recipient.Message.ID)
	}
	owed, err := s.owedMessageAttempts(ctx, messageIDs)
	if err != nil || len(owed) == 0 {
		return err
	}
	sends := make([]broadcastSend, 0, len(owed))
	for _, recipient := range read.Recipients {
		if attempt, ok := owed[recipient.Message.ID]; ok {
			sends = append(sends, broadcastSend{message: recipient.Message, attempt: attempt})
		}
	}
	// Detached from the request and counted in the server's background wait, as the original
	// send's delivery is; the listener client's receipt timeout bounds each recipient's send.
	sending := context.WithoutCancel(ctx)
	s.detached.goDetached(func() { s.deliverBroadcast(sending, sends, read.Delivery, read.Author) })
	return nil
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
