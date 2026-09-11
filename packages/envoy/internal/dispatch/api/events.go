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

const eventSelect = `select e.id, e.issue_key, e.artifact_id::text, coalesce(i.project_key, ar.project_key), e.seq, e.type, e.actor, e.notify, e.created_at, e.payload
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
	writeJSON(w, http.StatusOK, events)
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
	// A cold client (no since= and no Last-Event-ID) used to fetch its own head
	// via a separate GET /api/v1/events/head request, then open this connection
	// with since=<that head>. An event whose id was allocated before that first
	// request read the head, but committed after the head response and before
	// this connection's Subscribe below, was excluded from catch-up (id <= since)
	// and missed by the subscription (registered too late) — lost forever. This
	// handler now IS the client's only request for a cold start: subscribing
	// before computing its own head closes that gap, since anything committing
	// after Subscribe lands in the channel regardless of its id (see below).
	subscription, cancel := s.deps.Events.Subscribe()
	defer cancel()
	if !sinceProvided {
		if err := s.deps.Store.Pool.QueryRow(r.Context(), `select coalesce(max(id), 0) from events`).Scan(&since); err != nil {
			s.writeHandlerError(w, err)
			return
		}
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
	// capped page used to end the stream and force a client reconnect, but that
	// left a gap between "read this page" and "reopen a new subscription" where a
	// low id could commit and be missed by both the next page's `id > cursor` query
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
	// The live loop below must never use an id comparison to decide whether to
	// forward: it would silently drop a late-committing lower id (see above).
	// Dedup only against the bounded set of ids catch-up actually sent, in case one
	// of them is *also* still sitting in the subscription channel (committed, and
	// thus queryable, in the narrow window between Subscribe and a catch-up query,
	// but not yet drained from it).

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-subscription:
			if !open {
				return
			}
			if sentDuringCatchup[event.ID] {
				continue
			}
			if err := writeSSEEvent(w, event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
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
// behavior without seeding thousands of events to trip the replay cap.
func (s *server) disconnectAllStreams(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.deps.Events.CloseAll()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
		if err := json.Unmarshal(payload, &event.Payload); err != nil {
			return nil, fmt.Errorf("decode event payload: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachAskOpenedEventIDs(ctx, events); err != nil {
		return nil, err
	}
	return events, nil
}

func (s *server) attachAskOpenedEventIDs(ctx context.Context, events []model.Event) error {
	asks := []model.Ask{}
	payloads := []map[string]any{}
	for index := range events {
		switch events[index].Type {
		case "ask.opened", "ask.answered", "ask.resolved":
		default:
			continue
		}
		payload, ok := events[index].Payload.(map[string]any)
		if !ok {
			return fmt.Errorf("decode %s payload: expected object", events[index].Type)
		}
		askID, ok := payload["id"].(string)
		if !ok || askID == "" {
			return fmt.Errorf("decode %s payload: ask id missing", events[index].Type)
		}
		asks = append(asks, model.Ask{ID: askID})
		payloads = append(payloads, payload)
	}
	askPointers := make([]*model.Ask, len(asks))
	for index := range asks {
		askPointers[index] = &asks[index]
	}
	if err := s.attachOpenedEventIDs(ctx, s.deps.Store.Pool, askPointers); err != nil {
		return err
	}
	for index, payload := range payloads {
		payload["opened_event_id"] = *asks[index].OpenedEventID
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
