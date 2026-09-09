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

func (s *server) listIssueEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireActor(w, r, nil); !ok {
		return
	}
	query := r.URL.Query()
	after, err := parseNonNegativeInt(query.Get("after"), "after")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	before, err := parseNonNegativeInt(query.Get("before"), "before")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ids, err := parseEventIDs(query.Get("ids"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	descending, err := parseEventOrder(query.Get("order"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	limit, err := parseEventLimit(query.Get("limit"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if query.Has("after") && (query.Has("before") || descending) {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY", "after cannot be combined with before or order=desc"))
		return
	}
	events, err := s.readEvents(r.Context(), r.PathValue("key"), eventListOptions{
		after:      after,
		before:     before,
		hasBefore:  query.Has("before"),
		descending: descending || query.Has("before"),
		ids:        ids,
		limit:      limit,
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *server) streamEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireActor(w, r, nil); !ok {
		return
	}
	sinceRaw := r.URL.Query().Get("since")
	if sinceRaw == "" {
		sinceRaw = r.Header.Get("Last-Event-ID")
	}
	since, err := parseNonNegativeInt(sinceRaw, "since")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, "STREAM_UNSUPPORTED", http.StatusInternalServerError, "streaming unsupported")
		return
	}
	subscription, cancel := s.deps.Events.Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	replay, err := s.readEventsAfterID(r.Context(), since)
	if err != nil {
		return
	}
	lastID := since
	for _, event := range replay {
		if err := writeSSEEvent(w, event); err != nil {
			return
		}
		lastID = event.ID
	}
	flusher.Flush()

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
			if event.ID <= lastID {
				continue
			}
			if err := writeSSEEvent(w, event); err != nil {
				return
			}
			lastID = event.ID
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

type eventListOptions struct {
	after      int64
	before     int64
	hasBefore  bool
	descending bool
	ids        []int64
	limit      int
}

func (s *server) readEvents(ctx context.Context, issueKey string, options eventListOptions) ([]model.Event, error) {
	if len(options.ids) > 0 {
		return s.readEventRows(ctx, `
			select id, issue_key, seq, type, actor, notify, created_at, payload
			from events where issue_key = $1 and id = any($2)
			order by seq asc
		`, issueKey, options.ids)
	}
	if options.descending {
		if options.hasBefore {
			return s.readEventRows(ctx, `
				select id, issue_key, seq, type, actor, notify, created_at, payload
				from events where issue_key = $1 and seq < $2
				order by seq desc limit $3
			`, issueKey, options.before, options.limit)
		}
		return s.readEventRows(ctx, `
			select id, issue_key, seq, type, actor, notify, created_at, payload
			from events where issue_key = $1
			order by seq desc limit $2
		`, issueKey, options.limit)
	}
	return s.readEventRows(ctx, `
		select id, issue_key, seq, type, actor, notify, created_at, payload
		from events where issue_key = $1 and seq > $2
		order by seq asc limit $3
	`, issueKey, options.after, options.limit)
}

func (s *server) readEventsAfterID(ctx context.Context, after int64) ([]model.Event, error) {
	return s.readEventRows(ctx, `
		select id, issue_key, seq, type, actor, notify, created_at, payload
		from events where id > $1
		order by id asc
	`, after)
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
		if err := rows.Scan(&event.ID, &event.IssueKey, &event.Seq, &event.Type, &actor, &event.Notify, &event.CreatedAt, &payload); err != nil {
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
	return events, rows.Err()
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
