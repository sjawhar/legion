package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const maxSSEReplay = 1000

const eventSelect = `select e.id, e.issue_key, e.artifact_id::text, coalesce(e.project_key, i.project_key, ar.project_key, ''), e.seq, e.type, e.actor, e.notify, e.created_at, e.payload
	from events e
	left join issues i on i.key = e.issue_key
	left join artifacts ar on ar.id = e.artifact_id`

func (s *server) listIssueEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	options, err := parseEventListOptions(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events, err := s.readEvents(r.Context(), issueOwner(r.PathValue("key")), options)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, events)
}

func parseEventListOptions(r *http.Request) (eventListOptions, error) {
	query := r.URL.Query()
	hasBefore := query.Has("before")
	if hasBefore && strings.TrimSpace(query.Get("before")) == "" {
		return eventListOptions{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "before must be a non-negative integer")
	}
	after, err := parseNonNegativeInt(query.Get("after"), "after")
	if err != nil {
		return eventListOptions{}, err
	}
	before, err := parseNonNegativeInt(query.Get("before"), "before")
	if err != nil {
		return eventListOptions{}, err
	}
	ids, err := parseEventIDs(query.Get("ids"))
	if err != nil {
		return eventListOptions{}, err
	}
	descending, err := parseEventOrder(query.Get("order"))
	if err != nil {
		return eventListOptions{}, err
	}
	limit, err := parseEventLimit(query.Get("limit"))
	if err != nil {
		return eventListOptions{}, err
	}
	if query.Has("after") && (hasBefore || descending) {
		return eventListOptions{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "after cannot be combined with before or order=desc")
	}
	return eventListOptions{
		after:      after,
		before:     before,
		hasBefore:  hasBefore,
		descending: descending || hasBefore,
		ids:        ids,
		limit:      limit,
	}, nil
}

// eventReplayHeader is the response header GET /api/v1/events answers "complete" on a resuming
// connection (since=, or Last-Event-ID) whose since is at or below the event log's newest id when
// it connects, which is the connection that opens early (streamEvents' opensEarly): the stream
// replays every event after since before it goes live, so the client has missed nothing. A cold
// connect and a since above the newest id (a replaced log) open only once their subscription is
// in place, start at the relay's published cursor and carry no header. A connection that opened
// early and then finds the log reset while it subscribes ends rather than replay a log that no
// longer exists, so the client reconnects. The browser reads it same-origin, so no CORS exposure
// is needed.
const eventReplayHeader = "X-Dispatch-Event-Replay"

