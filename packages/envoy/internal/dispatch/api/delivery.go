package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

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

// getDeliveryTimeline answers GET /api/v1/delivery/timeline: merges, deploys, pipeline failures
// and waiting-to-deploy PRs within [from, to) and the given facets, computed from stored facts at
// request time (deployed_status, root_failing_job and the parent_agent/session display labels are
// never stored -- "the server computes every measure from stored facts on request").
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

	ctx := r.Context()
	pool := s.deps.Store.Pool

	settings, err := delivery.GetSettings(ctx, pool)
	if err != nil {
		if errors.Is(err, delivery.ErrNoSettings) {
			writeError(w, "DELIVERY_NOT_CONFIGURED", http.StatusNotFound, "delivery is not configured; set delivery_settings through PUT /api/v1/settings/delivery")
			return
		}
		s.writeHandlerError(w, err)
		return
	}

	prs, err := delivery.ListPullRequestsInWindow(ctx, pool, from, to)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	prs = filterDeliveryPullRequests(prs, query)

	// Every deploy run with a head commit at or after `from`: the containment algorithm needs
	// every apply from there forward, unbounded past `to`, since a PR merged just before `to` may
	// first ship after it -- bounding this query to [from, to) would wrongly read such a PR as
	// "waiting". The displayed runs[] list is filtered to [from, to) separately, below.
	applyRuns, err := delivery.ListRuns(ctx, pool, settings.DeployRepo, delivery.DeliveryRunKindDeploy, from)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	runIDs := make([]int64, len(applyRuns))
	for i, run := range applyRuns {
		runIDs[i] = run.RunID
	}
	jobsByRun, err := delivery.ListRunJobsForRuns(ctx, pool, settings.DeployRepo, runIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	applies := delivery.ProductionApplies(applyRuns, jobsByRun, settings.ProductionJobName)

	sessionIDs := []string{}
	issueKeys := []string{}
	for _, pr := range prs {
		sessionIDs = append(sessionIDs, pr.Sessions...)
		if pr.IssueKey != nil {
			issueKeys = append(issueKeys, *pr.IssueKey)
		}
	}
	titles, err := delivery.ResolveSessionTitles(ctx, pool, sessionIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	priorities, components, err := s.resolveIssueFacetData(ctx, issueKeys)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	// shippedBy only ever gains an entry for a PR that actually survives every facet below (the
	// append happens in the same branch that keeps the view in prViews): a PR a facet excludes
	// from prs[] never appears in runs[].prs either, so the two lists agree about what a deploy
	// shipped under the active filters.
	shippedBy := map[int64][]string{}
	prViews := make([]delivery.DeliveryPRView, 0, len(prs))
	for _, pr := range prs {
		apply := delivery.ContainingRun(pr, settings.DeployRepo, applies)
		status := delivery.ComputeDeployedStatus(pr, settings.DeployRepo, apply)
		id := pr.Repo + "#" + strconv.Itoa(pr.Number)
		view := delivery.DeliveryPRView{
			ID: id, Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL, Author: pr.Author,
			CreatedAt: pr.CreatedAt, MergedAt: pr.MergedAt, FirstCommitAt: pr.FirstCommitAt,
			Additions: pr.Additions, Deletions: pr.Deletions, Partial: pr.Partial, Rework: pr.Rework,
			Issue: pr.IssueKey, Sessions: pr.Sessions, DeployedStatus: status,
		}
		if len(pr.Sessions) > 0 {
			agent := delivery.DisplayAgent(pr.Sessions[0], titles)
			view.ParentAgent = &agent
		}
		var applyRunID *int64
		if apply != nil {
			runID := apply.RunID
			applyRunID = &runID
			completedAt := apply.CompletedAt
			view.DeployRun = &runID
			view.DeployedAt = &completedAt
		}
		if !matchesParentAgentOrSessionFacet(view, query) || !matchesDeployedFacet(view, query) {
			continue
		}
		if !matchesPriorityFacet(pr.IssueKey, priorities, query) || !matchesComponentFacet(pr.IssueKey, components, query) {
			continue
		}
		prViews = append(prViews, view)
		if applyRunID != nil {
			shippedBy[*applyRunID] = append(shippedBy[*applyRunID], id)
		}
	}

	runViews := make([]delivery.DeliveryRunView, 0, len(applyRuns))
	for _, run := range applyRuns {
		if run.StartedAt.Before(from) || !run.StartedAt.Before(to) {
			continue
		}
		jobs := jobsByRun[run.RunID]
		prs := shippedBy[run.RunID]
		if prs == nil {
			// Always a slice, never nil, so it always serializes as `[]`, not `null` -- the
			// common case for any successful deploy that shipped no in-window population PR.
			prs = []string{}
		}
		runViews = append(runViews, delivery.DeliveryRunView{
			ID: run.RunID, URL: run.URL, HeadSHA: run.HeadSHA, HeadAt: run.HeadCommitAt,
			StartedAt: run.StartedAt, CompletedAt: run.CompletedAt, Conclusion: run.Conclusion,
			FailedJobs:     deliveryJobViews(delivery.FailedJobs(jobs)),
			RootFailingJob: deliveryJobView(delivery.RootFailingJob(jobs)),
			PRs:            prs,
		})
	}

	WriteJSON(w, http.StatusOK, delivery.DeliveryTimelineResponse{
		Window: delivery.DeliveryWindowView{From: from, To: to},
		PRs:    prViews,
		Runs:   runViews,
		Freshness: delivery.DeliveryFreshnessView{
			LastEventAt: settings.LastEventAt, LastReconcileAt: settings.LastReconcileAt, LastError: settings.LastError,
		},
	})
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

// filterDeliveryPullRequests applies the facets that filter on a PR's own stored fields directly
// (repo, issue, author, rework): the ones that need no derived value. parent_agent/session and
// deployed are applied after deployed_status and the agent titles are resolved, by
// matchesParentAgentOrSessionFacet/matchesDeployedFacet below.
func filterDeliveryPullRequests(prs []delivery.DeliveryPullRequest, query url.Values) []delivery.DeliveryPullRequest {
	repos := query["repo"]
	issues := query["issue"]
	authors := query["author"]
	rework := query["rework"]
	return slices.DeleteFunc(slices.Clone(prs), func(pr delivery.DeliveryPullRequest) bool {
		if len(repos) > 0 && !slices.Contains(repos, pr.Repo) {
			return true
		}
		if len(authors) > 0 && !slices.Contains(authors, pr.Author) {
			return true
		}
		if len(issues) > 0 {
			if pr.IssueKey == nil || !slices.Contains(issues, *pr.IssueKey) {
				return true
			}
		}
		return !matchesReworkFacet(rework, pr.Rework)
	})
}

// matchesReworkFacet is the rework/value tri-state facet, spelled as two direct checks instead of
// one double-negated boolean expression: with neither value selected every PR matches; a rework
// PR matches only when "rework" is selected; a value (non-rework) PR matches only when "value" is
// selected.
func matchesReworkFacet(selected []string, isRework bool) bool {
	if len(selected) == 0 {
		return true
	}
	if isRework {
		return slices.Contains(selected, "rework")
	}
	return slices.Contains(selected, "value")
}

func matchesParentAgentOrSessionFacet(pr delivery.DeliveryPRView, query url.Values) bool {
	if parentAgents := query["parent_agent"]; len(parentAgents) > 0 {
		if pr.ParentAgent == nil || !slices.Contains(parentAgents, *pr.ParentAgent) {
			return false
		}
	}
	if sessions := query["session"]; len(sessions) > 0 {
		found := false
		for _, s := range pr.Sessions {
			if slices.Contains(sessions, s) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func matchesDeployedFacet(pr delivery.DeliveryPRView, query url.Values) bool {
	deployed := query["deployed"]
	if len(deployed) == 0 {
		return true
	}
	return slices.Contains(deployed, string(pr.DeployedStatus))
}

// resolveIssueFacetData reads each issue key's priority and direct component membership, for the
// priority/component facets. Component resolution here is direct membership only
// (issue_component_members), not the full nearest-ancestor inheritance / descendant-expansion
// walk getArchitectureTree does -- a PR whose issue inherits its component from a parent, or a
// "parent component includes its children" facet selection, is not matched by this simplified
// version. Flagged here rather than silently approximated as exact.
func (s *server) resolveIssueFacetData(ctx context.Context, issueKeys []string) (priorities map[string]*int, components map[string][]string, err error) {
	priorities = make(map[string]*int, len(issueKeys))
	components = make(map[string][]string)
	if len(issueKeys) == 0 {
		return priorities, components, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `select key, priority from issues where key = any($1)`, issueKeys)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var priority *int
		if err := rows.Scan(&key, &priority); err != nil {
			return nil, nil, err
		}
		priorities[key] = priority
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	memberRows, err := s.deps.Store.Pool.Query(ctx, `select issue_key, component_id from issue_component_members where issue_key = any($1)`, issueKeys)
	if err != nil {
		return nil, nil, err
	}
	defer memberRows.Close()
	for memberRows.Next() {
		var key, component string
		if err := memberRows.Scan(&key, &component); err != nil {
			return nil, nil, err
		}
		components[key] = append(components[key], component)
	}
	return priorities, components, memberRows.Err()
}

// matchesPriorityFacet compares against the issue's stored integer priority, accepting either the
// bare digit ("0") or this feature's own "P0" display convention (DrillDown.tsx renders priority
// as `P${priority}`, and the picker's own placeholder tells the user to type "P0") -- stripping an
// optional leading P/p before comparing so the UI's own example actually matches.
func matchesPriorityFacet(issueKey *string, priorities map[string]*int, query url.Values) bool {
	wanted := query["priority"]
	if len(wanted) == 0 {
		return true
	}
	if issueKey == nil {
		return false
	}
	priority, ok := priorities[*issueKey]
	if !ok || priority == nil {
		return false
	}
	digits := strconv.Itoa(*priority)
	for _, want := range wanted {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(want, "P"), "p")
		if trimmed == digits {
			return true
		}
	}
	return false
}

func matchesComponentFacet(issueKey *string, components map[string][]string, query url.Values) bool {
	wanted := query["component"]
	if len(wanted) == 0 {
		return true
	}
	if issueKey == nil {
		return false
	}
	for _, id := range components[*issueKey] {
		if slices.Contains(wanted, id) {
			return true
		}
	}
	return false
}
