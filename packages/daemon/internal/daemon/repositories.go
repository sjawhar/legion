package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// repositories is the project's repository mapping as the daemon serves it: Dispatch's mapping of
// the project (Settings → Repositories → Projects), read at boot and on its
// settings.repo_project.updated, each repository served once both GitHub Apps are installed on it
// and every other one left out with the reason, for legion state. The mapping also files people's
// issues, so it can name a repository that lacks one of Legion's Apps; leaving it out costs only its
// issues' admission. Readers take the read lock; the readers of the mapping swap it whole under the
// write lock.
type repositories struct {
	project string

	mu       sync.RWMutex
	accepted map[string]mappedRepository
	refused  map[string]refusedRepository
}

// mappedRepository is a repository Legion serves, as GitHub names it, and whether it is the
// project's default in Dispatch.
type mappedRepository struct {
	Repo    ghrepo.Repository
	Default bool
}

// refusedRepository is a mapped repository Legion leaves out, as Dispatch maps it, and why.
type refusedRepository struct {
	Repo   string
	Reason string
}

func newRepositories(project string) *repositories {
	return &repositories{project: project, accepted: map[string]mappedRepository{}, refused: map[string]refusedRepository{}}
}

// set replaces the mapping with accepted and refused, and reports the repositories that became
// served and those that stopped being served, by name.
func (r *repositories) set(accepted []mappedRepository, refused []refusedRepository) (added, removed []string) {
	next := make(map[string]mappedRepository, len(accepted))
	for _, mapped := range accepted {
		next[strings.ToLower(mapped.Repo.String())] = mapped
	}
	left := make(map[string]refusedRepository, len(refused))
	for _, refusal := range refused {
		left[strings.ToLower(refusal.Repo)] = refusal
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, mapped := range next {
		if _, ok := r.accepted[key]; !ok {
			added = append(added, mapped.Repo.String())
		}
	}
	for key, mapped := range r.accepted {
		if _, ok := next[key]; !ok {
			removed = append(removed, mapped.Repo.String())
		}
	}
	r.accepted, r.refused = next, left
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

// Owners is every served repository's owner, lowercased, sorted and once each: the owners a role's
// GitHub credential holds a directory for.
func (r *repositories) Owners() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	owners := []string{}
	for _, mapped := range r.accepted {
		owners = append(owners, strings.ToLower(mapped.Repo.Owner()))
	}
	slices.Sort(owners)
	return slices.Compact(owners)
}

// Accepted is every served repository, as GitHub names it, sorted by name: what GitHub intake
// hears, beside the repositories a live record names.
func (r *repositories) Accepted() []ghrepo.Repository {
	mapped := r.Mapped()
	repos := make([]ghrepo.Repository, 0, len(mapped))
	for _, m := range mapped {
		repos = append(repos, m.Repo)
	}
	return repos
}

// Mapped is every served repository with whether it is the project's default, sorted by name.
func (r *repositories) Mapped() []mappedRepository {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mapped := make([]mappedRepository, 0, len(r.accepted))
	for _, m := range r.accepted {
		mapped = append(mapped, m)
	}
	slices.SortFunc(mapped, func(a, b mappedRepository) int {
		return strings.Compare(strings.ToLower(a.Repo.String()), strings.ToLower(b.Repo.String()))
	})
	return mapped
}

// Refused is every mapped repository Legion leaves out, with why, sorted by name.
func (r *repositories) Refused() []refusedRepository {
	r.mu.RLock()
	defer r.mu.RUnlock()
	refused := make([]refusedRepository, 0, len(r.refused))
	for _, refusal := range r.refused {
		refused = append(refused, refusal)
	}
	slices.SortFunc(refused, func(a, b refusedRepository) int { return strings.Compare(a.Repo, b.Repo) })
	return refused
}

// lookup is the served repository resolved names, in any spelling, or why Legion does not serve
// it: left out (leftOut) with its reason, or not mapped to the project at all.
func (r *repositories) lookup(resolved string) (mapped mappedRepository, refusal string, leftOut bool) {
	key := strings.ToLower(resolved)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if mapped, ok := r.accepted[key]; ok {
		return mapped, "", false
	}
	if refused, ok := r.refused[key]; ok {
		return mappedRepository{}, refused.Reason, true
	}
	return mappedRepository{}, fmt.Sprintf("%s is not mapped to project %s in Dispatch", resolved, r.project), false
}

// view is the mapping as the daemon's state shows it, with caches, the Kubernetes runtime's
// repository caches (none under tmux): each on the mapped repository it is for, matched whatever
// the case, and the rest, caches of repositories the mapping no longer names, listed apart.
func (r *repositories) view(caches []sandbox.CacheStatus) api.RepositoriesView {
	byRepo := map[string]sandbox.CacheStatus{}
	for _, cache := range caches {
		byRepo[strings.ToLower(cache.Repo.String())] = cache
	}
	view := api.RepositoriesView{Owners: r.Owners()}
	for _, mapped := range r.Mapped() {
		entry := api.MappedRepositoryView{Repo: mapped.Repo.String(), Default: mapped.Default}
		key := strings.ToLower(mapped.Repo.String())
		if cache, held := byRepo[key]; held {
			entry.Cache = new(cacheView(cache))
			delete(byRepo, key)
		}
		view.Mapped = append(view.Mapped, entry)
	}
	for _, refused := range r.Refused() {
		view.Refused = append(view.Refused, api.RefusedRepositoryView{Repo: refused.Repo, Reason: refused.Reason})
	}
	for _, cache := range caches {
		if _, unmapped := byRepo[strings.ToLower(cache.Repo.String())]; unmapped {
			view.Caches = append(view.Caches, cacheView(cache))
		}
	}
	return view
}

// cacheView is a repository cache as the state shows it.
func cacheView(cache sandbox.CacheStatus) api.CacheView {
	view := api.CacheView{
		Repo: cache.Repo.String(), Sandbox: cache.Sandbox, Ready: cache.Ready, Address: cache.Address,
		HealthError: cache.HealthError, RetainedUntil: cache.RetainedUntil,
	}
	if health := cache.Health; health != nil {
		view.Head, view.LastFetchAt, view.Bytes = health.Head, health.LastFetchAt, health.Bytes
	}
	return view
}

// readMapping reads Dispatch's repository mapping of project and checks, for each repository, that
// both GitHub Apps are installed on it (installs, under each App's JWT; per repository, since an
// installation on an owner may cover only some of its repositories). A repository an App is not
// installed on is refused, naming the App; any read that fails is an error that fails the whole
// read, so nothing is left out on a guess. Accepted repositories carry Dispatch's spelling until
// nameAccepted reads GitHub's.
func readMapping(ctx context.Context, client dispatch.Client, project string, installs appauth.Installations) ([]mappedRepository, []refusedRepository, error) {
	mappings, err := client.ListRepoProjects(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read the repository mapping from Dispatch: %w", err)
	}
	var accepted []mappedRepository
	var refused []refusedRepository
	for _, mapping := range mappings {
		if mapping.Project != project {
			continue
		}
		repo, err := ghrepo.Parse("the repository Dispatch maps to "+project, mapping.Repo)
		if err != nil {
			refused = append(refused, refusedRepository{Repo: mapping.Repo, Reason: err.Error()})
			continue
		}
		var missing []string
		for _, role := range appauth.Roles {
			installed, err := installs.InstalledOn(ctx, role, repo)
			if err != nil {
				return nil, nil, fmt.Errorf("check the %s App's installation on %s: %w", role, repo, err)
			}
			if !installed {
				missing = append(missing, string(role))
			}
		}
		switch len(missing) {
		case 0:
		case 1:
			refused = append(refused, refusedRepository{Repo: mapping.Repo, Reason: fmt.Sprintf("the %s App is not installed on %s", missing[0], repo)})
			continue
		default:
			refused = append(refused, refusedRepository{Repo: mapping.Repo, Reason: fmt.Sprintf("the %s Apps are not installed on %s", strings.Join(missing, " and "), repo)})
			continue
		}
		accepted = append(accepted, mappedRepository{Repo: repo, Default: mapping.Default})
	}
	return accepted, refused, nil
}

// nameAccepted is accepted as GitHub names each repository (named: its own spelling of owner and
// name, which its events and the subjects Envoy publishes them on carry), each read with its
// default branch as the implement App.
func nameAccepted(ctx context.Context, accepted []mappedRepository, named *defaultBranches) ([]mappedRepository, error) {
	out := make([]mappedRepository, 0, len(accepted))
	for _, mapped := range accepted {
		read, err := named.Named(ctx, mapped.Repo)
		if err != nil {
			return nil, err
		}
		out = append(out, mappedRepository{Repo: read.Name, Default: mapped.Default})
	}
	return out, nil
}

// loadMapping is the boot's read of the repository mapping, run again after a failure GitHub or
// Dispatch reports as its own trouble (appauth.TransientError, dispatch.Unreachable) on the App
// mint's schedule; any other failure refuses the boot. A project with no repository mapped refuses
// it too, as does one none of whose repositories both Apps are installed on: the workflow would
// have nothing to work. nameMapping, once the Apps' tokens are minted, reads GitHub's spelling.
func (w *workflowRuntime) loadMapping(ctx context.Context) error {
	var accepted []mappedRepository
	var refused []refusedRepository
	err := bootprobe.Run(ctx, "repository mapping read", appMintRetry, w.log, func(ctx context.Context) bootprobe.Outcome {
		attempt, cancel := context.WithTimeout(ctx, appMintAttempt)
		defer cancel()
		var err error
		accepted, refused, err = readMapping(attempt, w.dispatch, w.dispatchProject, w.installs)
		if err == nil {
			return bootprobe.Outcome{Passed: true}
		}
		var transient *appauth.TransientError
		if errors.As(err, &transient) || dispatch.Unreachable(err) {
			return bootprobe.Outcome{Detail: err.Error()}
		}
		return bootprobe.Outcome{Refusal: err}
	})
	if err != nil {
		return err
	}
	if len(accepted) == 0 && len(refused) == 0 {
		return fmt.Errorf("project %s has no repository mapped in Dispatch (Settings → Repositories → Projects)", w.dispatchProject)
	}
	if len(accepted) == 0 {
		reasons := make([]string, 0, len(refused))
		for _, refusal := range refused {
			reasons = append(reasons, refusal.Reason)
		}
		return fmt.Errorf("no repository mapped to project %s in Dispatch has both GitHub Apps installed (%s): install them, or map a repository they are installed on", w.dispatchProject, strings.Join(reasons, "; "))
	}
	w.repositories.set(accepted, refused)
	return nil
}

// nameMapping replaces the served repositories with GitHub's spelling of each, on the boot's
// retry, and logs the mapping the daemon serves.
func (w *workflowRuntime) nameMapping(ctx context.Context) error {
	var named []mappedRepository
	err := bootprobe.Run(ctx, "repository read", appMintRetry, w.log, func(ctx context.Context) bootprobe.Outcome {
		attempt, cancel := context.WithTimeout(ctx, appMintAttempt)
		defer cancel()
		var err error
		if named, err = nameAccepted(attempt, w.repositories.Mapped(), w.defaultBranches); err == nil {
			return bootprobe.Outcome{Passed: true}
		}
		var transient *appauth.TransientError
		var answer *githubrest.Answer
		if errors.As(err, &transient) || errors.As(err, &answer) && answer.Status >= 500 {
			return bootprobe.Outcome{Detail: err.Error()}
		}
		return bootprobe.Outcome{Refusal: err}
	})
	if err != nil {
		return err
	}
	w.repositories.set(named, w.repositories.Refused())
	w.log.Info("legion workflow boot stage", "stage", "repositories", "accepted", repositoryNames(w.repositories.Accepted()), "refused", refusalReasons(w.repositories.Refused()))
	return nil
}

// refreshMapping reads the repository mapping again, for Dispatch's settings.repo_project.updated
// on the project and for an issue on a left-out repository coming up for admission. It has GitHub
// intake hear every repository the new mapping serves and every repository a live record names,
// so a repository unmapped while its issues run is still heard, and only then publishes the new
// mapping: a refresh that fails before the subscription took publishes nothing, so a repository
// is never served, and an issue never recorded on it, while GitHub intake does not hear it, and
// the issue's next admission recheck refreshes again.
func (w *workflowRuntime) refreshMapping(ctx context.Context) error {
	w.mappingMu.Lock()
	defer w.mappingMu.Unlock()
	accepted, refused, err := readMapping(ctx, w.dispatch, w.dispatchProject, w.installs)
	if err != nil {
		return err
	}
	if accepted, err = nameAccepted(ctx, accepted, w.defaultBranches); err != nil {
		return err
	}
	served := make([]ghrepo.Repository, 0, len(accepted))
	for _, mapped := range accepted {
		served = append(served, mapped.Repo)
	}
	// The live-record snapshot, the subscription update and the publication run under the lock
	// every fact applies under (intake.SerializeFacts), which an admission holds from resolving an
	// issue's repository through committing its record: one that resolved a repository of the old
	// mapping commits before the snapshot reads it, so the repository it recorded stays heard, and
	// one that resolves after resolves against the new mapping.
	var heard []ghrepo.Repository
	var added, removed []string
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("serialize the mapping's publication with admission: %w", err)
		}
		var err error
		if heard, err = w.heardRepositories(ctx, tx, served); err != nil {
			return err
		}
		if len(heard) == 0 {
			w.log.Warn("legion workflow repositories: no repository is mapped or named by a live record; GitHub intake keeps the repositories it hears")
		} else if err := w.consumers.SetRepositories(ctx, heard); err != nil {
			return err
		}
		added, removed = w.repositories.set(accepted, refused)
		return nil
	}); err != nil {
		return err
	}
	w.log.Info("legion workflow repositories changed", "added", added, "removed", removed,
		"refused", refusalReasons(w.repositories.Refused()), "heard", repositoryNames(heard))
	return nil
}

