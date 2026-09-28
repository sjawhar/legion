package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type userIssueState struct {
	Pinned      bool     `json:"pinned"`
	LastReadSeq int      `json:"last_read_seq"`
	Dismissed   []string `json:"dismissed"`
	Seq         int64    `json:"seq"`
}

func (s *server) getUserState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select issue_key, pinned, last_read_seq, dismissed, seq
		from user_issue_state where login = $1 order by issue_key
	`, actor.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	state := map[string]userIssueState{}
	for rows.Next() {
		var key string
		var value userIssueState
		var dismissed []byte
		if err := rows.Scan(&key, &value.Pinned, &value.LastReadSeq, &dismissed, &value.Seq); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := json.Unmarshal(dismissed, &value.Dismissed); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		state[key] = value
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, state)
}

func (s *server) putUserState(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Pinned      *bool        `json:"pinned"`
		LastReadSeq *int         `json:"last_read_seq"`
		Dismissed   *[]string    `json:"dismissed"`
		Seq         *int64       `json:"seq"`
		Actor       *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if actor.Kind != "user" {
		writeError(w, "HUMAN_ONLY", http.StatusForbidden, "user state is only available to users")
		return
	}
	if input.LastReadSeq != nil && *input.LastReadSeq < 0 {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "last_read_seq must be non-negative")
		return
	}
	key := r.PathValue("key")
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `
		insert into user_issue_state (login, issue_key) values ($1, $2)
		on conflict (login, issue_key) do nothing
	`, actor.ID, key); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	value := userIssueState{}
	var encodedDismissed []byte
	if err := tx.QueryRow(r.Context(), `
		select pinned, last_read_seq, dismissed, seq
		from user_issue_state where login = $1 and issue_key = $2 for update
	`, actor.ID, key).Scan(&value.Pinned, &value.LastReadSeq, &encodedDismissed, &value.Seq); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := json.Unmarshal(encodedDismissed, &value.Dismissed); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Seq != nil && *input.Seq <= value.Seq {
		WriteJSON(w, http.StatusConflict, map[string]any{
			"code":  "STATE_STALE",
			"error": "user state was updated by another write",
			"state": value,
		})
		return
	}
	if input.Pinned != nil {
		value.Pinned = *input.Pinned
	}
	if input.LastReadSeq != nil {
		value.LastReadSeq = *input.LastReadSeq
	}
	if input.Dismissed != nil {
		value.Dismissed = *input.Dismissed
	}
	if input.Seq != nil {
		value.Seq = *input.Seq
	}
	encodedDismissed, err = encodeJSON(value.Dismissed)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update user_issue_state
		set pinned = $3, last_read_seq = $4, dismissed = $5, seq = $6
		where login = $1 and issue_key = $2
	`, actor.ID, key, value.Pinned, value.LastReadSeq, encodedDismissed, value.Seq); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var project string
	if err := tx.QueryRow(r.Context(), `select project_key from issues where key = $1`, key).Scan(&project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(project).event(
		"user_state.updated", actor, map[string]any{"login": actor.ID, "state": value},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, value)
}

// userAgentState is one viewer's state for one agent's conversation. The Agents page hides every
// exchange whose newest message is at or before cleared_before (the viewer's Clear); read_through
// is how far the viewer has read; unread_replies counts the session's replies to messages this
// viewer sent that are newer than both, which is how the dashboard says an agent answered.
type userAgentState struct {
	ClearedBefore *string `json:"cleared_before,omitempty"`
	ReadThrough   *string `json:"read_through,omitempty"`
	UnreadReplies int     `json:"unread_replies"`
}

// agentStateCutoffSkew is how far ahead of the server clock a Clear or a read mark may land. The
// client stamps the cutoff with its own clock, and a browser a few seconds fast must not be
// refused.
const agentStateCutoffSkew = time.Minute

// userAgentStatesQuery reads a viewer's per-session state ($1 is the login as the actor id spells
// it, which keys the Clear; $3 is its canonical form, which keys the read mark and matches the
// viewer's own direct messages), narrowed to one session when $2 is not null: every session with
// a Clear (user_agent_state) or a read mark (user_agent_read), and every session with a reply the
// viewer has not read. A reply is unread
// when a session wrote it anywhere under a direct message this viewer sent that session (an
// issue-less message targeted at it, a broadcast's copy included) and it is newer than the
// viewer's read mark and Clear, whichever is later.
const userAgentStatesQuery = `
	with recursive roots as (
		select id, substr(target, length('session:') + 1) as session_id
		from messages
		where issue_key is null and in_reply_to is null and target like 'session:%'
		  and author->>'kind' = 'user' and lower(author->>'id') = $3
		  and ($2::text is null or target = 'session:' || $2::text)
	),
	replies as (
		select m.id, m.author, m.created_at, roots.session_id
		from messages m join roots on m.in_reply_to = roots.id
		union all
		select m.id, m.author, m.created_at, replies.session_id
		from messages m join replies on m.in_reply_to = replies.id
	),
	state as (
		select coalesce(cleared.session_id, marked.session_id) as session_id, cleared.cleared_before, marked.read_through
		from (
			select session_id, cleared_before from user_agent_state
			where login = $1 and ($2::text is null or session_id = $2::text)
		) cleared
		full join (
			select session_id, read_through from user_agent_read
			where login = $3 and ($2::text is null or session_id = $2::text)
		) marked on marked.session_id = cleared.session_id
	),
	unread as (
		select replies.session_id, count(*)::int as unread
		from replies left join state on state.session_id = replies.session_id
		where replies.author->>'kind' = 'session'
		  and replies.created_at > coalesce(greatest(state.read_through, state.cleared_before), '-infinity')
		group by replies.session_id
	)
	select coalesce(state.session_id, unread.session_id), state.cleared_before, state.read_through,
	       coalesce(unread.unread, 0)
	from state full join unread on unread.session_id = state.session_id
	order by 1
`

