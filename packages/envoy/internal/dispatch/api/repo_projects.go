package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *server) listRepoProjects(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select repo, project, created_by, created_at
		from repo_projects
		order by repo
	`)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()

	mappings := []model.RepoProject{}
	for rows.Next() {
		mapping, err := scanRepoProject(rows)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		mappings = append(mappings, mapping)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mappings)
}

func (s *server) putRepoProject(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	repo, err := repoProjectPath(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var input struct {
		Project string `json:"project"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	input.Project = strings.TrimSpace(input.Project)
	if !projectKeyPattern.MatchString(input.Project) {
		writeError(w, "INVALID_REPO_PROJECT", http.StatusBadRequest, "project is required")
		return
	}
	if err := s.deps.Store.Pool.QueryRow(r.Context(), "select key from projects where key = $1", input.Project).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actorJSON, err := encodeJSON(actor)
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
	mapping, err := scanRepoProject(tx.QueryRow(r.Context(), `
		insert into repo_projects (repo, project, created_by)
		values ($1, $2, $3)
		on conflict (repo) do update set project = excluded.project, created_by = excluded.created_by
		returning repo, project, created_by, created_at
	`, repo, input.Project, actorJSON))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(mapping.Project).event(
		"settings.repo_project.updated", actor, map[string]any{"mapping": mapping, "deleted": false},
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
	writeJSON(w, http.StatusOK, mapping)
}

func (s *server) deleteRepoProject(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	repo, err := repoProjectPath(r)
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
	mapping, err := scanRepoProject(tx.QueryRow(r.Context(), `
		delete from repo_projects where repo = $1
		returning repo, project, created_by, created_at
	`, repo))
	if err != nil {
		if err == pgx.ErrNoRows {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(mapping.Project).event(
		"settings.repo_project.updated", actor, map[string]any{"mapping": mapping, "deleted": true},
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
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) repoProject(ctx context.Context, repo string) (string, error) {
	var project string
	err := s.deps.Store.Pool.QueryRow(ctx, "select project from repo_projects where repo = $1", repo).Scan(&project)
	if err == nil {
		return project, nil
	}
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return "", err
}

func repoProjectPath(r *http.Request) (string, error) {
	repo, _, err := externalRef(r.PathValue("owner") + "/" + r.PathValue("repo") + "#1")
	return repo, err
}

func scanRepoProject(row rowScanner) (model.RepoProject, error) {
	var mapping model.RepoProject
	var createdBy []byte
	if err := row.Scan(&mapping.Repo, &mapping.Project, &createdBy, &mapping.CreatedAt); err != nil {
		return model.RepoProject{}, err
	}
	if err := json.Unmarshal(createdBy, &mapping.CreatedBy); err != nil {
		return model.RepoProject{}, fmt.Errorf("decode repository project author: %w", err)
	}
	return mapping, nil
}
