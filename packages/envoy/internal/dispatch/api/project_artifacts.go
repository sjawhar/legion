package api

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
		select id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at
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
	pointers := make([]*model.Artifact, len(artifacts))
	for index := range artifacts {
		versions, err := s.loadVersions(r.Context(), s.deps.Store.Pool, artifacts[index].ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		artifacts[index].Versions = versions
		pointers[index] = &artifacts[index]
	}
	if err := s.attachApprovals(r.Context(), s.deps.Store.Pool, pointers); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, artifacts)
}

func (s *server) uploadProjectArtifact(w http.ResponseWriter, r *http.Request) {
	s.uploadArtifactFor(w, r, artifactTarget{Project: r.PathValue("key")})
}
