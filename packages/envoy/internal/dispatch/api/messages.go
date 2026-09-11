package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

const maxMessageBody16 = 2000

// maxMessageReplyPreview16 bounds the reply target's body preview (MessageEventPayload.ReplyBody)
// carried on a reply's message.created event, so a long parent message does not bloat every
// reply's payload or notification "re:" line.
const maxMessageReplyPreview16 = 160

func (s *server) createMessage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Body    string       `json:"body"`
		ReplyTo *string      `json:"reply_to"`
		Actor   *model.Actor `json:"actor"`
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
	var replyBody string
	if input.ReplyTo != nil {
		if strings.TrimSpace(*input.ReplyTo) == "" {
			writeError(w, "INVALID_MESSAGE", http.StatusBadRequest, "reply_to must identify a message on this issue")
			return
		}
		if _, err := uuid.Parse(*input.ReplyTo); err != nil {
			writeError(w, "INVALID_MESSAGE", http.StatusBadRequest, "reply_to must identify a message on this issue")
			return
		}
		var parentBody string
		if err := tx.QueryRow(r.Context(), `
			select body from messages where id = $1 and issue_key = $2
		`, *input.ReplyTo, issueKey).Scan(&parentBody); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, "INVALID_MESSAGE", http.StatusBadRequest, "reply_to must identify a message on this issue")
				return
			}
			s.writeHandlerError(w, err)
			return
		}
		replyBody = truncateRunes(parentBody, maxMessageReplyPreview16)
	}
	author, err := json.Marshal(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var message model.Message
	var authorRaw []byte
	if err := tx.QueryRow(r.Context(), `
		insert into messages (issue_key, author, body, reply_to)
		values ($1, $2, $3, $4)
		returning id::text, issue_key, author, body, reply_to::text, created_at
	`, issueKey, author, input.Body, input.ReplyTo).Scan(
		&message.ID, &message.IssueKey, &authorRaw, &message.Body, &message.ReplyTo, &message.CreatedAt,
	); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := json.Unmarshal(authorRaw, &message.Author); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Replace(r.Context(), tx, "message", message.ID, message.Body, s.deps.ServerURL); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, issueOwner(issueKey).event(
		"message.created",
		actor,
		model.MessageEventPayload{Message: message, ReplyBody: replyBody},
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
	writeJSON(w, http.StatusCreated, message)
}

func (s *server) getMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	message, err := s.loadMessage(r.Context(), s.deps.Store.Pool, r.PathValue("key"), r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	replies, err := s.loadMessageReplyChain(r.Context(), s.deps.Store.Pool, message.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Message model.Message   `json:"message"`
		Replies []model.Message `json:"replies"`
	}{Message: message, Replies: replies})
}

func (s *server) loadMessage(ctx context.Context, q queryer, issueKey, id string) (model.Message, error) {
	return scanMessage(q.QueryRow(ctx, `
		select id::text, issue_key, author, body, reply_to::text, created_at
		from messages where id = $1 and issue_key = $2
	`, id, issueKey))
}

func scanMessage(row pgx.Row) (model.Message, error) {
	var message model.Message
	var author []byte
	if err := row.Scan(&message.ID, &message.IssueKey, &author, &message.Body, &message.ReplyTo, &message.CreatedAt); err != nil {
		return model.Message{}, err
	}
	if err := json.Unmarshal(author, &message.Author); err != nil {
		return model.Message{}, fmt.Errorf("decode message author: %w", err)
	}
	return message, nil
}

// loadMessageReplyChain returns every message transitively replying to the message
// identified by seedID, ordered oldest first, mirroring loadReplyChain for comments.
func (s *server) loadMessageReplyChain(ctx context.Context, q queryer, seedID string) ([]model.Message, error) {
	rows, err := q.Query(ctx, `
		with recursive replies as (
			select id, issue_key, author, body, reply_to, created_at
			from messages where reply_to = $1
			union all
			select m.id, m.issue_key, m.author, m.body, m.reply_to, m.created_at
			from messages m join replies r on m.reply_to = r.id
		)
		select id::text, issue_key, author, body, reply_to::text, created_at
		from replies
		order by created_at, id
	`, seedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	replies := []model.Message{}
	for rows.Next() {
		reply, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	return replies, rows.Err()
}

// truncateRunes returns value's leading limit runes, matching outbox.truncate's
// rune-based truncation so a preview never splits a multi-byte character.
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
