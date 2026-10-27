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

const (
	// maxSSEReplay and maxSSEReplayBytes bound what a resuming SSE connection is replayed: at most
	// 256 events, whose frames total at most 1 MiB. A connection further behind is replayed
	// nothing and its client refetches what it shows, which costs less than streaming that
	// backlog (resolveEventReplay).
	maxSSEReplay      = 256
	maxSSEReplayBytes = 1 << 20
	// eventStreamBuffer is how many relayed events one SSE connection may fall behind before the
	// relay's stream drops it, which ends its response and sends its client to a reconnect: 64,
	// more than any one request commits. A connection that cannot drain 64 frames is not reading.
	eventStreamBuffer = 64
	// eventReplayHeader is set to "complete" on an SSE response whose replay is everything its
	// client lacks; a client that reconnects without it refetches what it shows.
	eventReplayHeader = "X-Dispatch-Event-Replay"
)

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
	// envoy-dispatch process's alike — each with its frame encoded once for every connection. The
	// subscription answers head, the id the relay had published through when it was made: every
	// event above head arrives on the subscription, and every event at or below it has committed,
	// so the replay reads those from the log. Nothing falls between the two.
	//
	// That is also why a cold client (no since= and no Last-Event-ID) resolves its position here
	// and not through a request of its own: a client that fetched its own head first and then
	// opened this connection with since=<that head> would lose an event committing between the
	// two, excluded from the replay (id <= since) and missed by a subscription made only afterwards.
	subscription, cancel, head, err := s.eventRelay.subscribe(r.Context(), eventStreamBuffer)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer cancel()
	replay, err := s.resolveEventReplay(r.Context(), sinceProvided, since, head)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if replay.complete {
		w.Header().Set(eventReplayHeader, "complete")
	}
	w.WriteHeader(http.StatusOK)
	// One comment line with the headers, so the stream opens in every browser now. Firefox (the
	// browser suite's, e2e/keyboard-session-pickers.e2e.ts) resolved its fetch of this stream only
	// once a body byte arrived, and a connection with no backlog and no new event sends none until
	// the first heartbeat: until then the page held its "Reconnecting…" state and skipped the
	// refresh a reconnect owes every query, so what committed while it was away stayed unshown.
	if _, err := fmt.Fprint(w, ": open\n\n"); err != nil {
		return
	}
	for _, frame := range replay.frames {
		if _, err := w.Write(frame); err != nil {
			return
		}
	}
	flusher.Flush()
	// The live loop compares an event's id against one fixed position, replay.after, and never
	// against the last id it sent: the relay publishes in id order, and the commit-order lock
	// events.Broker.Append holds until commit means no id at or below a committed max(id) is still
	// uncommitted, so nothing above replay.after can hide below it later. What the comparison
	// skips is what this connection already holds: the replay, or what its client held through
	// since when the relay stands behind it.
	//
	// A reset reuses ids, and the comparison cannot tell this reader apart from the events of the
	// log it was reading; the relay ends the subscription at a reset instead, and the reader comes
	// back on a new connection, which resolves its position against the new log here.
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
			if event.Event.ID <= replay.after {
				continue
			}
			if _, err := w.Write(event.Frame); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			// The caller is resolved again on every heartbeat, so a stream outlives its session
			// (a logout, a membership the sign-in issuer no longer confirms) by one beat at most.
			if _, _, err := s.optionalActor(heartbeatRequest(r)); err != nil {
				return
			}
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// eventReplay is what one connection is replayed before its live loop: the frames, whether they
// are everything its client lacks (complete), and the id at or below which the live loop skips an
// event (after).
type eventReplay struct {
	frames   [][]byte
	complete bool
	after    int64
}

// resolveEventReplay answers a connection that subscribed at head (eventStreamRelay.subscribe).
//
// A cold connection asked for no backlog: it is replayed nothing, starts at head and is not
// complete. A resume at or below head is replayed (since, head] when that is at most
// maxSSEReplay events of at most maxSSEReplayBytes of frames, and is then complete; past either
// bound it is replayed nothing and its client refetches what it shows (the absent
// eventReplayHeader is its signal). Rows are counted, not ids subtracted: ids have gaps.
//
// A resume above head is either ahead of this process's relay or from a replaced log. When since
// is at or below the log's max(id), the relay has not yet published what the client already holds
// (another process served it, or the newest commit is not yet read): nothing is replayed, the
// replay is complete, and the live loop skips what the relay publishes through since. When since
// is above max(id), the log was replaced under the client (`truncate events restart identity`,
// the browser suite's e2e/seed.ts) and the browser, which only ever raises its lastEventId
// (web/src/api/sse.ts), resumes from an id of the old log: it is replayed nothing, is not
// complete and starts at head, so the new log's events reach it rather than sitting below a since
// that will never be passed.
func (s *server) resolveEventReplay(ctx context.Context, sinceProvided bool, since, head int64) (eventReplay, error) {
	if !sinceProvided {
		return eventReplay{after: head}, nil
	}
	if since > head {
		var newest int64
		if err := s.deps.Store.Pool.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&newest); err != nil {
			return eventReplay{}, err
		}
		if since <= newest {
			return eventReplay{complete: true, after: since}, nil
		}
		return eventReplay{after: head}, nil
	}
	var behind int
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select count(*) from (select 1 from events where id > $1 and id <= $2 order by id limit $3) as behind
	`, since, head, maxSSEReplay+1).Scan(&behind); err != nil {
		return eventReplay{}, err
	}
	if behind > maxSSEReplay {
		return eventReplay{after: head}, nil
	}
	missed, err := s.readEventRows(ctx, eventSelect+`
		where e.id > $1 and e.id <= $2
		order by e.id asc limit $3
	`, since, head, maxSSEReplay)
	if err != nil {
		return eventReplay{}, err
	}
	frames := make([][]byte, 0, len(missed))
	size := 0
	for _, event := range missed {
		frame, err := encodeSSEFrame(event)
		if err != nil {
			return eventReplay{}, err
		}
		size += len(frame)
		if size > maxSSEReplayBytes {
			return eventReplay{after: head}, nil
		}
		frames = append(frames, frame)
	}
	return eventReplay{frames: frames, complete: true, after: head}, nil
}

// disconnectAllStreams closes every currently open SSE connection, forcing each client through
// its reconnect-from-lastId path. Test-only: mounted by Register only when Deps.TestHooksEnabled
// is set, so e2e suites can prove reconnect behavior on demand. It ends the relay's running
// generation, whose subscriptions are what an open connection holds; the broker's subscribers
// are the outbox and the delivery hub, which no reconnect test is about.
func (s *server) disconnectAllStreams(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.eventRelay.disconnect()
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

// encodeSSEFrame is the whole SSE frame of one event: its id, event and data lines and the blank
// line that ends it. The relay encodes each live event once with it and a replay encodes each
// missed event with it, so a frame sent live and the same frame replayed are the same bytes.
func encodeSSEFrame(event model.Event) ([]byte, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	frame := make([]byte, 0, len("id: \nevent: \ndata: \n\n")+20+len(event.Type)+len(data))
	frame = append(frame, "id: "...)
	frame = strconv.AppendInt(frame, event.ID, 10)
	frame = append(frame, "\nevent: "...)
	frame = append(frame, event.Type...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	return frame, nil
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