// heardAtBoot is what GitHub intake hears when the boot opens it: the served repositories of the
// mapping the boot read and every repository a live record names.
func (w *workflowRuntime) heardAtBoot(ctx context.Context) ([]ghrepo.Repository, error) {
	var heard []ghrepo.Repository
	err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		heard, err = w.heardRepositories(ctx, tx, w.repositories.Accepted())
		return err
	})
	return heard, err
}

// heardRepositories is what GitHub intake hears: every repository of served and every repository
// a record of a live tree names, as tx reads the records, once each, sorted.
func (w *workflowRuntime) heardRepositories(ctx context.Context, tx pgx.Tx, served []ghrepo.Repository) ([]ghrepo.Repository, error) {
	heard := map[string]ghrepo.Repository{}
	for _, repo := range served {
		heard[strings.ToLower(repo.String())] = repo
	}
	issues, err := w.records.Issues(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("read the repositories live records name: %w", err)
	}
	for _, issue := range issues {
		if _, ok := heard[strings.ToLower(issue.Repo)]; ok {
			continue
		}
		root, err := w.records.Issue(ctx, tx, issue.Tree)
		if err != nil || root == nil {
			return nil, err
		}
		live, err := record.TreeLive(ctx, w.records, tx, *root)
		if err != nil {
			return nil, err
		}
		if !live {
			continue
		}
		repo, err := ghrepo.Parse("the repository of "+issue.Key, issue.Repo)
		if err != nil {
			return nil, err
		}
		heard[strings.ToLower(repo.String())] = repo
	}
	repos := make([]ghrepo.Repository, 0, len(heard))
	for _, repo := range heard {
		repos = append(repos, repo)
	}
	slices.SortFunc(repos, func(a, b ghrepo.Repository) int {
		return strings.Compare(strings.ToLower(a.String()), strings.ToLower(b.String()))
	})
	return repos, nil
}

