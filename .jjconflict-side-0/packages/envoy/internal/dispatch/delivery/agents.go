package delivery

import "context"

// ResolveSessionTitles answers each session's most recent known title, which the Delivery page
// shows a session by (LEGION-567): a subagent's own commit trailer carries no parent-session
// reference, so a pull request's Parent agent is the first session its Omp-Session trailers name,
// shown by that title, unless a session group holds it (ParentAgents, parent_agent.go; LEGION-682).
//
// A session's title is never stored in a dedicated table; every write stamps a model.Actor
// (including its ActorOrigin.SessionTitle) as JSONB onto the row it wrote and onto the matching
// events row, and events_session_actor_created_at_idx (migration 0026) makes "this session's most
// recent known title" one indexed query per id. This is durable -- it survives the session ending,
// unlike Envoy's live, TTL-expiring session registry -- for any session that ever claimed an
// issue, posted a message/comment/ask, or wrote a document. A session with no Dispatch footprint
// at all is simply absent from the returned map; SessionTitle falls back to its bare id.
func ResolveSessionTitles(ctx context.Context, q Querier, sessionIDs []string) (map[string]string, error) {
	titles := make(map[string]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return titles, nil
	}
	// One sessionIDs lookup table joined against the index, rather than one query per id: a
	// Delivery timeline page can name hundreds of distinct sessions across its window.
	rows, err := q.Query(ctx, `
		select distinct on (actor->>'id')
			actor->>'id' as session_id,
			actor->'origin'->>'session_title' as session_title
		from events
		where actor->>'kind' = 'session' and actor->>'id' = any($1)
		order by actor->>'id', created_at desc
	`, sessionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var title *string
		if err := rows.Scan(&sessionID, &title); err != nil {
			return nil, err
		}
		if title != nil && *title != "" {
			titles[sessionID] = *title
		}
	}
	return titles, rows.Err()
}

// SessionTitle is the title Dispatch knows for session, or session itself when it has no
// Dispatch footprint. ParentAgents and the timeline's editor payload share this fallback.
func SessionTitle(titles map[string]string, session string) string {
	if title, ok := titles[session]; ok {
		return title
	}
	return session
}

// ListPopulationSessionIDs lists every session any stored delivery population pull request names.
// The group title rule reads this population, while the timeline reads only its current window.
func ListPopulationSessionIDs(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.Query(ctx, `select distinct unnest(sessions) from delivery_pull_requests order by 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
