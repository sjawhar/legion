package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
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
// issues' admission. unverified is each served repository a later read of the mapping could not
// read the installation of: it stays served on its last answer, and is read again
// (refreshOnMappingChange). Readers take the read lock; the readers of the mapping swap it whole
// under the write lock.
type repositories struct {
	project string

	mu         sync.RWMutex
	accepted   map[string]mappedRepository
	refused    map[string]refusedRepository
	unverified []string
}

// mappedRepository is a repository Legion serves, as GitHub names it, and whether it is the
// project's default in Dispatch.
type mappedRepository struct {
	Repo    ghrepo.Repository
	Default bool
}

// refusedRepository is a mapped repository Legion leaves out, as Dispatch maps it, and why.
// readErr is the first failed installation read when that alone is why (no App answered not
// installed): GitHub gave no answer, so the refusal is for now, and refreshOnMappingChange reads
// the mapping again until GitHub answers. mappedDefault is whether Dispatch maps it as the
// project's default.
type refusedRepository struct {
	Repo          string
	Reason        string
	readErr       error
	mappedDefault bool
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
// hears, beside the repositories the project's records name.
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
// it: left out (leftOut) with its reason, or not mapped to the project at all. unread is a
// repository left out for now because its installation read failed (refusedRepository.readErr).
func (r *repositories) lookup(resolved string) (mapped mappedRepository, refusal string, leftOut, unread bool) {
	key := strings.ToLower(resolved)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if mapped, ok := r.accepted[key]; ok {
		return mapped, "", false, false
	}
	if refused, ok := r.refused[key]; ok {
		return mappedRepository{}, refused.Reason, true, refused.readErr != nil
	}
	return mappedRepository{}, fmt.Sprintf("%s is not mapped to project %s in Dispatch", resolved, r.project), false, false
}

// setUnverified records the served repositories the last read of the mapping kept on their last
// answer, its installation read having failed.
func (r *repositories) setUnverified(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unverified = names
}

// kept is whether repo, in any spelling, is served on its last answer (setUnverified).
func (r *repositories) kept(repo string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.ContainsFunc(r.unverified, func(name string) bool { return strings.EqualFold(name, repo) })
}

// awaitingRead is every repository a failed installation read left as it stands, by reason: each
// left out for its failed read, and each served one kept on its last answer. While any is, the
// mapping is read again of the daemon's own (refreshOnMappingChange).
func (r *repositories) awaitingRead() []string {
	waiting := refusalReasons(readFailures(r.Refused()))
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, name := range r.unverified {
		waiting = append(waiting, name+": served on its last answer, its installation read having failed")
	}
	return waiting
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
		Repo: cache.Repo.String(), Sandbox: cache.Sandbox, State: cache.State, Reason: cache.Reason, Ready: cache.Ready,
		Address: cache.Address, HealthError: cache.HealthError, RetainedUntil: cache.RetainedUntil,
	}
	if health := cache.Health; health != nil {
		view.Head, view.LastFetchAt, view.Bytes = health.Head, health.LastFetchAt, health.Bytes
	}
	return view
}

// readMapping reads Dispatch's repository mapping (listMapping) and checks it for project against
// both GitHub Apps' installations (checkMapping).
func readMapping(ctx context.Context, client dispatch.Client, project string, installs appauth.Installations) ([]mappedRepository, []refusedRepository, error) {
	mappings, err := listMapping(ctx, client)
	if err != nil {
		return nil, nil, err
	}
	return checkMapping(ctx, mappings, project, installs)
}

// listMapping is Dispatch's repository mapping, every project's.
func listMapping(ctx context.Context, client dispatch.Client) ([]dispatch.RepoProject, error) {
	mappings, err := client.ListRepoProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the repository mapping from Dispatch: %w", err)
	}
	return mappings, nil
}

// checkMapping checks, for each repository mappings maps to project, that both GitHub Apps are
// installed on it (installs, under each App's JWT; per repository, since an installation on an
// owner may cover only some of its repositories). A repository an App is not installed on is
// refused, naming the App. A repository whose installation read fails, and that no App GitHub
// answered for lacks (any failure, a read longer than installReadAttempt among them), is refused
// for now, naming the App, the repository and the failure (refusedRepository.readErr), and the
// rest of the mapping is checked as it stands: one repository GitHub will not answer for leaves
// out only its own issues, whose admission waits for it (prepareUnrecorded), and
// refreshOnMappingChange reads it again until GitHub answers, so nothing is left out for good on
// a guess. The check fails only when ctx ends. Accepted
// repositories carry Dispatch's spelling until nameAccepted reads GitHub's. The installation
// reads run at once, mappingReads at a time, so a check takes about as long as its slowest read
// rather than the sum of them.
func checkMapping(ctx context.Context, mappings []dispatch.RepoProject, project string, installs appauth.Installations) ([]mappedRepository, []refusedRepository, error) {
	type mapped struct {
		mapping dispatch.RepoProject
		repo    ghrepo.Repository
		parse   error
		// installed and failed are each App's answer, in appauth.Roles order.
		installed []bool
		failed    []error
	}
	var entries []mapped
	for _, mapping := range mappings {
		if mapping.Project != project {
			continue
		}
		repo, err := ghrepo.Parse("the repository Dispatch maps to "+project, mapping.Repo)
		entries = append(entries, mapped{mapping: mapping, repo: repo, parse: err,
			installed: make([]bool, len(appauth.Roles)), failed: make([]error, len(appauth.Roles))})
	}
	var group errgroup.Group
	group.SetLimit(mappingReads)
	for _, m := range entries {
		if m.parse != nil {
			continue
		}
		for index, role := range appauth.Roles {
			group.Go(func() error {
				read, cancel := context.WithTimeout(ctx, installReadAttempt)
				defer cancel()
				m.installed[index], m.failed[index] = installs.InstalledOn(read, role, m.repo)
				return nil
			})
		}
	}
	_ = group.Wait() // every read keeps its own failure, so none fails the group
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("check the GitHub Apps' installations: %w", err)
	}
	var accepted []mappedRepository
	var refused []refusedRepository
	for _, m := range entries {
		if m.parse != nil {
			refused = append(refused, refusedRepository{Repo: m.mapping.Repo, Reason: m.parse.Error()})
			continue
		}
		var missing, unread []string
		var readErr error
		for index, role := range appauth.Roles {
			switch {
			case m.failed[index] != nil:
				unread = append(unread, fmt.Sprintf("the %s App's installation on %s could not be read: %v", role, m.repo, m.failed[index]))
				if readErr == nil {
					readErr = fmt.Errorf("check the %s App's installation on %s: %w", role, m.repo, m.failed[index])
				}
			case !m.installed[index]:
				missing = append(missing, string(role))
			}
		}
		var reasons []string
		switch len(missing) {
		case 0:
		case 1:
			reasons = append(reasons, fmt.Sprintf("the %s App is not installed on %s", missing[0], m.repo))
		default:
			reasons = append(reasons, fmt.Sprintf("the %s Apps are not installed on %s", strings.Join(missing, " and "), m.repo))
		}
		if len(missing) > 0 {
			// An App GitHub answered is not installed leaves the repository out whatever the
			// other read says: it is refused as today, and read again only on request.
			readErr = nil
		}
		if len(unread) > 0 {
			reason := strings.Join(unread, "; ")
			if readErr != nil {
				reason += "; Legion reads it again"
			}
			reasons = append(reasons, reason)
		}
		if len(reasons) > 0 {
			refused = append(refused, refusedRepository{Repo: m.mapping.Repo, Reason: strings.Join(reasons, "; "), readErr: readErr, mappedDefault: m.mapping.Default})
			continue
		}
		accepted = append(accepted, mappedRepository{Repo: m.repo, Default: m.mapping.Default})
	}
	return accepted, refused, nil
}

