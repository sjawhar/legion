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
	Body     string             `json:"body"`
	Anchor   *model.AnchorInput `json:"anchor"`
	ReplyTo  *string            `json:"reply_to"`
	AskID    *string            `json:"ask_id"`
	Mentions []struct {
		Target string `json:"target"`
	} `json:"mentions"`
	Delivery *string `json:"delivery"`
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

func validateCommentMentions(input commentInput) (string, []string, error) {
	if len(input.Mentions) == 0 {
		if input.Delivery != nil {
			return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "delivery requires mentions")
		}
		return "", nil, nil
	}
	if len(input.Mentions) > 10 {
		return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "mentions allow at most 10 targets")
	}
	delivery := "steer"
	if input.Delivery != nil {
		delivery = *input.Delivery
		if !validDelivery(delivery) {
			return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "delivery must be one of btw, aside, steer")
		}
	}
	targets := make([]string, 0, len(input.Mentions))
	seen := make(map[string]struct{}, len(input.Mentions))
	for _, mention := range input.Mentions {
		if _, err := model.ParseRoute(mention.Target); err != nil {
			return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "mention target must be role:<name> or session:<id>")
		}
		if _, duplicate := seen[mention.Target]; duplicate {
			return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "mentions must not repeat a target")
		}
		seen[mention.Target] = struct{}{}
		targets = append(targets, mention.Target)
	}
	return delivery, targets, nil
}

type commentThreadTarget struct {
	ReplyTo     *string
	AskID       *string
	ReplyRoot   *model.Comment
	AskQuestion string
	AskState    string
}

// replyTurn is who holds the turn once a reply into this thread is posted. Only an open ask
// has a turn to hold: a reply under an answered or resolved ask records none, so the column
// always means "who the ask waits on after this".
func (t commentThreadTarget) replyTurn(actor model.Actor, requested *string) *string {
	if t.AskID == nil || t.AskState != "open" {
		return nil
	}
	return new(askReplyTurn(actor, requested))
}

// eventThread is what a comment event says about the thread the comment joined, given the
// turn replyTurn settled on.
func (t commentThreadTarget) eventThread(turn *string) commentEventThread {
	thread := commentEventThread{AskQuestion: t.AskQuestion, AskState: t.AskState}
	if turn != nil {
		thread.AskWaitingOn = *turn
	}
	if t.ReplyTo != nil {
		thread.ThreadRootID = *t.ReplyTo
	}
	return thread
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
	if input.ReplyTo != nil && input.AskID != nil {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to and ask_id cannot both be set")
	}
	if input.Anchor != nil && (input.ReplyTo != nil || input.AskID != nil) {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "replies cannot carry anchors")
	}
	if input.Suggestion != nil && (input.ReplyTo != nil || input.AskID != nil) {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "replies cannot carry suggestions")
	}
	if input.ReplyTo != nil {
		if strings.TrimSpace(*input.ReplyTo) == "" {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must be a full comment id")
		}
		if _, err := uuid.Parse(*input.ReplyTo); err != nil {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must be a full comment id")
		}
		parent, err := s.loadOwnedCommentForUpdate(ctx, tx, owner, *input.ReplyTo)
		if err != nil {
			return commentThreadTarget{}, err
		}
		return s.threadHeadOf(ctx, tx, owner, parent)
	}
	if input.AskID == nil {
		return commentThreadTarget{}, nil
	}
	if strings.TrimSpace(*input.AskID) == "" {
		return commentThreadTarget{}, s.askIDInputForOwner(ctx, tx, owner)
	}
	if _, err := uuid.Parse(*input.AskID); err != nil {
		return commentThreadTarget{}, s.askIDInputForOwner(ctx, tx, owner)
	}
	question, state, err := s.describeAsk(ctx, tx, owner, *input.AskID)
	if err != nil {
		return commentThreadTarget{}, err
	}
	return commentThreadTarget{AskID: input.AskID, AskQuestion: question, AskState: state}, nil
}

