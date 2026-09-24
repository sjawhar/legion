package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// ResolvedMention binds a canonical mention to the session that owned it when the comment was
// created. SessionID is nil when no live holder was found; attemptSessionID remains internal so
// legacy message attempts retain their existing session-id representation.
type ResolvedMention struct {
	Target       string
	Delivery     string
	SessionID    *string
	ResolveError string

	title            string
	attemptSessionID string
}

// resolveMentionTargets performs the only registry lookup for a comment's original deliveries.
// The result is persisted in the comment-created event and handed to the post-commit sender, so a
// role transition cannot change the target after routing suppression has been decided.
func (s *server) resolveMentionTargets(ctx context.Context, targets []string, delivery string) []ResolvedMention {
	resolved := make([]ResolvedMention, 0, len(targets))
	for _, target := range targets {
		item := ResolvedMention{Target: target, Delivery: delivery}
		route, err := model.ParseRoute(target)
		if err != nil {
			item.ResolveError = err.Error()
			resolved = append(resolved, item)
			continue
		}
		sessionID, title, resolveError := s.resolveDeliveryTarget(ctx, route, delivery)
		item.title = title
		item.attemptSessionID = sessionID
		item.ResolveError = resolveError
		// A missing session is not a resolved target, even though the legacy message delivery
		// model retains its requested id in an attempt row. A live session that lacks a mode is
		// still resolved and participates in the event's session-level suppression calculation.
		if sessionID != "" && !strings.HasPrefix(resolveError, "no live session ") {
			item.SessionID = new(sessionID)
		}
		resolved = append(resolved, item)
	}
	return resolved
}

func (s *server) sendResolvedDelivery(ctx context.Context, target ResolvedMention, body, idempotencyKey string, urgency *string, frame []byte) (*string, string) {
	if target.ResolveError != "" {
		return nil, target.ResolveError
	}
	if target.SessionID == nil {
		return nil, "no live session"
	}
	expectsReply := "optional"
	if target.Delivery == "btw" {
		expectsReply = "required"
	}
	send, err := s.deps.Envoy.Send(ctx, dispatchenvoy.SendInput{
		TargetSession:  *target.SessionID,
		Message:        body,
		Payload:        frame,
		IdempotencyKey: idempotencyKey,
		Urgency:        urgencyValue(urgency),
		ExpectsReply:   expectsReply,
	})
	if err != nil {
		return nil, deliveryErrorText(err)
	}
	return &send.EnvelopeID, ""
}

