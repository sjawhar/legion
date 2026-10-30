package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// lockedComment is the prologue of every comment mutation: read the comment, take its owner's lock
// and refuse a closed owner, then read it again in a statement after that lock, so the mutation
// decides on the comment as the lock's previous holder left it. It takes no row lock: every writer
// of a comment holds its owner's lock, so the comment holds still until this transaction ends.
func (s *server) lockedComment(ctx context.Context, tx pgx.Tx, id string) (model.Comment, error) {
	unlockedComment, err := s.loadComment(ctx, tx, id)
	if err != nil {
		return model.Comment{}, err
	}
	if err := s.requireOpenOwner(ctx, tx, ownerOf(unlockedComment.IssueKey, unlockedComment.ArtifactID)); err != nil {
		return model.Comment{}, err
	}
	return s.loadComment(ctx, tx, id)
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
	defer tx.Rollback(r.Context())
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	comment, err := s.lockedComment(r.Context(), tx, r.PathValue("id"))
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
	// Everything up to Commit is the read phase (Docs.Join): this route and ProjectMark queue their
	// writes on the ledger, in the order made, and Commit sends them as one batch.
	if err := queueExec(r.Context(), tx, ledger, `
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
		s.registerCommentAnchor(ledger, comment, actor, true)
		replies, err := s.loadReplyChain(r.Context(), tx, "reply_to", comment.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// A resolved orphaned suggestion reopens like any thread: its mark is gone, so it has no
		// projection kind to carry and ProjectMark writes only the marks map.
		projectionKind := ""
		kind, kindErr := s.commentProjectionKind(documentCtx, comment)
		if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
			s.writeHandlerError(w, kindErr)
			return
		}
		if kindErr == nil {
			projectionKind = kind
		}
		if err := s.deps.Docs.ProjectMark(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, replies, projectionKind), actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	payload, err := s.commentEventPayload(r.Context(), tx, &comment, artifactName, commentEventThread{}, model.ReferenceChanges{})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.queueEvent(r.Context(), tx, ledger, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.reopened",
		actor,
		payload,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(*event)
	WriteJSON(w, http.StatusOK, comment)
}

func (s *server) editComment(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	// blocks and base_revision are the ids the dashboard editor carried through the edit: each
	// block's id and where its span starts in body, in UTF-16 code units, against the comment's
	// revision the editor read them at (refs.IndexItem). Without them the ids follow the text.
	var input struct {
		Body         string               `json:"body"`
		BaseRevision *int                 `json:"base_revision"`
		Blocks       []itemblocks.Carried `json:"blocks"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if strings.TrimSpace(input.Body) == "" {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "comment body is required")
		return
	}
	if length := len16(input.Body); length > contracts.DispatchBodyMax {
		capExceeded(w, "body", length, contracts.DispatchBodyMax)
		return
	}
	var carried *refs.CarriedEdit
	if input.Blocks != nil {
		if input.BaseRevision == nil {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "blocks: requires base_revision, the comment's revision the blocks were read at")
			return
		}
		carried = &refs.CarriedEdit{BaseRevision: *input.BaseRevision, Blocks: input.Blocks}
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	comment, err := s.lockedComment(r.Context(), tx, r.PathValue("id"))
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
	// Everything up to Commit is the read phase (Docs.Join): this route and ProjectMark queue their
	// writes on the ledger, in the order made, and Commit sends them as one batch. edited_at is the
	// transaction's now(), which the queued update writes, read here so the projection and the
	// event carry it before that update runs.
	editedAt, err := ledger.Now(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := queueExec(r.Context(), tx, ledger, `
		update comments
		set body = $2, edited_at = $3
		where id = $1
	`, comment.ID, input.Body, editedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	previousBody := comment.Body
	comment.Body = input.Body
	comment.EditedAt = timestampPtr(&editedAt)
	referenceChanges, err := s.queueEditedItemReferences(r.Context(), tx, ledger, refs.ItemComment, comment.ID, previousBody, comment.Body, editedAt, carried)
	if carriedErr := (*itemblocks.CarriedError)(nil); errors.As(err, &carriedErr) {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, carriedErr.Error())
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if comment.Anchor != nil {
		s.registerCommentAnchor(ledger, comment, actor, true)
		replies, err := s.loadReplyChain(r.Context(), tx, "reply_to", comment.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		projectionKind, err := s.commentProjectionKind(documentCtx, comment)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := s.deps.Docs.ProjectMark(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, replies, projectionKind), actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	payload, err := s.commentEventPayload(r.Context(), tx, &comment, artifactName, commentEventThread{}, referenceChanges)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.queueStampedEvent(r.Context(), tx, ledger, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.edited",
		actor,
		payload,
	), "comment", comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(*event)
	WriteJSON(w, http.StatusOK, comment)
}

func (s *server) commentAction(w http.ResponseWriter, r *http.Request, action string) {
	var input struct {
		Actor *model.Actor `json:"actor"`
	}
	if !s.decodeOptionalJSON(w, r, &input) {
		return
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
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	comment, err := s.lockedComment(r.Context(), tx, r.PathValue("id"))
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
	// Everything up to Commit is the read phase (Docs.Join): this route and the document operations
	// queue their writes on the ledger, in the order made, and Commit sends them as one batch.
	// resolved_at is the transaction's now(), which the queued update writes, read here so the
	// projection and the event carry it before that update runs.
	switch action {
	case "resolve":
		// An orphaned suggestion's mark is gone, so it has no projection kind to carry; the
		// resolve still lands and ProjectMark writes only the marks map. Resolve is the one way
		// to close a suggestion that can no longer be accepted or rejected.
		kind, kindErr := s.commentProjectionKind(documentCtx, comment)
		if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
			s.writeHandlerError(w, kindErr)
			return
		}
		if kindErr == nil {
			projectionKind = kind
		}
		resolvedAt, err := ledger.Now(r.Context())
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := queueExec(r.Context(), tx, ledger, `
			update comments
			set resolved = true, resolved_by = $2, resolved_at = $3
			where id = $1
		`, comment.ID, resolvedBy, resolvedAt); err != nil {
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
		kind, kindErr := s.commentProjectionKind(documentCtx, comment)
		if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
			s.writeHandlerError(w, kindErr)
			return
		}
		if kindErr == nil {
			projectionKind = kind
		}
		if comment.Anchor != nil {
			var markErr error
			if action == "accept" {
				markErr = s.deps.Docs.AcceptSuggestion(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, comment.Suggestion.ReplaceWith, actor)
			} else {
				markErr = s.deps.Docs.RejectSuggestion(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, actor)
			}
			if markErr != nil {
				if !errors.Is(markErr, docs.ErrAnchorOrphaned) {
					s.writeHandlerError(w, markErr)
					return
				}
				comment.Anchor.Orphaned = true
				// Only the orphaned key is written, as the anchor refresh writes its keys: the
				// anchor was read with no row lock, and a whole-anchor write would drop a block_id
				// the anchor-block backfill pinned since (docs.BackfillAnchorBlocks), which takes
				// no owner lock.
				if err := queueExec(r.Context(), tx, ledger, `
					update comments set anchor = jsonb_set(anchor, '{orphaned}', to_jsonb(true)) where id = $1
				`, comment.ID); err != nil {
					s.writeHandlerError(w, err)
					return
				}
				if err := ledger.Commit(r.Context()); err != nil {
					s.writeHandlerError(w, err)
					return
				}
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
		resolvedAt, err := ledger.Now(r.Context())
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// The owner's lock this transaction holds keeps the suggestion as it was read above, so the
		// guard finds it undecided; were it decided, the update's callback refuses as the read
		// refused it, and Commit returns that refusal.
		if err := queueWrite(r.Context(), tx, ledger, func(b *store.WriteBatch) error {
			b.Queue("", `
				update comments
				set resolved = true, resolved_by = $2, resolved_at = $3, suggestion = $4
				where id = $1 and suggestion->>'accepted' is null
			`, comment.ID, resolvedBy, resolvedAt, suggestion).Exec(func(tag pgconn.CommandTag) error {
				if tag.RowsAffected() == 0 {
					return errorf(http.StatusConflict, "ALREADY_ACTIONED", "suggestion has already been actioned")
				}
				return nil
			})
			return nil
		}); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		comment.ResolvedBy = &actor
		comment.ResolvedAt = timestampPtr(&resolvedAt)
	}
	s.registerCommentAnchor(ledger, comment, actor, true)
	if comment.Anchor != nil {
		replies, err := s.loadReplyChain(r.Context(), tx, "reply_to", comment.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := s.deps.Docs.ProjectMark(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, replies, projectionKind), actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	events := make([]*model.Event, 0, 2)
	if action == "accept" {
		// Accepting a suggestion is a decision, so it names the version it produced; the
		// transactional apply never schedules a settle (R30), so this is the only version write.
		summary := strings.TrimSpace(comment.Body)
		if summary == "" {
			summary = fmt.Sprintf("Accepted suggestion: %q → %q", comment.Anchor.Quote, comment.Suggestion.ReplaceWith)
		}
		named, err := s.deps.Docs.NamedVersion(documentCtx, comment.Anchor.ArtifactID, summary, actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		diff, err := s.namedVersionDiff(r.Context(), tx, ledger, comment.Anchor.ArtifactID, named.Version)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		versionEvent, err := s.commitArtifactVersionEvent(r.Context(), tx, ledger, ownerOf(comment.IssueKey, comment.ArtifactID), actor, comment.Anchor.ArtifactID, artifactName, named, diff)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, versionEvent)
	}
	payload, err := s.commentEventPayload(r.Context(), tx, &comment, artifactName, commentEventThread{}, model.ReferenceChanges{})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.queueEvent(r.Context(), tx, ledger, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		eventType,
		actor,
		payload,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(appendedEvents(events...)...)
	if action == "accept" {
		// The accepted text may have gone the way an edit's can: removed by a concurrent browser
		// change after this write's version was rendered and before it reached the room, which is
		// past undoing (LEGION-269). The accept stands and says so.
		lost, known := ledger.LostOps(comment.Anchor.ArtifactID)
		accepted := acceptedComment{Comment: comment}
		if known {
			gone := len(lost) > 0
			accepted.Lost = &gone
		}
		WriteJSON(w, http.StatusOK, accepted)
		return
	}
	WriteJSON(w, http.StatusOK, comment)
}

// acceptedComment is the accept route's response: the comment, plus whether the live document
// already lacks the text the accept wrote. It is null when the check reached no verdict - the
// publish failed and the room is reloading, or the room holds a tree too deep to read - which is
// not the same statement as false, exactly as an edit's lost_ops distinguishes them.
type acceptedComment struct {
	model.Comment
	Lost *bool `json:"lost"`
}
