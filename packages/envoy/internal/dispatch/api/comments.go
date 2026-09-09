package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const maxCommentBody16 = 2000

func (s *server) listComments(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireActor(w, r, nil); !ok {
		return
	}
	artifactID := strings.TrimSpace(r.URL.Query().Get("artifact"))
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select id::text, issue_key, author, body, anchor, reply_to::text, resolved, suggestion, created_at
		from comments
		where issue_key = $1 and ($2 = '' or anchor->>'artifact_id' = $2)
		order by created_at, id
	`, r.PathValue("key"), artifactID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	comments := []model.Comment{}
	for rows.Next() {
		comment, err := scanComment(rows)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		comments = append(comments, comment)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, comments)
}

func (s *server) createComment(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Body       string             `json:"body"`
		Anchor     *model.AnchorInput `json:"anchor"`
		ReplyTo    *string            `json:"reply_to"`
		Suggestion *struct {
			ReplaceWith string `json:"replace_with"`
		} `json:"suggestion"`
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
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "comment body is required")
		return
	}
	if length := text.Len16(input.Body); length > maxCommentBody16 {
		capExceeded(w, "body", length, maxCommentBody16)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	issueKey := r.PathValue("key")
	var status string
	if err := tx.QueryRow(r.Context(), `select status from issues where key = $1 for update`, issueKey).Scan(&status); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if status == "closed" {
		writeError(w, "ISSUE_CLOSED", http.StatusConflict, "issue is closed")
		return
	}
	if input.ReplyTo != nil {
		if strings.TrimSpace(*input.ReplyTo) == "" {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "reply_to must identify a comment on this issue")
			return
		}
		var sameIssue bool
		if err := tx.QueryRow(r.Context(), `select exists(select 1 from comments where id = $1 and issue_key = $2)`, *input.ReplyTo, issueKey).Scan(&sameIssue); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if !sameIssue {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "reply_to must identify a comment on this issue")
			return
		}
	}
	anchor, artifactName, snapshot, err := s.resolveAnchor(r.Context(), tx, issueKey, input.Anchor, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	author, err := jsonActor(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var anchorJSON any
	if anchor != nil {
		anchorJSON, err = encodeJSON(anchor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var suggestion *model.Suggestion
	var suggestionJSON any
	if input.Suggestion != nil {
		suggestion = &model.Suggestion{ReplaceWith: input.Suggestion.ReplaceWith}
		suggestionJSON, err = encodeJSON(suggestion)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var comment model.Comment
	if err := tx.QueryRow(r.Context(), `
		insert into comments (issue_key, author, body, anchor, reply_to, suggestion)
		values ($1, $2, $3, $4, $5, $6)
		returning id::text, created_at
	`, issueKey, author, input.Body, anchorJSON, input.ReplyTo, suggestionJSON).Scan(&comment.ID, &comment.CreatedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comment.IssueKey = issueKey
	comment.Author = actor
	comment.Body = input.Body
	comment.Anchor = anchor
	comment.ReplyTo = input.ReplyTo
	comment.Suggestion = suggestion
	if err := s.replaceRefs(r.Context(), tx, "comment", comment.ID, comment.Body); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events := []model.Event{}
	if snapshot != nil {
		snapshotEvent, err := s.appendEvent(r.Context(), tx, model.Event{
			IssueKey: issueKey,
			Type:     "artifact.version",
			Actor:    actor,
			Payload:  map[string]any{"artifact_id": anchor.ArtifactID, "name": artifactName, "version": *snapshot},
		})
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, snapshotEvent)
	}
	event, err := s.appendEvent(r.Context(), tx, model.Event{
		IssueKey: issueKey,
		Type:     "comment.created",
		Actor:    actor,
		Payload:  commentEventPayload(comment, artifactName),
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if snapshot != nil {
		s.deps.Docs.CommitVersion(anchor.ArtifactID, *snapshot)
	}
	s.publish(events...)
	writeJSON(w, http.StatusCreated, comment)
}

func (s *server) resolveComment(w http.ResponseWriter, r *http.Request) {
	s.commentAction(w, r, "resolve")
}

func (s *server) acceptComment(w http.ResponseWriter, r *http.Request) {
	s.commentAction(w, r, "accept")
}

func (s *server) rejectComment(w http.ResponseWriter, r *http.Request) {
	s.commentAction(w, r, "reject")
}

func (s *server) commentAction(w http.ResponseWriter, r *http.Request, action string) {
	var input struct {
		Actor *model.Actor `json:"actor"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &input); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var actor model.Actor
	var ok bool
	if action == "resolve" {
		actor, ok = s.requireActor(w, r, input.Actor)
	} else {
		actor, ok = s.requireHuman(w, r)
	}
	if !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	comment, err := s.loadCommentForUpdate(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifactName, err := s.commentArtifactName(r.Context(), tx, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	eventType := "comment.resolved"
	switch action {
	case "resolve":
		if _, err := tx.Exec(r.Context(), `update comments set resolved = true where id = $1`, comment.ID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		comment.Resolved = true
	case "accept", "reject":
		if comment.Suggestion == nil {
			writeError(w, "INVALID_SUGGESTION", http.StatusBadRequest, "accept and reject require a suggestion")
			return
		}
		accepted := action == "accept"
		if accepted {
			if comment.Anchor == nil {
				writeError(w, "INVALID_SUGGESTION", http.StatusBadRequest, "accept requires an anchored suggestion")
				return
			}
			if err := s.deps.Docs.ApplyReplace(docs.WithTx(r.Context(), tx), comment.Anchor.ArtifactID, *comment.Anchor, comment.Suggestion.ReplaceWith, actor); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			eventType = "suggestion.accepted"
		} else {
			eventType = "suggestion.rejected"
		}
		comment.Resolved = true
		comment.Suggestion.Accepted = &accepted
		suggestion, err := encodeJSON(comment.Suggestion)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if _, err := tx.Exec(r.Context(), `update comments set resolved = true, suggestion = $2 where id = $1`, comment.ID, suggestion); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	event, err := s.appendEvent(r.Context(), tx, model.Event{
		IssueKey: comment.IssueKey,
		Type:     eventType,
		Actor:    actor,
		Payload:  commentEventPayload(comment, artifactName),
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
	writeJSON(w, http.StatusOK, comment)
}

type commentQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *server) loadCommentForUpdate(ctx context.Context, tx pgx.Tx, id string) (model.Comment, error) {
	return s.loadCommentRow(ctx, tx, `
		select id::text, issue_key, author, body, anchor, reply_to::text, resolved, suggestion, created_at
		from comments where id = $1 for update
	`, id)
}

func (s *server) loadCommentRow(ctx context.Context, q commentQueryer, query, id string) (model.Comment, error) {
	row := q.QueryRow(ctx, query, id)
	return scanComment(row)
}

type commentRow interface {
	Scan(dest ...any) error
}

func scanComment(row commentRow) (model.Comment, error) {
	var comment model.Comment
	var author, anchor, suggestion []byte
	if err := row.Scan(
		&comment.ID, &comment.IssueKey, &author, &comment.Body, &anchor, &comment.ReplyTo, &comment.Resolved, &suggestion, &comment.CreatedAt,
	); err != nil {
		return model.Comment{}, err
	}
	if err := json.Unmarshal(author, &comment.Author); err != nil {
		return model.Comment{}, fmt.Errorf("decode comment author: %w", err)
	}
	if len(anchor) > 0 {
		var value model.Anchor
		if err := json.Unmarshal(anchor, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode comment anchor: %w", err)
		}
		comment.Anchor = &value
	}
	if len(suggestion) > 0 {
		var value model.Suggestion
		if err := json.Unmarshal(suggestion, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode comment suggestion: %w", err)
		}
		comment.Suggestion = &value
	}
	return comment, nil
}

func commentEventPayload(comment model.Comment, artifactName string) map[string]any {
	return map[string]any{"comment": comment, "artifact_name": artifactName}
}

func (s *server) commentArtifactName(ctx context.Context, tx pgx.Tx, comment model.Comment) (string, error) {
	if comment.Anchor == nil {
		return "", nil
	}
	var name string
	if err := tx.QueryRow(ctx, `select name from artifacts where id = $1`, comment.Anchor.ArtifactID).Scan(&name); err != nil {
		return "", err
	}
	return name, nil
}
