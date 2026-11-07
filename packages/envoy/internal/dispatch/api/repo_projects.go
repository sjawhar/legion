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
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// putRepoProjectInput is the body of PUT /api/v1/settings/repo-projects/{owner}/{repo}.
type putRepoProjectInput struct {
	Project   string       `json:"project" required:"true"`
	Default   *bool        `json:"default"`
	RenamedTo *string      `json:"renamed_to"`
	Actor     *model.Actor `json:"actor"`
}

// putRepoProject maps a repository to a project. Default, when given, sets or clears the mapping as
// its project's default, and setting it demotes the project's previous default in the same
// transaction; left out, the mapping keeps its default while its project is unchanged, and a mapping
// moved to another project is never that project's default unless the body says so. A mapping
// issues name as their repository is not moved (409 REPO_PROJECT_IN_USE). Every mapping the write
// changes emits settings.repo_project.updated on its project, and a moved mapping emits one on the
// project it left too, deleted:true there. lockRepoProjectWrite orders the write against every
// other one.
//
// Renamed_to is the full `owner/name` this repository is called today, canonicalised as the
// route's own path is (externalRef). **Absent, and JSON null, leave the stored pair exactly as
// it is**, as Default does, and `""` clears it: the page and the client that write this route
// send `{project, default}` and nothing else (packages/dispatch/web/src/api/client.ts,
// putRepoProject), so clearing on absence would drop an operator's rename every time someone
// moved a mapping's project or its default in the dashboard. A pair naming this mapping's own
// repository, one naming a repository that is itself renamed, and one on a repository another
// mapping is already renamed to are each refused (400 INVALID_REPO_PROJECT), so the canonical
// name of a repository is reached in one step and no cycle exists (refs.CanonicalRepo).
func (s *server) putRepoProject(w http.ResponseWriter, r *http.Request) {
	repo, err := repoProjectPath(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var input putRepoProjectInput
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
	renamedTo, err := parseRepoRename(repo, input.RenamedTo)
	if err != nil {
		s.writeHandlerError(w, err)
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
	// An absent or null renamed_to keeps the pair the mapping already holds, and a row this
	// write creates holds none; `""` clears one, which adds no edge to the rename graph and so
	// needs no check of it.
	switch {
	case input.RenamedTo == nil:
		if previous != nil {
			renamedTo = previous.RenamedTo
		}
	case renamedTo != nil:
		if err := refuseRepoRenameChain(r.Context(), tx, repo, *renamedTo); err != nil {
			s.writeHandlerError(w, err)
			return
		}
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
		insert into repo_projects (repo, project, created_by, is_default, renamed_to)
		values ($1, $2, $3, $4, $5)
		on conflict (repo) do update set project = excluded.project, created_by = excluded.created_by,
			is_default = excluded.is_default, renamed_to = excluded.renamed_to
		returning `+repoProjectColumns, repo, input.Project, actorJSON, isDefault, renamedTo))
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
	var input actorInput
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

// parseRepoRename reads the `renamed_to` of a mapping write: nil for an absent or null field,
// which leaves the stored pair alone, nil for `""`, which clears it, and otherwise the
// canonical `owner/name` the value names, validated the way this route validates its own path
// (externalRef). A pair naming the mapping's own repository is refused here, where the one
// cycle a single row can hold is cheapest to see; the database refuses it too, so direct SQL is
// held to it (migration 20261105075455_repo_projects_renamed_to).
func parseRepoRename(repo string, raw *string) (*string, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	canonical, _, err := externalRef(strings.TrimSpace(*raw) + "#1")
	if err != nil {
		return nil, errorf(http.StatusBadRequest, "INVALID_REPO_PROJECT", `renamed_to must be "owner/name" (got %q)`, *raw)
	}
	if canonical == repo {
		return nil, errorf(http.StatusBadRequest, "INVALID_REPO_PROJECT", "%s cannot be renamed to itself", repo)
	}
	return &canonical, nil
}

// refuseRepoRenameChain holds the rename graph to one step for a write that sets the pair
// repo → renamedTo: it refuses a target that is itself renamed, and refuses renaming a
// repository some other mapping is already renamed to. Either would put the canonical name of
// a repository two steps away, which no reader walks (refs.CanonicalRepo takes one step), and
// together they make a cycle impossible. The target's own step is read through
// refs.CanonicalRepo, the rule itself, so what this route refuses and what every reader resolves
// are one answer: a target that canonicalises to itself is the end of the chain, compared
// without case as every comparison of a repository is, since the column's own check refuses only
// a rename to the identical spelling.
//
// It takes one lock over the whole graph first, since the rows it must hold still are not this
// mapping's own: the writer already holds its own mapping's locks by here
// (lockRepoProjectWrite) and takes no other mapping's, so this lock is always the last one
// taken and two renames cannot wait on each other.
func refuseRepoRenameChain(ctx context.Context, tx pgx.Tx, repo, renamedTo string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('dispatch-repo-rename'), 0)`); err != nil {
		return fmt.Errorf("lock the repository renames: %w", err)
	}
	var pointedAt string
	switch err := tx.QueryRow(ctx, `
		select repo from repo_projects where lower(renamed_to) = lower($1) limit 1
	`, repo).Scan(&pointedAt); {
	case err == nil:
		return errorf(http.StatusBadRequest, "INVALID_REPO_PROJECT",
			"%s is what %s was renamed to, so it cannot be renamed to %s as well; point %s at the new name instead",
			repo, pointedAt, renamedTo, pointedAt)
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	onwards, err := refs.CanonicalRepo(ctx, tx, renamedTo)
	if err != nil {
		return err
	}
	if strings.EqualFold(onwards, renamedTo) {
		return nil
	}
	return errorf(http.StatusBadRequest, "INVALID_REPO_PROJECT",
		"%s was itself renamed to %s, so %s cannot be renamed to it; name %s instead", renamedTo, onwards, repo, onwards)
}

// repoProjectColumns are the columns scanRepoProject reads, in its order.
const repoProjectColumns = `repo, project, is_default, created_by, created_at, renamed_to`

func scanRepoProject(row rowScanner) (model.RepoProject, error) {
	var mapping model.RepoProject
	var createdBy []byte
	if err := row.Scan(&mapping.Repo, &mapping.Project, &mapping.Default, &createdBy, &mapping.CreatedAt, &mapping.RenamedTo); err != nil {
		return model.RepoProject{}, err
	}
	if err := json.Unmarshal(createdBy, &mapping.CreatedBy); err != nil {
		return model.RepoProject{}, fmt.Errorf("decode repository project author: %w", err)
	}
	return mapping, nil
}
