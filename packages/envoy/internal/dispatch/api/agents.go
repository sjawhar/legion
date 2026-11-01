package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
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
// capabilities it advertises. Sending to a session without an issue stays human-only. The list is
// the stored copy of the listener's sessions every process shares (readRouteRoster, the
// route_roster row), at most routeRosterTTL (5 s) old, so one listener call serves agents lists
// and issue reads alike; a listener that did not answer answers 503 for as long as that copy is a
// silence, a request that ends while it waits on the shared fetch answers 503 saying so, and a
// failed read of the stored copy answers as the database's error. Once the process is shutting
// down, a list the drain still serves asks the listener on its own request instead, since the
// shared fetch runs on the process's lifetime and can no longer answer.
//
// The copy carries the unresponsive sessions as well as the live ones, each row with its own
// `state`, so the Agents page shows a session that missed its heartbeats as stalled rather than
// dropping it: it is still working, still holds its claims, and is still a target a reader may
// pick (dispatch://INFRA-392).
func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	if s.deps.Envoy == nil {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, "ENVOY_URL is not configured")
		return
	}

	roster, err := s.readRouteRoster(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !roster.answered && s.lifetime.Err() != nil {
		// The process is shutting down: the shared fetch runs on its lifetime, so it has ended or
		// failed at once, and its silence says nothing about the listener. A request the drain
		// still serves asks the listener on its own context instead, as every agents list did
		// before the copy existed.
		roster = s.listRouteRoster(r.Context())
	}
	if !roster.answered {
		s.writeEnvoyError(w, roster.err)
		return
	}
	if len(roster.sessions) == 0 {
		// Nothing to look up: both aggregates would bind an empty id list and return no rows.
		WriteJSON(w, http.StatusOK, []agentResponse{})
		return
	}
	// The roster's list is every read's, so the sort orders a copy of it.
	sessions := slices.Clone(roster.sessions)
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
		// A listener that predates the "host" field (packages/envoy/internal/session's
		// SessionEntry.Host) answers with none; fall back to the routing machine_id rather
		// than showing an empty machine on the Agents page during a partial rollout.
		session.Host = cmp.Or(session.Host, session.MachineID)
		rows = append(rows, agentResponse{
			Session:      session,
			OpenAsks:     openAsks[session.SessionID],
			LastActivity: lastActivity,
		})
	}
	WriteJSON(w, http.StatusOK, rows)
}
