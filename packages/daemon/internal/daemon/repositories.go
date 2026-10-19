package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
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

// view is the mapping as the daemon's state shows it.
func (r *repositories) view() api.RepositoriesView {
	view := api.RepositoriesView{Owners: r.Owners()}
	for _, mapped := range r.Mapped() {
		view.Mapped = append(view.Mapped, api.MappedRepositoryView{Repo: mapped.Repo.String(), Default: mapped.Default})
	}
	for _, refused := range r.Refused() {
		view.Refused = append(view.Refused, api.RefusedRepositoryView{Repo: refused.Repo, Reason: refused.Reason})
	}
	return view
}

// readMapping reads Dispatch's repository mapping of project and checks, for each repository, that
// both GitHub Apps are installed on it (installs, under each App's JWT; per repository, since an
// installation on an owner may cover only some of its repositories). A repository an App is not
// installed on is refused, naming the App; any read that fails is an error that fails the whole
// read, so nothing is left out on a guess. Accepted repositories carry Dispatch's spelling until
// nameAccepted reads GitHub's. The installation reads run at once, mappingReads at a time, so a
// read of the mapping takes about as long as its slowest read rather than the sum of them.
func readMapping(ctx context.Context, client dispatch.Client, project string, installs appauth.Installations) ([]mappedRepository, []refusedRepository, error) {
	mappings, err := client.ListRepoProjects(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read the repository mapping from Dispatch: %w", err)
	}
	type mapped struct {
		mapping dispatch.RepoProject
		repo    ghrepo.Repository
		parse   error
		// installed is each App's answer, in appauth.Roles order.
		installed []bool
	}
	var entries []mapped
	for _, mapping := range mappings {
		if mapping.Project != project {
			continue
		}
		repo, err := ghrepo.Parse("the repository Dispatch maps to "+project, mapping.Repo)
		entries = append(entries, mapped{mapping: mapping, repo: repo, parse: err, installed: make([]bool, len(appauth.Roles))})
	}
	group, read := errgroup.WithContext(ctx)
	group.SetLimit(mappingReads)
	for _, m := range entries {
		if m.parse != nil {
			continue
		}
		for index, role := range appauth.Roles {
			group.Go(func() error {
				installed, err := installs.InstalledOn(read, role, m.repo)
				if err != nil {
					return fmt.Errorf("check the %s App's installation on %s: %w", role, m.repo, err)
				}
				m.installed[index] = installed
				return nil
			})
		}
	}
	if err := group.Wait(); err != nil {
		return nil, nil, err
	}
	var accepted []mappedRepository
	var refused []refusedRepository
	for _, m := range entries {
		if m.parse != nil {
			refused = append(refused, refusedRepository{Repo: m.mapping.Repo, Reason: m.parse.Error()})
			continue
		}
		var missing []string
		for index, role := range appauth.Roles {
			if !m.installed[index] {
				missing = append(missing, string(role))
			}
		}
		switch len(missing) {
		case 0:
		case 1:
			refused = append(refused, refusedRepository{Repo: m.mapping.Repo, Reason: fmt.Sprintf("the %s App is not installed on %s", missing[0], m.repo)})
			continue
		default:
			refused = append(refused, refusedRepository{Repo: m.mapping.Repo, Reason: fmt.Sprintf("the %s Apps are not installed on %s", strings.Join(missing, " and "), m.repo)})
			continue
		}
		accepted = append(accepted, mappedRepository{Repo: m.repo, Default: m.mapping.Default})
	}
	return accepted, refused, nil
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

// loadMapping is the boot's read of the repository mapping, run again after a failure GitHub or
// Dispatch reports as its own trouble (appauth.TransientError, dispatch.Unreachable) on the App
// mint's schedule; any other failure refuses the boot. A project with no repository mapped refuses
// it too. One whose every mapped repository lacks an App boots serving none, each left out and
// reported: it admits nothing, saying why on each issue, until an App is installed and a read of
// the mapping finds it, one the next mapping change or an issue held for it asks for
// (requestMappingRead). nameMapping, once the Apps' tokens are minted, reads GitHub's spelling.
func (w *workflowRuntime) loadMapping(ctx context.Context) error {
	var accepted []mappedRepository
	var refused []refusedRepository
	err := bootprobe.Run(ctx, "repository mapping read", appMintRetry, w.log, readinessAttempt(func(ctx context.Context) error {
		var err error
		accepted, refused, err = readMapping(ctx, w.dispatch, w.dispatchProject, w.installs)
		return err
	}, appMintAttempt, func(err error) bool { return appTransient(err) || dispatch.Unreachable(err) }))
	if err != nil {
		return err
	}
	if len(accepted) == 0 && len(refused) == 0 {
		return fmt.Errorf("project %s has no repository mapped in Dispatch (Settings → Repositories → Projects)", w.dispatchProject)
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
// mapping change in Dispatch or an admission held for it asks for (requestMappingRead). It has
// GitHub intake hear every repository the new mapping serves and every repository a record of the
// project names, so a repository unmapped while an issue is recorded on it is still heard, and
// only then publishes the new mapping: a read that fails before the subscription took publishes
// nothing, so a repository is never served, and an issue never recorded on it, while GitHub intake
// does not hear it, and the refresher reads it again.
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
		added, removed = w.repositories.set(accepted, refused)
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

// refreshOnMappingChange reads the mapping (refreshMapping) on each request, until ctx ends. Each
// attempt is bounded by appMintAttempt, and a failed one is logged and tried again on the App
// mint's backoff, with no limit, until it applies: a read takes the whole mapping, so retrying
// loses no request, and a daemon stopped before one applies reads the mapping at boot. A finished
// read covers every request made before it started.
func (w *workflowRuntime) refreshOnMappingChange(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.mappingChanged:
		}
		wait := appMintRetry.Initial
		for {
			covers := w.mappingRequested.Load()
			attempt, cancel := context.WithTimeout(ctx, appMintAttempt)
			err := w.refreshMapping(attempt)
			cancel()
			if err == nil {
				w.mappingApplied.Store(covers)
				break
			}
			if ctx.Err() != nil {
				return
			}
			w.log.Warn("workflow: the repository mapping read failed, so the daemon serves the mapping it last read; reading it again", "retryIn", wait.String(), "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(2*wait, appMintRetry.Max)
		}
	}
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
// record's once recorded, so a recorded issue's event asks nothing of Dispatch or GitHub: a
// retained event that names no repository (ResolvedRepoMissing) takes the record's, and one naming
// another is applied as it is, for the workflow to keep the record's and say so. For an issue not
// yet recorded, a retained event's repository is read from Dispatch's issue detail, a read that
// fails leaving the event to be retried; then, for an event in todo, what the workflow reads in
// memory when it records the issue (ResolveRepository) is readied: an issue coming up for
// admission (handed to Legion, or a child of a tree) on a repository the mapping did not serve
// when last read waits for a read of the mapping that covers it (awaitMappingRead), so a
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
func (w *workflowRuntime) prepareRepository(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
	return w.prepareIssue(ctx, issue, true, true)
}

// prepareObserved readies release_children's todo child, already observed as Dispatch's issue
// detail shows it now (intake.ObservedIssue), as prepareRepository readies an event's.
func (w *workflowRuntime) prepareObserved(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
	return w.prepareIssue(ctx, issue, true, false)
}

// prepareIssue is prepareRepository, holding back an admission event on a repository not served
// for a read of the mapping only when await is set, and preparing an admission event from
// Dispatch's current view only when observe is set: reconcile's boot read prepares on the mapping
// the boot has just read, from a listing it has just read, and release_children's child is that
// view already.
func (w *workflowRuntime) prepareIssue(ctx context.Context, issue intake.DispatchIssue, await, observe bool) (intake.DispatchIssue, error) {
	recorded, err := w.recordOf(ctx, issue.Key)
	if err != nil {
		return issue, err
	}
	if recorded != nil {
		w.forgetIssue(issue.Key)
		if issue.ResolvedRepoMissing {
			issue.ResolvedRepo, issue.ResolvedRepoMissing = recorded.Repo, false
		}
		return issue, nil
	}
	if !await || !admissionBound(issue) {
		return w.prepareUnrecorded(ctx, issue, false, 0)
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
	w.forgetHolds(issue.Key, prepared.Seq)
	return prepared, nil
}

// prepareUnrecorded readies the event of an issue the workflow does not record, as
// prepareRepository says, holding back an admission on a repository not served for a read of the
// mapping, under the sequence the event was delivered at (held), when await is set.
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
	mapped, refusal, _ := w.repositories.lookup(issue.ResolvedRepo)
	if refusal != "" && await && admissionBound(issue) {
		if err := w.awaitMappingRead(issue, held); err != nil {
			return issue, err
		}
		mapped, refusal, _ = w.repositories.lookup(issue.ResolvedRepo)
	}
	if refusal != "" {
		return issue, nil
	}
	_, err := w.defaultBranches.Named(ctx, mapped.Repo)
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
