package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The Delivery page's session groups (LEGION-682): a group is a stable id, a name and the agent
// sessions it holds, and a pull request whose first session is a member shows the group as its
// Parent agent (api/delivery.go). The timeline carries every group, so these routes only write.
// Access is the delivery settings' rule, any signed-in person or agent (putDeliverySettings), and,
// as there, no event is appended: a group has no project, issue or artifact to own one.

// createDeliverySessionGroup answers POST /api/v1/delivery/session-groups.
func (s *server) createDeliverySessionGroup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name     string       `json:"name"`
		Sessions []string     `json:"sessions"`
		Actor    *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		writeError(w, "SESSION_GROUP_INPUT", http.StatusBadRequest, "name is required")
		return
	}
	sessions, ok := sessionGroupSessions(w, "sessions", input.Sessions)
	if !ok {
		return
	}
	if len(sessions) == 0 {
		writeError(w, "SESSION_GROUP_INPUT", http.StatusBadRequest, "sessions must name at least one session")
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	group, err := delivery.CreateSessionGroup(r.Context(), tx, name, sessions, actor)
	if err != nil {
		s.writeSessionGroupError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, group)
}

// updateDeliverySessionGroup answers PATCH /api/v1/delivery/session-groups/{id}: a new name,
// sessions to add and sessions to remove, each optional.
func (s *server) updateDeliverySessionGroup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name   *string      `json:"name"`
		Add    []string     `json:"add"`
		Remove []string     `json:"remove"`
		Actor  *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	id, ok := sessionGroupID(w, r)
	if !ok {
		return
	}
	update := delivery.SessionGroupUpdate{}
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" {
			writeError(w, "SESSION_GROUP_INPUT", http.StatusBadRequest, "name must not be blank")
			return
		}
		update.Name = &name
	}
	if update.Add, ok = sessionGroupSessions(w, "add", input.Add); !ok {
		return
	}
	if update.Remove, ok = sessionGroupSessions(w, "remove", input.Remove); !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	group, err := delivery.UpdateSessionGroup(r.Context(), tx, id, update, actor)
	if err != nil {
		s.writeSessionGroupError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, group)
}

// deleteDeliverySessionGroup answers DELETE /api/v1/delivery/session-groups/{id}: the group and
// its memberships go, and its sessions show as themselves again.
func (s *server) deleteDeliverySessionGroup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor *model.Actor `json:"actor"`
	}
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	if _, ok := s.requireActor(w, r, input.Actor); !ok {
		return
	}
	id, ok := sessionGroupID(w, r)
	if !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err := delivery.DeleteSessionGroup(r.Context(), tx, id); err != nil {
		s.writeSessionGroupError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sessionGroupID reads the {id} path value, refusing one that is not a uuid before it reaches the
// table's uuid column, where the cast would answer 500.
func sessionGroupID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, "SESSION_GROUP_ID_INPUT", http.StatusBadRequest, "session group ids are UUIDs")
		return "", false
	}
	return id, true
}

// sessionGroupSessions trims each session id the field names and drops repeats, refusing a blank
// one.
func sessionGroupSessions(w http.ResponseWriter, field string, values []string) ([]string, bool) {
	sessions := make([]string, 0, len(values))
	for _, value := range values {
		session := strings.TrimSpace(value)
		if session == "" {
			writeError(w, "SESSION_GROUP_INPUT", http.StatusBadRequest, field+" holds a blank session id")
			return nil, false
		}
		if !slices.Contains(sessions, session) {
			sessions = append(sessions, session)
		}
	}
	return sessions, true
}

// writeSessionGroupError answers a group write's refusal with its code, naming what it refers to.
func (s *server) writeSessionGroupError(w http.ResponseWriter, err error) {
	var inGroup *delivery.SessionInGroupError
	var taken *delivery.SessionGroupNameTakenError
	var titled *delivery.SessionGroupNameIsTitleError
	switch {
	case errors.Is(err, delivery.ErrSessionGroupNotFound):
		writeError(w, "NOT_FOUND", http.StatusNotFound, err.Error())
	case errors.Is(err, delivery.ErrSessionGroupEmpty):
		writeError(w, "SESSION_GROUP_INPUT", http.StatusBadRequest, err.Error())
	case errors.As(err, &inGroup):
		writeError(w, "SESSION_IN_GROUP", http.StatusConflict, err.Error())
	case errors.As(err, &taken):
		writeError(w, "SESSION_GROUP_NAME_TAKEN", http.StatusConflict, err.Error())
	case errors.As(err, &titled):
		writeError(w, "SESSION_GROUP_NAME_IS_SESSION_TITLE", http.StatusConflict, err.Error())
	default:
		s.writeHandlerError(w, err)
	}
}
