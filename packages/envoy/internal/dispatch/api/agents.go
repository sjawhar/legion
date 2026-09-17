package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
)

type agentResponse struct {
	envoy.Session
	OpenAsks     int     `json:"open_asks"`
	LastActivity *string `json:"last_activity"`
}

func (s *server) agentOpenAskCounts(ctx context.Context, sessionIDs []string) (map[string]int, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select a.author->>'id', count(*)::int
		from asks a
		left join issues i on i.key = a.issue_key
		where a.state = 'open'
			and a.author->>'kind' = 'session'
			and a.author->>'id' = any($1::text[])
			and (i.key is null or i.closed_at is null)
		group by a.author->>'id'
	`, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("query agent open asks: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var sessionID string
		var count int
		if err := rows.Scan(&sessionID, &count); err != nil {
			return nil, fmt.Errorf("scan agent open asks: %w", err)
		}
		counts[sessionID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent open asks: %w", err)
	}
	return counts, nil
}

func (s *server) agentLastActivity(ctx context.Context, sessionIDs []string) (map[string]time.Time, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select actor->>'id', max(created_at)
		from events
		where actor->>'kind' = 'session'
			and actor->>'id' = any($1::text[])
		group by actor->>'id'
	`, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("query agent activity: %w", err)
	}
	defer rows.Close()

	activity := make(map[string]time.Time)
	for rows.Next() {
		var sessionID string
		var createdAt time.Time
		if err := rows.Scan(&sessionID, &createdAt); err != nil {
			return nil, fmt.Errorf("scan agent activity: %w", err)
		}
		activity[sessionID] = createdAt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent activity: %w", err)
	}
	return activity, nil
}

// listAgents is readable by any authenticated caller: a session picks a target by the
// capabilities it advertises. Sending to a session without an issue stays human-only.
func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	if s.deps.Envoy == nil {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, "ENVOY_URL is not configured")
		return
	}

	sessions, err := s.deps.Envoy.Sessions(r.Context())
	if err != nil {
		if errors.Is(err, envoy.ErrUnavailable) {
			writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, err.Error())
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	if len(sessions) == 0 {
		// Nothing to look up: both aggregates would bind an empty id list and return no rows.
		WriteJSON(w, http.StatusOK, []agentResponse{})
		return
	}
	sort.SliceStable(sessions, func(left, right int) bool {
		if sessions[left].LastSeen != sessions[right].LastSeen {
			return sessions[left].LastSeen > sessions[right].LastSeen
		}
		return sessions[left].SessionID < sessions[right].SessionID
	})
	sessionIDs := make([]string, len(sessions))
	for index, session := range sessions {
		sessionIDs[index] = session.SessionID
	}
	openAsks, err := s.agentOpenAskCounts(r.Context(), sessionIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	activity, err := s.agentLastActivity(r.Context(), sessionIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	rows := make([]agentResponse, 0, len(sessions))
	for _, session := range sessions {
		var lastActivity *string
		if createdAt, ok := activity[session.SessionID]; ok {
			lastActivity = timestampPtr(&createdAt)
		}
		rows = append(rows, agentResponse{
			Session:      session,
			OpenAsks:     openAsks[session.SessionID],
			LastActivity: lastActivity,
		})
	}
	WriteJSON(w, http.StatusOK, rows)
}