// issueRepository is the repository issue's record names and its recorded default branch: what
// every launch of the issue's claims works (specs.repository) and the owner its commit identity is
// minted for (adoptIdentity). An issue the workflow does not record, such as an operator's claim,
// works no repository: the zero repository and no error.
func (w *workflowRuntime) issueRepository(ctx context.Context, key string) (ghrepo.Repository, string, error) {
	issue, err := w.recordOf(ctx, key)
	if err != nil || issue == nil {
		return ghrepo.Repository{}, "", err
	}
	repo, err := ghrepo.Parse("the repository of "+key, issue.Repo)
	if err != nil {
		return ghrepo.Repository{}, "", err
	}
	return repo, issue.DefaultBranch, nil
}

// recordOf is key's issue record, nil when the workflow does not record it.
func (w *workflowRuntime) recordOf(ctx context.Context, key string) (*record.Issue, error) {
	var issue *record.Issue
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = w.records.Issue(ctx, tx, key)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read the record of %s: %w", key, err)
	}
	return issue, nil
}

// prepareRepository readies a Dispatch issue event before it is applied (intake's
// ConsumerSpec.PrepareRepository), and is the event to apply. An issue's repository is its record's
// once recorded, so a recorded issue's event asks nothing of Dispatch or GitHub: a retained event
// that names no repository (ResolvedRepoMissing) takes the record's, and one naming another is
// applied as it is, for the workflow to keep the record's and say so. For an issue not yet
// recorded, a retained event's repository is read from Dispatch's issue detail, a read that fails
// leaving the event to be retried; then, for an event in todo, what the workflow reads in memory
// when it records the issue (ResolveRepository) is readied: a left-out repository's Apps are
// checked again when an issue on it comes up for admission (handed to Legion, or a child of a
// tree), and a served repository's default branch is read when the read Legion holds is older
// than its TTL. An unrecorded issue's event in any other status asks GitHub nothing.
func (w *workflowRuntime) prepareRepository(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
	recorded, err := w.recordOf(ctx, issue.Key)
	if err != nil {
		return issue, err
	}
	if recorded != nil {
		if issue.ResolvedRepoMissing {
			issue.ResolvedRepo, issue.ResolvedRepoMissing = recorded.Repo, false
		}
		return issue, nil
	}
	if issue.ResolvedRepoMissing {
		detail, err := w.dispatch.GetIssue(ctx, issue.Key)
		if err != nil {
			return issue, fmt.Errorf("read the repository Dispatch resolves for %s, which its event %d predates: %w", issue.Key, issue.Seq, err)
		}
		issue.ResolvedRepo, issue.ResolvedRepoMissing = detail.ResolvedRepo, false
	}
	if issue.Status != "todo" || issue.ResolvedRepo == "" {
		return issue, nil
	}
	if _, _, leftOut := w.repositories.lookup(issue.ResolvedRepo); leftOut && (issue.HandedOver || issue.Parent != "") {
		if err := w.refreshMapping(ctx); err != nil {
			return issue, fmt.Errorf("check the left-out repository %s again: %w", issue.ResolvedRepo, err)
		}
	}
	mapped, refusal, _ := w.repositories.lookup(issue.ResolvedRepo)
	if refusal != "" {
		return issue, nil
	}
	_, err = w.defaultBranches.Named(ctx, mapped.Repo)
	return issue, err
}

// ResolveRepository answers the workflow (workflow.Repositories) from memory: the served
// repository resolved names, as GitHub names it, at the default branch last read for it, or why
// Legion does not serve it. A served repository whose default branch was never read is an error:
// its event's preparation (prepareRepository) did not run.
func (w *workflowRuntime) ResolveRepository(resolved string) (workflow.Resolution, error) {
	mapped, refusal, leftOut := w.repositories.lookup(resolved)
	if refusal != "" {
		return workflow.Resolution{Refusal: refusal, LeftOut: leftOut}, nil
	}
	read, ok := w.defaultBranches.Kept(mapped.Repo.String())
	if !ok {
		return workflow.Resolution{}, fmt.Errorf("the default branch of %s was never read before an issue on it was recorded", mapped.Repo)
	}
	return workflow.Resolution{Repo: read.Name.String(), DefaultBranch: read.DefaultBranch}, nil
}

func repositoryNames(repos []ghrepo.Repository) []string {
	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		names = append(names, repo.String())
	}
	return names
}

func refusalReasons(refused []refusedRepository) []string {
	reasons := make([]string, 0, len(refused))
	for _, refusal := range refused {
		reasons = append(reasons, refusal.Reason)
	}
	return reasons
}
