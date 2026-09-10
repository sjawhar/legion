package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const maxMessageBody16 = 2000

func (s *server) createMessage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Body  string       `json:"body"`
		Actor *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if strings.TrimSpace(input.Body) == "" {
		writeError(w, "INVALID_MESSAGE", http.StatusBadRequest, "message body is required")
		return
	}
	if length := len16(input.Body); length > maxMessageBody16 {
		capExceeded(w, "body", length, maxMessageBody16)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	issueKey := r.PathValue("key")
	if err := s.requireOpenIssue(r.Context(), tx, issueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	author, err := json.Marshal(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var message model.Message
	var authorRaw []byte
	if err := tx.QueryRow(r.Context(), `
		insert into messages (issue_key, author, body)
		values ($1, $2, $3)
		returning id::text, issue_key, author, body, created_at
	`, issueKey, author, input.Body).Scan(
		&message.ID, &message.IssueKey, &authorRaw, &message.Body, &message.CreatedAt,
	); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := json.Unmarshal(authorRaw, &message.Author); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.replaceRefs(r.Context(), tx, "message", message.ID, message.Body); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, model.Event{
		IssueKey: issueKey,
		Type:     "message.created",
		Actor:    actor,
		Payload:  message,
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
	writeJSON(w, http.StatusCreated, message)
}
