package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Ask followers: the sessions an ask's answer, edits, resolution, and replies reach
// directly. Every session that writes to an ask follows it (asks.FollowAuthor at each
// insert site); these routes let a session leave or rejoin and a human curate the list.
// A bearer names its own session in the body and may act only on that session; a human
// (cookie identity) may add or remove any session and sends no body.

func parseAskID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		return "", errorf(http.StatusBadRequest, "ASK_ID_INPUT", "ask id must be a UUID")
	}
	return id, nil
}

// loadAskOwner returns the owner of an ask (its issue or unlinked document) or
// pgx.ErrNoRows when the ask does not exist.
func (s *server) loadAskOwner(ctx context.Context, q queryer, askID string) (owner, error) {
	var issueKey, artifactID *string
	if err := q.QueryRow(ctx, `
		select issue_key, artifact_id::text from asks where id = $1
	`, askID).Scan(&issueKey, &artifactID); err != nil {
		return owner{}, err
	}
	return ownerOf(issueKey, artifactID), nil
}

// GET /api/v1/asks/{id}/followers  (any authenticated actor)
func (s *server) listAskFollowers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	askID, err := parseAskID(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := s.loadAskOwner(r.Context(), s.deps.Store.Pool, askID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	followers, err := asks.Followers(r.Context(), s.deps.Store.Pool, askID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Followers []model.AskFollower `json:"followers"`
	}{Followers: followers})
}

// PUT /api/v1/asks/{id}/followers/{session_id}
func (s *server) followAsk(w http.ResponseWriter, r *http.Request) {
	s.changeAskFollower(w, r, true)
}

// DELETE /api/v1/asks/{id}/followers/{session_id}
func (s *server) unfollowAsk(w http.ResponseWriter, r *http.Request) {
	s.changeAskFollower(w, r, false)
}

func (s *server) changeAskFollower(w http.ResponseWriter, r *http.Request, follow bool) {
	askID, err := parseAskID(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	if sessionID == "" {
		writeError(w, "FOLLOWER_INPUT", http.StatusBadRequest, "follower session id is required")
		return
	}
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	if !human {
		var input struct {
			Actor *model.Actor `json:"actor"`
		}
		if err := decodeJSON(r, &input); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		actor, err = bearerSessionActor(actor, input.Actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if actor.ID != sessionID {
			writeError(w, "FOLLOWER_FORBIDDEN", http.StatusForbidden, "a session may follow or unfollow only itself")
			return
		}
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	own, err := s.loadAskOwner(r.Context(), tx, askID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.deps.Events.LockOwners(r.Context(), tx, own.event("", actor, nil)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	eventType := "ask.follower_added"
	var changed bool
	if follow {
		changed, err = asks.Follow(r.Context(), tx, askID, sessionID)
	} else {
		eventType = "ask.follower_removed"
		changed, err = asks.Unfollow(r.Context(), tx, askID, sessionID)
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !changed {
		if !follow {
			writeError(w, "FOLLOWER_NOT_FOUND", http.StatusNotFound, "session does not follow this ask")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, own.event(eventType, actor, model.AskFollowerEventPayload{
		AskID: askID, SessionID: sessionID, By: actor,
	}))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	w.WriteHeader(http.StatusNoContent)
}
