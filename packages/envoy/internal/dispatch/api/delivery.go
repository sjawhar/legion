package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// defaultDeliveryWindow is the timeline's default when the caller passes neither from nor to: the
// last 7 days, matching the Acceptance criterion's "one recent 7-day window".
const defaultDeliveryWindow = 7 * 24 * time.Hour

// maxDeliveryWindow bounds how wide [from, to) may be: CONTRACT.md measures 28 days of this
// population at roughly 3,500 pull requests, so three times that backfill window is generous
// headroom for a real dashboard query while still refusing the unbounded "from the epoch" request
// an authenticated-but-careless caller (any agent session; GET /api/v1/delivery/timeline is
// authAny) could otherwise send against the shared pool.
const maxDeliveryWindow = 3 * 28 * 24 * time.Hour

// The timeline's facets, by the query parameter that selects each and the facet_counts key that
// counts it. A pull request carries one or more values for each (deliveryRow.values).
var deliveryFacets = []string{"repo", "parent_agent", "session", "issue", "priority", "component", "author", "rework", "deployed"}

// The facets the page colours merges by, by the color_counts key that counts each.
var deliveryColorFacets = []string{"repo", "author", "priority", "component", "parent_agent"}

// Placeholder facet values, split by cause as the page labels them (features/delivery/lib/facets.ts
// holds the same four): a pull request naming no issue, an issue with no priority or no component,
// and a pull request whose commits name no session.
const (
	deliveryNoIssue     = "__no_issue__"
	deliveryNoPriority  = "__no_priority__"
	deliveryNoComponent = "__no_component__"
	deliveryNoSession   = "__no_session__"
)

// The rework facet's two values, as the page names them (features/delivery/lib/facets.ts's
// ReworkFacet): a pull request that adds value, and one that reworks earlier work.
const (
	deliveryReworkValue  = "value"
	deliveryReworkRework = "rework"
)

// deliveryPrioritySelection reads a priority selection as the page writes it (P0-P3) or as a
// person types it (p0, or the bare digit the issue routes store).
var deliveryPrioritySelection = regexp.MustCompile(`^[pP]?([0-3])$`)

// parseDeliveryWindow reads from/to (RFC3339) from query, defaulting to the last 7 days when
// either is absent, and refuses a malformed value, an inverted window, or one wider than
// maxDeliveryWindow.
func parseDeliveryWindow(query url.Values) (from, to time.Time, err error) {
	if to, err = parseOptionalRFC3339(query, "to", time.Now().UTC()); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if from, err = parseOptionalRFC3339(query, "from", to.Add(-defaultDeliveryWindow)); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, errors.New("from must be before to")
	}
	if to.Sub(from) > maxDeliveryWindow {
		return time.Time{}, time.Time{}, errors.New("from..to must not span more than 84 days")
	}
	return from, to, nil
}

// parseOptionalRFC3339 is query's name parameter as an RFC3339 time, or fallback when the query
// has no such parameter, refusing a value that does not parse with an error naming the parameter.
func parseOptionalRFC3339(query url.Values, name string, fallback time.Time) (time.Time, error) {
	if !query.Has(name) {
		return fallback, nil
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(query.Get(name)))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC3339 timestamp", name)
	}
	return parsed, nil
}

// deliverySettings reads the timeline's configuration record, answering 404
// DELIVERY_NOT_CONFIGURED (or the store's error) and false when it cannot.
func (s *server) deliverySettings(w http.ResponseWriter, r *http.Request) (delivery.DeliverySettings, bool) {
	settings, err := delivery.GetSettings(r.Context(), s.deps.Store.Pool)
	if err != nil {
		if errors.Is(err, delivery.ErrNoSettings) {
			writeError(w, "DELIVERY_NOT_CONFIGURED", http.StatusNotFound, "delivery is not configured; set delivery_settings through PUT /api/v1/settings/delivery")
			return delivery.DeliverySettings{}, false
		}
		s.writeHandlerError(w, err)
		return delivery.DeliverySettings{}, false
	}
	return settings, true
}

// getDeliveryTimeline answers GET /api/v1/delivery/timeline: merges, deploys, pipeline failures
// and waiting-to-deploy PRs within [from, to) and the given facets and search, computed from
// stored facts at request time (deployed_status, root_failing_job, the Parent agent and session
// labels, the issue facts and every count are never stored -- "the server computes every
// measure from stored facts on request"), and the measures of that same window and selection, so a
// page showing both reads the population once. GET /api/v1/delivery/measures is the measures
// alone, for another window. The answer is readDeliveryPopulation's read and
// deliveryTimelineCompute's compute, the two halves the live hub calls too, so a live page's
// snapshot is this body.
func (s *server) getDeliveryTimeline(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	from, to, err := parseDeliveryWindow(query)
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	settings, ok := s.deliverySettings(w, r)
	if !ok {
		return
	}
	population, err := s.readDeliveryPopulation(r.Context(), settings, from, to, query, true)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, deliveryTimelineCompute(settings, population, from, to, time.Now().UTC()))
}