// threadHeadOf climbs reply_to from an already-locked comment to the head of its thread: the
// ask the thread hangs under, or the thread's root comment when it hangs under no ask. The
// walk is bounded like every other parent walk (the outbox's thread walk, the issue ancestor
// walk): comments.reply_to has no acyclicity constraint, so a cyclic chain answers 400
// instead of holding the transaction open.
func (s *server) threadHeadOf(
	ctx context.Context, tx pgx.Tx, owner owner, from model.Comment,
) (commentThreadTarget, error) {
	root := from
	for depth := 1; ; depth++ {
		if root.AskID != nil {
			question, state, err := s.describeAsk(ctx, tx, owner, *root.AskID)
			if err != nil {
				return commentThreadTarget{}, err
			}
			return commentThreadTarget{AskID: root.AskID, AskQuestion: question, AskState: state}, nil
		}
		if root.ReplyTo == nil {
			return commentThreadTarget{ReplyTo: &root.ID, ReplyRoot: &root}, nil
		}
		if depth >= parentDepthCap {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must identify a comment on this owner")
		}
		next, err := s.loadOwnedCommentForUpdate(ctx, tx, owner, *root.ReplyTo)
		if err != nil {
			return commentThreadTarget{}, err
		}
		root = next
	}
}

// describeAsk is the question and state of an ask on this owner, refusing one that belongs
// to another.
func (s *server) describeAsk(
	ctx context.Context, tx pgx.Tx, owner owner, askID string,
) (string, string, error) {
	var question, state string
	err := tx.QueryRow(ctx, `
		select question, state from asks
		where id = $1
		  and issue_key is not distinct from $2
		  and artifact_id is not distinct from $3
	`, askID, owner.IssueKey, owner.ArtifactID).Scan(&question, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", errorf(http.StatusBadRequest, "INVALID_COMMENT", "ask_id must identify an ask on this owner")
	}
	return question, state, err
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
	delivery, mentionTargets, err := validateCommentMentions(input)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	documentCtx, documentEvents := documentMutationContext(r.Context(), tx)
	defer s.deps.Docs.DiscardLiveWrites(documentEvents)
	status, err := s.requireOpenOwnerStatus(r.Context(), tx, owner)
	if err != nil {
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
	resolvedMentions := s.resolveMentionTargets(r.Context(), mentionTargets, delivery)
	mentions := make([]model.Mention, 0, len(resolvedMentions))
	mentionedSessions := make(map[string]struct{}, len(resolvedMentions))
	for _, resolved := range resolvedMentions {
		mentions = append(mentions, model.Mention{
			Target: resolved.Target, Delivery: resolved.Delivery, SessionID: resolved.SessionID,
		})
		if resolved.SessionID != nil {
			mentionedSessions[*resolved.SessionID] = struct{}{}
		}
	}
	suppressRoute := false
	suppressedRoute := ""
	var suppressedRouteSessionID *string
	if owner.IssueKey != nil && len(resolvedMentions) > 0 {
		var route *string
		if err := tx.QueryRow(r.Context(), `select route from issues where key = $1`, *owner.IssueKey).Scan(&route); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if route != nil && *route != "" {
			var routeSessionID *string
			routeWasMentioned := false
			for index := range resolvedMentions {
				if resolvedMentions[index].Target == *route {
					routeWasMentioned = true
					routeSessionID = resolvedMentions[index].SessionID
					break
				}
			}
			if !routeWasMentioned {
				routeSessionID = s.resolveMentionTargets(r.Context(), []string{*route}, "steer")[0].SessionID
			}
			if routeSessionID != nil {
				if _, mentioned := mentionedSessions[*routeSessionID]; mentioned {
					suppressRoute = true
					suppressedRoute = *route
					suppressedRouteSessionID = routeSessionID
				}
			}
		}
	}
	suppressedAuthors := []string{}
	if replyRoot != nil && replyRoot.Author.Kind == "session" {
		if _, mentioned := mentionedSessions[replyRoot.Author.ID]; mentioned {
			suppressedAuthors = append(suppressedAuthors, replyRoot.Author.ID)
		}
	}
	turn := threadTarget.replyTurn(actor, input.Turn)
	var rowID string
	if err := tx.QueryRow(r.Context(), `select gen_random_uuid()::text`).Scan(&rowID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	markKind := docs.MarkComment
	if input.Suggestion != nil {
		markKind = docs.MarkSuggestion
	}
	anchor, artifactName, snapshot, err := s.resolveAnchor(documentCtx, tx, owner, input.Anchor, markKind, rowID, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	projection := model.Comment{Anchor: anchor}
	if input.Suggestion != nil {
		projection.Suggestion = &model.Suggestion{ReplaceWith: input.Suggestion.ReplaceWith}
	}
	projectionKind, err := s.commentProjectionKind(documentCtx, projection)
	if err != nil {
		s.writeHandlerError(w, err)
		return
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
	comment.Mentions = mentions
	comment.Deliveries = []model.CommentDelivery{}
	if comment.Anchor != nil {
		if err := s.deps.Docs.ProjectMark(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, nil, projectionKind), actor); err != nil {
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
			rootProjectionKind, err := s.commentProjectionKind(documentCtx, root)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if err := s.deps.Docs.ProjectMark(documentCtx, root.Anchor.ArtifactID, root.Anchor.MarkID, commentMarkRecord(root, replies, rootProjectionKind), actor); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
	}
	for _, mention := range resolvedMentions {
		if _, err := tx.Exec(r.Context(), `
			insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
			values ($1, $2, $3, $4)
		`, comment.ID, mention.Target, mention.Delivery, mention.SessionID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		resolveError := (*string)(nil)
		if mention.ResolveError != "" {
			resolveError = &mention.ResolveError
		}
		if _, err := tx.Exec(r.Context(), `
			insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, resolve_error, state)
			values ($1, $2, 1, $3, $4, $5, 'pending')
		`, comment.ID, mention.Target, mention.Delivery, mention.SessionID, resolveError); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	referenceChanges, err := s.replaceReferences(r.Context(), tx, "comment", comment.ID, comment.Body)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events := []model.Event{}
	if snapshot != nil {
		snapshotEvent, err := s.appendEvent(r.Context(), tx, owner.event(
			"artifact.version",
			actor,
			docs.ArtifactVersionEventPayload(anchor.ArtifactID, artifactName, snapshot.Version, nil, snapshot.Changes),
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
		payload, err := s.commentEventPayload(r.Context(), tx, *reopenedRoot, reopenedArtifactName, commentEventThread{}, model.ReferenceChanges{})
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
	payload, err := s.commentEventPayload(
		r.Context(), tx, comment, artifactName, threadTarget.eventThread(turn), referenceChanges,
	)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload.SuppressRoute = suppressRoute
	payload.SuppressedAuthors = suppressedAuthors
	payload.SuppressedRoute = suppressedRoute
	payload.SuppressedRouteSessionID = suppressedRouteSessionID
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
	var advice *writeAdvice
	if owner.IssueKey != nil && status != nil {
		advice = s.writeAdvice(
			r.Context(), tx, "POST /api/v1/issues/{key}/comments", *owner.IssueKey, actor, "", *status,
		)
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if snapshot != nil {
		s.deps.Docs.CommitVersion(anchor.ArtifactID, snapshot.Version)
	}
	s.publishDocumentEvents(documentEvents, events...)
	for _, mention := range resolvedMentions {
		attempt, err := s.deliverResolvedCommentMention(r.Context(), comment, event, mention, actor, true)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		comment.Deliveries = append(comment.Deliveries, attempt)
	}
	WriteJSON(w, http.StatusCreated, withAdvice(comment, advice))
}
