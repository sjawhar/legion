package api

import (
	"net/http"
)

func (s *server) listProjectArtifacts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project := r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	unlinked := r.URL.Query().Get("unlinked") == "true"
	query := `
		select ` + artifactColumns + `
		from artifacts
		where project_key = $1 and not is_primary`
	if unlinked {
		query += ` and issue_key is null`
	}
	query += ` order by created_at, id`
	rows, err := s.deps.Store.Pool.Query(r.Context(), query, project)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifacts, err := collectArtifacts(rows)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := loadArtifactDetails(r.Context(), s.deps.Store.Pool, artifactPointers(artifacts)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, artifacts)
}

func (s *server) uploadProjectArtifact(w http.ResponseWriter, r *http.Request) {
	s.uploadArtifactFor(w, r, artifactTarget{Project: r.PathValue("key")})
}