func (s *server) deliverResolvedCommentMention(
	ctx context.Context,
	comment model.Comment,
	created model.Event,
	target ResolvedMention,
	actor model.Actor,
	initial bool,
) (model.CommentDelivery, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.CommentDelivery{}, err
	}
	defer tx.Rollback(ctx)
	stored, err := s.lockedComment(ctx, tx, comment.ID)
	if err != nil {
		return model.CommentDelivery{}, err
	}
	if err := tx.QueryRow(ctx, `
		select target from comment_mentions where comment_id = $1 and target = $2 for update
	`, stored.ID, target.Target).Scan(new(string)); err != nil {
		return model.CommentDelivery{}, err
	}
	var attempt model.CommentDelivery
	pending := true
	if err := tx.QueryRow(ctx, `
		select comment_id::text, target, attempt, delivery, session_id, envelope_id, state, error, resolve_error, reply_id::text, created_at
		from comment_deliveries
		where comment_id = $1 and target = $2 and state = 'pending'
		order by attempt
		limit 1
		for update
	`, stored.ID, target.Target).Scan(
		&attempt.CommentID, &attempt.Target, &attempt.Attempt, &attempt.Delivery, &attempt.SessionID,
		&attempt.EnvelopeID, &attempt.State, &attempt.Error, &attempt.ResolveError, &attempt.ReplyID, &attempt.CreatedAt,
	); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return model.CommentDelivery{}, err
		}
		pending = false
		if initial {
			if err := tx.QueryRow(ctx, `
				select comment_id::text, target, attempt, delivery, session_id, envelope_id, state, error, resolve_error, reply_id::text, created_at
				from comment_deliveries
				where comment_id = $1 and target = $2 and attempt = 1
			`, stored.ID, target.Target).Scan(
				&attempt.CommentID, &attempt.Target, &attempt.Attempt, &attempt.Delivery, &attempt.SessionID,
				&attempt.EnvelopeID, &attempt.State, &attempt.Error, &attempt.ResolveError, &attempt.ReplyID, &attempt.CreatedAt,
			); err != nil {
				return model.CommentDelivery{}, err
			}
			if attempt.State == "sent" || attempt.State == "failed" {
				if err := tx.Commit(ctx); err != nil {
					return model.CommentDelivery{}, err
				}
				return attempt, nil
			}
			return model.CommentDelivery{}, fmt.Errorf("initial comment delivery %s/%s is no longer pending", stored.ID, target.Target)
		}
		if err := tx.QueryRow(ctx, `
			select coalesce(max(attempt), 0) + 1
			from comment_deliveries where comment_id = $1 and target = $2
		`, stored.ID, target.Target).Scan(&attempt.Attempt); err != nil {
			return model.CommentDelivery{}, err
		}
	}
	if pending {
		target.Delivery = attempt.Delivery
		target.SessionID = attempt.SessionID
		if attempt.ResolveError != nil {
			target.ResolveError = *attempt.ResolveError
		}
		if attempt.SessionID != nil {
			target.attemptSessionID = *attempt.SessionID
		}
		// The resolution above was made before the comment-creation transaction committed (or,
		// for a genuine retry, at an even earlier comment-creation moment); the actual send
		// always happens after that commit, whether this is the synchronous post-commit
		// completion of a fresh comment or a much later retry. That gap is real either way — a
		// session's advertised capabilities and liveness are live state that changes on every
		// registration — so both callers must recheck, not just retries. The routing decision —
		// which session this pinned attempt targets — stays fixed, exactly as resolveMentionTargets
		// already guarantees against a role transition redirecting an in-flight delivery; only
		// whether that pinned session can still receive this delivery mode right now is
		// re-derived, through the same resolveMentionTargets/resolveDeliveryTarget every other
		// send uses, by resolving it as a direct session target so no role lookup (and no chance
		// of re-picking a different holder) is involved.
		if attempt.SessionID != nil {
			resolved := s.resolveMentionTargets(ctx, []string{"session:" + *attempt.SessionID}, attempt.Delivery)[0]
			target.SessionID = resolved.SessionID
			target.ResolveError = resolved.ResolveError
			target.attemptSessionID = resolved.attemptSessionID
			target.title = resolved.title
		}
	} else {
		target = s.resolveMentionTargets(ctx, []string{target.Target}, target.Delivery)[0]
	}
	frame, err := json.Marshal(struct {
		Event    model.Event `json:"event"`
		Delivery any         `json:"delivery"`
	}{
		Event: created,
		Delivery: map[string]any{
			"attempt":    attempt.Attempt,
			"mode":       target.Delivery,
			"comment_id": stored.ID,
			"target":     target.Target,
		},
	})
	if err != nil {
		return model.CommentDelivery{}, fmt.Errorf("encode comment mention delivery frame: %w", err)
	}
	envelopeID, deliveryError := s.sendResolvedDelivery(
		ctx, target, stored.Body, stored.ID+":"+target.Target+":"+fmt.Sprint(attempt.Attempt), nil, frame,
	)
	state := "sent"
	if deliveryError != "" {
		state = "failed"
	}
	attempt.CommentID = stored.ID
	attempt.Target = target.Target
	attempt.Delivery = target.Delivery
	attempt.SessionID = target.SessionID
	attempt.EnvelopeID = envelopeID
	attempt.State = state
	attempt.Error = nil
	if deliveryError != "" {
		attempt.Error = &deliveryError
	}
	if target.ResolveError != "" {
		attempt.ResolveError = &target.ResolveError
	}
	if pending {
		if err := tx.QueryRow(ctx, `
			update comment_deliveries
			set delivery = $4, session_id = $5, envelope_id = $6, state = $7, error = $8
			where comment_id = $1 and target = $2 and attempt = $3 and state = 'pending'
			returning comment_id::text, target, attempt, delivery, session_id, envelope_id, state, error, resolve_error, reply_id::text, created_at
		`, attempt.CommentID, attempt.Target, attempt.Attempt, attempt.Delivery, attempt.SessionID, attempt.EnvelopeID, attempt.State, attempt.Error).Scan(
			&attempt.CommentID, &attempt.Target, &attempt.Attempt, &attempt.Delivery, &attempt.SessionID,
			&attempt.EnvelopeID, &attempt.State, &attempt.Error, &attempt.ResolveError, &attempt.ReplyID, &attempt.CreatedAt,
		); err != nil {
			if initial && errors.Is(err, pgx.ErrNoRows) {
				answered, err := loadCommentDelivery(ctx, tx, attempt.CommentID, attempt.Target, attempt.Attempt)
				if err != nil {
					return model.CommentDelivery{}, err
				}
				if answered.State == "sent" || answered.State == "failed" {
					if err := tx.Commit(ctx); err != nil {
						return model.CommentDelivery{}, err
					}
					return answered, nil
				}
			}
			return model.CommentDelivery{}, err
		}
	} else if err := tx.QueryRow(ctx, `
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, envelope_id, state, error, resolve_error)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		returning comment_id::text, target, attempt, delivery, session_id, envelope_id, state, error, resolve_error, reply_id::text, created_at
	`, attempt.CommentID, attempt.Target, attempt.Attempt, attempt.Delivery, attempt.SessionID, attempt.EnvelopeID, attempt.State, attempt.Error, attempt.ResolveError).Scan(
		&attempt.CommentID, &attempt.Target, &attempt.Attempt, &attempt.Delivery, &attempt.SessionID,
		&attempt.EnvelopeID, &attempt.State, &attempt.Error, &attempt.ResolveError, &attempt.ReplyID, &attempt.CreatedAt,
	); err != nil {
		return model.CommentDelivery{}, err
	}
	event, err := s.appendEvent(ctx, tx, ownerOf(stored.IssueKey, stored.ArtifactID).event(
		"comment.delivery",
		actor,
		model.CommentDeliveryEventPayload{
			CommentID: stored.ID, AskID: stored.AskID, Target: target.Target, Attempt: attempt.Attempt,
			Delivery: target.Delivery, SessionID: target.SessionID, State: state, Error: deliveryError,
		},
	))
	if err != nil {
		return model.CommentDelivery{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.CommentDelivery{}, err
	}
	s.publish(event)
	return attempt, nil
}