// deliveryTimelineCompute is GET /api/v1/delivery/timeline's body (deliveryTimelineWithMeasures)
// for [from, to) from a population read for that window or narrowed to it. It reads nothing; now
// is only the measures' computed_at.
func deliveryTimelineCompute(settings delivery.DeliverySettings, pop deliveryPopulation, from, to, now time.Time) deliveryTimelineWithMeasures {
	derived := pop.derive(settings)

	// A run's prs[] is every window pull request it shipped first, whatever the facets: the page
	// sizes a deploy and lists its drill-down by everything it shipped.
	prViews := make([]delivery.DeliveryPRView, 0, len(derived.rows))
	for _, row := range derived.rows {
		if row.matches(derived.selection, "") {
			prViews = append(prViews, row.view)
		}
	}

	// The pull requests still waiting at `from`, under the same facets and search, so the
	// waiting line starts the window at their count rather than at zero.
	waitingViews := make([]delivery.DeliveryWaitingPRView, 0, len(derived.waiting))
	for _, waiting := range derived.waiting {
		if waiting.row.matches(derived.selection, "") {
			waitingViews = append(waitingViews, waiting.view)
		}
	}

	runViews := make([]delivery.DeliveryRunView, 0, len(pop.applyRuns))
	for _, run := range pop.applyRuns {
		if run.StartedAt.Before(from) || !run.StartedAt.Before(to) {
			continue
		}
		jobs := pop.jobsByRun[run.RunID]
		shipped := derived.shipped[run.RunID]
		if shipped == nil {
			// Always a slice, never nil, so it always serializes as `[]`, not `null` -- the
			// common case for any successful deploy that shipped no in-window population PR.
			shipped = []delivery.DeliveryShippedPRView{}
		}
		runViews = append(runViews, delivery.DeliveryRunView{
			ID: run.RunID, URL: run.URL, HeadSHA: run.HeadSHA, HeadAt: run.HeadCommitAt,
			StartedAt: run.StartedAt, CompletedAt: run.CompletedAt, Conclusion: run.Conclusion,
			Production:     deliveryProductionView(jobs, settings.Production()),
			FailedJobs:     deliveryJobViews(delivery.FailedJobs(jobs)),
			RootFailingJob: deliveryJobView(delivery.RootFailingJob(jobs)),
			PRs:            shipped,
		})
	}
	// Listed by head commit time, then id, which the page's live merge mirrors
	// (applyDelta.ts's ORDERS); applyRuns itself is in ListRuns' creation order.
	slices.SortFunc(runViews, func(a, b delivery.DeliveryRunView) int {
		return cmp.Or(a.HeadAt.Compare(b.HeadAt), cmp.Compare(a.ID, b.ID))
	})

	return deliveryTimelineWithMeasures{
		DeliveryTimelineResponse: delivery.DeliveryTimelineResponse{
			Window:          delivery.DeliveryWindowView{From: from, To: to},
			PRs:             prViews,
			Waiting:         waitingViews,
			Runs:            runViews,
			FacetCounts:     deliveryFacetCounts(derived.rows, derived.selection),
			ColorCounts:     deliveryColorCounts(derived.rows),
			Components:      derived.components,
			IssueTitles:     derived.issueTitles,
			Freshness:       derived.freshness,
			SessionGroups:   pop.groups,
			SessionTitles:   derived.sessionTitles,
			SessionPRCounts: derived.sessionPRCounts,
		},
		Measures: deliveryMeasuresOver(settings, pop, derived, from, to, now),
	}
}

// deliveryTimelineWithMeasures is the timeline's wire shape: delivery.DeliveryTimelineResponse
// with the window's measures beside it (packages/contracts/src/dispatch-api.ts's
// DeliveryTimelineResponse.measures). The measures' type lives in this package, since the
// measures package imports delivery, so the field is added here rather than in delivery/model.go.
type deliveryTimelineWithMeasures struct {
	delivery.DeliveryTimelineResponse
	Measures deliveryMeasuresResponse `json:"measures"`
}

// deliveryPopulation is every read the timeline's and the measures' answers for one window and
// query make, as stored, before anything is computed from them: the population pull requests
// merged in the window, the deploy repository's pull requests still waiting at its start, the
// deploy runs on main GitHub created from its start (containment's input) and those started inside
// it (the measures'), each with its jobs, the open unowned P0 issues, the session groups, the titles
// of the sessions and the facts of the issues those pull requests name, and the components of
// those issues' projects. Nothing that computes from it reads again (derive), so the live hub
// keeps it and computes a later window from it narrowed (narrowedTo) without a read.
type deliveryPopulation struct {
	query url.Values
	// prs is every population pull request merged in the window, oldest merge first.
	prs []delivery.DeliveryPullRequest
	// withWaiting is whether waiting was read: the deploy repository's pull requests merged before
	// the window that had not shipped by its start, oldest merge first, which only the timeline
	// reads.
	withWaiting bool
	waiting     []delivery.WaitingPullRequest
	// applyRuns is every deploy run on main GitHub created at or after the window's start
	// (delivery.ListRuns), oldest creation first, and jobsByRun their jobs.
	applyRuns []delivery.DeliveryRun
	jobsByRun map[int64][]delivery.DeliveryRunJob
	// windowRuns is every deploy run on main started in the window, oldest start first, and
	// windowJobs their jobs: the measures' deploys.
	windowRuns []delivery.DeliveryRun
	windowJobs map[int64][]delivery.DeliveryRunJob
	unownedP0  []delivery.UnownedIssue
	groups     []delivery.SessionGroup
	titles     map[string]string
	issues     map[string]deliveryIssue
	components map[string]delivery.DeliveryComponentView
	// unfetchable is how many population pull requests can no longer be fetched from GitHub.
	unfetchable int
}