// installReadRetry is how long refreshOnMappingChange waits before it reads the mapping again of
// its own while a failed installation read leaves a repository as it stands (awaitingRead): 30 s
// doubling to 10 min, without limit (Attempts is unused), back to 30 s once none is left.
var installReadRetry = bootprobe.Retry{Initial: 30 * time.Second, Max: 10 * time.Minute}

// recoveryHoldBound is how long a recovery's release waits stay untold while its listing is held
// for the Dispatch consumer (recoverRepositories): the hold watchdog's warning interval
// (defaultHoldWarnEvery), by which a hold that has not released is a fault worth naming.
var recoveryHoldBound = defaultHoldWarnEvery

// installReadAttempt bounds one installation read: as long as any one GitHub call the boot waits
// for (appMintAttempt), so a GitHub that answers slowly is still answered for, while a read GitHub
// has not answered by then is a failed read, and a repository GitHub hangs on is left out for now
// rather than holding up the mapping. Nothing bounds the check as a whole: mappingReads reads run at
// once, so a check of n repositories takes at most ceil(2n / mappingReads) of these.
var installReadAttempt = 15 * time.Second

// readFailures is the refusals of refused that a failed installation read made (readErr).
func readFailures(refused []refusedRepository) []refusedRepository {
	var failed []refusedRepository
	for _, refusal := range refused {
		if refusal.readErr != nil {
			failed = append(failed, refusal)
		}
	}
	return failed
}

// mappingReads is how many of a mapping read's GitHub reads run at once.
const mappingReads = 8

