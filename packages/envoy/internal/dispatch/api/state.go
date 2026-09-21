package api

import (
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

// userAgentState is a viewer's Clear on one agent's conversation: the Agents page hides every
// exchange whose newest message is at or before cleared_before, for this login only.
type userAgentState struct {
	ClearedBefore string `json:"cleared_before"`
}

// clearedBeforeSkew is how far ahead of the server clock a Clear may land. The client stamps
// the cutoff with its own clock, and a browser a few seconds fast must not be refused.
const clearedBeforeSkew = time.Minute

func (s *server) getUserAgentState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select session_id, cleared_before
		from user_agent_state where login = $1 order by session_id
	`, actor.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	state := map[string]userAgentState{}
	for rows.Next() {
		var sessionID string
		var clearedBefore time.Time
		if err := rows.Scan(&sessionID, &clearedBefore); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		state[sessionID] = userAgentState{ClearedBefore: timestampValue(clearedBefore)}
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, state)
}

func (s *server) putUserAgentState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		ClearedBefore *string `json:"cleared_before"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.ClearedBefore == nil {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "cleared_before is required")
		return
	}
	clearedBefore, err := time.Parse(time.RFC3339, *input.ClearedBefore)
	if err != nil {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "cleared_before must be an RFC3339 timestamp")
		return
	}
	if clearedBefore.After(time.Now().Add(clearedBeforeSkew)) {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "cleared_before must not be in the future")
		return
	}
	var stored time.Time
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		insert into user_agent_state (login, session_id, cleared_before)
		values ($1, $2, $3)
		on conflict (login, session_id) do update set cleared_before = excluded.cleared_before
		returning cleared_before
	`, actor.ID, r.PathValue("session_id"), clearedBefore).Scan(&stored); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, userAgentState{ClearedBefore: timestampValue(stored)})
}