// deliveryDerived is what the timeline and the measures compute from a deliveryPopulation: each
// pull request with its view and its facet values, the request's search and facet selection, the
// waiting rows, which window pull requests each deploy run shipped first, the labels of every
// component and issue the pull requests name, and the freshness.
type deliveryDerived struct {
	// rows is every population pull request merged in the window, before the facets, in merge
	// order: the timeline counts facets and colours over it. A row passing
	// row.matches(selection, "") is one the timeline's prs[] shows and the measures' pull-request
	// figures count.
	rows []deliveryRow
	// shipped is, for each deploy run, every row whose first shipping apply it is, whatever the
	// facets: the timeline sizes a deploy and lists its drill-down by it, and the measures count a
	// deploy as one that shipped a pull request by it, so deploys_with_prs belongs to the deploy.
	shipped     map[int64][]delivery.DeliveryShippedPRView
	selection   deliverySelection
	waiting     []deliveryWaitingRow
	components  map[string]delivery.DeliveryComponentView
	issueTitles map[string]string
	// sessionTitles and sessionPRCounts are, for each session that is the first session of a
	// window pull request (the sessions a Parent agent can be), its title (or its id when it has
	// none) and how many window pull requests it is first on, before the facets.
	sessionTitles   map[string]string
	sessionPRCounts map[string]int
	// freshness is the timeline's and the measures' freshness object: the settings row's intake
	// and reconcile times, the last reconcile error, and how many population pull requests can no
	// longer be fetched from GitHub.
	freshness delivery.DeliveryFreshnessView
}

// deliveryWaitingRow is one pull request still waiting at the window's start: its facet values, so
// the timeline's waiting line counts it under the same facets and search, and its waiting view.
type deliveryWaitingRow struct {
	row  deliveryRow
	view delivery.DeliveryWaitingPRView
}

// readDeliveryPopulation is every read the timeline's and the measures' answers for one window and
// query make (the population pull requests, the deploy runs on main GitHub created at or after
// `from` with their jobs, the measures' runs started in the window with their jobs, and the
// unowned P0 list); nothing that computes from it reads again. With withWaiting it also reads the
// deploy repository's pull requests still waiting at `from` (ListPullRequestsWaitingAt), resolved
// through the same issue and session lookups so the timeline's waiting line filters them alike;
// the measures read none.
func (s *server) readDeliveryPopulation(ctx context.Context, settings delivery.DeliverySettings, from, to time.Time, query url.Values, withWaiting bool) (deliveryPopulation, error) {
	return s.readDeliveryRows(ctx, settings, from, to, query, withWaiting, true)
}