func (s *server) streamEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	lastEventID := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	sinceProvided := lastEventID != "" || r.URL.Query().Has("since")
	sinceRaw := lastEventID
	if sinceRaw == "" {
		sinceRaw = r.URL.Query().Get("since")
	}
	var since int64
	if sinceProvided {
		var err error
		since, err = parseNonNegativeInt(sinceRaw, "since")
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, "STREAM_UNSUPPORTED", http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// The live feed is the process's event stream relay (event_stream_relay.go), which publishes
	// every committed event it reads back out of Postgres — this process's and the other
	// envoy-dispatch process's alike — in id order, and only up to the safe horizon of the
	// server's events.Horizon, at or below which every event is final. Event ids are not commit
	// order (writes to different owners take no lock in common, events.Broker.Append), so a
	// reader that went by the log's newest id could step over a lower id still open; the relay's
	// cursor never does.
	//
	// subscribe answers the subscription together with published, the id the relay had published
	// through when the subscription began, both read under the one lock the relay also holds
	// across each page's publish and its cursor's move over that page. So every event at or below
	// published is final and was published before this subscription existed, and every event the
	// relay publishes into it is above published, each once, in strictly increasing id order.
	// published is where this connection hands over from the log to the live feed.
	//
	// That handover is also why the start is resolved here and not by the client: for a cold
	// client (no since= and no Last-Event-ID) this handler IS its only request, and a client that
	// fetched a head through a separate request and then opened this connection with
	// since=<that head> would lose an event committing between the two, or a lower id still open
	// when it read the head, excluded from catch-up (id <= since) and dropped by the live loop.
	//
	// subscribe can wait a long time. The first subscription on a process starts the relay, whose
	// first cursor waits for a horizon proven after the call (awaitEventCursor), and so for every
	// transaction holding a transaction id anywhere on the Postgres instance, another database's
	// included; every other connection waits for that start. So the subscription is taken on a
	// goroutine (awaitEventSubscription), and until it arrives the handler still answers the
	// request's end, the server's shutdown and its heartbeat, on which it resolves the caller
	// again. A subscription that fails ends the stream and sends the client to its reconnect.
	//
	// Whether the stream opens (its headers and ": open") before the subscription is decided once,
	// before the handler subscribes (opensEarly). It does only for a connection that resumes from
	// an id of the log as it stands: its start point is the since it brought, which needs nothing
	// from the subscription, so it opens at once and heartbeats while the relay starts. A cold
	// connection, or one whose since the clamp below replaces, starts at published instead, and the
	// browser refreshes every query when a reconnect opens (web/src/api/sse.ts): opened before
	// published is fixed, that refresh could read before an event that then lands at or below
	// published, sent neither by the refresh nor by the stream. So such a connection opens once
	// its subscription has arrived, which is at once whenever the relay is already running.
	//
	// Until then such a connection sends no bytes. The browser aborts an attempt that receives
	// nothing for 45 s (WATCHDOG_MS, web/src/api/sse.ts:80-86, armed at :757-760) and reconnects
	// after a backoff that grows to 30 s (reconnectDelayMs, web/src/api/live.ts:39-45), cold again
	// when it was cold, since its lastEventId stays 0 until an event arrives (sse.ts:872), and
	// with the same since when it was clamped. Each abort ends its request's context, and with it
	// the relay start when that request was the one running it (eventStreamRelay.start), so the
	// next connection starts a new Await, which again waits for every transaction open when it
	// began. So on a newly started process, while a transaction anywhere on the instance stays
	// open for more than about 45 s, every first page load cycles through "Reconnecting…" until
	// that transaction ends. No event is lost: the connection that finally subscribes starts at
	// published. An operator who sees such a reconnect storm at boot finds its cause in the
	// horizon's stall warning (events.Horizon), which names the oldest transaction holding the
	// horizon back.
	//
	// The event log can be reset under a reader (`truncate events restart identity`, the browser
	// suite's e2e/seed.ts): the ids restart at 1 and the relay, which notices, ends every open
	// subscription and so sends the browser through its reconnect. The browser only ever raises
	// its lastEventId and never lowers it (web/src/api/sse.ts), so it resumes with a since above
	// the new log's newest id. Unclamped, the live loop below would then drop every event of the
	// new log for ever, on a connection that stays open and heartbeats: a reader silent for good,
	// with nothing logged and nothing failed. A since above the newest id can only be an id of a
	// log this one replaced, so it starts at published too. The newest id is read once here, to
	// decide opensEarly, and for a connection that opened early again once the subscription has
	// arrived: a reset while the subscription waited (the refill's open writes hold back the
	// relay's new cursor) leaves a since this first read did not see as stale. That connection
	// cannot start at published then, because the browser ran its refresh when the headers
	// arrived, and an event at or below published that committed after that refresh would reach
	// it neither way. So it ends instead, and the browser reconnects with a since above the new
	// log's newest id, which the new connection's first read sends down the path that opens once
	// the subscription has arrived.
	opensEarly := false
	if sinceProvided {
		head, err := s.eventLogHead(r.Context())
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		opensEarly = since <= head
	}
	// The subscription's reads run beside this handler's heartbeat, which resolves the caller on
	// the shared pool too, so they take a hold mark of their own (store.ForConcurrentRead). Its
	// context ends with the handler, so a subscribe still waiting then returns, and one that
	// arrives after the handler has gone is cancelled (awaitEventSubscription).
	reads, err := store.ForConcurrentRead(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	opened := false
	if opensEarly {
		if openEventStream(w, flusher, true) != nil {
			return
		}
		opened = true
	}
	subscribing, stopSubscribing := context.WithCancel(reads)
	defer stopSubscribing()
	// ready delivers a subscription once and is then set to nil, which no select receives from;
	// subscription is nil, and so never ready, until then.
	ready := s.awaitEventSubscription(subscribing)
	var subscription <-chan model.Event
	var floor int64
	// replaced is a subscription a cold connection took from a relay whose cursor still names a
	// position in a log that was reset: its published stands above the log's newest id, which in a
	// log whose ids only grow it never does. The relay ends every subscription once its reset check
	// runs (resetCursor), and the connection, which has sent nothing yet, then subscribes again on
	// the cursor it moved to. Opened at the stale published instead, it would drop the new log's
	// ids up to it, and then end at the relay's reset, which sends the browser through a reconnect
	// and the whole refresh a reconnect owes, for a connection the page had only just opened.
	//
	// The relay's new cursor is no start point for that connection: it is taken under a horizon
	// proven after the reset, so an event of the new log that commits after the request, while
	// that proof is awaited, can stand at or below it. So the connection starts at replacedFrom,
	// read when it found the relay on the replaced log: the id before the first one missing from
	// the new log, whose ids restart at 1, which is every event committed then and none still
	// open. It is replayed from there to the relay's new cursor, and so may be sent events of the
	// new log that committed just before its request.
	var replaced <-chan model.Event
	replacedFrom := int64(-1)

	heartbeat := time.NewTicker(s.deps.StreamHeartbeat)
	defer heartbeat.Stop()
	shutdown := s.lifetime().Done()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-shutdown:
			// The server is shutting down, and a stream never goes idle on its own, so
			// http.Server.Shutdown would wait its whole budget for it. Ending it sends the client
			// to its reconnect, which resumes from the last id it saw on whichever server is up.
			return
		case subscribed := <-ready:
			ready = nil
			if subscribed.err != nil {
				if !opened {
					s.writeHandlerError(w, subscribed.err)
				}
				return
			}
			// Deferred inside the loop: ready delivers once per subscription, and a cold
			// connection takes another only after a reset ended the one before (replaced).
			defer subscribed.cancel()
			published := subscribed.published
			// The log's newest id as it stands now, after the subscription began, which costs
			// each connection one more round trip. A published above it is a relay whose reset
			// check has not yet run.
			head, err := s.eventLogHead(r.Context())
			if err != nil {
				if !opened {
					s.writeHandlerError(w, err)
				}
				return
			}
			relayOnReplacedLog := published > head
			if opensEarly {
				// A since above the head is a reset this connection's early open already let the
				// browser refresh before, and a relay on a replaced log would drop the new log's
				// ids up to its published, so either way the stream ends rather than go on.
				if since > head || relayOnReplacedLog {
					return
				}
			} else {
				if relayOnReplacedLog {
					if replacedFrom < 0 {
						if replacedFrom, err = s.eventLogUnbrokenThrough(r.Context()); err != nil {
							s.writeHandlerError(w, err)
							return
						}
					}
					replaced = subscribed.events
					signal(s.eventRelay.wake)
					continue
				}
				// A cold client asked for no backlog, and a clamped one's since is of a log this
				// one replaced: both start at published, the handover this connection holds, or
				// at replacedFrom when this connection first found the relay on a replaced log.
				since = published
				if replacedFrom >= 0 {
					since = min(replacedFrom, published)
				}
				if openEventStream(w, flusher, false) != nil {
					return
				}
				opened = true
			}
			if s.replayEvents(r.Context(), w, flusher, since, published) != nil {
				return
			}
			// The live feed sends only events above both since and published. The relay
			// publishes into this subscription only events above published, each once and in
			// strictly increasing id order, so catch-up's (since, published] and the feed's ids
			// above max(since, published) are together every event after since, each sent
			// exactly once and in id order. A since above published belongs to a client that has
			// already seen events this process's relay has not yet published (read from the
			// other process, or before a reconnect): the relay's ids at or below it are events
			// that client has, and the comparison drops them.
			//
			// That argument covers a log whose ids only ever grow. A reset reuses them, and the
			// clamp above is what extends the comparison across one: a since above the newest id
			// is replaced by published, or ends a stream that opened early, so an id the new log
			// hands out is not dropped as one the old log already showed this reader. What the
			// clamp cannot do is tell this reader apart from the events of the log it was
			// reading; the relay ends the subscription at a reset instead, and the reader comes
			// back on a new connection, which clamps its since against the log as it then stands.
			subscription, floor = subscribed.events, max(since, published)
		case event, open := <-subscription:
			if !open {
				return
			}
			if event.ID <= floor {
				continue
			}
			if err := writeSSEEvent(w, event); err != nil {
				return
			}
			flusher.Flush()
		case _, open := <-replaced:
			// Whatever the relay publishes before its reset check is a position in the old log,
			// so it is dropped; the close is its reset ending this subscription.
			if open {
				continue
			}
			replaced = nil
			ready = s.awaitEventSubscription(subscribing)
		case <-heartbeat.C:
			// The caller is resolved again on every beat, so a stream outlives its session (a
			// logout, a membership the sign-in issuer no longer confirms) by one beat at most,
			// and a connection that has not opened yet is refused rather than opened later.
			if _, _, err := s.optionalActor(heartbeatRequest(r)); err != nil {
				if !opened {
					s.writeHandlerError(w, err)
				}
				return
			}
			if !opened {
				continue
			}
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// openEventStream writes the event stream's headers and one comment line with them, so the stream
// opens in every browser now. Firefox (the browser suite's, e2e/keyboard-session-pickers.e2e.ts)
// resolved its fetch of this stream only once a body byte arrived, and a connection with no
// backlog and no new event sends none until the first heartbeat: until then the page held its
// "Reconnecting…" state and skipped the refresh a reconnect owes every query, so what committed
// while it was away stayed unshown. replayComplete adds eventReplayHeader: the caller passes it
// for a connection that opens early, whose since is at or below the log's newest id and which is
// replayed every event after it. An error is a client already gone.
func openEventStream(w http.ResponseWriter, flusher http.Flusher, replayComplete bool) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if replayComplete {
		w.Header().Set(eventReplayHeader, "complete")
	}
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, ": open\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// replayEvents is a stream's catch-up: it writes (since, published] from the log. Every event in
// it is final, since published is at or below the safe horizon, so a page read now holds the rows
// any later read would, and moving the cursor to a page's last row skips nothing. It pages
// through the whole range on the caller's one subscription, never ending the stream partway:
// whatever the relay publishes meanwhile waits in the subscription (or overflows it, which ends
// the stream and sends the client to its reconnect, Stream.Publish). A since at or above
// published needs no catch-up. An error ends the stream.
func (s *server) replayEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, since, published int64) error {
	cursor := since
	for cursor < published {
		replay, err := s.readEventsAfterID(ctx, cursor, published)
		if err != nil {
			return err
		}
		for _, event := range replay {
			if err := writeSSEEvent(w, event); err != nil {
				return err
			}
			cursor = event.ID
		}
		flusher.Flush()
		if len(replay) < maxSSEReplay {
			break
		}
	}
	return nil
}

// eventSubscription is what subscribe answered, delivered by awaitEventSubscription: the live
// feed, the cancel that ends it, the relay's published cursor when it began, or the error that
// left the connection without one.
type eventSubscription struct {
	events    <-chan model.Event
	cancel    func()
	published int64
	err       error
}

// awaitEventSubscription subscribes to the relay on a goroutine of its own and delivers the
// result on the returned channel, so a handler can wait for it beside its heartbeat, its
// request's end and the server's shutdown. ctx bounds the subscribe and is the receiver's to end
// once it stops waiting: a subscribe still running returns with ctx's error, and a subscription
// nobody receives is cancelled, so no stream is left subscribed with no reader.
func (s *server) awaitEventSubscription(ctx context.Context) <-chan eventSubscription {
	ready := make(chan eventSubscription)
	go func() {
		var subscribed eventSubscription
		subscribed.events, subscribed.cancel, subscribed.published, subscribed.err = s.eventRelay.subscribe(ctx)
		select {
		case ready <- subscribed:
		case <-ctx.Done():
			if subscribed.err == nil {
				subscribed.cancel()
			}
		}
	}()
	return ready
}

// eventLogUnbrokenThrough is the id before the first one missing from the event log counted up
// from 1, or its newest id when none is missing: every id at or below it is a committed event.
// It reads the whole log, so streamEvents asks it only of a log that was reset beneath the relay,
// whose ids restart at 1 (`truncate events restart identity`) and which is as short as the time
// since that reset.
func (s *server) eventLogUnbrokenThrough(ctx context.Context) (int64, error) {
	var through int64
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce(
			(select min(n) - 1 from (select id, row_number() over (order by id) as n from events) numbered where id <> n),
			(select max(id) from events),
			0)
	`).Scan(&through)
	return through, err
}

// eventLogHead is the event log's newest committed id, 0 for an empty log. It is no cursor: ids
// are not commit order, so a lower id can still be open below it. streamEvents only compares a
// client's since against it, to tell an id of this log from one of a log it replaced.
func (s *server) eventLogHead(ctx context.Context) (int64, error) {
	var head int64
	err := s.deps.Store.Pool.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&head)
	return head, err
}

// disconnectAllStreams closes every currently open SSE connection, forcing each
// client through its reconnect-from-lastId path. Test-only: mounted by Register
// only when Deps.TestHooksEnabled is set, so e2e suites can prove reconnect
// behavior without seeding thousands of events to trip the replay cap. It closes the relay's
// subscribers, which is what an open connection holds; the broker's are the outbox and the
// delivery hub, which no reconnect test is about.
func (s *server) disconnectAllStreams(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.eventRelay.stream.CloseAll()
	WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type eventListOptions struct {
	after      int64
	before     int64
	hasBefore  bool
	descending bool
	ids        []int64
	limit      int
}

func (s *server) readEvents(ctx context.Context, owner owner, options eventListOptions) ([]model.Event, error) {
	var predicate, ownerValue string
	switch {
	case owner.IssueKey != nil:
		predicate, ownerValue = "e.issue_key = $1", *owner.IssueKey
	case owner.ArtifactID != nil:
		predicate, ownerValue = "e.artifact_id = $1", *owner.ArtifactID
	default:
		return nil, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	if len(options.ids) > 0 {
		return s.readEventRows(ctx, eventSelect+`
			where `+predicate+` and e.id = any($2)
			order by e.seq asc
		`, ownerValue, options.ids)
	}
	if options.descending {
		if options.hasBefore {
			return s.readEventRows(ctx, eventSelect+`
				where `+predicate+` and e.seq < $2
				order by e.seq desc limit $3
			`, ownerValue, options.before, options.limit)
		}
		return s.readEventRows(ctx, eventSelect+`
			where `+predicate+`
			order by e.seq desc limit $2
		`, ownerValue, options.limit)
	}
	return s.readEventRows(ctx, eventSelect+`
		where `+predicate+` and e.seq > $2
		order by e.seq asc limit $3
	`, ownerValue, options.after, options.limit)
}

// readEventsAfterID reads one page of the global event log in id order: the events whose ids are
// in (after, through]. Ids are not commit order, so a reader that moves its cursor to a page's
// last row passes a through at or below the safe horizon (events.Horizon), where every event is
// final: no lower id the page skipped can commit later.
func (s *server) readEventsAfterID(ctx context.Context, after, through int64) ([]model.Event, error) {
	return s.readEventRows(ctx, eventSelect+`
		where e.id > $1 and e.id <= $2
		order by e.id asc limit $3
	`, after, through, maxSSEReplay)
}

func (s *server) readEventRows(ctx context.Context, query string, arguments ...any) ([]model.Event, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []model.Event{}
	for rows.Next() {
		var event model.Event
		var actor, payload []byte
		if err := rows.Scan(
			&event.ID,
			&event.IssueKey,
			&event.ArtifactID,
			&event.Project,
			&event.Seq,
			&event.Type,
			&actor,
			&event.Notify,
			&event.CreatedAt,
			&payload,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(actor, &event.Actor); err != nil {
			return nil, fmt.Errorf("decode event actor: %w", err)
		}
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachAskEventFields(ctx, events); err != nil {
		return nil, err
	}
	return events, nil
}

// decodedAskEventPayload is the ask payload that needs read-time fields added before it is written
// to an event response. Other event payloads remain their database JSON.
type decodedAskEventPayload struct {
	event   *model.Event
	payload map[string]any
}

// attachAskEventFields fills the ask fields an event payload cannot carry from the write that
// appended it: the id of the event that opened the ask, its inbound backlink count, and its anchor
// document. Ask payloads are decoded because those fields are added, and an ask.answered payload's
// selected lists are read as model.AskAnswer reads them (normalizeAnsweredSelected); every other
// payload passes through as the database returned it.
func (s *server) attachAskEventFields(ctx context.Context, events []model.Event) error {
	asks := []model.Ask{}
	payloads := []decodedAskEventPayload{}
	for index := range events {
		switch events[index].Type {
		case "ask.opened", "ask.anchor_refreshed", "ask.answered", "ask.resolved", "ask.edited", "ask.handed_back":
		default:
			continue
		}
		raw, ok := events[index].Payload.(json.RawMessage)
		if !ok {
			return fmt.Errorf("decode %s payload: expected raw JSON", events[index].Type)
		}
		payload := map[string]any{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode %s payload: %w", events[index].Type, err)
		}
		if events[index].Type == "ask.answered" {
			if err := normalizeAnsweredSelected(raw, payload); err != nil {
				return err
			}
		}
		askID, ok := payload["id"].(string)
		if !ok || askID == "" {
			return fmt.Errorf("decode %s payload: ask id missing", events[index].Type)
		}
		asks = append(asks, model.Ask{ID: askID})
		payloads = append(payloads, decodedAskEventPayload{event: &events[index], payload: payload})
	}
	askPointers := make([]*model.Ask, len(asks))
	for index := range asks {
		askPointers[index] = &asks[index]
	}
	if err := s.attachOpenedEventIDs(ctx, s.deps.Store.Pool, askPointers); err != nil {
		return err
	}
	if err := attachAskBacklinkCounts(ctx, s.deps.Store.Pool, askPointers); err != nil {
		return err
	}
	for index := range payloads {
		payloads[index].payload["opened_event_id"] = *asks[index].OpenedEventID
		payloads[index].payload[model.ReferencedByCountKey] = *asks[index].ReferencedByCount
	}
	if err := s.attachAskDocumentArtifacts(ctx, payloads); err != nil {
		return err
	}
	for _, payload := range payloads {
		encoded, err := json.Marshal(payload.payload)
		if err != nil {
			return fmt.Errorf("encode %s payload: %w", payload.event.Type, err)
		}
		payload.event.Payload = json.RawMessage(encoded)
	}
	return nil
}

// normalizeAnsweredSelected writes model.AskAnswer's reading of selected into an ask.answered
// payload's answer and previous_answer: an ask.answered event written before answers stored an
// empty list holds null there, and a client reads the list's length. Every other field of either
// answer stays as the payload holds it, and an answer the payload does not carry stays absent.
func normalizeAnsweredSelected(raw json.RawMessage, payload map[string]any) error {
	var answers struct {
		Answer         *model.AskAnswer `json:"answer"`
		PreviousAnswer *model.AskAnswer `json:"previous_answer"`
	}
	if err := json.Unmarshal(raw, &answers); err != nil {
		return fmt.Errorf("decode ask.answered answers: %w", err)
	}
	setSelected := func(key string, answer *model.AskAnswer) {
		if fields, ok := payload[key].(map[string]any); ok && answer != nil {
			fields["selected"] = answer.Selected
		}
	}
	setSelected("answer", answers.Answer)
	setSelected("previous_answer", answers.PreviousAnswer)
	return nil
}

// attachAskDocumentArtifacts hydrates anchor_artifact and approval_artifact on every ask event
// payload that names one, the same live lookup ask_rows.go's scanAskRow does for a read: an
// anchored ask's quoted document, or an approval request's document, both resolved by their
// current slug and primary flag rather than a value frozen at write time.
func (s *server) attachAskDocumentArtifacts(ctx context.Context, eventPayloads []decodedAskEventPayload) error {
	payloads := map[string][]decodedAskEventPayload{}
	for _, eventPayload := range eventPayloads {
		payload := eventPayload.payload
		_, hasAnchor := payload["anchor"].(map[string]any)
		_, hasApproval := payload["approval"].(map[string]any)
		if !hasAnchor && !hasApproval {
			if payload["anchor"] != nil {
				return fmt.Errorf("decode %s payload: anchor must be an object", eventPayload.event.Type)
			}
			continue
		}
		askID, ok := payload["id"].(string)
		if !ok || askID == "" {
			return fmt.Errorf("decode %s payload: ask id missing", eventPayload.event.Type)
		}
		payloads[askID] = append(payloads[askID], eventPayload)
	}
	if len(payloads) == 0 {
		return nil
	}
	askIDs := make([]string, 0, len(payloads))
	for askID := range payloads {
		askIDs = append(askIDs, askID)
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select a.id::text, aa.project_key, aa.slug, aa.name, aa.is_primary,
			pa.project_key, pa.slug, pa.name, pa.is_primary
		from asks a
		left join artifacts aa on aa.id = (a.anchor->>'artifact_id')::uuid
		left join artifacts pa on pa.id = (a.approval->>'artifact_id')::uuid
		where a.id::text = any($1)
	`, askIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	anchorArtifacts := make(map[string]model.AskAnchorArtifact, len(payloads))
	approvalArtifacts := make(map[string]model.AskAnchorArtifact, len(payloads))
	for rows.Next() {
		var askID string
		var anchor, approval nullableAskArtifactRef
		if err := rows.Scan(
			&askID,
			&anchor.project, &anchor.slug, &anchor.name, &anchor.primary,
			&approval.project, &approval.slug, &approval.name, &approval.primary,
		); err != nil {
			return err
		}
		if artifact := anchor.artifact(); artifact != nil {
			anchorArtifacts[askID] = *artifact
		}
		if artifact := approval.artifact(); artifact != nil {
			approvalArtifacts[askID] = *artifact
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for askID, asks := range payloads {
		anchorArtifact, anchorFound := anchorArtifacts[askID]
		approvalArtifact, approvalFound := approvalArtifacts[askID]
		for _, ask := range asks {
			delete(ask.payload, "anchor_artifact")
			delete(ask.payload, "approval_artifact")
			if anchorFound {
				ask.payload["anchor_artifact"] = anchorArtifact
			}
			if approvalFound {
				ask.payload["approval_artifact"] = approvalArtifact
			}
		}
	}
	return nil
}

func writeSSEEvent(w http.ResponseWriter, event model.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Type, data)
	return err
}

func parseNonNegativeInt(raw, field string) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "%s must be a non-negative integer", field)
	}
	return value, nil
}

func parseEventLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 200, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 200 {
		return 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "limit must be between 1 and 200")
	}
	return limit, nil
}

func parseEventOrder(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "", "asc":
		return false, nil
	case "desc":
		return true, nil
	default:
		return false, errorf(http.StatusBadRequest, "INVALID_QUERY", "order must be asc or desc")
	}
}

func parseEventIDs(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 50 {
		return nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "ids must include at most 50 values")
	}
	ids := make([]int64, len(parts))
	for index, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id < 1 {
			return nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "ids must be positive integers")
		}
		ids[index] = id
	}
	return ids, nil
}
