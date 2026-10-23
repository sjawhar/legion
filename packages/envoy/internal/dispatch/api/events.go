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
	// envoy-dispatch process's alike. Subscribing before this connection reads the log's head is
	// what leaves no gap: an event committing after the subscription exists lands in the
	// channel whatever its id, while one committing before it is at or below the head read next
	// and so is replayed by the catch-up below. The relay reads its cursor before it answers a
	// subscription, so it cannot already stand past that head either.
	//
	// That ordering is also why the head is read here and not by the client: for a cold client
	// (no since= and no Last-Event-ID) this handler IS its only request, and a client that
	// fetched its own head through a separate request and then opened this connection with
	// since=<that head> would lose an event committing between the two, excluded from catch-up
	// (id <= since) and missed by a subscription registered only afterwards.
	subscription, cancel, err := s.eventRelay.subscribe(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer cancel()
	// Every connection resolves the head, a resuming one too, which costs it one extra round
	// trip. The event log can be reset under a reader (`truncate events restart identity`, the
	// browser suite's e2e/seed.ts): the ids restart at 1 and the relay, which notices, ends every
	// open subscription and so sends the browser through its reconnect. The browser only ever
	// raises its lastEventId and never lowers it (web/src/api/sse.ts), so it resumes with a since
	// above the new log's head. Unclamped, the live loop below would then drop every event of the
	// new log for ever, on a connection that stays open and heartbeats: a reader silent for good,
	// with nothing logged and nothing failed.
	var head int64
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select coalesce(max(id), 0) from events`).Scan(&head); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A cold client asked for no backlog, and a since above the head can only be an id of a log
	// this one replaced; both start at the head this connection just read.
	if !sinceProvided || since > head {
		since = head
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	// Ids are assigned by nextval() when the insert statement runs, before commit,
	// so a lower id can commit after a higher one is already visible to a catch-up
	// query — a still-open transaction that grabbed an earlier id is exactly the
	// case the subscription above (taken before any catch-up query runs) exists to
	// cover. Page through the full backlog here without ever closing the stream: a
	// capped page that ended the stream and forced a client reconnect would leave
	// a gap between "read this page" and "reopen a new subscription" where a low
	// id could commit and be missed by both the next page's `id > cursor` query
	// (cursor has already moved past it) and the old subscription (already
	// cancelled). Keeping one subscription live across every page closes that gap.
	cursor := since
	sentDuringCatchup := make(map[int64]bool)
	for {
		replay, err := s.readEventsAfterID(r.Context(), cursor)
		if err != nil {
			return
		}
		for _, event := range replay {
			if err := writeSSEEvent(w, event); err != nil {
				return
			}
			sentDuringCatchup[event.ID] = true
			cursor = event.ID
		}
		flusher.Flush()
		if len(replay) < maxSSEReplay {
			break
		}
	}
	// The live loop below must never compare an event's id against the last id it sent: a lower
	// id committing after a higher one was already forwarded would be dropped forever, and that
	// is exactly what the relay re-emits after a redial. The one comparison it does make is
	// against since, which is fixed for this connection: the commit-order lock events.Broker.Append
	// holds until commit means no id at or below a committed max(id) is still uncommitted, so
	// nothing above since can hide below it later. A relay still catching up to a connection
	// opened after it re-emits ids at or below since — events this client has already seen, or
	// backlog a cold client asked not to get — and sentDuringCatchup cannot be asked about them,
	// since it holds only the ids catch-up sent, all above since.
	//
	// That argument covers a log whose ids only ever grow. A reset reuses them, and the clamp
	// above is what extends the comparison across one: since is held at or below the head this
	// connection resolved, so an id the new log hands out is never taken for an id the old log
	// already showed this reader. What the clamp cannot do is tell this reader apart from the
	// events of the log it was reading; the relay ends the subscription at a reset instead, and
	// the reader comes back on a new connection, which resolves the new log's head here.
	//
	// Dedup against that bounded set covers an id catch-up sent that is *also* still sitting in
	// the subscription channel (committed, and thus queryable, in the narrow window between the
	// subscription and a catch-up query, but not yet drained from it).

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
		case event, open := <-subscription:
			if !open {
				return
			}
			if event.ID <= since {
				continue
			}
			if sentDuringCatchup[event.ID] {
				continue
			}
			if err := writeSSEEvent(w, event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			// The caller is resolved again on every heartbeat, so a stream outlives its session
			// (a logout, a membership the sign-in issuer no longer confirms) by one beat at most.
			if _, _, err := s.optionalActor(r); err != nil {
				return
			}
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
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

func (s *server) readEventsAfterID(ctx context.Context, after int64) ([]model.Event, error) {
	return s.readEventRows(ctx, eventSelect+`
		where e.id > $1
		order by e.id asc limit $2
	`, after, maxSSEReplay)
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
// document. Ask payloads are decoded because those fields are added; every other payload passes
// through as the database returned it.
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