// readDeliveryRows is readDeliveryPopulation, and with withMeasures false leaves out the measures'
// runs and unowned P0 list, which the Pipeline page's lead times do not read.
func (s *server) readDeliveryRows(ctx context.Context, settings delivery.DeliverySettings, from, to time.Time, query url.Values, withWaiting, withMeasures bool) (deliveryPopulation, error) {
	pool := s.deps.Store.Pool
	pop := deliveryPopulation{query: query, withWaiting: withWaiting, jobsByRun: map[int64][]delivery.DeliveryRunJob{}, windowJobs: map[int64][]delivery.DeliveryRunJob{}}

	// Independent reads, side by side. They never shared a transaction (each is its own
	// statement on the pool, so each already read its own snapshot), so running them at once
	// loses no consistency they had. Each takes one connection under a hold mark of its own
	// (store.ForConcurrentRead): on the request's shared mark one read's open cursor would refuse
	// the rest. The run jobs depend on the runs, so they follow the runs in the same goroutine.
	//
	// A timeline request therefore holds up to seven of the pool's 16 connections
	// (store.sharedPoolSize) for the length of its longest read, where it held one at a time. That
	// is fine because none of them holds a lock or opens a transaction, so none waits on
	// another caller or makes another caller wait on it: the deadlock the pool's guard exists to
	// prevent needs a caller already holding a connection, which ForConcurrentRead refuses. A read
	// past the pool's free connections waits for one, as any request does.
	runsWithJobs := func(list func(ctx context.Context) ([]delivery.DeliveryRun, error), runs *[]delivery.DeliveryRun, jobs *map[int64][]delivery.DeliveryRunJob) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			listed, err := list(ctx)
			if err != nil {
				return err
			}
			runIDs := make([]int64, len(listed))
			for i, run := range listed {
				runIDs[i] = run.RunID
			}
			byRun, err := delivery.ListRunJobsForRuns(ctx, pool, settings.DeployRepo, runIDs)
			if err != nil {
				return err
			}
			*runs, *jobs = listed, byRun
			return nil
		}
	}
	independent := []func(ctx context.Context) error{
		func(ctx context.Context) (err error) {
			pop.prs, err = delivery.ListPullRequestsInWindow(ctx, pool, from, to)
			return err
		},
		// Every deploy run on main GitHub created at or after `from`: the containment
		// algorithm needs every apply from there forward, unbounded past `to`, since a PR merged
		// just before `to` may first ship after it -- bounding this query to [from, to) would
		// wrongly read such a PR as "waiting". A production job on another branch ships no PR
		// (decision 7). The timeline's runs[] list is filtered to [from, to) separately.
		runsWithJobs(func(ctx context.Context) ([]delivery.DeliveryRun, error) {
			return delivery.ListRuns(ctx, pool, settings.DeployRepo, delivery.DeliveryRunKindDeploy, "main", from)
		}, &pop.applyRuns, &pop.jobsByRun),
		func(ctx context.Context) (err error) {
			pop.unfetchable, err = delivery.CountUnfetchablePullRequests(ctx, pool)
			return err
		},
		func(ctx context.Context) (err error) {
			pop.groups, err = delivery.ListSessionGroups(ctx, pool)
			return err
		},
	}
	if withMeasures {
		// Whether a run reached production is decided on the window's own runs and their jobs,
		// never on the creation-time list containment reads, so every run the window counts is
		// judged.
		independent = append(independent,
			runsWithJobs(func(ctx context.Context) ([]delivery.DeliveryRun, error) {
				return delivery.ListRunsStartedIn(ctx, pool, settings.DeployRepo, delivery.DeliveryRunKindDeploy, "main", "", from, to)
			}, &pop.windowRuns, &pop.windowJobs),
			func(ctx context.Context) (err error) {
				pop.unownedP0, err = delivery.ListUnownedP0(ctx, pool)
				return err
			})
	}
	if withWaiting {
		// The deploy repository's pull requests still waiting at `from`, so the waiting line
		// starts the window at their count rather than at zero.
		independent = append(independent, func(ctx context.Context) (err error) {
			pop.waiting, err = delivery.ListPullRequestsWaitingAt(ctx, pool, settings.DeployRepo, settings.Production(), from)
			return err
		})
	}
	reads, readsCtx := errgroup.WithContext(ctx)
	read := func(fn func(ctx context.Context) error) error {
		readCtx, err := store.ForConcurrentRead(readsCtx)
		if err != nil {
			return err
		}
		reads.Go(func() error { return fn(readCtx) })
		return nil
	}
	for _, fn := range independent {
		if err := read(fn); err != nil {
			_ = reads.Wait()
			return deliveryPopulation{}, err
		}
	}
	if err := reads.Wait(); err != nil {
		return deliveryPopulation{}, err
	}
	// Warned here, once per run, rather than in the compute, which runs again on every expiry.
	s.productionApplies(pop.applyRuns, pop.jobsByRun, settings.Production())
	s.productionApplies(pop.windowRuns, pop.windowJobs, settings.Production())

	sessionIDs := []string{}
	issueKeys := []string{}
	for _, pr := range pop.prs {
		sessionIDs = append(sessionIDs, pr.Sessions...)
		if pr.IssueKey != nil {
			issueKeys = append(issueKeys, *pr.IssueKey)
		}
	}
	for _, pr := range pop.waiting {
		sessionIDs = append(sessionIDs, pr.Sessions...)
		if pr.IssueKey != nil {
			issueKeys = append(issueKeys, *pr.IssueKey)
		}
	}
	var err error
	if pop.titles, err = delivery.ResolveSessionTitles(ctx, pool, sessionIDs); err != nil {
		return deliveryPopulation{}, err
	}
	if pop.issues, err = s.deliveryIssueFacts(ctx, issueKeys); err != nil {
		return deliveryPopulation{}, err
	}
	if pop.components, err = s.deliveryComponents(ctx, deliveryIssueProjects(pop.issues)); err != nil {
		return deliveryPopulation{}, err
	}
	return pop, nil
}

// deliveryIssueProjects is the projects issues belong to, once each.
func deliveryIssueProjects(issues map[string]deliveryIssue) []string {
	projects := []string{}
	for _, issue := range issues {
		if !slices.Contains(projects, issue.project) {
			projects = append(projects, issue.project)
		}
	}
	return projects
}

