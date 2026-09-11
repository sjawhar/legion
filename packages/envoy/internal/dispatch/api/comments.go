package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

const maxCommentBody16 = 2000

func (s *server) listComments(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	comments, err := s.loadOwnerComments(
		r.Context(),
		s.deps.Store.Pool,
		issueOwner(r.PathValue("key")),
		strings.TrimSpace(r.URL.Query().Get("artifact")),
	)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, comments)
}

func (s *server) loadOwnerComments(ctx context.Context, q queryer, owner owner, artifactFilter string) ([]model.Comment, error) {
	var ownerColumn, ownerValue string
	switch {
	case owner.IssueKey != nil:
		ownerColumn, ownerValue = "issue_key", *owner.IssueKey
	case owner.ArtifactID != nil:
		ownerColumn, ownerValue = "artifact_id", *owner.ArtifactID
	default:
		return nil, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`
		select id::text, issue_key, artifact_id::text, author, body, anchor, reply_to::text, ask_id::text, resolved, resolved_by, resolved_at, edited_at, suggestion, created_at
		from comments
		where %s = $1 and ($2 = '' or anchor->>'artifact_id' = $2)
		order by created_at, id
	`, ownerColumn), ownerValue, artifactFilter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	comments := []model.Comment{}
	for rows.Next() {
		comment, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		comments = append(comments, comment)
	}
	return comments, rows.Err()
}

func commentHasOwner(comment model.Comment, owner owner) bool {
	if owner.IssueKey != nil {
		return comment.IssueKey != nil && *comment.IssueKey == *owner.IssueKey
	}
	return owner.ArtifactID != nil && comment.ArtifactID != nil && *comment.ArtifactID == *owner.ArtifactID
}

func (s *server) getComment(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	comment, err := s.loadComment(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	replies, err := s.loadReplyChain(r.Context(), s.deps.Store.Pool, "reply_to", comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Comment model.Comment   `json:"comment"`
		Replies []model.Comment `json:"replies"`
	}{Comment: comment, Replies: replies})
}

// loadReplyChain returns every comment transitively replying to the row(s)
// matching "<seedColumn> = seedValue", ordered oldest first. seedColumn
// selects the thread root: "reply_to" for a comment's own replies, "ask_id"
// for an ask's directly-replying comments (plus, either way, every comment
// that replies to one of those via reply_to). seedColumn is caller-controlled
// and never derived from request input.
func (s *server) loadReplyChain(ctx context.Context, q queryer, seedColumn, seedValue string) ([]model.Comment, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`
		with recursive replies as (
			select id, issue_key, artifact_id, author, body, anchor, reply_to, ask_id, resolved, resolved_by, resolved_at, edited_at, suggestion, created_at
			from comments where %s = $1
			union all
			select c.id, c.issue_key, c.artifact_id, c.author, c.body, c.anchor, c.reply_to, c.ask_id, c.resolved, c.resolved_by, c.resolved_at, c.edited_at, c.suggestion, c.created_at
			from comments c join replies r on c.reply_to = r.id
		)
		select id::text, issue_key, artifact_id::text, author, body, anchor, reply_to::text, ask_id::text, resolved, resolved_by, resolved_at, edited_at, suggestion, created_at
		from replies
		order by created_at, id
	`, seedColumn), seedValue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	replies := []model.Comment{}
	for rows.Next() {
		reply, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	return replies, rows.Err()
}

func (s *server) createComment(w http.ResponseWriter, r *http.Request) {
	s.createCommentFor(w, r, issueOwner(r.PathValue("key")))
}

