package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
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
	to = time.Now().UTC()
	if query.Has("to") {
		to, err = time.Parse(time.RFC3339, strings.TrimSpace(query.Get("to")))
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("to must be an RFC3339 timestamp")
		}
	}
	from = to.Add(-defaultDeliveryWindow)
	if query.Has("from") {
		from, err = time.Parse(time.RFC3339, strings.TrimSpace(query.Get("from")))
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("from must be an RFC3339 timestamp")
		}
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, errors.New("from must be before to")
	}
	if to.Sub(from) > maxDeliveryWindow {
		return time.Time{}, time.Time{}, errors.New("from..to must not span more than 84 days")
	}
	return from, to, nil
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
// stored facts at request time (deployed_status, root_failing_job, the parent_agent/session
// display labels, the issue facts and every count are never stored -- "the server computes every
// measure from stored facts on request").
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

	ctx := r.Context()
	// The deploy repository's pull requests still waiting at `from`, so the waiting line starts
	// the window at their count rather than at zero.
	waitingPRs, err := delivery.ListPullRequestsWaitingAt(ctx, s.deps.Store.Pool, settings.DeployRepo, settings.ProductionJobName, from)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	population, err := s.filteredDeliveryPullRequests(ctx, settings, from, to, query, waitingPRs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	// A run's prs[] is every window pull request it shipped first, whatever the facets: the page
	// sizes a deploy and lists its drill-down by everything it shipped.
	shippedBy := map[int64][]delivery.DeliveryShippedPRView{}
	prViews := make([]delivery.DeliveryPRView, 0, len(population.rows))
	for _, row := range population.rows {
		if row.view.DeployRun != nil {
			shippedBy[*row.view.DeployRun] = append(shippedBy[*row.view.DeployRun], delivery.DeliveryShippedPRView{ID: row.view.ID, Title: row.view.Title})
		}
		if row.matches(population.selection, "") {
			prViews = append(prViews, row.view)
		}
	}

	// The pull requests still waiting at `from`, under the same facets and search, so the
	// waiting line starts the window at their count rather than at zero.
	waitingViews := make([]delivery.DeliveryWaitingPRView, 0, len(population.waiting))
	for _, waiting := range population.waiting {
		if waiting.row.matches(population.selection, "") {
			waitingViews = append(waitingViews, waiting.view)
		}
	}

	runViews := make([]delivery.DeliveryRunView, 0, len(population.applyRuns))
	for _, run := range population.applyRuns {
		if run.StartedAt.Before(from) || !run.StartedAt.Before(to) {
			continue
		}
		jobs := population.jobsByRun[run.RunID]
		shipped := shippedBy[run.RunID]
		if shipped == nil {
			// Always a slice, never nil, so it always serializes as `[]`, not `null` -- the
			// common case for any successful deploy that shipped no in-window population PR.
			shipped = []delivery.DeliveryShippedPRView{}
		}
		runViews = append(runViews, delivery.DeliveryRunView{
			ID: run.RunID, URL: run.URL, HeadSHA: run.HeadSHA, HeadAt: run.HeadCommitAt,
			StartedAt: run.StartedAt, CompletedAt: run.CompletedAt, Conclusion: run.Conclusion,
			Production:     deliveryProductionView(jobs, settings.ProductionJobName),
			FailedJobs:     deliveryJobViews(delivery.FailedJobs(jobs)),
			RootFailingJob: deliveryJobView(delivery.RootFailingJob(jobs)),
			PRs:            shipped,
		})
	}

	freshness, err := s.deliveryFreshness(ctx, settings)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	WriteJSON(w, http.StatusOK, delivery.DeliveryTimelineResponse{
		Window:      delivery.DeliveryWindowView{From: from, To: to},
		PRs:         prViews,
		Waiting:     waitingViews,
		Runs:        runViews,
		FacetCounts: deliveryFacetCounts(population.rows, population.selection),
		ColorCounts: deliveryColorCounts(population.rows),
		Components:  population.components,
		IssueTitles: population.issueTitles,
		Freshness:   freshness,
	})
}

// deliveryPopulation is one window's population pull requests, each with its view and its facet
// values, and what the timeline reads beside them: the request's search and facet selection, the
// pull requests still waiting at the window's start, the deploy runs whose applies decided each
// pull request's deploy, those runs' jobs, and the labels of every component and issue the
// window's pull requests name.
type deliveryPopulation struct {
	// rows is every population pull request merged in [from, to), before the facets, in merge
	// order: the timeline counts facets and colours over it and sizes each deploy by it, and the
	// measures' deploys_with_prs reads it. A row passing row.matches(selection, "") is one the
	// timeline's prs[] shows and the measures' pull-request figures count.
	rows        []deliveryRow
	selection   deliverySelection
	waiting     []deliveryWaitingRow
	applyRuns   []delivery.DeliveryRun
	jobsByRun   map[int64][]delivery.DeliveryRunJob
	components  map[string]delivery.DeliveryComponentView
	issueTitles map[string]string
}

// deliveryWaitingRow is one pull request still waiting at the window's start: its facet values, so
// the timeline's waiting line counts it under the same facets and search, and its waiting view.
type deliveryWaitingRow struct {
	row  deliveryRow
	view delivery.DeliveryWaitingPRView
}