func (s *server) loadCommentCreatedEvent(ctx context.Context, q queryer, commentID string) (model.Event, error) {
	var event model.Event
	var actor, payload []byte
	if err := q.QueryRow(ctx, `
		select id, issue_key, artifact_id::text, coalesce(project_key, ''), seq, type, actor, notify, created_at, payload
		from events
		where type = 'comment.created' and payload ->> 'id' = $1
		order by id
		limit 1
	`, commentID).Scan(
		&event.ID, &event.IssueKey, &event.ArtifactID, &event.Project, &event.Seq, &event.Type,
		&actor, &event.Notify, &event.CreatedAt, &payload,
	); err != nil {
		return model.Event{}, err
	}
	if err := json.Unmarshal(actor, &event.Actor); err != nil {
		return model.Event{}, fmt.Errorf("decode comment-created event actor: %w", err)
	}
	if err := json.Unmarshal(payload, &event.Payload); err != nil {
		return model.Event{}, fmt.Errorf("decode comment-created event payload: %w", err)
	}
	return event, nil
}

func loadCommentDelivery(ctx context.Context, q queryer, commentID, target string, attemptNumber int) (model.CommentDelivery, error) {
	var attempt model.CommentDelivery
	if err := q.QueryRow(ctx, `
		select comment_id::text, target, attempt, delivery, session_id, envelope_id, state, error, resolve_error, reply_id::text, created_at
		from comment_deliveries
		where comment_id = $1 and target = $2 and attempt = $3
	`, commentID, target, attemptNumber).Scan(
		&attempt.CommentID, &attempt.Target, &attempt.Attempt, &attempt.Delivery, &attempt.SessionID,
		&attempt.EnvelopeID, &attempt.State, &attempt.Error, &attempt.ResolveError, &attempt.ReplyID, &attempt.CreatedAt,
	); err != nil {
		return model.CommentDelivery{}, err
	}
	return attempt, nil
}

