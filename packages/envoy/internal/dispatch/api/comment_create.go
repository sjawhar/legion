package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

const maxCommentBody16 = 2000

type commentInput struct {
	Body    string             `json:"body"`
	Anchor  *model.AnchorInput `json:"anchor"`
	ReplyTo *string            `json:"reply_to"`
	AskID   *string            `json:"ask_id"`
	// Turn is who holds the turn after this ask reply: "agent" for a progress note
	// that keeps the ask waiting on its asker, "human" (the default for a session
	// author) when the human needs to act. Ignored for a human author, whose reply
	// always hands the turn to the agent. Rejected on a comment that is not an ask reply.
	Turn       *string `json:"turn"`
	Suggestion *struct {
		ReplaceWith string `json:"replace_with"`
	} `json:"suggestion"`
	Actor *model.Actor `json:"actor"`
}

// askReplyTurn is who holds the turn once an ask reply by actor is posted: a human's
// reply always hands it to the agent; a session's reply hands it to the human unless
// the request marks it a progress note (turn "agent").
func askReplyTurn(actor model.Actor, requested *string) string {
	if actor.Kind == "user" {
		return "agent"
	}
	if requested != nil {
		return *requested
	}
	return "human"
}

type commentThreadTarget struct {
	ReplyTo     *string
	AskID       *string
	ReplyRoot   *model.Comment
	AskQuestion string
	AskState    string
}

func (s *server) createComment(w http.ResponseWriter, r *http.Request) {
	s.createCommentFor(w, r, issueOwner(r.PathValue("key")))
}

func (s *server) normalizeCommentThreadTarget(
	ctx context.Context,
	tx pgx.Tx,
	owner owner,
	input commentInput,
) (commentThreadTarget, error) {
	target := commentThreadTarget{ReplyTo: input.ReplyTo, AskID: input.AskID}
	if target.ReplyTo != nil && target.AskID != nil {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to and ask_id cannot both be set")
	}
	if input.Anchor != nil && (target.ReplyTo != nil || target.AskID != nil) {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "replies cannot carry anchors")
	}
	if input.Suggestion != nil && (target.ReplyTo != nil || target.AskID != nil) {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "replies cannot carry suggestions")
	}
	if target.ReplyTo != nil {
		if strings.TrimSpace(*target.ReplyTo) == "" {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must be a full comment id")
		}
		if _, err := uuid.Parse(*target.ReplyTo); err != nil {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must be a full comment id")
		}
		root, err := s.loadCommentForUpdate(ctx, tx, *target.ReplyTo)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !commentHasOwner(root, owner)) {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must identify a comment on this owner")
		}
		if err != nil {
			return commentThreadTarget{}, err
		}
		// The walk up to the thread root is bounded like every other parent walk (the
		// outbox's thread walk, the issue ancestor walk): comments.reply_to has no acyclicity
		// constraint, so a cyclic chain answers 400 instead of holding the transaction open.
		for depth := 1; ; depth++ {
			if root.AskID != nil {
				target.AskID = root.AskID
				target.ReplyTo = nil
				break
			}
			if root.ReplyTo == nil {
				target.ReplyTo = &root.ID
				target.ReplyRoot = &root
				break
			}
			if depth >= parentDepthCap {
				return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must identify a comment on this owner")
			}
			root, err = s.loadCommentForUpdate(ctx, tx, *root.ReplyTo)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !commentHasOwner(root, owner)) {
				return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must identify a comment on this owner")
			}
			if err != nil {
				return commentThreadTarget{}, err
			}
		}
	}
	if target.AskID != nil {
		if strings.TrimSpace(*target.AskID) == "" {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "ask_id must be a full ask id")
		}
		if _, err := uuid.Parse(*target.AskID); err != nil {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "ask_id must be a full ask id")
		}
		if err := tx.QueryRow(ctx, `
			select question, state from asks
			where id = $1
			  and issue_key is not distinct from $2
			  and artifact_id is not distinct from $3
		`, *target.AskID, owner.IssueKey, owner.ArtifactID).Scan(&target.AskQuestion, &target.AskState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "ask_id must identify an ask on this owner")
			}
			return commentThreadTarget{}, err
		}
	}
	return target, nil
}

func (s *server) createCommentFor(w http.ResponseWriter, r *http.Request, owner owner) {
	var input commentInput
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
	threadTarget, err := s.normalizeCommentThreadTarget(r.Context(), tx, owner, input)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Turn != nil {
		if threadTarget.AskID == nil {
			writeError(w, "TURN_REQUIRES_ASK", http.StatusBadRequest, "turn is only valid on a reply to an ask")
			return
		}
		if *input.Turn != "human" && *input.Turn != "agent" {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "turn must be human or agent")
			return
		}
	}
	input.ReplyTo = threadTarget.ReplyTo
	input.AskID = threadTarget.AskID
	replyRoot := threadTarget.ReplyRoot
	eventThread := commentEventThread{AskQuestion: threadTarget.AskQuestion, AskState: threadTarget.AskState}
	// Only an open ask has a turn to hold: a reply under an answered or resolved ask
	// records none, so the column always means "who the ask waits on after this".
	var turn *string
	if input.AskID != nil && threadTarget.AskState == "open" {
		turn = new(askReplyTurn(actor, input.Turn))
		eventThread.AskWaitingOn = *turn
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
		insert into comments (id, issue_key, artifact_id, author, body, anchor, reply_to, ask_id, turn, suggestion)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		returning created_at
	`, rowID, owner.IssueKey, owner.ArtifactID, author, input.Body, anchorJSON, input.ReplyTo, input.AskID, turn, suggestionJSON).Scan(&comment.CreatedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.AskID != nil {
		if err := asks.FollowAuthor(r.Context(), tx, *input.AskID, actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	comment.ID = rowID
	comment.IssueKey = owner.IssueKey
	comment.ArtifactID = owner.ArtifactID
	comment.Author = actor
	comment.Body = input.Body
	comment.Anchor = anchor
	comment.ReplyTo = input.ReplyTo
	comment.AskID = input.AskID
	comment.Turn = turn
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
		if err := refs.Stamp(r.Context(), tx, "artifact", anchor.ArtifactID, snapshotEvent.ID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, snapshotEvent)
	}
	if reopenedRoot != nil {
		payload, err := s.commentEventPayload(r.Context(), tx, *reopenedRoot, reopenedArtifactName, commentEventThread{})
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		event, err := s.appendEvent(r.Context(), tx, owner.event(
			"comment.reopened",
			actor,
			payload,
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, event)
	}
	if replyRoot != nil {
		eventThread.ThreadRootID = replyRoot.ID
	}
	payload, err := s.commentEventPayload(r.Context(), tx, comment, artifactName, eventThread)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, owner.event(
		"comment.created",
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
	WriteJSON(w, http.StatusCreated, comment)
}
