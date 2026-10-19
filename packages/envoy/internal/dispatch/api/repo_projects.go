package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *server) listRepoProjects(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select `+repoProjectColumns+`
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
	WriteJSON(w, http.StatusOK, mappings)
}

// putRepoProject maps a repository to a project. Default, when given, sets or clears the mapping as
// its project's default, and setting it demotes the project's previous default in the same
// transaction; left out, the mapping keeps its default while its project is unchanged, and a mapping
// moved to another project is never that project's default unless the body says so. A mapping
// issues name as their repository is not moved (409 REPO_PROJECT_IN_USE). Every mapping the write
// changes emits settings.repo_project.updated on its project, and a moved mapping emits one on the
// project it left too, deleted:true there. lockRepoProjectWrite orders the write against every
// other one.
func (s *server) putRepoProject(w http.ResponseWriter, r *http.Request) {
	repo, err := repoProjectPath(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var input struct {
		Project string       `json:"project"`
		Default *bool        `json:"default"`
		Actor   *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	input.Project = strings.TrimSpace(input.Project)
	if !projectKeyPattern.MatchString(input.Project) {
		writeError(w, "INVALID_REPO_PROJECT", http.StatusBadRequest, projectKeyProblem("project", input.Project))
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
	previous, err := lockRepoProjectWrite(r.Context(), tx, s.deps.Events, repo, actor, input.Project)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	moved := previous != nil && previous.Project != input.Project
	if moved {
		if err := refuseRepoProjectInUse(r.Context(), tx, *previous); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	isDefault := previous != nil && !moved && previous.Default
	if input.Default != nil {
		isDefault = *input.Default
	}
	var changed []model.RepoProject
	if isDefault {
		rows, err := tx.Query(r.Context(), `
			update repo_projects set is_default = false
			where project = $1 and is_default and repo <> $2
			returning `+repoProjectColumns, input.Project, repo)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		changed, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.RepoProject, error) { return scanRepoProject(row) })
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	mapping, err := scanRepoProject(tx.QueryRow(r.Context(), `
		insert into repo_projects (repo, project, created_by, is_default)
		values ($1, $2, $3, $4)
		on conflict (repo) do update set project = excluded.project, created_by = excluded.created_by, is_default = excluded.is_default
		returning `+repoProjectColumns, repo, input.Project, actorJSON, isDefault))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var events []model.Event
	appendMappingEvent := func(mapping model.RepoProject, deleted bool) bool {
		event, err := s.appendEvent(r.Context(), tx, repoProjectEvent(mapping, actor, deleted))
		if err != nil {
			s.writeHandlerError(w, err)
			return false
		}
		events = append(events, event)
		return true
	}
	for _, demoted := range changed {
		if !appendMappingEvent(demoted, false) {
			return
		}
	}
	if moved && !appendMappingEvent(*previous, true) {
		return
	}
	if !appendMappingEvent(mapping, false) {
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(events...)
	WriteJSON(w, http.StatusOK, mapping)
}

// deleteRepoProject removes a mapping no issue names as its repository; one that issues name is
// refused (409 REPO_PROJECT_IN_USE).
func (s *server) deleteRepoProject(w http.ResponseWriter, r *http.Request) {
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
	previous, err := lockRepoProjectWrite(r.Context(), tx, s.deps.Events, repo, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if previous == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := refuseRepoProjectInUse(r.Context(), tx, *previous); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `delete from repo_projects where repo = $1`, repo); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, repoProjectEvent(*previous, actor, true))
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

// lockRepoProjectWrite takes, in this order, every lock a write to repo's mapping needs, and returns
// the mapping as it stands under them (nil when repo has none). projects are the projects the write
// may put the mapping in.
//
//   - repo's own advisory lock, which every writer of the mapping takes whether or not its row
//     exists yet, so the mapping's project and its existence hold still from here on (a default
//     switch in its project may still clear its is_default until the project lock below is held,
//     and the row read after that lock sees it);
//   - the event owners of the mapping's current project and of projects, in one LockOwners call
//     before the first event, which serializes each project's default switch and orders two
//     moves between the same projects;
//   - the mapping's row, for update, which conflicts with the key-share lock an issue write naming
//     it takes (checkIssueRepoMapped and the issues foreign key), so the count
//     refuseRepoProjectInUse reads after it holds until the write commits.
//
// An issue create locks its project before the mapping too (createIssue), so the two never wait on
// each other in a circle. Every writer of a mapping goes through here: the PUT, the DELETE and the
// boot seed (SeedRepoProjects).
func lockRepoProjectWrite(ctx context.Context, tx pgx.Tx, broker *events.Broker, repo string, actor model.Actor, projects ...string) (*model.RepoProject, error) {
	if _, err := tx.Exec(ctx, `
		select pg_advisory_xact_lock(hashtext('dispatch-repo-project'), hashtext($1))
	`, repo); err != nil {
		return nil, fmt.Errorf("lock repository mapping %s: %w", repo, err)
	}
	var current string
	err := tx.QueryRow(ctx, `select project from repo_projects where repo = $1`, repo).Scan(&current)
	switch {
	case err == nil:
		projects = append(projects, current)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	if len(projects) == 0 {
		return nil, nil
	}
	owners := make([]model.Event, 0, len(projects))
	for _, project := range projects {
		owners = append(owners, projectOwner(project).event("", actor, nil))
	}
	if err := broker.LockOwners(ctx, tx, owners...); err != nil {
		return nil, err
	}
	mapping, err := scanRepoProject(tx.QueryRow(ctx, `
		select `+repoProjectColumns+` from repo_projects where repo = $1 for update
	`, repo))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mapping, nil
}

// SeedRepoProjects adds the DISPATCH_REPO_PROJECTS mappings (`owner/repo=KEY,...`) whose
// repository has no mapping yet, at boot; a mapping already stored, set in the dashboard, wins.
// Each repository is seeded in its own transaction under a mapping write's locks
// (lockRepoProjectWrite), so a seed racing a PUT or DELETE of the same repository waits for it and
// then sees what it left. A mapping the seed adds emits settings.repo_project.updated on its project,
// deleted:false, as the PUT's does, actor the environment session; a repository already mapped, in
// the seeded project or another, gets no write and no event. A project that does not exist refuses
// the seed, naming the repository and the project.
func SeedRepoProjects(ctx context.Context, database *store.Store, broker *events.Broker, raw string) error {
	mappings, err := ParseRepoProjects(raw)
	if err != nil {
		return err
	}
	repos := make([]string, 0, len(mappings))
	for repo := range mappings {
		repos = append(repos, repo)
	}
	slices.Sort(repos)
	for _, repo := range repos {
		if err := seedRepoProject(ctx, database, broker, repo, mappings[repo]); err != nil {
			return fmt.Errorf("seed repository project %q: %w", repo, err)
		}
	}
	return nil
}

func seedRepoProject(ctx context.Context, database *store.Store, broker *events.Broker, repo, project string) error {
	actor := model.Actor{Kind: "session", ID: "environment"}
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		return err
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from projects where key = $1)`, project).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("project %s does not exist", project)
	}
	previous, err := lockRepoProjectWrite(ctx, tx, broker, repo, actor, project)
	if err != nil || previous != nil {
		return err
	}
	mapping, err := scanRepoProject(tx.QueryRow(ctx, `
		insert into repo_projects (repo, project, created_by)
		values ($1, $2, $3)
		returning `+repoProjectColumns, repo, project, actorJSON))
	if err != nil {
		return err
	}
	event, err := broker.Append(ctx, tx, repoProjectEvent(mapping, actor, false))
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	broker.Publish(event)
	return nil
}

// repoProjectEvent is settings.repo_project.updated for one mapping, on the mapping's project:
// deleted when the mapping left that project (removed, or moved to another).
func repoProjectEvent(mapping model.RepoProject, actor model.Actor, deleted bool) model.Event {
	return projectOwner(mapping.Project).event(
		"settings.repo_project.updated", actor, map[string]any{"mapping": mapping, "deleted": deleted},
	)
}

// refuseRepoProjectInUse refuses to delete or move a mapping that issues of its project name as
// their repository (409 REPO_PROJECT_IN_USE, naming how many); the issues foreign key would refuse it
// without saying which.
func refuseRepoProjectInUse(ctx context.Context, tx pgx.Tx, mapping model.RepoProject) error {
	var count int
	if err := tx.QueryRow(ctx, `
		select count(*) from issues where repo = $1 and project_key = $2
	`, mapping.Repo, mapping.Project).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errorf(http.StatusConflict, "REPO_PROJECT_IN_USE", "%d issue(s) of %s name %s as their repository; change their repo first", count, mapping.Project, mapping.Repo)
	}
	return nil
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

// repoProjectColumns are the columns scanRepoProject reads, in its order.
const repoProjectColumns = `repo, project, is_default, created_by, created_at`

func scanRepoProject(row rowScanner) (model.RepoProject, error) {
	var mapping model.RepoProject
	var createdBy []byte
	if err := row.Scan(&mapping.Repo, &mapping.Project, &mapping.Default, &createdBy, &mapping.CreatedAt); err != nil {
		return model.RepoProject{}, err
	}
	if err := json.Unmarshal(createdBy, &mapping.CreatedBy); err != nil {
		return model.RepoProject{}, fmt.Errorf("decode repository project author: %w", err)
	}
	return mapping, nil
}
