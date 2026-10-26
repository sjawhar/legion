package api

import (
	"context"
	"errors"
	"log/slog"
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
	sources, err := querySources(r.Context(), s.deps.Store.Pool, `where project_key = $1 order by id`, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if len(sources) == 0 {
		if err := s.deps.Store.Pool.QueryRow(r.Context(), "select key from projects where key = $1", key).Scan(new(string)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	WriteJSON(w, http.StatusOK, sources)
}

// getArchitectureSource answers the singular read that dispatch clients older than this release
// make on every issue create: the project's first source in id order, or null when it has none
// (they read only whether it is null), and 404 for a project that does not exist. The CLI and the
// server ship separately, so a removed route turned every create into false "Could not check"
// advice (#1522 fixed the same skew); it stays until no such client is left.
func (s *server) getArchitectureSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	slog.Warn("dispatch: GET /api/v1/projects/{key}/architecture-source called by a dispatch client older than this release; upgrade it, the route will be removed", "project", key)
	sources, err := querySources(r.Context(), s.deps.Store.Pool, `where project_key = $1 order by id limit 1`, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if len(sources) == 0 {
		if err := s.deps.Store.Pool.QueryRow(r.Context(), "select key from projects where key = $1", key).Scan(new(string)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, nil)
		return
	}
	WriteJSON(w, http.StatusOK, sources[0])
}

// querySources reads the source rows the tail (a where and order by clause) selects.
func querySources(ctx context.Context, q queryer, tail string, args ...any) ([]model.ArchitectureSource, error) {
	rows, err := q.Query(ctx, `select `+architecture.SourceColumns+` from architecture_sources `+tail, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.ArchitectureSource, error) { return architecture.ScanSource(row) })
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

	// The access check, of the repository and of the branch, runs before any write: a value that
	// is not exactly the name of a branch the repository has (a commit SHA, a relative ref such as
	// main~1, HEAD, a refs/ path) is refused like a missing branch, and a failed re-PUT leaves the
	// stored source untouched.
	installationID, err := s.checkArchitectureSource(r.Context(), owner, name, branch)
	if err != nil {
		if errors.Is(err, githubapp.ErrNoAppKey) || errors.Is(err, githubapp.ErrNoInstallation) || errors.Is(err, githubapp.ErrNoContentsRead) || errors.Is(err, githubapp.ErrNoBranch) {
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
		key, owner+"/"+name, branch, installationID, actorJSON))
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

// checkArchitectureSource proves the GitHub App can read owner/name and that branch is the name of
// a branch it has, and answers the installation that reads it. The sync refuses a missing branch
// the same way, so a save that skipped the branch would store a source no sync can import. It
// reads the branch's exact ref (githubapp.Client.Branch), not the commit the sync's Ref resolves:
// GitHub's commits read takes any commit-ish, so a SHA or main~1 would pass it and be stored as
// the source's branch.
func (s *server) checkArchitectureSource(ctx context.Context, owner, name, branch string) (int64, error) {
	check, err := s.deps.GitHub.CheckSource(ctx, owner, name)
	if err != nil {
		return 0, err
	}
	token, err := s.deps.GitHub.Token(ctx, check.InstallationID)
	if err != nil {
		return 0, err
	}
	if _, err := s.deps.GitHub.Branch(ctx, token, owner, name, branch); err != nil {
		return 0, err
	}
	return check.InstallationID, nil
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
	sources, ok := s.syncProjectSources(w, r)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, sources)
}

// syncSingularArchitectureSource answers the singular sync that `dispatch architecture-sync`
// from dispatch clients older than this release makes. It imports every source as
// syncArchitectureSources does and answers one row in the shape those clients read: the first
// row in id order whose last_error is set, or the first row when none failed, so a failure on any
// source reaches them. A project with no source is a 404 SOURCE_NOT_FOUND, and a failure that
// could not be recorded is a 500, as before. An access failure (no installation, no Contents:
// read, no such branch) was a 409 SOURCE_ACCESS; it is now recorded on its row, so it answers
// 200 with that row's last_error, which those clients print as "Sync failed for ...". It stays
// until no such client is left.
func (s *server) syncSingularArchitectureSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	slog.Warn("dispatch: POST /api/v1/projects/{key}/architecture-source/sync called by a dispatch client older than this release; upgrade it, the route will be removed", "project", r.PathValue("key"))
	sources, ok := s.syncProjectSources(w, r)
	if !ok {
		return
	}
	answer := sources[0]
	for _, source := range sources {
		if source.LastError != nil {
			answer = source
			break
		}
	}
	WriteJSON(w, http.StatusOK, answer)
}

// syncProjectSources imports every source of the request's project and answers the rows in id
// order, at least one. When there is nothing to answer it writes the error itself and reports
// false: 404 SOURCE_NOT_FOUND for a project with no source, and the handler error for a failure
// that could not be recorded on its row.
func (s *server) syncProjectSources(w http.ResponseWriter, r *http.Request) ([]model.ArchitectureSource, bool) {
	key := r.PathValue("key")
	results, err := s.deps.Architecture.SyncProject(r.Context(), key)
	if errors.Is(err, architecture.ErrNoSource) || (err == nil && len(results) == 0) {
		writeError(w, "SOURCE_NOT_FOUND", http.StatusNotFound, "no architecture source configured for "+key)
		return nil, false
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return nil, false
	}
	sources := make([]model.ArchitectureSource, 0, len(results))
	for _, result := range results {
		if result.Err != nil && result.Source.Project == "" {
			s.writeHandlerError(w, result.Err)
			return nil, false
		}
		sources = append(sources, result.Source)
	}
	return sources, true
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