// nameAccepted is accepted as GitHub names each repository (named: its own spelling of owner and
// name, which its events and the subjects Envoy publishes them on carry), each read with its
// default branch as the implement App, mappingReads at a time.
func nameAccepted(ctx context.Context, accepted []mappedRepository, named *defaultBranches) ([]mappedRepository, error) {
	out := make([]mappedRepository, len(accepted))
	group, read := errgroup.WithContext(ctx)
	group.SetLimit(mappingReads)
	for index, mapped := range accepted {
		group.Go(func() error {
			repo, err := named.Named(read, mapped.Repo)
			if err != nil {
				return err
			}
			out[index] = mappedRepository{Repo: repo.Name, Default: mapped.Default}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// loadMapping is the boot's read of the repository mapping. Dispatch's read (listMapping) waits
// out an unreachable Dispatch (dispatch.Unreachable) on readinessRetry, without limit, as the
// boot's Dispatch listing for admission does: a Dispatch down for longer than any bound (no ready
// endpoint, every dial refused) would otherwise exit the daemon into a supervisor that restarts it
// straight back into the same refusal, and the API does not listen until the read passes, so the
// daemon serves nothing meanwhile; any other failure of it refuses the boot. The installation
// check (checkMapping) never refuses the boot: a repository whose read fails is left out for now
// and read again by refreshOnMappingChange, on installReadRetry, until GitHub answers, and served
// then with no restart. A boot whose every repository's read fails comes up serving none and logs
// so at ERROR with the first failure, since a daemon that exits instead restarts straight into
// GitHub's same refusal with its API and its recovery down, while one that runs heals once GitHub
// answers. A project with no repository mapped refuses the boot. One whose every mapped
// repository lacks an App boots serving none, each left out and reported: it admits nothing,
// saying why on each issue, until an App is installed and a read of the mapping finds it, one the
// next mapping change or an issue held for it asks for (requestMappingRead). nameMapping, once the
// Apps' tokens are minted, reads GitHub's spelling.
func (w *workflowRuntime) loadMapping(ctx context.Context) error {
	var mappings []dispatch.RepoProject
	err := bootprobe.Run(ctx, "repository mapping read", readinessRetry, w.log, readinessAttempt(func(ctx context.Context) error {
		var err error
		mappings, err = listMapping(ctx, w.dispatch)
		return err
	}, appMintAttempt, dispatch.Unreachable))
	if err != nil {
		return err
	}
	accepted, refused, err := checkMapping(ctx, mappings, w.dispatchProject, w.installs)
	if err != nil {
		return err
	}
	if len(accepted) == 0 && len(refused) == 0 {
		return fmt.Errorf("project %s has no repository mapped in Dispatch (Settings → Repositories → Projects)", w.dispatchProject)
	}
	switch failed := readFailures(refused); {
	case len(failed) == 0:
	case len(accepted) == 0 && len(failed) == len(refused):
		w.log.Error("workflow: every mapped repository's GitHub App installation read failed, so the daemon boots serving none and reads them again",
			"repositories", len(failed), "retryIn", installReadRetry.Initial.String(), "error", failed[0].readErr)
	default:
		w.log.Warn("workflow: a mapped repository's GitHub App installation read failed, so the daemon boots leaving it out and reads it again",
			"refused", refusalReasons(failed), "retryIn", installReadRetry.Initial.String())
	}
	w.repositories.set(accepted, refused)
	return nil
}

// nameMapping replaces the served repositories with GitHub's spelling of each, on the boot's
// retry, and logs the mapping the daemon serves.
func (w *workflowRuntime) nameMapping(ctx context.Context) error {
	var named []mappedRepository
	err := bootprobe.Run(ctx, "repository read", appMintRetry, w.log, readinessAttempt(func(ctx context.Context) error {
		var err error
		named, err = nameAccepted(ctx, w.repositories.Mapped(), w.defaultBranches)
		return err
	}, appMintAttempt, func(err error) bool {
		var answer *githubrest.Answer
		return appTransient(err) || errors.As(err, &answer) && answer.Status >= 500
	}))
	if err != nil {
		return err
	}
	w.repositories.set(named, w.repositories.Refused())
	w.log.Info("legion workflow boot stage", "stage", "repositories", "accepted", repositoryNames(w.repositories.Accepted()), "refused", refusalReasons(w.repositories.Refused()))
	return nil
}

// refreshMapping reads the repository mapping again: refreshOnMappingChange's read, which a
// mapping change in Dispatch, an admission held for it (requestMappingRead) or a repository whose
// installation read failed (installReadRetry) asks for. A repository the daemon serves whose
// installation read fails stays served on its last answer, with the name GitHub last gave it and
// no read of GitHub, recorded as unverified so it is read again: GitHub's failure to answer is no
// reason to stop serving a repository it answered for, and no reason either to hold back the rest
// of the mapping. One left out whose read fails stays left out, with the new failure. Dispatch's
// read, and the naming and publication after the check, are each bounded by appMintAttempt; the
// check itself is bounded per read (installReadAttempt), so a GitHub that answers slowly is
// answered for whatever the mapping's size. Each repository the mapping as last read left out for
// its failed read, and that this read no longer does (now served, refused for a missing App, or no
// longer mapped), is to be recovered (recoverRepositories): an event of an issue on it applied
// while it was unread said nothing and recorded nothing, so the issues it left are taken from
// Dispatch's listing once. It has
// GitHub intake hear every repository the new mapping serves and every repository a record of the
// project names, so a repository unmapped while an issue is recorded on it is still heard, and
// only then publishes the new mapping: a read that fails before the subscription took publishes
// nothing, so a repository is never served, and an issue never recorded on it, while GitHub intake
// does not hear it, and the refresher reads it again.
func (w *workflowRuntime) refreshMapping(ctx context.Context) error {
	w.mappingMu.Lock()
	defer w.mappingMu.Unlock()
	listing, cancel := context.WithTimeout(ctx, appMintAttempt)
	mappings, err := listMapping(listing, w.dispatch)
	cancel()
	if err != nil {
		return err
	}
	accepted, refused, err := checkMapping(ctx, mappings, w.dispatchProject, w.installs)
	if err != nil {
		return err
	}
	var kept []mappedRepository
	var unverified []string
	refused = slices.DeleteFunc(refused, func(failed refusedRepository) bool {
		mapped, refusal, _, _ := w.repositories.lookup(failed.Repo)
		if failed.readErr == nil || refusal != "" {
			return false
		}
		kept = append(kept, mappedRepository{Repo: mapped.Repo, Default: failed.mappedDefault})
		unverified = append(unverified, mapped.Repo.String())
		w.log.Warn("workflow: a served repository's GitHub App installation read failed, so the daemon keeps serving it on its last answer and reads it again",
			"repo", mapped.Repo.String(), "error", failed.readErr)
		return true
	})
	ctx, cancel = context.WithTimeout(ctx, appMintAttempt)
	defer cancel()
	if accepted, err = nameAccepted(ctx, accepted, w.defaultBranches); err != nil {
		return err
	}
	accepted = append(accepted, kept...)
	served := make([]ghrepo.Repository, 0, len(accepted))
	for _, mapped := range accepted {
		served = append(served, mapped.Repo)
	}
	// The record snapshot, the subscription update and the publication run under the lock every
	// fact applies under (intake.SerializeFacts), which an admission holds from resolving an
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
		if err := w.consumers.SetRepositories(ctx, heard); err != nil {
			return err
		}
		before := readFailures(w.repositories.Refused())
		added, removed = w.repositories.set(accepted, refused)
		w.repositories.setUnverified(unverified)
		w.markRecovered(before, readFailures(w.repositories.Refused()))
		return nil
	}); err != nil {
		return err
	}
	w.log.Info("legion workflow repositories changed", "added", added, "removed", removed,
		"refused", refusalReasons(w.repositories.Refused()), "heard", repositoryNames(heard))
	return nil
}

// mappingChange is Dispatch's consumer's RepositoryMapping: the project's
// settings.repo_project.updated asks refreshOnMappingChange to read the mapping again
// (requestMappingRead), and its event is acknowledged at once. A read takes Dispatch and GitHub
// and can take longer than the event's acknowledgement window (AckWait), and one run inside that
// window would be cut off and its event delivered again, never finishing.
func (w *workflowRuntime) mappingChange(context.Context) error {
	w.requestMappingRead()
	return nil
}

// requestMappingRead asks refreshOnMappingChange for a read of the mapping and is the request's
// number: a read that finishes having started after the request covers it (mappingApplied). A
// request made while one waits is covered by the same read, since a read takes the whole mapping.
func (w *workflowRuntime) requestMappingRead() uint64 {
	request := w.mappingRequested.Add(1)
	select {
	case w.mappingChanged <- struct{}{}:
	default:
	}
	return request
}

// refreshOnMappingChange reads the mapping (refreshMapping, which bounds each of its own parts) on
// each request, until ctx ends. A failed read is logged and tried again on the App mint's backoff,
// with no limit, until it applies: a read takes the whole mapping, so retrying loses no request,
// and a daemon stopped before one applies reads the mapping at boot. A finished read covers every
// request made before it started, and then recovers each repository the read stopped leaving out
// for a failed installation read (recoverRepositories). While a failed installation read leaves a
// repository as it stands (awaitingRead: left out for now, or served on its last answer), or a
// recovery has not committed yet, it reads the mapping again of its own on installReadRetry's
// backoff, 30 s doubling to 10 min, reset once nothing waits, so the repository is served, or
// confirmed, and its issues recovered, once GitHub answers, with no restart and no mapping change.
// While a held recovery has release waits to tell and time left to recoveryHoldBound
// (recoveryHoldLeft), the next wait is cut to that time, so the bound is checked when it falls, not
// up to a backoff later; such a cut wait does not move the backoff on. Once the bound has passed,
// a run that did not tell the waits (its listing or its notice failed) waits the backoff as any
// failed read does, so a failure past the bound never turns the refresher into a busy loop.
func (w *workflowRuntime) refreshOnMappingChange(ctx context.Context) {
	after := w.recheckAfter
	if after == nil {
		after = time.After
	}
	recheckIn := installReadRetry.Initial
	for {
		var recheck <-chan time.Time
		waiting := append(w.repositories.awaitingRead(), w.recoveringReasons()...)
		wait, cut := recheckIn, false
		if len(waiting) == 0 {
			recheckIn = installReadRetry.Initial
		} else {
			if left, held := w.recoveryHoldLeft(); held && left > 0 && left < wait {
				wait, cut = left, true
			}
			recheck = after(wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-w.mappingChanged:
		case <-recheck:
			w.log.Info("workflow: reading the repository mapping again for what a failed installation read left as it stands",
				"waiting", waiting, "waited", wait.String())
			if !cut {
				recheckIn = min(2*recheckIn, installReadRetry.Max)
			}
		}
		retry := appMintRetry.Initial
		for {
			covers := w.mappingRequested.Load()
			err := w.refreshMapping(ctx)
			if err == nil {
				w.mappingApplied.Store(covers)
				w.recoverRepositories(ctx)
				break
			}
			if ctx.Err() != nil {
				return
			}
			w.log.Warn("workflow: the repository mapping read failed, so the daemon serves the mapping it last read; reading it again", "retryIn", retry.String(), "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
			retry = min(2*retry, appMintRetry.Max)
		}
	}
}

// markRecovered adds to the repositories to recover (recovering) each repository before names, a
// refusal for a failed installation read in the mapping as last read, that after, the same of the
// mapping just published, does not name: GitHub answered for it, or it is no longer mapped. A
// repository after names is dropped from the set: it is unread again, and the read that next stops
// leaving it out adds it back. It runs under the fact lock, with the publication, so every event
// applied after it sees the new mapping, and every one applied before it is in the listing the
// recovery reads afterwards.
func (w *workflowRuntime) markRecovered(before, after []refusedRepository) {
	unread := make(map[string]bool, len(after))
	for _, refusal := range after {
		unread[strings.ToLower(refusal.Repo)] = true
	}
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	for repo := range w.recovering {
		if unread[repo] {
			delete(w.recovering, repo)
			// The repository left the recovery: a later one is held on a clock of its own.
			w.recoveryHeldSince = time.Time{}
		}
	}
	for _, refusal := range before {
		repo := strings.ToLower(refusal.Repo)
		if unread[repo] {
			continue
		}
		if w.recovering == nil {
			w.recovering = map[string]bool{}
		}
		w.recovering[repo] = true
	}
}

// recoveryHoldLeft is the time left to recoveryHoldBound for a recovery whose listing is held with
// release waits to tell, zero once the bound has passed, and whether one is.
func (w *workflowRuntime) recoveryHoldLeft() (time.Duration, bool) {
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	if w.recoveryHeldSince.IsZero() {
		return 0, false
	}
	waits := false
	for repo := range w.recovering {
		waits = waits || len(w.releaseWaits[repo]) != 0
	}
	if !waits {
		return 0, false
	}
	return max(0, recoveryHoldBound-time.Since(w.recoveryHeldSince)), true
}

// recoveringReasons is every repository still to recover, as the refresher logs what it waits on.
func (w *workflowRuntime) recoveringReasons() []string {
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	reasons := make([]string, 0, len(w.recovering))
	for repo := range w.recovering {
		reasons = append(reasons, repo+": its issues are to be recovered from Dispatch's listing")
	}
	slices.Sort(reasons)
	return reasons
}

// recoverRepositories recovers the repositories markRecovered named, after a read of the mapping
// that published them as no longer unread: one read of Dispatch's project listing, applied as the
// boot's reconcile applies it (reconcileListing), scoped to the issues the workflow does not record
// that resolve to one of them, whose parent chain in the listing reaches one that does (a child on
// a served repository under a parent that was unread), or whose apply said nothing while one of
// them was unread (noteUnread: an issue that took its repository from a parent, or from the
// project's default, changed meanwhile). Those are what an event applied while the repository was
// unread left: it said nothing and recorded nothing. A recorded issue is left out: its own events
// carried it, and a snapshot with no actor applied mid-run could overwrite a session's write. Each
// issue then ends where its event would have ended had the repository been served: a labelled
// root admitted or queued, a child of a live tree recorded under it (parents first), and a
// person's issue left as it is. A listing the consumer may still be behind is held as the boot's
// is (admission's pending), and released by pollHoldRelease. Then each tree's architect is told,
// once, of every child its release_children left unrecorded on one of them that is still
// unrecorded (tellReleaseWaits). While the listing stays held, that telling waits, but for
// recoveryHoldBound at most: a hold that outlasts it (the consumer stuck below the stream's last
// position, an event that fails at every delivery, say) is told the waits still unrecorded then,
// once, and the full recovery still runs when the hold releases. The clock starts when a run first
// finds the listing held with waits to tell, and runs on across holds released and made again,
// so a hold that flaps is told at the bound all the same; it stops when the recovery commits or a
// repository leaves the recovery (markRecovered). The repositories are let go once
// both commit; a failed one stays to recover, tried again on the refresher's backoff. A restart
// loses the set, and the boot's reconcile is that recovery; what a release left waiting is told to
// no one then.
func (w *workflowRuntime) recoverRepositories(ctx context.Context) {
	w.recoveringMu.Lock()
	repos := maps.Clone(w.recovering)
	silenced := map[string]bool{}
	waits := map[string]releaseWait{}
	for repo := range repos {
		maps.Copy(silenced, w.silenced[repo])
		maps.Copy(waits, w.releaseWaits[repo])
	}
	w.recoveringMu.Unlock()
	if len(repos) == 0 {
		return
	}
	names := slices.Sorted(maps.Keys(repos))
	attempt, cancel := context.WithTimeout(ctx, appMintAttempt)
	defer cancel()
	issues, err := w.dispatch.ListIssues(attempt, w.dispatchProject, nil)
	if err == nil {
		err = w.reconcileListing(attempt, issues, recoveredOn(repos, silenced, issues), false)
	}
	if err == nil && len(waits) != 0 && w.admission != nil && w.admission.Held() {
		// The listing waits on the consumer (admission's pending), so a child it records is not
		// recorded yet: the waits are told once it is released, or once the hold has lasted
		// recoveryHoldBound, whichever comes first.
		w.recoveringMu.Lock()
		if w.recoveryHeldSince.IsZero() {
			w.recoveryHeldSince = time.Now()
		}
		bounded := time.Since(w.recoveryHeldSince) >= recoveryHoldBound
		w.recoveringMu.Unlock()
		if !bounded {
			w.log.Info("workflow: the recovery's listing waits on the Dispatch consumer; its release waits are told once it is released", "repositories", names)
			return
		}
		if err := w.tellReleaseWaits(attempt, names, waits); err != nil {
			w.log.Warn("workflow: the release waits of a recovery held past its bound were not told; telling them again", "repositories", names, "error", err)
			return
		}
		w.log.Warn("workflow: the recovery's listing has waited on the Dispatch consumer past its bound, so its release waits still unrecorded are told now",
			"repositories", names, "bound", recoveryHoldBound.String())
		w.recoveringMu.Lock()
		for _, repo := range names {
			for key := range waits {
				delete(w.releaseWaits[repo], key)
			}
		}
		w.recoveringMu.Unlock()
		return
	}
	if err == nil {
		err = w.tellReleaseWaits(attempt, names, waits)
	}
	if err != nil {
		w.log.Warn("workflow: the issues of the repositories GitHub answered for again were not recovered from Dispatch's listing; recovering them again",
			"repositories", names, "error", err)
		return
	}
	w.recoveringMu.Lock()
	w.recoveryHeldSince = time.Time{}
	for _, repo := range names {
		delete(w.recovering, repo)
		for key := range silenced {
			delete(w.silenced[repo], key)
		}
		for key := range waits {
			delete(w.releaseWaits[repo], key)
		}
	}
	w.recoveringMu.Unlock()
	w.log.Info("workflow: recovered the issues of the repositories GitHub answered for again", "repositories", names)
}

// releaseWait is a child release_children left unrecorded while a repository was unread
// (awaitRecovery): the tree it was released in, and its Dispatch parent.
type releaseWait struct {
	tree, parent string
}

// noteUnread is the workflow's Config.NoteUnread: key's apply said nothing because resolved was
// unread. It is called inside the apply's transaction, so it is noted before any read of the
// mapping can publish resolved again, under the fact lock, and so before that recovery's scope is
// read. A rolled-back apply leaves a note that only takes an unrecorded issue into the scope.
func (w *workflowRuntime) noteUnread(key, resolved string) {
	repo := strings.ToLower(resolved)
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	if w.silenced == nil {
		w.silenced = map[string]map[string]bool{}
	}
	if w.silenced[repo] == nil {
		w.silenced[repo] = map[string]bool{}
	}
	w.silenced[repo][key] = true
}

// awaitRecovery is api.Options.AwaitRecovery: child, released in tree under parent, is left
// unrecorded while repo is unread, and the recovery of repo tells tree's architect if it is still
// unrecorded then.
func (w *workflowRuntime) awaitRecovery(repo, tree, child, parent string) {
	repo = strings.ToLower(repo)
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	if w.releaseWaits == nil {
		w.releaseWaits = map[string]map[string]releaseWait{}
	}
	if w.releaseWaits[repo] == nil {
		w.releaseWaits[repo] = map[string]releaseWait{}
	}
	w.releaseWaits[repo][child] = releaseWait{tree: tree, parent: parent}
}

// tellReleaseWaits tells each tree's architect, in one child-release-stalled notice, which of waits
// the recovery of repos left unrecorded, each with whether its parent is recorded now: what to
// release again. A child recorded since is told nothing; its record's own notices carry it.
func (w *workflowRuntime) tellReleaseWaits(ctx context.Context, repos []string, waits map[string]releaseWait) error {
	if len(waits) == 0 {
		return nil
	}
	byTree := map[string][]string{}
	for _, child := range slices.Sorted(maps.Keys(waits)) {
		wait := waits[child]
		recorded, err := w.recordOf(ctx, child)
		if err != nil {
			return err
		}
		if recorded != nil {
			continue
		}
		parent, err := w.recordOf(ctx, wait.parent)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s (its parent %s is recorded now: release %s again)", child, wait.parent, child)
		if parent == nil {
			line = fmt.Sprintf("%s (its parent %s is not recorded: release %s first, then %s)", child, wait.parent, wait.parent, child)
		}
		byTree[wait.tree] = append(byTree[wait.tree], line)
	}
	if len(byTree) == 0 {
		return nil
	}
	now := time.Now().UTC()
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, w.pool, func(tx pgx.Tx) error {
		for _, tree := range slices.Sorted(maps.Keys(byTree)) {
			row, err := record.NewOutboxRow(tree, record.Notice{Kind: "child-release-stalled", Role: claim.RoleArchitect, Reason: fmt.Sprintf(
				"GitHub answered %s's App installation read again, and the daemon recovered its issues from Dispatch's listing, but these children your release_children released while it was unread are still not recorded: %s",
				strings.Join(repos, ", "), strings.Join(byTree[tree], "; "))}, now)
			if err != nil {
				return err
			}
			if err := w.records.Enqueue(scoped, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("tell the architects of the children the recovery left unrecorded: %w", err)
	}
	committed()
	return nil
}

// recoveredOn is recoverRepositories' scope over listing: an issue records does not hold whose
// repository repos names (lowercased), one of whose ancestors in listing resolves to one, or one
// silenced names.
func recoveredOn(repos, silenced map[string]bool, listing []dispatch.IssueSummary) func(records map[string]*record.Issue, summary dispatch.IssueSummary) bool {
	byKey := make(map[string]dispatch.IssueSummary, len(listing))
	for _, summary := range listing {
		byKey[summary.Key] = summary
	}
	return func(records map[string]*record.Issue, summary dispatch.IssueSummary) bool {
		if records[summary.Key] != nil {
			return false
		}
		if silenced[summary.Key] || repos[strings.ToLower(summary.ResolvedRepo)] {
			return true
		}
		seen := map[string]bool{summary.Key: true}
		for parent := parentKey(summary.Parent); parent != "" && !seen[parent]; {
			above, listed := byKey[parent]
			if !listed {
				return false
			}
			if silenced[parent] || repos[strings.ToLower(above.ResolvedRepo)] {
				return true
			}
			seen[parent] = true
			parent = parentKey(above.Parent)
		}
		return false
	}
}

// repositoryUnread is whether resolved is left out for now because its installation read failed:
// release_children's answer for a child whose apply recorded nothing (api.Options.RepositoryUnread).
func (w *workflowRuntime) repositoryUnread(resolved string) bool {
	_, _, _, unread := w.repositories.lookup(resolved)
	return unread
}

// awaitMappingRead holds back the admission event of issue delivered at seq, on a repository the
// mapping did not serve as last read, until a read of the mapping that started after the event
// first came covers it: a repository mapped, or an App installed, just before the event is then
// served for it. The first call for an event (by issue key and the sequence it was delivered at,
// which a redelivery keeps whatever Dispatch has run to since) requests the read; until it is
// covered, every call answers an intake.RetryLater, so the event is delivered again after its
// delay, off its acknowledgement window; once covered, it answers nil, for the caller to look the
// repository up again.
func (w *workflowRuntime) awaitMappingRead(issue intake.DispatchIssue, seq int64) error {
	w.awaitingMu.Lock()
	defer w.awaitingMu.Unlock()
	request, asked := w.awaiting[issue.Key][seq]
	if !asked {
		if w.awaiting == nil {
			w.awaiting = map[string]map[int64]uint64{}
		}
		if w.awaiting[issue.Key] == nil {
			w.awaiting[issue.Key] = map[int64]uint64{}
		}
		request = w.requestMappingRead()
		w.awaiting[issue.Key][seq] = request
	}
	if w.mappingApplied.Load() >= request {
		return nil
	}
	return &intake.RetryLater{After: mappingReadWait,
		Err: fmt.Errorf("%s resolves to %s, which the repository mapping did not serve when last read: the mapping is being read again before the issue is admitted", issue.Key, issue.ResolvedRepo)}
}

// forgetHolds drops issue's holds (awaitMappingRead) for every event delivered at or below seq,
// its event prepared at that sequence at last: every event at or below it is reflected in what
// was prepared, and one still held is held again on its next delivery.
func (w *workflowRuntime) forgetHolds(issue string, seq int64) {
	w.awaitingMu.Lock()
	defer w.awaitingMu.Unlock()
	for held := range w.awaiting[issue] {
		if held <= seq {
			delete(w.awaiting[issue], held)
		}
	}
	if len(w.awaiting[issue]) == 0 {
		delete(w.awaiting, issue)
	}
}

// forgetIssue drops every hold of issue's events, its record written: no event of it is prepared
// as an admission any longer.
func (w *workflowRuntime) forgetIssue(issue string) {
	w.awaitingMu.Lock()
	defer w.awaitingMu.Unlock()
	delete(w.awaiting, issue)
}

// admissionBound is whether issue's event is one admission records an unrecorded issue from: in
// todo, handed to Legion or a child of a tree.
func admissionBound(issue intake.DispatchIssue) bool {
	return issue.Status == "todo" && (issue.HandedOver || issue.Parent != "")
}

// mappingReadWait is how long an admission event held back for a read of the mapping
// (awaitMappingRead) waits before it is tried again.
const mappingReadWait = time.Second

// heardAtBoot is what GitHub intake hears when the boot opens it: the served repositories of the
// mapping the boot read and every repository a record of the project names.
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
// a record of the project names, as tx reads the records, once each, sorted. A record is heard
// whatever its tree's state, since a root that lingers or closed comes back, by re-admission, a
// hand-over or an orphan's carry, on the repository it keeps, with no refresh between; a fact
// from a repository an issue is recorded on reaches only that issue (workflow's issueForBranch
// and ownPullRequest). Each is spelled as GitHub spells it, which Envoy's subjects carry and NATS
// matches case-sensitively, whatever spelling a record keeps for its workspace (a backfilled
// record keeps its clone's): a served repository as GitHub names it (nameAccepted), and any other
// as its record keeps GitHub's spelling (record.Issue.GitHubRepo), so a restart subscribes with
// no read of GitHub.
func (w *workflowRuntime) heardRepositories(ctx context.Context, tx pgx.Tx, served []ghrepo.Repository) ([]ghrepo.Repository, error) {
	heard := map[string]ghrepo.Repository{}
	for _, repo := range served {
		heard[strings.ToLower(repo.String())] = repo
	}
	issues, err := w.records.Issues(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("read the repositories the project's records name: %w", err)
	}
	for _, issue := range issues {
		if _, ok := heard[strings.ToLower(issue.GitHubRepo)]; ok {
			continue
		}
		repo, err := ghrepo.Parse("GitHub's spelling of the repository of "+issue.Key, issue.GitHubRepo)
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
// works the project's default repository, as Dispatch resolves an issue that names none, at its
// default branch as GitHub names it (defaultBranches): a pod provisions its workspace from one, so
// a launch on none would be refused. A project that serves no default repository refuses that
// launch, naming the fix.
func (w *workflowRuntime) issueRepository(ctx context.Context, key string) (ghrepo.Repository, string, error) {
	issue, err := w.recordOf(ctx, key)
	if err != nil {
		return ghrepo.Repository{}, "", err
	}
	if issue == nil {
		for _, mapped := range w.repositories.Mapped() {
			if mapped.Default {
				read, err := w.defaultBranches.Named(ctx, mapped.Repo)
				if err != nil {
					return ghrepo.Repository{}, "", fmt.Errorf("the default repository of project %s, which %s works since it is not recorded: %w", w.dispatchProject, key, err)
				}
				return read.Name, read.DefaultBranch, nil
			}
		}
		return ghrepo.Repository{}, "", fmt.Errorf("%s is not recorded, so it works the project's default repository, and project %s serves none: set the project's default repository in Dispatch (Settings → Repositories → Projects) to one both GitHub Apps are installed on",
			key, w.dispatchProject)
	}
	repo, err := issue.Repository()
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
// ConsumerSpec.PrepareRepository), and is the event to apply. An issue's repository is its
// record's once recorded, so a recorded issue's event asks nothing of GitHub, nor of Dispatch about
// its repository: a retained event that names no repository (ResolvedRepoMissing) takes the
// record's, and one naming another is applied as it is, for the workflow to keep the record's and
// say so. For an issue not yet recorded, a retained event's repository is read from Dispatch's
// issue detail, a read that fails leaving the event to be retried; then, for an event in todo, what
// the workflow reads in memory when it records the issue (ResolveRepository) is readied: an issue
// coming up for admission (handed to Legion, or a child of a tree) on a repository the mapping did
// not serve when last read waits for a read of the mapping that covers it (awaitMappingRead), so a
// repository just mapped, or an App just installed, is served for it, and a served repository's
// default branch is read when the read Legion holds is older than its TTL. An unrecorded issue's
// event in any other status asks GitHub nothing.
//
// An unrecorded issue has no sequence to fence a stale event with, and its admission event may come
// after the issue's newer events: redelivered after its preparation or apply failed or its
// acknowledgement window ran out, or put off by Dispatch's outbox, which publishes the events behind
// a failed publish first. So an admission event of an unrecorded issue is prepared as Dispatch's
// issue detail now shows the issue, at the sequence Dispatch has run to (intake.ObservedIssue),
// not as the event says: a person who withdrew the issue meanwhile is not overruled, and an issue
// whose repository changed with no event of its own (an ancestor's, or the project's default) is
// admitted on the one it resolves to now. That view applies only while the issue stays unrecorded
// (intake.DispatchIssue.Observed): once release_children records it first, the event applies as
// it was delivered, and once the issue is recorded, a stale event below its sequence is fenced as
// any is.
//
// Every labelled todo of an issue unrecorded or recorded as a root, under a Dispatch parent recorded
// or not, also reads that parent's chain from Dispatch (withAncestry), which keeps the issue out
// of admission's line while any live root is on it. An event of an issue the workflow does not
// record, or a fresh event of a recorded issue whose Dispatch parent is not its record's, reads
// again the chain of every queued root whose recorded chain holds the issue: that issue may be the
// ancestor a person just moved.
func (w *workflowRuntime) prepareRepository(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
	return w.prepareIssue(ctx, issue, true)
}

// prepareObserved readies release_children's todo child, already observed as Dispatch's issue
// detail shows it now (intake.ObservedIssue), as prepareRepository readies an event's, but reads no
// other root's chain again: the view is no re-parent.
func (w *workflowRuntime) prepareObserved(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
	return w.prepareIssue(ctx, issue, false)
}

// prepareIssue is prepareRepository, preparing an admission event from Dispatch's current view, and
// reading other roots' chains again, only when observe is set: release_children's child is that
// view already.
func (w *workflowRuntime) prepareIssue(ctx context.Context, issue intake.DispatchIssue, observe bool) (intake.DispatchIssue, error) {
	recorded, err := w.recordOf(ctx, issue.Key)
	if err != nil {
		return issue, err
	}
	if recorded != nil {
		return w.withAncestry(ctx, w.prepareRecorded(issue, recorded), recorded, observe)
	}
	if !admissionBound(issue) {
		prepared, err := w.prepareUnrecorded(ctx, issue, false, 0)
		if err != nil {
			return prepared, err
		}
		return w.withAncestry(ctx, prepared, nil, observe)
	}
	delivered := issue.Seq
	if observe {
		now, err := w.dispatch.GetIssue(ctx, issue.Key)
		if err != nil {
			return issue, fmt.Errorf("read %s from Dispatch before applying its admission event %d: %w", issue.Key, delivered, err)
		}
		event := issue
		issue = intake.ObservedIssue(now)
		issue.Seq, issue.Delivered = max(delivered, issue.Seq), &event
	}
	prepared, err := w.prepareUnrecorded(ctx, issue, true, delivered)
	if err != nil {
		return prepared, err
	}
	if prepared, err = w.withAncestry(ctx, prepared, nil, observe); err != nil {
		return prepared, err
	}
	w.forgetHolds(issue.Key, prepared.Seq)
	return prepared, nil
}

// prepareListed readies a todo issue of reconcile's boot listing as prepareRepository readies an
// event's, on the mapping the boot has just read and from the listing it has just read: an issue
// records holds a record of takes that record's repository, and an unrecorded one's served
// repository's default branch is read, with no hold for a read of the mapping and no read of
// Dispatch's view. Its Dispatch chain is read up walk, the boot's one cache over its listing
// (listingAncestry), and no other root's is read again: the listing holds every queued root's own
// todo.
func (w *workflowRuntime) prepareListed(ctx context.Context, records map[string]*record.Issue, walk ancestry, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
	recorded := records[issue.Key]
	if recorded != nil {
		issue = w.prepareRecorded(issue, recorded)
	} else {
		prepared, err := w.prepareUnrecorded(ctx, issue, false, 0)
		if err != nil {
			return prepared, err
		}
		issue = prepared
	}
	if !queuesUnderParent(issue, recorded) {
		return issue, nil
	}
	chain, err := w.chainOf(ctx, walk, issue.Key, issue.Parent)
	issue.Ancestry = chain
	return issue, err
}

// prepareRecorded readies an event of an issue recorded as recorded, an event's or the boot
// listing's: no event of it is held as an admission any longer (forgetIssue), and one that names no
// repository (ResolvedRepoMissing) takes the record's.
func (w *workflowRuntime) prepareRecorded(issue intake.DispatchIssue, recorded *record.Issue) intake.DispatchIssue {
	w.forgetIssue(issue.Key)
	if issue.ResolvedRepoMissing {
		issue.ResolvedRepo, issue.ResolvedRepoMissing = recorded.Repo, false
	}
	return issue
}

// listingAncestry is what reconcile's boot prepares its listing from: every record, by key, read
// once in a read-only transaction, and the walk every chain is read up, holding the Dispatch parent
// of every listed issue as the listing shows it, so only an ancestor outside the listing is read
// from Dispatch.
func (w *workflowRuntime) listingAncestry(ctx context.Context, listed []dispatch.IssueSummary) (map[string]*record.Issue, ancestry, error) {
	var issues []record.Issue
	if err := pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		issues, err = w.records.Issues(ctx, tx)
		return err
	}); err != nil {
		return nil, nil, fmt.Errorf("read the records for the boot listing: %w", err)
	}
	records := make(map[string]*record.Issue, len(issues))
	for i := range issues {
		records[issues[i].Key] = &issues[i]
	}
	walk := make(ancestry, len(listed))
	for _, summary := range listed {
		walk[summary.Key] = parentKey(summary.Parent)
	}
	return records, walk, nil
}

// withAncestry is an event's issue, recorded as recorded (nil when the workflow does not record
// it), with what its preparation reads up Dispatch's parent chains, outside the fact lock as the
// rest of the preparation reads. Its Ancestry (intake.DispatchIssue.Ancestry) is read for a
// labelled todo of an issue unrecorded or recorded as a root under a parent, recorded or not
// (queuesUnderParent); any other issue's is empty: an unlabelled todo never queues, and a child
// already runs in its tree.
//
// With reread set, an event of an issue the workflow does not record, or a fresh event of a
// recorded issue whose Dispatch parent differs from its record's (a person moved it), reads again
// the Ancestry of every queued root whose recorded Ancestry holds the issue's key
// (record.Store.QueuedRootsUnder), and carries each that differs from its record's as Reread: the
// issue may be the ancestor a person moved, whose roots below it have no event of their own. A
// re-parent of any issue on a chain is an event of a key already on that chain, so no other root
// can be moved by it. A recorded issue's event at or below its record's sequence is stale and
// reads no root again, so a late event never brings back an ancestry hold its newer re-parent
// removed; a fresh one is read as the event shows it, so the moved ancestor is read as moved, and an
// unrecorded issue is read as Dispatch shows it now, as any other ancestor is. A failed read of
// Dispatch while reading one such root again leaves that root's record as it stands, and says
// so; only a failed read of the issue's own chain fails the preparation, and the event is
// delivered again. Every walk the preparation takes reads each Dispatch issue once, reads no
// record, and holds no transaction open across a read of Dispatch.
func (w *workflowRuntime) withAncestry(ctx context.Context, issue intake.DispatchIssue, recorded *record.Issue, reread bool) (intake.DispatchIssue, error) {
	walk := ancestry{}
	if queuesUnderParent(issue, recorded) {
		chain, err := w.chainOf(ctx, walk, issue.Key, issue.Parent)
		if err != nil {
			return issue, err
		}
		issue.Ancestry = chain
	}
	if !reread || recorded != nil && (issue.Parent == parentKey(recorded.Parent) || issue.Seq != 0 && issue.Seq <= recorded.LastDispatchSeq) {
		return issue, nil
	}
	var roots []record.Issue
	if err := pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		roots, err = w.records.QueuedRootsUnder(ctx, tx, issue.Key)
		return err
	}); err != nil {
		return issue, fmt.Errorf("read the queued roots whose chain holds %s: %w", issue.Key, err)
	}
	if len(roots) != 0 && recorded != nil {
		walk[issue.Key] = issue.Parent
	}
	for i := range roots {
		root := &roots[i]
		chain, err := w.chainOf(ctx, walk, root.Key, parentKey(root.Parent))
		if err != nil {
			w.log.Warn("workflow: a queued root's Dispatch chain was not read again, so its record keeps the one it has",
				"root", root.Key, "event", issue.Key, "ancestry", root.Ancestry, "error", err)
			continue
		}
		if slices.Equal(chain, root.Ancestry) {
			continue
		}
		if issue.Reread == nil {
			issue.Reread = map[string][]string{}
		}
		issue.Reread[root.Key] = chain
	}
	return issue, nil
}

// queuesUnderParent says whether issue, recorded as recorded (nil when the workflow does not record
// it), is a root the waiting line may hold under an unreleased child of an ancestor's tree (the
// ancestry hold): a labelled todo under a Dispatch parent, unrecorded or recorded as a root.
func queuesUnderParent(issue intake.DispatchIssue, recorded *record.Issue) bool {
	return issue.Status == "todo" && issue.HandedOver && issue.Parent != "" &&
		(recorded == nil || claim.IsTreeRoot(recorded.Key, recorded.Tree))
}

// ancestry is what one preparation's walks up Dispatch's parent chains read, each issue once: the
// Dispatch parent ("" for none) of every issue read from Dispatch or shown to the walk.
type ancestry map[string]string

// chainOf is the keys of key's Dispatch parent chain, read up walk from parent upward to the top,
// stopping only at a repeated key or back at key; nil when parent is "". It reads no record: it is
// the chain release_children's own walk reads (api's releasableChild), so an ancestry hold lasts
// exactly while a live root on it could release the issue into its tree (record.Waiting).
func (w *workflowRuntime) chainOf(ctx context.Context, walk ancestry, key, parent string) ([]string, error) {
	var chain []string
	for ancestor := parent; ancestor != "" && ancestor != key && !slices.Contains(chain, ancestor); {
		chain = append(chain, ancestor)
		next, ok := walk[ancestor]
		if !ok {
			detail, err := w.dispatch.GetIssue(ctx, ancestor)
			if err != nil {
				return nil, fmt.Errorf("read %s from Dispatch, an ancestor of %s: %w", ancestor, key, err)
			}
			next = parentKey(detail.Parent)
			walk[ancestor] = next
		}
		ancestor = next
	}
	return chain, nil
}

// parentKey is the key a Dispatch parent names, "" for none.
func parentKey(parent *string) string {
	if parent == nil {
		return ""
	}
	return *parent
}

// prepareUnrecorded readies the event of an issue the workflow does not record, as
// prepareRepository says, holding back an admission on a repository not served for a read of the
// mapping, under the sequence the event was delivered at (held), when await is set. An admission
// on a repository a failed installation read leaves out is not held back: GitHub is being read
// again for it already, so the event is returned to be applied and acknowledged, its apply says
// nothing and keeps nothing (workflow.Resolution.Unread), and the issue is taken from Dispatch's
// listing once GitHub answers for the repository (recoverRepositories). A served repository kept
// on its last answer (repositories.kept) is read from GitHub no further: its default branch is the
// last read of it, so an admission on it waits for no GitHub read that is failing.
func (w *workflowRuntime) prepareUnrecorded(ctx context.Context, issue intake.DispatchIssue, await bool, held int64) (intake.DispatchIssue, error) {
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
	mapped, refusal, _, unread := w.repositories.lookup(issue.ResolvedRepo)
	if refusal != "" && !unread && await && admissionBound(issue) {
		if err := w.awaitMappingRead(issue, held); err != nil {
			return issue, err
		}
		mapped, refusal, _, _ = w.repositories.lookup(issue.ResolvedRepo)
	}
	if refusal != "" {
		return issue, nil
	}
	if w.repositories.kept(mapped.Repo.String()) {
		if _, read := w.defaultBranches.Kept(mapped.Repo.String()); read {
			return issue, nil
		}
	}
	_, err := w.defaultBranches.Named(ctx, mapped.Repo)
	return issue, err
}

// ResolveRepository answers the workflow (workflow.Repositories) from memory: the served
// repository resolved names, as GitHub names it, at the default branch last read for it, or why
// Legion does not serve it. A served repository whose default branch was never read is an error:
// its event's preparation (prepareRepository) did not run.
func (w *workflowRuntime) ResolveRepository(resolved string) (workflow.Resolution, error) {
	mapped, refusal, leftOut, unread := w.repositories.lookup(resolved)
	if refusal != "" {
		return workflow.Resolution{Refusal: refusal, LeftOut: leftOut, Unread: unread}, nil
	}
	read, ok := w.defaultBranches.Kept(mapped.Repo.String())
	if !ok {
		return workflow.Resolution{}, fmt.Errorf("the default branch of %s was never read before an issue on it was recorded", mapped.Repo)
	}
	return workflow.Resolution{Repo: read.Name.String(), DefaultBranch: read.DefaultBranch, GitHubRepo: read.Name.String()}, nil
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
