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
	after, err := parseNonNegativeInt(r.URL.Query().Get("after"), "after")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	limit, err := parseEventLimit(r.URL.Query().Get("limit"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events, err := s.readEvents(r.Context(), r.PathValue("key"), after, limit)
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

func (s *server) readEvents(ctx context.Context, issueKey string, after int64, limit int) ([]model.Event, error) {
	return s.readEventRows(ctx, `
		select id, issue_key, seq, type, actor, notify, created_at, payload
		from events where issue_key = $1 and seq > $2
		order by seq asc limit $3
	`, issueKey, after, limit)
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