// derive computes, from the population alone, each pull request's deploy from the deploy runs'
// applies, its issue's facts, its Parent agent and its facet values, the waiting rows, and the
// labels and selection the timeline reads beside them. The issue titles and components are those
// of the issues the population's pull requests name, so a narrowed population derives what a fresh
// read of its window would.
func (p deliveryPopulation) derive(settings delivery.DeliverySettings) deliveryDerived {
	applies := delivery.ProductionApplies(p.applyRuns, p.jobsByRun, settings.Production()).Applies
	parentAgents := delivery.NewParentAgents(p.groups, p.titles)
	sessionTitles := map[string]string{}
	sessionPRCounts := map[string]int{}
	issueTitles := map[string]string{}
	projects := map[string]bool{}
	nameIssue := func(pr delivery.DeliveryPullRequest) {
		if pr.IssueKey == nil {
			return
		}
		if issue, found := p.issues[*pr.IssueKey]; found {
			issueTitles[*pr.IssueKey] = issue.title
			projects[issue.project] = true
		}
	}
	for _, pr := range p.prs {
		nameIssue(pr)
		if len(pr.Sessions) == 0 {
			continue
		}
		first := pr.Sessions[0]
		sessionPRCounts[first]++
		sessionTitles[first] = delivery.SessionTitle(p.titles, first)
	}
	for _, pr := range p.waiting {
		nameIssue(pr.DeliveryPullRequest)
	}
	components := map[string]delivery.DeliveryComponentView{}
	for key, component := range p.components {
		if project, _, _ := strings.Cut(key, "/"); projects[project] {
			components[key] = component
		}
	}

	// viewOf is a pull request as the timeline shows it, shipped by deployRun at deployedAt.
	viewOf := func(pr delivery.DeliveryPullRequest, status delivery.DeployedStatus, deployRun *int64, deployedAt *time.Time) delivery.DeliveryPRView {
		view := delivery.DeliveryPRView{
			ID: pr.Repo + "#" + strconv.Itoa(pr.Number), Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL,
			Author: pr.Author, CreatedAt: pr.CreatedAt, MergedAt: pr.MergedAt, FirstCommitAt: pr.FirstCommitAt,
			Additions: pr.Additions, Deletions: pr.Deletions, Partial: pr.Partial, Rework: pr.Rework,
			Issue: pr.IssueKey, Components: []string{}, Sessions: pr.Sessions,
			DeployRun: deployRun, DeployedAt: deployedAt, DeployedStatus: status,
			UnfetchableReason: pr.UnfetchableReason,
		}
		if pr.IssueKey != nil {
			if issue, found := p.issues[*pr.IssueKey]; found {
				view.IssueTitle = &issue.title
				view.Components = issue.components
				if issue.priority != nil {
					label := "P" + strconv.Itoa(*issue.priority)
					view.Priority = &label
				}
			}
		}
		if len(pr.Sessions) > 0 {
			agent := parentAgents.Value(pr.Sessions[0])
			view.ParentAgent = &agent
		}
		return view
	}

	rows := make([]deliveryRow, 0, len(p.prs))
	shipped := map[int64][]delivery.DeliveryShippedPRView{}
	for _, pr := range p.prs {
		apply := delivery.ContainingRun(pr, settings.DeployRepo, applies)
		var deployRun *int64
		var deployedAt *time.Time
		if apply != nil {
			runID, completedAt := apply.RunID, apply.CompletedAt
			deployRun, deployedAt = &runID, &completedAt
		}
		view := viewOf(pr, delivery.ComputeDeployedStatus(pr, settings.DeployRepo, apply), deployRun, deployedAt)
		if deployRun != nil {
			shipped[*deployRun] = append(shipped[*deployRun], delivery.DeliveryShippedPRView{ID: view.ID, Title: view.Title})
		}
		rows = append(rows, newDeliveryRow(view))
	}

	waitingRows := make([]deliveryWaitingRow, 0, len(p.waiting))
	for _, pr := range p.waiting {
		status := delivery.DeployedStatusWaiting
		if pr.DeployRun != nil {
			status = delivery.DeployedStatusDeployed
		}
		waitingRows = append(waitingRows, deliveryWaitingRow{
			row:  newDeliveryRow(viewOf(pr.DeliveryPullRequest, status, pr.DeployRun, pr.DeployedAt)),
			view: delivery.DeliveryWaitingPRView{MergedAt: *pr.MergedAt, DeployedAt: pr.DeployedAt},
		})
	}

	return deliveryDerived{
		rows:            rows,
		shipped:         shipped,
		selection:       parseDeliverySelection(p.query, components),
		waiting:         waitingRows,
		components:      components,
		issueTitles:     issueTitles,
		sessionTitles:   sessionTitles,
		sessionPRCounts: sessionPRCounts,
		freshness: delivery.DeliveryFreshnessView{
			LastEventAt: settings.LastEventAt, LastReconcileAt: settings.LastReconcileAt, LastError: settings.LastError,
			UnfetchableCount: p.unfetchable,
		},
	}
}