// loadUserAgentStates runs userAgentStatesQuery for login, narrowed to sessionID when it is
// not nil. The read mark and the viewer's own direct messages are matched on the canonical
// login, so one person is one viewer however their identity source spells them; the Clear
// (user_agent_state, migration 0033) is keyed on the raw actor id.
func (s *server) loadUserAgentStates(ctx context.Context, login string, sessionID *string) (map[string]userAgentState, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, userAgentStatesQuery, login, sessionID, canonicalLogin(login))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := map[string]userAgentState{}
	for rows.Next() {
		var session string
		var clearedBefore, readThrough *time.Time
		var state userAgentState
		if err := rows.Scan(&session, &clearedBefore, &readThrough, &state.UnreadReplies); err != nil {
			return nil, err
		}
		state.ClearedBefore = timestampPtr(clearedBefore)
		state.ReadThrough = timestampPtr(readThrough)
		states[session] = state
	}
	return states, rows.Err()
}

func (s *server) getUserAgentState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	states, err := s.loadUserAgentStates(r.Context(), actor.ID, nil)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, states)
}

// parseAgentStateCutoff reads one optional cutoff of a PUT: absent is nil, anything else must
// be an RFC3339 timestamp no further ahead of the server clock than agentStateCutoffSkew.
func parseAgentStateCutoff(name string, value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil, errorf(http.StatusBadRequest, "INVALID_STATE", "%s must be an RFC3339 timestamp", name)
	}
	if parsed.After(time.Now().Add(agentStateCutoffSkew)) {
		return nil, errorf(http.StatusBadRequest, "INVALID_STATE", "%s must not be in the future", name)
	}
	return &parsed, nil
}

// putUserAgentState records a Clear (cleared_before, which replaces the previous one) and/or a
// read mark (read_through, which only ever moves forward, so a tab that read less a moment ago
// cannot make a reply unread again), and answers with the session's whole state.
func (s *server) putUserAgentState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		ClearedBefore *string `json:"cleared_before"`
		ReadThrough   *string `json:"read_through"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.ClearedBefore == nil && input.ReadThrough == nil {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "cleared_before or read_through is required")
		return
	}
	clearedBefore, err := parseAgentStateCutoff("cleared_before", input.ClearedBefore)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	readThrough, err := parseAgentStateCutoff("read_through", input.ReadThrough)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	sessionID := r.PathValue("session_id")
	// The state is announced on an event the session owns, and only a session id a route can
	// name can own one, so any other is refused here rather than failing the write.
	if _, err := model.ParseRoute("session:" + sessionID); err != nil {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_STATE",
			"session id %q is not one Dispatch can route", sessionID))
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// A cutoff up to agentStateCutoffSkew ahead of the server clock is accepted, so a browser a few
	// seconds fast is not refused, but it is stored as no later than now: a reply that lands in
	// the gap is still after the viewer's Clear and read mark, so it shows and it counts.
	if clearedBefore != nil {
		if _, err := tx.Exec(r.Context(), `
			insert into user_agent_state (login, session_id, cleared_before)
			values ($1, $2, least($3::timestamptz, now()))
			on conflict (login, session_id) do update set cleared_before = excluded.cleared_before
		`, actor.ID, sessionID, *clearedBefore); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if readThrough != nil {
		if _, err := tx.Exec(r.Context(), `
			insert into user_agent_read (login, session_id, read_through)
			values ($1, $2, least($3::timestamptz, now()))
			on conflict (login, session_id) do update set
				read_through = greatest(user_agent_read.read_through, excluded.read_through)
		`, canonicalLogin(actor.ID), sessionID, *readThrough); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The viewer's other open tabs and devices refresh their badge from this event rather than
	// waiting for a focus or the next message. It is owned by the session, like its messages,
	// so the outbox publishes it nowhere and it reaches only the dashboard's event stream.
	event, err := s.appendEvent(r.Context(), tx, model.Event{
		Type:    "user_agent_state.updated",
		Actor:   actor,
		Payload: model.UserAgentStateEventPayload{Login: actor.ID, SessionID: sessionID},
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	states, err := s.loadUserAgentStates(r.Context(), actor.ID, &sessionID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, states[sessionID])
}
