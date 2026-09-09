package api

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type userIssueState struct {
	Pinned      bool     `json:"pinned"`
	LastReadSeq int      `json:"last_read_seq"`
	Dismissed   []string `json:"dismissed"`
}

func (s *server) getUserState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireActor(w, r, nil)
	if !ok {
		return
	}
	if actor.Kind != "user" {
		writeError(w, "HUMAN_ONLY", http.StatusForbidden, "user state is only available to users")
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
	value := userIssueState{Dismissed: []string{}}
	var dismissed []byte
	err := s.deps.Store.Pool.QueryRow(r.Context(), `
		select pinned, last_read_seq, dismissed
		from user_issue_state where login = $1 and issue_key = $2
	`, actor.ID, key).Scan(&value.Pinned, &value.LastReadSeq, &dismissed)
	if err == nil {
		if err := json.Unmarshal(dismissed, &value.Dismissed); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else if err != pgx.ErrNoRows {
		s.writeHandlerError(w, err)
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
	dismissed, err = encodeJSON(value.Dismissed)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := s.deps.Store.Pool.Exec(r.Context(), `
		insert into user_issue_state (login, issue_key, pinned, last_read_seq, dismissed)
		values ($1, $2, $3, $4, $5)
		on conflict (login, issue_key) do update
		set pinned = excluded.pinned, last_read_seq = excluded.last_read_seq, dismissed = excluded.dismissed
	`, actor.ID, key, value.Pinned, value.LastReadSeq, dismissed); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}