// narrowedTo is the population for [from', to') with from' at or after the start it was read for.
// A row merged before from' leaves the window, and joins the waiting entries when its repo is the
// deploy repository and it had not shipped by from' (ListPullRequestsWaitingAt reads only
// pr.repo = deployRepo; the window's rows hold every population repo). A waiting entry that shipped
// before from' leaves them. A run started before from', or one GitHub created before from', leaves
// the lists that filter on that time. The maps keep only the keys the remaining rows and runs name
// (derive reads them by those keys). This is ListPullRequestsWaitingAt's rule: a pull request
// shipped by from' exactly when its ship time (the earliest successful production job of a run
// created after its merge) is before from'. A leaving row's ship is read from the runs read here,
// which hold every run created after its merge, since it merged at or after the start they were
// read from. The window's end moves with from', past the read's end: a preset's read ends at the
// present, so nothing stored lies past it.
func (p deliveryPopulation) narrowedTo(settings delivery.DeliverySettings, from time.Time) deliveryPopulation {
	narrowed := p
	narrowed.prs = make([]delivery.DeliveryPullRequest, 0, len(p.prs))
	var left []delivery.DeliveryPullRequest
	for _, pr := range p.prs {
		if pr.MergedAt.Before(from) {
			left = append(left, pr)
			continue
		}
		narrowed.prs = append(narrowed.prs, pr)
	}
	narrowed.applyRuns, narrowed.jobsByRun = runsWhere(p.applyRuns, p.jobsByRun, func(run delivery.DeliveryRun) bool { return !run.Created().Before(from) })
	narrowed.windowRuns, narrowed.windowJobs = runsWhere(p.windowRuns, p.windowJobs, func(run delivery.DeliveryRun) bool { return !run.StartedAt.Before(from) })
	if !p.withWaiting {
		return narrowed
	}
	narrowed.waiting = make([]delivery.WaitingPullRequest, 0, len(p.waiting)+len(left))
	for _, waiting := range p.waiting {
		if waiting.DeployedAt == nil || !waiting.DeployedAt.Before(from) {
			narrowed.waiting = append(narrowed.waiting, waiting)
		}
	}
	applies := delivery.ProductionApplies(p.applyRuns, p.jobsByRun, settings.Production()).Applies
	for _, pr := range left {
		if pr.Repo != settings.DeployRepo {
			continue
		}
		waiting := delivery.WaitingPullRequest{DeliveryPullRequest: pr}
		if apply := delivery.ContainingRun(pr, settings.DeployRepo, applies); apply != nil {
			if apply.CompletedAt.Before(from) {
				continue
			}
			runID, completedAt := apply.RunID, apply.CompletedAt
			waiting.DeployRun, waiting.DeployedAt = &runID, &completedAt
		}
		narrowed.waiting = append(narrowed.waiting, waiting)
	}
	return narrowed
}

// runsWhere is the runs keep holds, in their order, with only their jobs.
func runsWhere(runs []delivery.DeliveryRun, jobs map[int64][]delivery.DeliveryRunJob, keep func(delivery.DeliveryRun) bool) ([]delivery.DeliveryRun, map[int64][]delivery.DeliveryRunJob) {
	kept := make([]delivery.DeliveryRun, 0, len(runs))
	keptJobs := make(map[int64][]delivery.DeliveryRunJob, len(runs))
	for _, run := range runs {
		if !keep(run) {
			continue
		}
		kept = append(kept, run)
		if runJobs, ok := jobs[run.RunID]; ok {
			keptJobs[run.RunID] = runJobs
		}
	}
	return kept, keptJobs
}

// nextExpiry is the first moment after now at which a preset window of length n, ending at the
// present, loses something the population holds or its daily series moves: just past the oldest
// row's merge + n, the oldest run start (each list that filters on starts) + n, the oldest creation
// among the runs ListRuns read + n, and each waiting entry's ship time + n; and the next UTC
// midnight (measures.DailySeries, measures.go:240), where a whole-day window's first and last days
// move together. Nothing else in the timeline or the measures reads the present (the LEGION-690 PR's
// Task 0 checks, Step 17).
func nextExpiry(pop deliveryPopulation, n time.Duration, now time.Time) time.Time {
	next := nextUTCMidnight(now)
	consider := func(leaves time.Time) {
		// A row is in the window while its time is at or after the start, so it leaves just past
		// its time + n.
		if at := leaves.Add(n).Add(time.Nanosecond); at.After(now) && at.Before(next) {
			next = at
		}
	}
	for _, pr := range pop.prs {
		consider(*pr.MergedAt)
	}
	for _, run := range pop.applyRuns {
		consider(run.Created())
		consider(run.StartedAt)
	}
	for _, run := range pop.windowRuns {
		consider(run.StartedAt)
	}
	for _, waiting := range pop.waiting {
		if waiting.DeployedAt != nil {
			consider(*waiting.DeployedAt)
		}
	}
	return next
}

