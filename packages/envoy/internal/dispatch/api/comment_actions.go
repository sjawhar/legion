package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

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
		// A resolved orphaned suggestion reopens like any thread: its mark is gone, so it has no
		// projection kind to carry and ProjectMark writes only the marks map.
		projectionKind := ""
		kind, kindErr := s.commentProjectionKind(r.Context(), comment)
		if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
			s.writeHandlerError(w, kindErr)
			return
		}
		if kindErr == nil {
			projectionKind = kind
		}
		evictArtifactID = comment.Anchor.ArtifactID
		evictOnFailure = true
		if err := s.deps.Docs.ProjectMark(docs.WithTx(r.Context(), tx), comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, replies, projectionKind)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	payload, err := s.commentEventPayload(r.Context(), tx, comment, artifactName, commentEventThread{})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.reopened",
		actor,
		payload,
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
	WriteJSON(w, http.StatusOK, comment)
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
	comment.EditedAt = timestampPtr(&editedAt)
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
	payload, err := s.commentEventPayload(r.Context(), tx, comment, artifactName, commentEventThread{})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.edited",
		actor,
		payload,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Stamp(r.Context(), tx, "comment", comment.ID, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = false
	s.publish(event)
	WriteJSON(w, http.StatusOK, comment)
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
		// An orphaned suggestion's mark is gone, so it has no projection kind to carry; the
		// resolve still lands and ProjectMark writes only the marks map. Resolve is the one way
		// to close a suggestion that can no longer be accepted or rejected.
		kind, kindErr := s.commentProjectionKind(r.Context(), comment)
		if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
			s.writeHandlerError(w, kindErr)
			return
		}
		if kindErr == nil {
			projectionKind = kind
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
		comment.ResolvedAt = timestampPtr(&resolvedAt)
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
		comment.ResolvedAt = timestampPtr(&resolvedAt)
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
		if err := refs.Stamp(r.Context(), tx, "artifact", comment.Anchor.ArtifactID, versionEvent.ID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, versionEvent)
	}
	payload, err := s.commentEventPayload(r.Context(), tx, comment, artifactName, commentEventThread{})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		eventType,
		actor,
		payload,
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
	WriteJSON(w, http.StatusOK, comment)
}