func (s *server) createCommentFor(w http.ResponseWriter, r *http.Request, owner owner) {
	var input struct {
		Body       string             `json:"body"`
		Anchor     *model.AnchorInput `json:"anchor"`
		ReplyTo    *string            `json:"reply_to"`
		AskID      *string            `json:"ask_id"`
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
	if input.Suggestion == nil && strings.TrimSpace(input.Body) == "" {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "comment body is required")
		return
	}
	if length := len16(input.Body); length > maxCommentBody16 {
		capExceeded(w, "body", length, maxCommentBody16)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure := false
	evictArtifactID := ""
	defer func() {
		if evictOnFailure {
			_ = s.deps.Docs.Evict(r.Context(), evictArtifactID)
		}
	}()
	defer tx.Rollback(r.Context())
	if err := s.requireOpenOwner(r.Context(), tx, owner); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.ReplyTo != nil && input.AskID != nil {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "reply_to and ask_id cannot both be set")
		return
	}
	if input.Anchor != nil && (input.ReplyTo != nil || input.AskID != nil) {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "replies cannot carry anchors")
		return
	}
	var replyRoot *model.Comment
	if input.ReplyTo != nil {
		if strings.TrimSpace(*input.ReplyTo) == "" {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "reply_to must identify a comment on this owner")
			return
		}
		if _, err := uuid.Parse(*input.ReplyTo); err != nil {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "reply_to must identify a comment on this owner")
			return
		}
		root, err := s.loadCommentForUpdate(r.Context(), tx, *input.ReplyTo)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !commentHasOwner(root, owner)) {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "reply_to must identify a comment on this owner")
			return
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if root.ReplyTo != nil || root.AskID != nil {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "reply_to must be a thread root")
			return
		}
		replyRoot = &root
	}
	var askQuestion string
	if input.AskID != nil {
		if strings.TrimSpace(*input.AskID) == "" {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "ask_id must identify an ask on this owner")
			return
		}
		if _, err := uuid.Parse(*input.AskID); err != nil {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "ask_id must identify an ask on this owner")
			return
		}
		if err := tx.QueryRow(r.Context(), `
			select question from asks
			where id = $1
			  and issue_key is not distinct from $2
			  and artifact_id is not distinct from $3
		`, *input.AskID, owner.IssueKey, owner.ArtifactID).Scan(&askQuestion); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "ask_id must identify an ask on this owner")
				return
			}
			s.writeHandlerError(w, err)
			return
		}
	}
	var rowID string
	if err := tx.QueryRow(r.Context(), `select gen_random_uuid()::text`).Scan(&rowID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	markKind := docs.MarkComment
	if input.Suggestion != nil {
		markKind = docs.MarkSuggestion
	}
	anchor, artifactName, snapshot, err := s.resolveAnchor(r.Context(), tx, owner, input.Anchor, markKind, rowID, actor)
	if anchor != nil {
		evictOnFailure = true
		evictArtifactID = anchor.ArtifactID
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	projectionKind := ""
	if input.Suggestion != nil && anchor != nil {
		projectionKind, err = s.deps.Docs.SuggestionKind(r.Context(), anchor.ArtifactID, anchor.MarkID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	author, err := encodeJSON(actor)
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
		insert into comments (id, issue_key, artifact_id, author, body, anchor, reply_to, ask_id, suggestion)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		returning created_at
	`, rowID, owner.IssueKey, owner.ArtifactID, author, input.Body, anchorJSON, input.ReplyTo, input.AskID, suggestionJSON).Scan(&comment.CreatedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comment.ID = rowID
	comment.IssueKey = owner.IssueKey
	comment.ArtifactID = owner.ArtifactID
	comment.Author = actor
	comment.Body = input.Body
	comment.Anchor = anchor
	comment.ReplyTo = input.ReplyTo
	comment.AskID = input.AskID
	comment.Suggestion = suggestion
	if comment.Anchor != nil {
		evictArtifactID = comment.Anchor.ArtifactID
		evictOnFailure = true
		if err := s.deps.Docs.ProjectMark(docs.WithTx(r.Context(), tx), comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, nil, projectionKind)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var reopenedRoot *model.Comment
	var reopenedArtifactName string
	if replyRoot != nil {
		root := *replyRoot
		if root.Resolved {
			if _, err := tx.Exec(r.Context(), `
				update comments
				set resolved = false, resolved_by = null, resolved_at = null
				where id = $1
			`, root.ID); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			root.Resolved = false
			root.ResolvedBy = nil
			root.ResolvedAt = nil
			reopenedRoot = &root
			reopenedArtifactName, err = s.commentArtifactName(r.Context(), tx, root)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
		if root.Anchor != nil {
			replies, err := s.loadReplyChain(r.Context(), tx, "reply_to", root.ID)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			rootProjectionKind, err := s.commentProjectionKind(r.Context(), root)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			evictArtifactID = root.Anchor.ArtifactID
			evictOnFailure = true
			if err := s.deps.Docs.ProjectMark(docs.WithTx(r.Context(), tx), root.Anchor.ArtifactID, root.Anchor.MarkID, commentMarkRecord(root, replies, rootProjectionKind)); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
	}
	if err := refs.Replace(r.Context(), tx, "comment", comment.ID, comment.Body, s.deps.ServerURL); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events := []model.Event{}
	if snapshot != nil {
		snapshotEvent, err := s.appendEvent(r.Context(), tx, owner.event(
			"artifact.version",
			actor,
			versionEventPayload(anchor.ArtifactID, artifactName, *snapshot, nil),
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, snapshotEvent)
	}
	if reopenedRoot != nil {
		event, err := s.appendEvent(r.Context(), tx, owner.event(
			"comment.reopened",
			actor,
			commentEventPayload(*reopenedRoot, reopenedArtifactName, "", ""),
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, event)
	}
	threadRootID := ""
	if replyRoot != nil {
		threadRootID = replyRoot.ID
	}
	event, err := s.appendEvent(r.Context(), tx, owner.event(
		"comment.created",
		actor,
		commentEventPayload(comment, artifactName, askQuestion, threadRootID),
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = false
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

func (s *server) reopenComment(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure := false
	evictArtifactID := ""
	defer func() {
		if evictOnFailure {
			_ = s.deps.Docs.Evict(r.Context(), evictArtifactID)
		}
	}()
	defer tx.Rollback(r.Context())
	unlockedComment, err := s.loadComment(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.requireOpenOwner(r.Context(), tx, ownerOf(unlockedComment.IssueKey, unlockedComment.ArtifactID)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comment, err := s.loadCommentForUpdate(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if comment.ReplyTo != nil || comment.AskID != nil {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "reopen the thread root")
		return
	}
	artifactName, err := s.commentArtifactName(r.Context(), tx, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update comments
		set resolved = false, resolved_by = null, resolved_at = null
		where id = $1
	`, comment.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comment.Resolved = false
	comment.ResolvedBy = nil
	comment.ResolvedAt = nil
	if comment.Anchor != nil {
		replies, err := s.loadReplyChain(r.Context(), tx, "reply_to", comment.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		projectionKind, err := s.commentProjectionKind(r.Context(), comment)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		evictArtifactID = comment.Anchor.ArtifactID
		evictOnFailure = true
		if err := s.deps.Docs.ProjectMark(docs.WithTx(r.Context(), tx), comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, replies, projectionKind)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	event, err := s.appendEvent(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.reopened",
		actor,
		commentEventPayload(comment, artifactName, "", ""),
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = false
	s.publish(event)
	writeJSON(w, http.StatusOK, comment)
}

func (s *server) editComment(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		Body string `json:"body"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if strings.TrimSpace(input.Body) == "" {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "comment body is required")
		return
	}
	if length := len16(input.Body); length > maxCommentBody16 {
		capExceeded(w, "body", length, maxCommentBody16)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure := false
	evictArtifactID := ""
	defer func() {
		if evictOnFailure {
			_ = s.deps.Docs.Evict(r.Context(), evictArtifactID)
		}
	}()
	defer tx.Rollback(r.Context())
	unlockedComment, err := s.loadComment(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.requireOpenOwner(r.Context(), tx, ownerOf(unlockedComment.IssueKey, unlockedComment.ArtifactID)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comment, err := s.loadCommentForUpdate(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if comment.Author != actor {
		writeError(w, "NOT_AUTHOR", http.StatusForbidden, "only the comment author may edit it")
		return
	}
	artifactName, err := s.commentArtifactName(r.Context(), tx, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var editedAt time.Time
	if err := tx.QueryRow(r.Context(), `
		update comments
		set body = $2, edited_at = now()
		where id = $1
		returning edited_at
	`, comment.ID, input.Body).Scan(&editedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comment.Body = input.Body
	comment.EditedAt = commentTimestamp(editedAt)
	if comment.Anchor != nil {
		replies, err := s.loadReplyChain(r.Context(), tx, "reply_to", comment.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		projectionKind, err := s.commentProjectionKind(r.Context(), comment)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		evictArtifactID = comment.Anchor.ArtifactID
		evictOnFailure = true
		if err := s.deps.Docs.ProjectMark(docs.WithTx(r.Context(), tx), comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, replies, projectionKind)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if err := refs.Replace(r.Context(), tx, "comment", comment.ID, comment.Body, s.deps.ServerURL); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.edited",
		actor,
		commentEventPayload(comment, artifactName, "", ""),
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = false
	s.publish(event)
	writeJSON(w, http.StatusOK, comment)
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
	// A live mark mutation cannot roll back from memory with the SQL transaction.
	// Evict after the transaction releases its locks when a later step fails.
	evictOnFailure := false
	evictArtifactID := ""
	defer func() {
		if evictOnFailure {
			_ = s.deps.Docs.Evict(r.Context(), evictArtifactID)
		}
	}()
	defer tx.Rollback(r.Context())
	unlockedComment, err := s.loadComment(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.requireOpenOwner(r.Context(), tx, ownerOf(unlockedComment.IssueKey, unlockedComment.ArtifactID)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
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
	projectionKind := ""
	eventType := "comment.resolved"
	resolvedBy, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	switch action {
	case "resolve":
		projectionKind, err = s.commentProjectionKind(r.Context(), comment)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		var resolvedAt time.Time
		if err := tx.QueryRow(r.Context(), `
			update comments
			set resolved = true, resolved_by = $2, resolved_at = now()
			where id = $1
			returning resolved_at
		`, comment.ID, resolvedBy).Scan(&resolvedAt); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		comment.Resolved = true
		comment.ResolvedBy = &actor
		comment.ResolvedAt = commentTimestamp(resolvedAt)
	case "accept", "reject":
		if comment.Suggestion == nil {
			writeError(w, "INVALID_SUGGESTION", http.StatusBadRequest, "accept and reject require a suggestion")
			return
		}
		if comment.Suggestion.Accepted != nil {
			writeError(w, "ALREADY_ACTIONED", http.StatusConflict, "suggestion has already been actioned")
			return
		}
		if action == "accept" && comment.Anchor == nil {
			writeError(w, "INVALID_SUGGESTION", http.StatusBadRequest, "accept requires an anchored suggestion")
			return
		}
		kind, kindErr := s.commentProjectionKind(r.Context(), comment)
		if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
			s.writeHandlerError(w, kindErr)
			return
		}
		if kindErr == nil {
			projectionKind = kind
		}
		if comment.Anchor != nil {
			evictArtifactID = comment.Anchor.ArtifactID
			evictOnFailure = true
			var markErr error
			if action == "accept" {
				markErr = s.deps.Docs.AcceptSuggestion(docs.WithTx(r.Context(), tx), comment.Anchor.ArtifactID, comment.Anchor.MarkID, comment.Suggestion.ReplaceWith, actor)
			} else {
				markErr = s.deps.Docs.RejectSuggestion(docs.WithTx(r.Context(), tx), comment.Anchor.ArtifactID, comment.Anchor.MarkID, actor)
			}
			if markErr != nil {
				if !errors.Is(markErr, docs.ErrAnchorOrphaned) {
					s.writeHandlerError(w, markErr)
					return
				}
				comment.Anchor.Orphaned = true
				anchor, err := encodeJSON(comment.Anchor)
				if err != nil {
					s.writeHandlerError(w, err)
					return
				}
				if _, err := tx.Exec(r.Context(), `update comments set anchor = $2 where id = $1`, comment.ID, anchor); err != nil {
					s.writeHandlerError(w, err)
					return
				}
				if err := tx.Commit(r.Context()); err != nil {
					s.writeHandlerError(w, err)
					return
				}
				evictOnFailure = false
				s.writeHandlerError(w, markErr)
				return
			}
		}
		accepted := action == "accept"
		if accepted {
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
		var resolvedAt time.Time
		if err := tx.QueryRow(r.Context(), `
			update comments
			set resolved = true, resolved_by = $2, resolved_at = now(), suggestion = $3
			where id = $1 and suggestion->>'accepted' is null
			returning resolved_at
		`, comment.ID, resolvedBy, suggestion).Scan(&resolvedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, "ALREADY_ACTIONED", http.StatusConflict, "suggestion has already been actioned")
				return
			}
			s.writeHandlerError(w, err)
			return
		}
		comment.ResolvedBy = &actor
		comment.ResolvedAt = commentTimestamp(resolvedAt)
	}
	if comment.Anchor != nil {
		replies, err := s.loadReplyChain(r.Context(), tx, "reply_to", comment.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		evictArtifactID = comment.Anchor.ArtifactID
		evictOnFailure = true
		if err := s.deps.Docs.ProjectMark(docs.WithTx(r.Context(), tx), comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, replies, projectionKind)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	events := make([]model.Event, 0, 2)
	var version *model.Version
	if action == "accept" {
		// Accepting a suggestion is a decision, so it names the version it produced; the
		// transactional apply never schedules a settle (R30), so this is the only version write.
		summary := strings.TrimSpace(comment.Body)
		if summary == "" {
			summary = fmt.Sprintf("Accepted suggestion: %q → %q", comment.Anchor.Quote, comment.Suggestion.ReplaceWith)
		}
		namedVersion, err := s.deps.Docs.NamedVersion(docs.WithTx(r.Context(), tx), comment.Anchor.ArtifactID, summary, actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		version = &namedVersion
		diff, err := s.namedVersionDiff(r.Context(), tx, comment.Anchor.ArtifactID, namedVersion)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		versionEvent, err := s.appendEvent(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID).event(
			"artifact.version",
			actor,
			versionEventPayload(comment.Anchor.ArtifactID, artifactName, namedVersion, diff),
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, versionEvent)
	}
	event, err := s.appendEvent(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		eventType,
		actor,
		commentEventPayload(comment, artifactName, "", ""),
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = false
	if version != nil {
		s.deps.Docs.CommitVersion(comment.Anchor.ArtifactID, *version)
	}
	s.publish(events...)
	writeJSON(w, http.StatusOK, comment)
}

func (s *server) loadComment(ctx context.Context, q queryer, id string) (model.Comment, error) {
	return scanComment(q.QueryRow(ctx, `
		select id::text, issue_key, artifact_id::text, author, body, anchor, reply_to::text, ask_id::text, resolved, resolved_by, resolved_at, edited_at, suggestion, created_at
		from comments where id = $1
	`, id))
}

func (s *server) commentProjectionKind(ctx context.Context, comment model.Comment) (string, error) {
	if comment.Suggestion == nil || comment.Anchor == nil {
		return "", nil
	}
	return s.deps.Docs.SuggestionKind(ctx, comment.Anchor.ArtifactID, comment.Anchor.MarkID)
}

func commentMarkRecord(comment model.Comment, replies []model.Comment, suggestionKind string) docs.MarkRecord {
	record := docs.MarkRecord{
		Kind:      "comment",
		By:        docs.ActorRef(comment.Author),
		CreatedAt: comment.CreatedAt.UTC().Format(time.RFC3339Nano),
		Text:      comment.Body,
		Resolved:  comment.Resolved,
		Replies:   make([]docs.MarkReply, 0, len(replies)),
	}
	for _, reply := range replies {
		record.Replies = append(record.Replies, docs.MarkReply{
			By:   docs.ActorRef(reply.Author),
			Text: reply.Body,
			At:   reply.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	if comment.Suggestion != nil {
		record.Kind = suggestionKind
		record.Content = comment.Suggestion.ReplaceWith
		record.Status = "pending"
		if comment.Suggestion.Accepted != nil {
			if *comment.Suggestion.Accepted {
				record.Status = "accepted"
			} else {
				record.Status = "rejected"
			}
		}
	}
	return record
}

func (s *server) loadCommentForUpdate(ctx context.Context, tx pgx.Tx, id string) (model.Comment, error) {
	return scanComment(tx.QueryRow(ctx, `
		select id::text, issue_key, artifact_id::text, author, body, anchor, reply_to::text, ask_id::text, resolved, resolved_by, resolved_at, edited_at, suggestion, created_at
		from comments where id = $1 for update
	`, id))
}

func scanComment(row pgx.Row) (model.Comment, error) {
	var comment model.Comment
	var author, anchor, resolvedBy, suggestion []byte
	var resolvedAt, editedAt *time.Time
	if err := row.Scan(
		&comment.ID, &comment.IssueKey, &comment.ArtifactID, &author, &comment.Body, &anchor, &comment.ReplyTo, &comment.AskID, &comment.Resolved,
		&resolvedBy, &resolvedAt, &editedAt, &suggestion, &comment.CreatedAt,
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
	if len(resolvedBy) > 0 {
		var value model.Actor
		if err := json.Unmarshal(resolvedBy, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode comment resolved_by: %w", err)
		}
		comment.ResolvedBy = &value
	}
	comment.ResolvedAt = commentTimestampPtr(resolvedAt)
	comment.EditedAt = commentTimestampPtr(editedAt)
	if len(suggestion) > 0 {
		var value model.Suggestion
		if err := json.Unmarshal(suggestion, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode comment suggestion: %w", err)
		}
		comment.Suggestion = &value
	}
	return comment, nil
}

func commentTimestampPtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	return commentTimestamp(*value)
}

func commentTimestamp(value time.Time) *string {
	text := value.UTC().Format(time.RFC3339Nano)
	return &text
}

func commentEventPayload(comment model.Comment, artifactName, askQuestion, threadRootID string) model.CommentEventPayload {
	return model.CommentEventPayload{Comment: comment, ArtifactName: artifactName, AskQuestion: askQuestion, ThreadRootID: threadRootID}
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
