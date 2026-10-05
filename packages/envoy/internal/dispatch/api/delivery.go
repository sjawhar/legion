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
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// defaultDeliveryWindow is the timeline's default when the caller passes neither from nor to: the
// last 7 days, matching the Acceptance criterion's "one recent 7-day window".
const defaultDeliveryWindow = 7 * 24 * time.Hour

// parseDeliveryWindow reads from/to (RFC3339) from query, defaulting to the last 7 days when
// either is absent, and refuses a malformed value or an inverted window.
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
	applyRuns, err := delivery.ListRuns(ctx, pool, settings.DeployRepo, model.DeliveryRunKindDeploy, from)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	jobsByRun := make(map[int64][]model.DeliveryRunJob, len(applyRuns))
	for _, run := range applyRuns {
		jobs, err := delivery.ListRunJobs(ctx, pool, run.Repo, run.RunID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		jobsByRun[run.RunID] = jobs
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

	shippedBy := map[int64][]string{}
	prViews := make([]model.DeliveryPRView, 0, len(prs))
	for _, pr := range prs {
		apply := delivery.ContainingRun(pr, settings.DeployRepo, applies)
		status := delivery.DeployedStatus(pr, settings.DeployRepo, apply)
		id := pr.Repo + "#" + strconv.Itoa(pr.Number)
		view := model.DeliveryPRView{
			ID: id, Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL, Author: pr.Author,
			CreatedAt: pr.CreatedAt, MergedAt: pr.MergedAt, FirstCommitAt: pr.FirstCommitAt,
			Additions: pr.Additions, Deletions: pr.Deletions, Partial: pr.Partial, Rework: pr.Rework,
			Issue: pr.IssueKey, Sessions: pr.Sessions, DeployedStatus: status,
		}
		if len(pr.Sessions) > 0 {
			agent := delivery.DisplayAgent(pr.Sessions[0], titles)
			view.ParentAgent = &agent
		}
		if apply != nil {
			runID := apply.RunID
			view.DeployRun = &runID
			completedAt := apply.CompletedAt
			view.DeployedAt = &completedAt
			shippedBy[apply.RunID] = append(shippedBy[apply.RunID], id)
		}
		if !matchesParentAgentOrSessionFacet(view, query) || !matchesDeployedFacet(view, query) {
			continue
		}
		if !matchesPriorityFacet(pr.IssueKey, priorities, query) || !matchesComponentFacet(pr.IssueKey, components, query) {
			continue
		}
		prViews = append(prViews, view)
	}

	runViews := make([]model.DeliveryRunView, 0, len(applyRuns))
	for _, run := range applyRuns {
		if run.StartedAt.Before(from) || !run.StartedAt.Before(to) {
			continue
		}
		jobs := jobsByRun[run.RunID]
		runViews = append(runViews, model.DeliveryRunView{
			ID: run.RunID, URL: run.URL, HeadSHA: run.HeadSHA, HeadAt: run.HeadCommitAt,
			StartedAt: run.StartedAt, CompletedAt: run.CompletedAt, Conclusion: run.Conclusion,
			FailedJobs:     deliveryJobViews(delivery.FailedJobs(jobs)),
			RootFailingJob: deliveryJobView(delivery.RootFailingJob(jobs)),
			PRs:            shippedBy[run.RunID],
		})
	}

	WriteJSON(w, http.StatusOK, model.DeliveryTimelineResponse{
		Window: model.DeliveryWindowView{From: from, To: to},
		PRs:    prViews,
		Runs:   runViews,
		Freshness: model.DeliveryFreshnessView{
			LastEventAt: settings.LastEventAt, LastReconcileAt: settings.LastReconcileAt,
		},
	})
}

func deliveryJobViews(jobs []model.DeliveryRunJob) []model.DeliveryRunJobView {
	views := make([]model.DeliveryRunJobView, len(jobs))
	for i, job := range jobs {
		views[i] = model.DeliveryRunJobView{Name: job.Name, CompletedAt: job.CompletedAt}
	}
	return views
}

func deliveryJobView(job *model.DeliveryRunJob) *model.DeliveryRunJobView {
	if job == nil {
		return nil
	}
	return &model.DeliveryRunJobView{Name: job.Name, CompletedAt: job.CompletedAt}
}

// filterDeliveryPullRequests applies the facets that filter on a PR's own stored fields directly
// (repo, issue, author, rework): the ones that need no derived value. parent_agent/session and
// deployed are applied after deployed_status and the agent titles are resolved, by
// matchesParentAgentOrSessionFacet/matchesDeployedFacet below.
func filterDeliveryPullRequests(prs []model.DeliveryPullRequest, query url.Values) []model.DeliveryPullRequest {
	repos := query["repo"]
	issues := query["issue"]
	authors := query["author"]
	rework := query["rework"]
	return slices.DeleteFunc(slices.Clone(prs), func(pr model.DeliveryPullRequest) bool {
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
		if len(rework) > 0 {
			want := slices.Contains(rework, "rework")
			if pr.Rework != want && !(slices.Contains(rework, "value") && !pr.Rework) {
				return true
			}
		}
		return false
	})
}

func matchesParentAgentOrSessionFacet(pr model.DeliveryPRView, query url.Values) bool {
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

func matchesDeployedFacet(pr model.DeliveryPRView, query url.Values) bool {
	deployed := query["deployed"]
	if len(deployed) == 0 {
		return true
	}
	return slices.Contains(deployed, pr.DeployedStatus)
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
	return slices.Contains(wanted, strconv.Itoa(*priority))
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