// nextUTCMidnight is the first UTC midnight after t.
func nextUTCMidnight(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

// getDeliveryRun answers GET /api/v1/delivery/runs/{id}: one run of the deploy repository with
// every job it ran, by when each started (a job that never started last, then by name), for the
// drill-down's job lists.
func (s *server) getDeliveryRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	runID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || runID < 1 {
		writeError(w, "RUN_ID_INPUT", http.StatusBadRequest, "run id must be a positive integer")
		return
	}
	settings, ok := s.deliverySettings(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	pool := s.deps.Store.Pool
	run, err := delivery.ScanRun(pool.QueryRow(ctx, `select `+delivery.RunColumns+` from delivery_runs where repo = $1 and run_id = $2`, settings.DeployRepo, runID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "NOT_FOUND", http.StatusNotFound, "no run "+strconv.FormatInt(runID, 10)+" of "+settings.DeployRepo)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	jobs, err := delivery.ListRunJobs(ctx, pool, settings.DeployRepo, runID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	slices.SortFunc(jobs, func(a, b delivery.DeliveryRunJob) int {
		switch {
		case a.StartedAt == nil && b.StartedAt != nil:
			return 1
		case a.StartedAt != nil && b.StartedAt == nil:
			return -1
		case a.StartedAt != nil && !a.StartedAt.Equal(*b.StartedAt):
			return a.StartedAt.Compare(*b.StartedAt)
		}
		return strings.Compare(a.Name, b.Name)
	})
	views := make([]delivery.DeliveryRunJobDetailView, len(jobs))
	for i, job := range jobs {
		views[i] = delivery.DeliveryRunJobDetailView{Name: job.Name, StartedAt: job.StartedAt, CompletedAt: job.CompletedAt, Conclusion: job.Conclusion}
	}
	WriteJSON(w, http.StatusOK, delivery.DeliveryRunDetailView{ID: run.RunID, URL: run.URL, Jobs: views})
}

func deliveryJobViews(jobs []delivery.DeliveryRunJob) []delivery.DeliveryRunJobView {
	views := make([]delivery.DeliveryRunJobView, len(jobs))
	for i, job := range jobs {
		views[i] = delivery.DeliveryRunJobView{Name: job.Name, CompletedAt: job.CompletedAt}
	}
	return views
}

func deliveryJobView(job *delivery.DeliveryRunJob) *delivery.DeliveryRunJobView {
	if job == nil {
		return nil
	}
	return &delivery.DeliveryRunJobView{Name: job.Name, CompletedAt: job.CompletedAt}
}

// deliveryProductionView is the run's production job (Production.Job), its conclusion as a deploy
// reads it (Production.Conclusion: skipped for a job that succeeded having applied nothing), or nil
// when the run has none.
func deliveryProductionView(jobs []delivery.DeliveryRunJob, production delivery.Production) *delivery.DeliveryRunProductionView {
	job := production.Job(jobs)
	if job == nil {
		return nil
	}
	return &delivery.DeliveryRunProductionView{Conclusion: production.Conclusion(*job), CompletedAt: job.CompletedAt}
}

// productionApplies is delivery.ProductionApplies for a read. It warns once per run and
// configuration for the life of the process about each successful run with no listed production
// job, naming every listed name, and about each run whose production job lists no step named the
// configured apply step: the timeline and the measures judge the same runs on every read, so a
// line would otherwise repeat on every page load. A changed list or step is a new key, so a
// misconfiguration is named again under its new value.
func (s *server) productionApplies(runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob, production delivery.Production) []delivery.Apply {
	judged := delivery.ProductionApplies(runs, jobsByRun, production)
	names := strings.Join(production.JobNames, "\x00")
	for _, missing := range judged.Unmatched {
		if _, warned := s.warnedUnmatchedRuns.LoadOrStore(warnedProductionRun{repo: missing.Run.Repo, runID: missing.Run.RunID, jobNames: names}, struct{}{}); warned {
			continue
		}
		slog.Warn("dispatch delivery: a successfully concluded run has no job matching the configured production_job_names -- it will never count as a deploy",
			"repo", missing.Run.Repo, "run_id", missing.Run.RunID, "configured_job_names", production.JobNames, "job_names_seen", missing.JobNames)
	}
	for _, run := range judged.StepMissing {
		key := warnedProductionRun{repo: run.Repo, runID: run.RunID, jobNames: names, applyStep: *production.ApplyStep}
		if _, warned := s.warnedUnmatchedRuns.LoadOrStore(key, struct{}{}); warned {
			continue
		}
		slog.Warn("dispatch delivery: a run's production job succeeded but lists no step named the configured production_apply_step_name -- it will never count as a deploy",
			"repo", run.Repo, "run_id", run.RunID, "configured_job_names", production.JobNames, "configured_apply_step", *production.ApplyStep)
	}
	return judged.Applies
}

// deliveryIssue is what the timeline reads of one issue a pull request names: its project, title,
// priority, and effective components (qualified `<project>/<id>`, resolved as every issue read
// resolves them: the nearest issue on its parent chain that chose, through issueComponentsLateral).
type deliveryIssue struct {
	project    string
	title      string
	priority   *int
	components []string
}

func (s *server) deliveryIssueFacts(ctx context.Context, keys []string) (map[string]deliveryIssue, error) {
	issues := make(map[string]deliveryIssue, len(keys))
	if len(keys) == 0 {
		return issues, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select i.key, i.project_key, i.title, i.priority, coalesce(comp.ids, '{}'::text[])
		from issues i
		`+issueComponentsLateral+`
		where i.key = any($1)
	`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var issue deliveryIssue
		var ids []string
		if err := rows.Scan(&key, &issue.project, &issue.title, &issue.priority, &ids); err != nil {
			return nil, err
		}
		issue.components = make([]string, len(ids))
		for i, id := range ids {
			issue.components[i] = deliveryComponentKey(issue.project, id)
		}
		issues[key] = issue
	}
	return issues, rows.Err()
}

// deliveryComponentKey names a component as the graph addresses it, `<project>/<id>`: component
// ids are unique only within their project.
func deliveryComponentKey(project, id string) string {
	return project + "/" + id
}

// deliveryComponents names every component of projects, keyed by deliveryComponentKey.
func (s *server) deliveryComponents(ctx context.Context, projects []string) (map[string]delivery.DeliveryComponentView, error) {
	components := map[string]delivery.DeliveryComponentView{}
	if len(projects) == 0 {
		return components, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `select project_key, id, title, parent from components where project_key = any($1)`, projects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var project, id, title string
		var parent *string
		if err := rows.Scan(&project, &id, &title, &parent); err != nil {
			return nil, err
		}
		view := delivery.DeliveryComponentView{Title: title}
		if parent != nil {
			qualified := deliveryComponentKey(project, *parent)
			view.Parent = &qualified
		}
		components[deliveryComponentKey(project, id)] = view
	}
	return components, rows.Err()
}

// deliveryRow is one window pull request's view with its value for each facet: one value for most,
// every session for session, every effective component for component, and none for issue when it
// names no issue (the issue facet offers no placeholder).
type deliveryRow struct {
	view   delivery.DeliveryPRView
	values map[string][]string
	search string
}

// newDeliveryRow reads a view's facet values. Every colour-by facet (deliveryColorFacets) gets at
// least one value, a placeholder when the pull request has none, since deliveryColorCounts colours
// a row by its first. session gets no placeholder: parent_agent's __no_session__ already filters
// the pull requests no session wrote.
func newDeliveryRow(view delivery.DeliveryPRView) deliveryRow {
	values := map[string][]string{
		"repo":     {view.Repo},
		"session":  view.Sessions,
		"issue":    {},
		"author":   {view.Author},
		"rework":   {deliveryReworkValue},
		"deployed": {string(view.DeployedStatus)},
	}
	if view.ParentAgent != nil {
		values["parent_agent"] = []string{*view.ParentAgent}
	} else {
		values["parent_agent"] = []string{deliveryNoSession}
	}
	if view.Rework {
		values["rework"] = []string{deliveryReworkRework}
	}
	switch {
	case view.Issue == nil:
		values["priority"] = []string{deliveryNoIssue}
		values["component"] = []string{deliveryNoIssue}
	default:
		values["issue"] = []string{*view.Issue}
		values["priority"] = []string{deliveryNoPriority}
		if view.Priority != nil {
			values["priority"] = []string{*view.Priority}
		}
		values["component"] = []string{deliveryNoComponent}
		if len(view.Components) > 0 {
			values["component"] = view.Components
		}
	}
	return deliveryRow{view: view, values: values, search: strings.ToLower(view.Title) + "\x00" + strings.ToLower(view.ID)}
}

// deliverySelection is the request's facet selections, each a set of values a row must hold one of
// (a selected component standing for itself and every component below it), and its search, the
// lowercased text a row's title or id must contain.
type deliverySelection struct {
	facets map[string][]string
	search string
}

func parseDeliverySelection(query url.Values, components map[string]delivery.DeliveryComponentView) deliverySelection {
	selection := deliverySelection{facets: map[string][]string{}, search: strings.ToLower(strings.TrimSpace(query.Get("q")))}
	for _, facet := range deliveryFacets {
		values := query[facet]
		if len(values) == 0 {
			continue
		}
		switch facet {
		case "priority":
			normalized := make([]string, len(values))
			for i, value := range values {
				normalized[i] = value
				if match := deliveryPrioritySelection.FindStringSubmatch(value); match != nil {
					normalized[i] = "P" + match[1]
				}
			}
			values = normalized
		case "component":
			values = deliveryComponentDescendants(components, values)
		}
		selection.facets[facet] = values
	}
	return selection
}

// deliveryComponentDescendants is selected plus every component below a selected one.
func deliveryComponentDescendants(components map[string]delivery.DeliveryComponentView, selected []string) []string {
	children := map[string][]string{}
	for id, component := range components {
		if component.Parent != nil {
			children[*component.Parent] = append(children[*component.Parent], id)
		}
	}
	expanded := []string{}
	stack := slices.Clone(selected)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if slices.Contains(expanded, id) {
			continue
		}
		expanded = append(expanded, id)
		stack = append(stack, children[id]...)
	}
	return expanded
}

// matches is whether the row passes the search and every selected facet but skip.
func (row deliveryRow) matches(selection deliverySelection, skip string) bool {
	if selection.search != "" && !strings.Contains(row.search, selection.search) {
		return false
	}
	for facet, selected := range selection.facets {
		if facet == skip {
			continue
		}
		if !slices.ContainsFunc(row.values[facet], func(value string) bool { return slices.Contains(selected, value) }) {
			return false
		}
	}
	return true
}

// deliveryFacetCounts counts, per facet, the rows passing the search and every other facet's
// selection by that facet's values, so selecting a value never zeroes its own count.
func deliveryFacetCounts(rows []deliveryRow, selection deliverySelection) map[string]map[string]int {
	counts := make(map[string]map[string]int, len(deliveryFacets))
	for _, facet := range deliveryFacets {
		counts[facet] = map[string]int{}
	}
	for _, row := range rows {
		for _, facet := range deliveryFacets {
			if !row.matches(selection, facet) {
				continue
			}
			for _, value := range row.values[facet] {
				counts[facet][value]++
			}
		}
	}
	return counts
}

// deliveryColorCounts counts every row, with no facet or search applied, by the value it is
// coloured by under each colour-by facet: its first value there. newDeliveryRow gives every
// colour-by facet at least one value (TestNewDeliveryRowGivesEveryColourFacetAValue holds it), so
// the index never misses.
func deliveryColorCounts(rows []deliveryRow) map[string]map[string]int {
	counts := make(map[string]map[string]int, len(deliveryColorFacets))
	for _, facet := range deliveryColorFacets {
		counts[facet] = map[string]int{}
	}
	for _, row := range rows {
		for _, facet := range deliveryColorFacets {
			counts[facet][row.values[facet][0]]++
		}
	}
	return counts
}
