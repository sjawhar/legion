package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func (s *server) listArchitectureSources(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select `+architecture.SourceColumns+`
		from architecture_sources
		order by project_key
	`)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()

	sources := []model.ArchitectureSource{}
	for rows.Next() {
		source, err := architecture.ScanSource(rows)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, sources)
}

func (s *server) getArchitectureSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	source, err := architecture.ScanSource(s.deps.Store.Pool.QueryRow(r.Context(), `
		select `+architecture.SourceColumns+`
		from architecture_sources
		where project_key = $1
	`, r.PathValue("key")))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "SOURCE_NOT_FOUND", http.StatusNotFound, "no architecture source configured for "+r.PathValue("key"))
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, source)
}

func (s *server) putArchitectureSource(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	var input struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	owner, name, err := parseSourceRepo(input.Repo)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	branch := strings.TrimSpace(input.Branch)
	if branch == "" {
		writeError(w, "SOURCE_INPUT", http.StatusBadRequest, "branch is required")
		return
	}
	if err := s.deps.Store.Pool.QueryRow(r.Context(), "select key from projects where key = $1", key).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	// The access check runs before any write: a failed re-PUT leaves the stored
	// source untouched.
	check, err := s.deps.GitHub.CheckSource(r.Context(), owner, name)
	if err != nil {
		if errors.Is(err, githubapp.ErrNoAppKey) || errors.Is(err, githubapp.ErrNoInstallation) || errors.Is(err, githubapp.ErrNoContentsRead) {
			writeError(w, "SOURCE_ACCESS", http.StatusConflict, err.Error())
			return
		}
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
	source, err := architecture.ScanSource(tx.QueryRow(r.Context(), `
		insert into architecture_sources (project_key, repo, branch, enabled, installation_id, created_by)
		values ($1, $2, $3, true, $4, $5)
		on conflict (project_key) do update set
			repo = excluded.repo,
			branch = excluded.branch,
			enabled = true,
			installation_id = excluded.installation_id,
			created_by = excluded.created_by,
			last_sync_at = null,
			last_commit = null,
			last_tree_sha = null,
			last_error = null
		returning `+architecture.SourceColumns,
		key, owner+"/"+name, branch, check.InstallationID, actorJSON))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(key).event(
		"settings.architecture_source.updated", actor, map[string]any{"source": source, "deleted": false},
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
	WriteJSON(w, http.StatusOK, source)
}

// deleteArchitectureSource removes the source and, in the same transaction,
// the model it projected: the project's components and their dependency
// edges (graph_edges loses its component arms with them). Snapshots stay as
// history.
func (s *server) deleteArchitectureSource(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	source, err := architecture.ScanSource(tx.QueryRow(r.Context(), `
		delete from architecture_sources where project_key = $1
		returning `+architecture.SourceColumns,
		key))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "SOURCE_NOT_FOUND", http.StatusNotFound, "no architecture source configured for "+key)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	// component_depends cascades from components.
	if _, err := tx.Exec(r.Context(), `delete from components where project_key = $1`, key); err != nil {
		s.writeHandlerError(w, fmt.Errorf("retire architecture components: %w", err))
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(key).event(
		"settings.architecture_source.updated", actor, map[string]any{"source": source, "deleted": true},
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

// syncArchitectureSource imports the project's architecture model now, inline,
// and answers the updated source row. A model or upstream failure is a 200
// whose row carries last_error: the HTTP call itself succeeded and the
// previous projection is still up. Only a credential- or configuration-shaped
// failure — no App key, no installation, no Contents: read, no such branch —
// is a 409 the caller must act on.
func (s *server) syncArchitectureSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	source, err := s.deps.Architecture.Sync(r.Context(), r.PathValue("key"))
	if err == nil {
		WriteJSON(w, http.StatusOK, source)
		return
	}
	if errors.Is(err, architecture.ErrNoSource) {
		writeError(w, "SOURCE_NOT_FOUND", http.StatusNotFound, "no architecture source configured for "+r.PathValue("key"))
		return
	}
	if source.Project == "" {
		// Sync could not even record the failure on the row: the database, not
		// the model or GitHub, is what broke.
		s.writeHandlerError(w, err)
		return
	}
	if architecture.IsAccessFailure(err) {
		writeError(w, "SOURCE_ACCESS", http.StatusConflict, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, source)
}

// parseSourceRepo validates and canonicalizes an owner/name repository input
// the same way repository mappings are stored (lowercase, .git trimmed). The
// segments are restricted to GitHub's own charset so a stored repo can never
// smuggle path segments into an outbound API URL (".." would traverse).
var sourceRepoOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)
var sourceRepoNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func parseSourceRepo(raw string) (owner, name string, err error) {
	trimmed := strings.TrimSpace(raw)
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || !sourceRepoOwnerPattern.MatchString(parts[0]) ||
		!sourceRepoNamePattern.MatchString(parts[1]) || parts[1] == "." || parts[1] == ".." {
		return "", "", errorf(http.StatusBadRequest, "SOURCE_INPUT", "repository must be owner/name, got %q", raw)
	}
	canonical := strings.SplitN(canonicalRepo(parts[0], parts[1]), "/", 2)
	// canonicalRepo trims a trailing ".git", so the canonical name needs the same
	// empty/"."/".." check as the raw input ("legion/...git" canonicalizes to "..").
	if canonical[1] == "" || canonical[1] == "." || canonical[1] == ".." {
		return "", "", errorf(http.StatusBadRequest, "SOURCE_INPUT", "repository must be owner/name, got %q", raw)
	}
	return canonical[0], canonical[1], nil
}
