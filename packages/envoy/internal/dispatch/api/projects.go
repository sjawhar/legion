package api

import (
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func (s *server) listProjects(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select p.key, p.name, (
			select count(*)
			from asks a
			left join issues i on i.key = a.issue_key
			left join artifacts ar on ar.id = a.artifact_id
			where a.state = 'open'
			  and coalesce(i.project_key, ar.project_key) = p.key
			  and (i.key is null or i.closed_at is null)
		), p.created_at
		from projects p
		order by p.key
	`)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	projects := []model.Project{}
	for rows.Next() {
		var project model.Project
		if err := rows.Scan(&project.Key, &project.Name, &project.OpenAsks, &project.CreatedAt); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *server) createProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Key   string       `json:"key"`
		Name  string       `json:"name"`
		Actor *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if actor.Kind != "user" {
		writeError(w, "HUMAN_ONLY", http.StatusForbidden, "projects may only be created by a user")
		return
	}
	input.Key = strings.TrimSpace(input.Key)
	input.Name = strings.TrimSpace(input.Name)
	if !projectKeyPattern.MatchString(input.Key) || input.Name == "" {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, "project key and name are required")
		return
	}
	var project model.Project
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		insert into projects (key, name) values ($1, $2) returning key, name, created_at
	`, input.Key, input.Name).Scan(&project.Key, &project.Name, &project.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			writeError(w, "PROJECT_EXISTS", http.StatusConflict, "a project with this key already exists")
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}
