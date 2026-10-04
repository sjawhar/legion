package api

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// An agent's conversation owns the files and images sent in it (LEGION-541): the third artifact
// owner beside an issue and a project, addressed dispatch://agent/<session id>/artifact/<slug>.
// It holds no document, appends no event and takes no lock but its own (storeArtifact).

// agentOwnsFilesOnly is the refusal of a markdown document uploaded to an agent's conversation.
const agentOwnsFilesOnly = "an agent's conversation holds files and images; a document belongs to an issue or a project"

// requestSessionID is the path's session_id, held to the rule every reference to an agent's
// artifact is (text.IsSessionID), so every owner an upload accepts is one a reference can name.
func requestSessionID(r *http.Request) (string, error) {
	session := r.PathValue("session_id")
	if !text.IsSessionID(session) {
		return "", errorf(http.StatusBadRequest, "INVALID_SESSION_ID",
			"session_id %q is not a session id: it must be non-empty and hold no /, ?, #, whitespace, control character or <>\"'`[]|", session)
	}
	return session, nil
}

// POST /api/v1/agents/{session_id}/artifacts
func (s *server) uploadAgentArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	session, err := requestSessionID(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.uploadArtifactFor(w, r, artifactTarget{Session: session})
}

// GET /api/v1/agents/{session_id}/artifacts
func (s *server) listAgentArtifacts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	session, err := requestSessionID(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select `+artifactColumns+`
		from artifacts where session_id = $1 order by created_at desc, id desc
	`, session)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifacts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Artifact, error) {
		return scanArtifact(row)
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifacts == nil {
		artifacts = []model.Artifact{}
	}
	for index := range artifacts {
		versions, err := s.loadVersions(r.Context(), s.deps.Store.Pool, artifacts[index].ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		artifacts[index].Versions = versions
	}
	WriteJSON(w, http.StatusOK, artifacts)
}
