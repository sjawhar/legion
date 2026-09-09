package api

import (
	"encoding/json"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type userIssueState struct {
	Pinned      bool     `json:"pinned"`
	LastReadSeq int      `json:"last_read_seq"`
	Dismissed   []string `json:"dismissed"`
}

func (s *server) getUserState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select issue_key, pinned, last_read_seq, dismissed
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
		if err := rows.Scan(&key, &value.Pinned, &value.LastReadSeq, &dismissed); err != nil {
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
	writeJSON(w, http.StatusOK, state)
}

func (s *server) putUserState(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Pinned      *bool        `json:"pinned"`
		LastReadSeq *int         `json:"last_read_seq"`
		Dismissed   *[]string    `json:"dismissed"`
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
	var pinned any
	if input.Pinned != nil {
		pinned = *input.Pinned
	}
	var lastReadSeq any
	if input.LastReadSeq != nil {
		lastReadSeq = *input.LastReadSeq
	}
	var dismissed any
	if input.Dismissed != nil {
		encoded, err := encodeJSON(*input.Dismissed)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		dismissed = encoded
	}
	value := userIssueState{}
	var encodedDismissed []byte
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		insert into user_issue_state (login, issue_key, pinned, last_read_seq, dismissed)
		values ($1, $2, coalesce($3, false), coalesce($4, 0), coalesce($5, '[]'::jsonb))
		on conflict (login, issue_key) do update
		set pinned = coalesce($3, user_issue_state.pinned),
		    last_read_seq = coalesce($4, user_issue_state.last_read_seq),
		    dismissed = coalesce($5, user_issue_state.dismissed)
		returning pinned, last_read_seq, dismissed
	`, actor.ID, key, pinned, lastReadSeq, dismissed).Scan(&value.Pinned, &value.LastReadSeq, &encodedDismissed); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := json.Unmarshal(encodedDismissed, &value.Dismissed); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}
