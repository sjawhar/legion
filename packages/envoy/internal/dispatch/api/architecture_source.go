package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A project imports its component model from any number of sources, one per repository, and a
// source is addressed by its project and repository, as a repository mapping is. Every route here
// is authAny: agents save, remove and refresh sources as people do, and a bearer save or removal
// names its session in actor.

// listArchitectureSources answers every source of every project, by project and then in the
// order each project's sources were added.
func (s *server) listArchitectureSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	sources, err := querySources(r.Context(), s.deps.Store.Pool, `order by project_key, id`)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, sources)
}

// listProjectArchitectureSources answers the project's sources in the order they were added. A
// project with none answers [], since whether a project has a model is a question with an
// ordinary negative answer; a project that does not exist is still a 404, so a mistyped key is
// reported rather than read as "no model".
func (s *server) listProjectArchitectureSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if err := s.deps.Store.Pool.QueryRow(r.Context(), "select key from projects where key = $1", key).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	sources, err := querySources(r.Context(), s.deps.Store.Pool, `where project_key = $1 order by id`, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, sources)
}

// querySources reads the source rows the tail (a where and order by clause) selects.
func querySources(ctx context.Context, q queryer, tail string, args ...any) ([]model.ArchitectureSource, error) {
	rows, err := q.Query(ctx, `select `+architecture.SourceColumns+` from architecture_sources `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := []model.ArchitectureSource{}
	for rows.Next() {
		source, err := architecture.ScanSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// putArchitectureSource adds the repository in the path as a source of the project, or changes
// the branch of the source it already is. A re-save keeps the source's id and its components and
// resets its sync bookkeeping, so the next sync re-imports from the new branch.
func (s *server) putArchitectureSource(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var input struct {
		Branch string       `json:"branch"`
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
	owner, name, err := sourcePathRepo(r)
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
		on conflict (project_key, repo) do update set
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

// deleteArchitectureSource removes the source and, in the same transaction, the model it
// projected: its components (by the components foreign key's cascade) and their dependency
// edges (graph_edges loses their component arms with them). Every other source's components stay,
// and snapshots stay as history.
func (s *server) deleteArchitectureSource(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor *model.Actor `json:"actor"`
	}
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	key := r.PathValue("key")
	owner, name, err := sourcePathRepo(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo := owner + "/" + name
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	source, err := architecture.ScanSource(tx.QueryRow(r.Context(), `
		delete from architecture_sources where project_key = $1 and repo = $2
		returning `+architecture.SourceColumns,
		key, repo))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeSourceNotFound(w, key, repo)
			return
		}
		s.writeHandlerError(w, err)
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

// syncArchitectureSource imports one source's model now, inline, and answers its updated row. A
// model or upstream failure is a 200 whose row carries last_error: the HTTP call itself succeeded
// and the source's previous projection is still up. Only a credential- or configuration-shaped
// failure — no App key, no installation, no Contents: read, no such branch — is a 409 the caller
// must act on.
func (s *server) syncArchitectureSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	owner, name, err := sourcePathRepo(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo := owner + "/" + name
	source, err := s.deps.Architecture.Sync(r.Context(), key, repo)
	switch {
	case err == nil:
		WriteJSON(w, http.StatusOK, source)
	case errors.Is(err, architecture.ErrNoSource):
		writeSourceNotFound(w, key, repo)
	case source.Project == "":
		// Sync could not even record the failure on the row: the database, not
		// the model or GitHub, is what broke.
		s.writeHandlerError(w, err)
	case architecture.IsAccessFailure(err):
		writeError(w, "SOURCE_ACCESS", http.StatusConflict, err.Error())
	default:
		WriteJSON(w, http.StatusOK, source)
	}
}

// syncArchitectureSources imports every source of the project now, inline, each attempted
// whatever an earlier one did, and answers every row in the order the sources were added. Each
// failure, an access failure included, is on its own row's last_error; the call answers 200
// because the import was attempted and recorded. Only a failure that could not be recorded on its
// row is a 500.
func (s *server) syncArchitectureSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	results, err := s.deps.Architecture.SyncProject(r.Context(), key)
	if errors.Is(err, architecture.ErrNoSource) || (err == nil && len(results) == 0) {
		writeError(w, "SOURCE_NOT_FOUND", http.StatusNotFound, "no architecture source configured for "+key)
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	sources := make([]model.ArchitectureSource, 0, len(results))
	for _, result := range results {
		if result.Err != nil && result.Source.Project == "" {
			s.writeHandlerError(w, result.Err)
			return
		}
		sources = append(sources, result.Source)
	}
	WriteJSON(w, http.StatusOK, sources)
}

func writeSourceNotFound(w http.ResponseWriter, key, repo string) {
	writeError(w, "SOURCE_NOT_FOUND", http.StatusNotFound, "no architecture source from "+repo+" configured for "+key)
}

// sourcePathRepo reads the {owner}/{repo} path segments as parseSourceRepo reads a repository.
func sourcePathRepo(r *http.Request) (owner, name string, err error) {
	return parseSourceRepo(r.PathValue("owner") + "/" + r.PathValue("repo"))
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