func (s *server) createCommentDelivery(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	var input struct {
		Actor    *model.Actor `json:"actor"`
		Target   *string      `json:"target"`
		Delivery string       `json:"delivery"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !human {
		if actor, err = bearerSessionActor(actor, input.Actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if !validDelivery(input.Delivery) {
		writeError(w, "MENTION_INPUT", http.StatusBadRequest, "delivery must be one of btw, aside, steer")
		return
	}
	comment, err := s.loadComment(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment not found")
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select target from comment_mentions where comment_id = $1 order by target
	`, comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	targets := []string{}
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows.Close()
	if len(targets) == 0 {
		writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment has no mentions")
		return
	}
	target := ""
	if input.Target == nil {
		if len(targets) != 1 {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target is required when a comment has multiple mentions")
			return
		}
		target = targets[0]
	} else {
		target = *input.Target
		found := false
		for _, candidate := range targets {
			if candidate == target {
				found = true
				break
			}
		}
		if !found {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target must name one of the comment mentions")
			return
		}
	}
	created, err := s.loadCommentCreatedEvent(r.Context(), s.deps.Store.Pool, comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	resolved := ResolvedMention{Target: target, Delivery: input.Delivery}
	attempt, err := s.deliverResolvedCommentMention(r.Context(), comment, created, resolved, actor, false)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, attempt)
}

func (s *server) replyComment(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "reply requires an agent bearer token")
		return
	}
	var input struct {
		Actor   *model.Actor `json:"actor"`
		Target  *string      `json:"target"`
		Attempt int          `json:"attempt"`
		Body    *string      `json:"body"`
		Error   *string      `json:"error"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if actor.Kind != "session" || input.Attempt < 1 || (input.Body == nil) == (input.Error == nil) {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "reply requires one body or error for a delivery attempt")
		return
	}
	if input.Body != nil && strings.TrimSpace(*input.Body) == "" {
		writeError(w, "MENTION_INPUT", http.StatusBadRequest, "comment body is required")
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	comment, err := s.lockedComment(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment not found")
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	rows, err := tx.Query(r.Context(), `
		select target from comment_mentions where comment_id = $1 order by target for update
	`, comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	targets := []string{}
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			rows.Close()
			s.writeHandlerError(w, err)
			return
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.writeHandlerError(w, err)
		return
	}
	rows.Close()
	target := ""
	if input.Target == nil {
		if len(targets) != 1 {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target is required when a comment has multiple mentions")
			return
		}
		target = targets[0]
	} else {
		target = *input.Target
		found := false
		for _, candidate := range targets {
			if candidate == target {
				found = true
				break
			}
		}
		if !found {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target must name one of the comment mentions")
			return
		}
	}

	var attempt model.CommentDelivery
	if err := tx.QueryRow(r.Context(), `
		select comment_id::text, target, attempt, delivery, session_id, envelope_id, state, error, resolve_error, reply_id::text, created_at
		from comment_deliveries where comment_id = $1 and target = $2 and attempt = $3 for update
	`, comment.ID, target, input.Attempt).Scan(
		&attempt.CommentID, &attempt.Target, &attempt.Attempt, &attempt.Delivery, &attempt.SessionID,
		&attempt.EnvelopeID, &attempt.State, &attempt.Error, &attempt.ResolveError, &attempt.ReplyID, &attempt.CreatedAt,
	); err != nil {
		writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment delivery not found")
		return
	}
	if attempt.SessionID == nil || *attempt.SessionID != actor.ID {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "session may reply only to its own delivery")
		return
	}
	if attempt.ReplyID != nil {
		reply, err := s.loadComment(r.Context(), tx, *attempt.ReplyID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, reply)
		return
	}
	if attempt.State == "failed" && input.Error != nil {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, attempt)
		return
	}
	if input.Error != nil {
		if err := tx.QueryRow(r.Context(), `
			update comment_deliveries
			set state = 'failed', error = $4
			where comment_id = $1 and target = $2 and attempt = $3
			returning comment_id::text, target, attempt, delivery, session_id, envelope_id, state, error, resolve_error, reply_id::text, created_at
		`, comment.ID, attempt.Target, input.Attempt, *input.Error).Scan(
			&attempt.CommentID, &attempt.Target, &attempt.Attempt, &attempt.Delivery, &attempt.SessionID,
			&attempt.EnvelopeID, &attempt.State, &attempt.Error, &attempt.ResolveError, &attempt.ReplyID, &attempt.CreatedAt,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		event, err := s.appendEvent(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID).event(
			"comment.delivery",
			actor,
			model.CommentDeliveryEventPayload{
				CommentID: comment.ID, AskID: comment.AskID, Target: attempt.Target, Attempt: attempt.Attempt,
				Delivery: attempt.Delivery, SessionID: attempt.SessionID, State: attempt.State,
				Error: *attempt.Error, ReplyID: attempt.ReplyID,
			},
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
		WriteJSON(w, http.StatusOK, attempt)
		return
	}
	author, err := json.Marshal(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A callback reply joins its thread exactly as `POST .../comments` would: the shared climb
	// walks `reply_to` up to the head of the thread, so a reply anywhere under an ask stores
	// that ask's `ask_id` - the column every ask read, the Inbox row and the ask card select
	// on - instead of a bare `reply_to` none of them can see. The comment it answers is
	// already loaded and locked, so the climb starts there rather than re-reading it.
	thread, err := s.threadHeadOf(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID), comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A callback reply requests no turn, so it hands the turn back the way any session reply
	// with no `turn` does.
	turn := thread.replyTurn(actor, nil)
	reply, err := scanComment(tx.QueryRow(r.Context(), `
		insert into comments (issue_key, artifact_id, author, body, reply_to, ask_id, turn)
		values ($1, $2, $3, $4, $5, $6, $7)
		returning `+commentColumns+`
	`, comment.IssueKey, comment.ArtifactID, author, *input.Body, thread.ReplyTo, thread.AskID, turn))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if thread.AskID != nil {
		if err := asks.FollowAuthor(r.Context(), tx, *thread.AskID, actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	referenceChanges, err := s.replaceReferences(r.Context(), tx, "comment", reply.ID, reply.Body)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update comment_deliveries set reply_id = $4, state = 'sent', error = null
		where comment_id = $1 and target = $2 and attempt = $3
	`, comment.ID, attempt.Target, input.Attempt, reply.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifactName, err := s.commentArtifactName(r.Context(), tx, reply)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload, err := s.commentEventPayload(
		r.Context(), tx, reply, artifactName, thread.eventThread(turn), referenceChanges,
	)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerOf(reply.IssueKey, reply.ArtifactID).event(
		"comment.answered", actor, payload,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Stamp(r.Context(), tx, "comment", reply.ID, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusCreated, reply)
}