// filteredDeliveryPullRequests reads the population pull requests merged in [from, to), derives
// each one's deploy from the deploy runs on main listed from `from` by head commit, joins its
// issue's facts and its sessions' titles, and parses the request's search and facets. waiting is
// the pull requests still waiting at `from` (ListPullRequestsWaitingAt), resolved through the
// same issue and session lookups so the timeline's waiting line filters them alike; the measures
// pass none. The timeline and the measures both call it, so a pull request the timeline shows is
// exactly one the measures count.
func (s *server) filteredDeliveryPullRequests(ctx context.Context, settings delivery.DeliverySettings, from, to time.Time, query url.Values, waiting []delivery.WaitingPullRequest) (deliveryPopulation, error) {
	pool := s.deps.Store.Pool
	prs, err := delivery.ListPullRequestsInWindow(ctx, pool, from, to)
	if err != nil {
		return deliveryPopulation{}, err
	}

	// Every deploy run on main with a head commit at or after `from`: the containment algorithm
	// needs every apply from there forward, unbounded past `to`, since a PR merged just before `to`
	// may first ship after it -- bounding this query to [from, to) would wrongly read such a PR as
	// "waiting". A production job on another branch ships no PR (decision 7). The timeline's
	// runs[] list is filtered to [from, to) separately.
	applyRuns, err := delivery.ListRuns(ctx, pool, settings.DeployRepo, delivery.DeliveryRunKindDeploy, "main", from)
	if err != nil {
		return deliveryPopulation{}, err
	}
	runIDs := make([]int64, len(applyRuns))
	for i, run := range applyRuns {
		runIDs[i] = run.RunID
	}
	jobsByRun, err := delivery.ListRunJobsForRuns(ctx, pool, settings.DeployRepo, runIDs)
	if err != nil {
		return deliveryPopulation{}, err
	}
	applies := delivery.ProductionApplies(applyRuns, jobsByRun, settings.ProductionJobName)

	sessionIDs := []string{}
	issueKeys := []string{}
	collect := func(pr delivery.DeliveryPullRequest) {
		sessionIDs = append(sessionIDs, pr.Sessions...)
		if pr.IssueKey != nil {
			issueKeys = append(issueKeys, *pr.IssueKey)
		}
	}
	for _, pr := range prs {
		collect(pr)
	}
	for _, pr := range waiting {
		collect(pr.DeliveryPullRequest)
	}
	titles, err := delivery.ResolveSessionTitles(ctx, pool, sessionIDs)
	if err != nil {
		return deliveryPopulation{}, err
	}
	issues, err := s.deliveryIssueFacts(ctx, issueKeys)
	if err != nil {
		return deliveryPopulation{}, err
	}
	projects := []string{}
	issueTitles := make(map[string]string, len(issues))
	for key, issue := range issues {
		issueTitles[key] = issue.title
		if !slices.Contains(projects, issue.project) {
			projects = append(projects, issue.project)
		}
	}
	components, err := s.deliveryComponents(ctx, projects)
	if err != nil {
		return deliveryPopulation{}, err
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
			if issue, found := issues[*pr.IssueKey]; found {
				view.IssueTitle = &issue.title
				view.Components = issue.components
				if issue.priority != nil {
					label := "P" + strconv.Itoa(*issue.priority)
					view.Priority = &label
				}
			}
		}
		if len(pr.Sessions) > 0 {
			agent := delivery.DisplayAgent(pr.Sessions[0], titles)
			view.ParentAgent = &agent
		}
		return view
	}

	rows := make([]deliveryRow, 0, len(prs))
	for _, pr := range prs {
		apply := delivery.ContainingRun(pr, settings.DeployRepo, applies)
		var deployRun *int64
		var deployedAt *time.Time
		if apply != nil {
			runID, completedAt := apply.RunID, apply.CompletedAt
			deployRun, deployedAt = &runID, &completedAt
		}
		rows = append(rows, newDeliveryRow(viewOf(pr, delivery.ComputeDeployedStatus(pr, settings.DeployRepo, apply), deployRun, deployedAt)))
	}

	waitingRows := make([]deliveryWaitingRow, 0, len(waiting))
	for _, pr := range waiting {
		status := delivery.DeployedStatusWaiting
		if pr.DeployRun != nil {
			status = delivery.DeployedStatusDeployed
		}
		waitingRows = append(waitingRows, deliveryWaitingRow{
			row:  newDeliveryRow(viewOf(pr.DeliveryPullRequest, status, pr.DeployRun, pr.DeployedAt)),
			view: delivery.DeliveryWaitingPRView{MergedAt: *pr.MergedAt, DeployedAt: pr.DeployedAt},
		})
	}

	return deliveryPopulation{
		rows:        rows,
		selection:   parseDeliverySelection(query, components),
		waiting:     waitingRows,
		applyRuns:   applyRuns,
		jobsByRun:   jobsByRun,
		components:  components,
		issueTitles: issueTitles,
	}, nil
}

// deliveryFreshness is the timeline's and the measures' freshness object: the settings row's
// intake and reconcile times, the last reconcile error, and how many population pull requests can
// no longer be fetched from GitHub.
func (s *server) deliveryFreshness(ctx context.Context, settings delivery.DeliverySettings) (delivery.DeliveryFreshnessView, error) {
	unfetchableCount, err := delivery.CountUnfetchablePullRequests(ctx, s.deps.Store.Pool)
	if err != nil {
		return delivery.DeliveryFreshnessView{}, err
	}
	return delivery.DeliveryFreshnessView{
		LastEventAt: settings.LastEventAt, LastReconcileAt: settings.LastReconcileAt, LastError: settings.LastError,
		UnfetchableCount: unfetchableCount,
	}, nil
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

// deliveryProductionView is the run's production job (the configured production_job_name), or nil
// when the run has none.
func deliveryProductionView(jobs []delivery.DeliveryRunJob, productionJobName string) *delivery.DeliveryRunProductionView {
	for _, job := range jobs {
		if job.Name == productionJobName {
			return &delivery.DeliveryRunProductionView{Conclusion: job.Conclusion, CompletedAt: job.CompletedAt}
		}
	}
	return nil
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
