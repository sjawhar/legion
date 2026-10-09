package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestGetDeliveryTimelineWithoutSettingsIs404(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline", nil, "alice")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404 DELIVERY_NOT_CONFIGURED", response.Code, response.Body.String())
	}
}

func TestGetDeliveryTimelineRejectsMalformedWindow(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if _, err := delivery.PutSettings(t.Context(), database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "widgets-release / widgets-release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	for _, target := range []string{
		"/api/v1/delivery/timeline?from=not-a-date",
		"/api/v1/delivery/timeline?to=not-a-date",
		"/api/v1/delivery/timeline?from=2024-01-02T00:00:00Z&to=2024-01-01T00:00:00Z",
		"/api/v1/delivery/timeline?from=2020-01-01T00:00:00Z&to=2024-01-01T00:00:00Z",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, body = %s, want 400 INVALID_QUERY", target, response.Code, response.Body.String())
		}
	}
}

func TestGetDeliveryTimelineComputesDeployedStatusAndFacets(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	pool := database.Pool
	ctx := t.Context()

	if _, err := delivery.PutSettings(ctx, pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "widgets-release / widgets-release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	merged := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	deployedAt := merged.Add(2 * time.Hour)
	waitingMerged := merged.Add(time.Hour)

	// A PR a deploy ships (deployed), and one merged after the only deploy run (waiting), both in
	// the deploy repository, plus one merged in a different repository the configured deploy
	// pipeline never tracks (not_tracked). acme/widgets#1 also names a P0 issue, for the priority
	// facet case below.
	if _, err := pool.Exec(ctx, `insert into projects (key, name) values ('ACME', 'Acme') on conflict do nothing`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into issues (key, project_key, number, title, status, priority, created_by, rank) values ('ACME-1', 'ACME', 1, 'x', 'todo', 0, '{"kind":"system","id":"test"}', 'U')`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	issueKey := "ACME-1"
	for _, pr := range []delivery.DeliveryPullRequest{
		{Repo: "acme/widgets", Number: 1, Title: "feat: shipped", URL: "https://github.com/acme/widgets/pull/1", Author: "octocat", MergedAt: &merged, CreatedAt: &merged, IssueKey: &issueKey},
		{Repo: "acme/widgets", Number: 2, Title: "feat: waiting", URL: "https://github.com/acme/widgets/pull/2", Author: "octocat", MergedAt: &waitingMerged, CreatedAt: &waitingMerged},
		{Repo: "acme/other", Number: 3, Title: "feat: elsewhere", URL: "https://github.com/acme/other/pull/3", Author: "octocat", MergedAt: &merged, CreatedAt: &merged},
	} {
		if err := delivery.UpsertPullRequest(ctx, pool, pr); err != nil {
			t.Fatalf("seed PR %s#%d: %v", pr.Repo, pr.Number, err)
		}
	}

	runSuccess := delivery.DeliveryRunConclusionSuccess
	jobSuccess := delivery.DeliveryJobConclusionSuccess
	if err := delivery.UpsertRun(ctx, pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: 100, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "deadbeef",
		HeadCommitAt: merged, StartedAt: deployedAt, CompletedAt: &deployedAt, Conclusion: &runSuccess,
		URL: "https://github.com/acme/widgets/actions/runs/100",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := delivery.UpsertRunJobs(ctx, pool, "acme/widgets", 100, []delivery.DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 100, Name: "widgets-release / widgets-release", StartedAt: &deployedAt, CompletedAt: &deployedAt, Conclusion: &jobSuccess},
	}); err != nil {
		t.Fatalf("seed run job: %v", err)
	}

	from := merged.Add(-time.Hour).Format(time.RFC3339)
	to := merged.Add(24 * time.Hour).Format(time.RFC3339)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, response.Body.String())
	}
	if len(body.PRs) != 3 {
		t.Fatalf("len(prs) = %d, want 3; body=%s", len(body.PRs), response.Body.String())
	}
	status := map[string]delivery.DeployedStatus{}
	for _, pr := range body.PRs {
		status[pr.ID] = pr.DeployedStatus
	}
	if status["acme/widgets#1"] != delivery.DeployedStatusDeployed {
		t.Errorf("acme/widgets#1 deployed_status = %q, want deployed", status["acme/widgets#1"])
	}
	if status["acme/widgets#2"] != delivery.DeployedStatusWaiting {
		t.Errorf("acme/widgets#2 deployed_status = %q, want waiting", status["acme/widgets#2"])
	}
	if status["acme/other#3"] != delivery.DeployedStatusNotTracked {
		t.Errorf("acme/other#3 deployed_status = %q, want not_tracked", status["acme/other#3"])
	}
	if len(body.Runs) != 1 || len(body.Runs[0].PRs) != 1 || body.Runs[0].PRs[0].ID != "acme/widgets#1" || body.Runs[0].PRs[0].Title != "feat: shipped" {
		t.Fatalf("runs = %+v, want one run shipping acme/widgets#1 with its title", body.Runs)
	}
	production := body.Runs[0].Production
	if production == nil || production.Conclusion == nil || *production.Conclusion != delivery.DeliveryJobConclusionSuccess ||
		production.CompletedAt == nil || !production.CompletedAt.Equal(deployedAt) {
		t.Fatalf("runs[0].production = %+v, want the production job's success at %s", production, deployedAt)
	}

	// deployed= facet narrows to exactly the deployed PR.
	deployedOnly := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to+"&deployed=deployed", nil, "alice")
	var deployedBody delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(deployedOnly.Body.Bytes(), &deployedBody); err != nil {
		t.Fatalf("decode deployed-only response: %v", err)
	}
	if len(deployedBody.PRs) != 1 || deployedBody.PRs[0].ID != "acme/widgets#1" {
		t.Fatalf("deployed=deployed filter: prs = %+v, want only acme/widgets#1", deployedBody.PRs)
	}

	// priority=P0, exactly as the UI's own placeholder ("Type a priority (e.g. P0).") tells a
	// user to type, must match the P0 issue's PR -- not silently match nothing.
	priorityP0 := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to+"&priority=P0", nil, "alice")
	var priorityBody delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(priorityP0.Body.Bytes(), &priorityBody); err != nil {
		t.Fatalf("decode priority=P0 response: %v", err)
	}
	if len(priorityBody.PRs) != 1 || priorityBody.PRs[0].ID != "acme/widgets#1" {
		t.Fatalf("priority=P0 filter: prs = %+v, want only acme/widgets#1 (the P0 issue's PR)", priorityBody.PRs)
	}
	// The bare digit form still works too.
	priorityDigit := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to+"&priority=0", nil, "alice")
	var priorityDigitBody delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(priorityDigit.Body.Bytes(), &priorityDigitBody); err != nil {
		t.Fatalf("decode priority=0 response: %v", err)
	}
	if len(priorityDigitBody.PRs) != 1 || priorityDigitBody.PRs[0].ID != "acme/widgets#1" {
		t.Fatalf("priority=0 filter: prs = %+v, want only acme/widgets#1", priorityDigitBody.PRs)
	}

	// repo= facet narrows prs[] to the other repository alone, while the one deploy run still names
	// the PR it shipped: a run's prs[] is every window PR it shipped first, whatever the facets, as
	// the prototype sizes a deploy by every PR it shipped.
	otherOnly := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to+"&repo=acme/other", nil, "alice")
	var otherBody delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(otherOnly.Body.Bytes(), &otherBody); err != nil {
		t.Fatalf("decode repo-filtered response: %v", err)
	}
	if len(otherBody.PRs) != 1 || otherBody.PRs[0].ID != "acme/other#3" {
		t.Fatalf("repo=acme/other filter: prs = %+v, want only acme/other#3", otherBody.PRs)
	}
	if len(otherBody.Runs) != 1 || len(otherBody.Runs[0].PRs) != 1 || otherBody.Runs[0].PRs[0].ID != "acme/widgets#1" {
		t.Fatalf("repo=acme/other filter: runs[0].prs = %+v, want the shipped acme/widgets#1 whatever the facets", otherBody.Runs)
	}
}

// TestGetDeliveryTimelineAnswersThePullRequestsStillWaitingAtFrom: waiting[] holds the deploy
// repository's pull requests that merged before the window and had not shipped by its start, with
// when they shipped afterwards, under the request's facets. One a deploy shipped before `from` is
// not there, nor is one of another repository, nor one merged inside the window. The run that
// ships a waiter after `from` has its head commit before `from`, so the window's own runs (heads
// at or after `from`) do not hold it.
func TestGetDeliveryTimelineAnswersThePullRequestsStillWaitingAtFrom(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	pool := database.Pool
	ctx := t.Context()
	if _, err := delivery.PutSettings(ctx, pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "release / release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat", "hubot"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	from := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := from.Add(d); return &v }
	day := 24 * time.Hour
	for _, pr := range []delivery.DeliveryPullRequest{
		{Repo: "acme/widgets", Number: 11, Title: "feat: shipped before", Author: "octocat", MergedAt: at(-3 * day)},
		{Repo: "acme/widgets", Number: 10, Title: "feat: still waiting", Author: "octocat", MergedAt: at(-2 * day)},
		{Repo: "acme/widgets", Number: 12, Title: "feat: ships later", Author: "hubot", MergedAt: at(-1 * day)},
		{Repo: "acme/other", Number: 13, Title: "feat: elsewhere", Author: "octocat", MergedAt: at(-1 * day)},
		{Repo: "acme/widgets", Number: 15, Title: "feat: never ships", Author: "octocat", MergedAt: at(-6 * time.Hour)},
		{Repo: "acme/widgets", Number: 14, Title: "feat: in window", Author: "octocat", MergedAt: at(time.Hour)},
	} {
		pr.URL = "https://github.com/" + pr.Repo + "/pull/" + strconv.Itoa(pr.Number)
		pr.CreatedAt = pr.MergedAt
		if err := delivery.UpsertPullRequest(ctx, pool, pr); err != nil {
			t.Fatalf("seed PR %s#%d: %v", pr.Repo, pr.Number, err)
		}
	}
	runSuccess := delivery.DeliveryRunConclusionSuccess
	jobSuccess := delivery.DeliveryJobConclusionSuccess
	for _, run := range []struct {
		id         int64
		head, done *time.Time
	}{
		{id: 200, head: at(-2*day - 12*time.Hour), done: at(-2*day - 11*time.Hour)}, // ships #11 before from
		{id: 201, head: at(-12 * time.Hour), done: at(2 * time.Hour)},               // ships #10 and #12 after from
	} {
		if err := delivery.UpsertRun(ctx, pool, delivery.DeliveryRun{
			Repo: "acme/widgets", RunID: run.id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha" + strconv.FormatInt(run.id, 10),
			HeadCommitAt: *run.head, StartedAt: *run.head, CompletedAt: run.done, Conclusion: &runSuccess,
			URL: "https://github.com/acme/widgets/actions/runs/" + strconv.FormatInt(run.id, 10),
		}); err != nil {
			t.Fatalf("seed run %d: %v", run.id, err)
		}
		if err := delivery.UpsertRunJobs(ctx, pool, "acme/widgets", run.id, []delivery.DeliveryRunJob{
			{Repo: "acme/widgets", RunID: run.id, Name: "release / release", StartedAt: run.head, CompletedAt: run.done, Conclusion: &jobSuccess},
		}); err != nil {
			t.Fatalf("seed run %d's job: %v", run.id, err)
		}
	}

	window := "from=" + from.Format(time.RFC3339) + "&to=" + from.Add(day).Format(time.RFC3339)
	body := getDeliveryTimeline(t, handler, window)
	want := []delivery.DeliveryWaitingPRView{
		{MergedAt: *at(-2 * day), DeployedAt: at(2 * time.Hour)},
		{MergedAt: *at(-1 * day), DeployedAt: at(2 * time.Hour)},
		{MergedAt: *at(-6 * time.Hour)},
	}
	if !waitingEqual(body.Waiting, want) {
		t.Fatalf("waiting = %+v, want #10 and #12 shipped by run 201 after from, then #15 never shipped", body.Waiting)
	}

	// A facet narrows waiting[] as it narrows prs[].
	octocat := getDeliveryTimeline(t, handler, window+"&author=octocat")
	if !waitingEqual(octocat.Waiting, []delivery.DeliveryWaitingPRView{want[0], want[2]}) {
		t.Fatalf("author=octocat waiting = %+v, want #10 and #15", octocat.Waiting)
	}
}

func waitingEqual(got, want []delivery.DeliveryWaitingPRView) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !got[i].MergedAt.Equal(want[i].MergedAt) || (got[i].DeployedAt == nil) != (want[i].DeployedAt == nil) ||
			(got[i].DeployedAt != nil && !got[i].DeployedAt.Equal(*want[i].DeployedAt)) {
			return false
		}
	}
	return true
}

// seedDeliveryIssueFacts seeds what the timeline joins a pull request's issue against: project
// ACME, its component tree (platform > api, and docs), ACME-1 (P0, attached to api), its child
// ACME-2 (P2, attaching nothing of its own, so it inherits api), and ACME-3 (no priority, attached
// to none with a reason).
func seedDeliveryIssueFacts(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := t.Context()
	for _, statement := range []string{
		`insert into projects (key, name) values ('ACME', 'Acme') on conflict do nothing`,
		`insert into architecture_snapshots (id, project_key, commit, files) values (900, 'ACME', 'c1', '{}')`,
		`insert into components (project_key, id, title, prose, parent, snapshot_id) values
			('ACME', 'platform', 'The platform', '', null, 900),
			('ACME', 'api', 'Public API', '', 'platform', 900),
			('ACME', 'docs', 'Documentation', '', null, 900)`,
		`insert into issues (key, project_key, number, title, status, priority, created_by, rank) values
			('ACME-1', 'ACME', 1, 'Ship widgets', 'todo', 0, '{"kind":"system","id":"test"}', 'U'),
			('ACME-3', 'ACME', 3, 'Tidy docs', 'todo', null, '{"kind":"system","id":"test"}', 'W')`,
		`insert into issues (key, project_key, number, title, status, priority, parent_key, created_by, rank) values
			('ACME-2', 'ACME', 2, 'Widget child', 'todo', 2, 'ACME-1', '{"kind":"system","id":"test"}', 'V')`,
		`insert into issue_components (issue_key, mode, reason) values ('ACME-1', 'explicit', null), ('ACME-3', 'none', 'chore')`,
		`insert into issue_component_members (issue_key, project_key, component_id) values ('ACME-1', 'ACME', 'api')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed issue facts: %v\n%s", err, statement)
		}
	}
}

func getDeliveryTimeline(t *testing.T, handler http.Handler, query string) delivery.DeliveryTimelineResponse {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET timeline?%s: status = %d, body = %s", query, response.Code, response.Body.String())
	}
	var body delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode timeline?%s: %v, body=%s", query, err, response.Body.String())
	}
	return body
}

func deliveryPRIDs(prs []delivery.DeliveryPRView) []string {
	ids := make([]string, len(prs))
	for i, pr := range prs {
		ids[i] = pr.ID
	}
	slices.Sort(ids)
	return ids
}

// TestGetDeliveryTimelineJoinsIssueFactsAndCountsFacets proves each PR carries its issue's title,
// P0-P3 priority and effective components (inherited from the nearest ancestor that chose, as the
// issue read resolves them), that the component facet's parent selection includes its children,
// and that facet_counts count each facet with every other facet and the search applied and its own
// selection ignored, while color_counts ignore every facet.
func TestGetDeliveryTimelineJoinsIssueFactsAndCountsFacets(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := t.Context()
	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat", "hubot"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	seedDeliveryIssueFacts(t, database)

	merged := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	key := func(k string) *string { return &k }
	for _, pr := range []delivery.DeliveryPullRequest{
		{Repo: "acme/widgets", Number: 1, Title: "feat: ship the api", Author: "octocat", IssueKey: key("ACME-1")},
		{Repo: "acme/widgets", Number: 2, Title: "fix: child widget", Author: "hubot", IssueKey: key("ACME-2"), Rework: true},
		{Repo: "acme/other", Number: 3, Title: "docs: tidy", Author: "octocat", IssueKey: key("ACME-3")},
		{Repo: "acme/other", Number: 4, Title: "chore: no issue", Author: "hubot"},
	} {
		pr.URL = "https://github.com/" + pr.Repo + "/pull/" + strconv.Itoa(pr.Number)
		at := merged.Add(time.Duration(pr.Number) * time.Minute)
		pr.MergedAt, pr.CreatedAt = &at, &at
		if err := delivery.UpsertPullRequest(ctx, database.Pool, pr); err != nil {
			t.Fatalf("seed PR %s#%d: %v", pr.Repo, pr.Number, err)
		}
	}
	window := "from=" + merged.Add(-time.Hour).Format(time.RFC3339) + "&to=" + merged.Add(time.Hour).Format(time.RFC3339)

	body := getDeliveryTimeline(t, handler, window)
	byID := map[string]delivery.DeliveryPRView{}
	for _, pr := range body.PRs {
		byID[pr.ID] = pr
	}
	for id, want := range map[string]struct {
		title, priority *string
		components      []string
	}{
		"acme/widgets#1": {key("Ship widgets"), key("P0"), []string{"ACME/api"}},
		"acme/widgets#2": {key("Widget child"), key("P2"), []string{"ACME/api"}},
		"acme/other#3":   {key("Tidy docs"), nil, []string{}},
		"acme/other#4":   {nil, nil, []string{}},
	} {
		got := byID[id]
		if !reflect.DeepEqual(got.IssueTitle, want.title) || !reflect.DeepEqual(got.Priority, want.priority) || !reflect.DeepEqual(got.Components, want.components) {
			t.Errorf("%s: issue_title=%v priority=%v components=%v, want %v %v %v", id,
				deref(got.IssueTitle), deref(got.Priority), got.Components, deref(want.title), deref(want.priority), want.components)
		}
	}
	wantComponents := map[string]delivery.DeliveryComponentView{
		"ACME/platform": {Title: "The platform"},
		"ACME/api":      {Title: "Public API", Parent: key("ACME/platform")},
		"ACME/docs":     {Title: "Documentation"},
	}
	if !reflect.DeepEqual(body.Components, wantComponents) {
		t.Errorf("components = %+v, want %+v", body.Components, wantComponents)
	}
	if want := map[string]string{"ACME-1": "Ship widgets", "ACME-2": "Widget child", "ACME-3": "Tidy docs"}; !reflect.DeepEqual(body.IssueTitles, want) {
		t.Errorf("issue_titles = %v, want %v", body.IssueTitles, want)
	}

	// A parent component's selection includes its children; the placeholders select by cause.
	for query, want := range map[string][]string{
		"component=ACME/platform":                  {"acme/widgets#1", "acme/widgets#2"},
		"component=__no_component__":               {"acme/other#3"},
		"component=__no_issue__":                   {"acme/other#4"},
		"priority=__no_priority__":                 {"acme/other#3"},
		"priority=p2&priority=__no_issue__":        {"acme/other#4", "acme/widgets#2"},
		"parent_agent=__no_session__&author=hubot": {"acme/other#4", "acme/widgets#2"},
		"q=CHILD":       {"acme/widgets#2"},
		"q=widgets%233": {},
		"q=other%233":   {"acme/other#3"},
	} {
		got := deliveryPRIDs(getDeliveryTimeline(t, handler, window+"&"+query).PRs)
		if !slices.Equal(got, want) {
			t.Errorf("%s: prs = %v, want %v", query, got, want)
		}
	}

	// repo is selected, so its own counts still name both repositories while author counts only
	// acme/widgets' PRs; the search applies to every count.
	counted := getDeliveryTimeline(t, handler, window+"&repo=acme/widgets")
	wantCounts := map[string]map[string]int{
		"repo":         {"acme/widgets": 2, "acme/other": 2},
		"parent_agent": {"__no_session__": 2},
		"session":      {},
		"issue":        {"ACME-1": 1, "ACME-2": 1},
		"priority":     {"P0": 1, "P2": 1},
		"component":    {"ACME/api": 2},
		"author":       {"octocat": 1, "hubot": 1},
		"rework":       {"value": 1, "rework": 1},
		"deployed":     {"waiting": 2},
	}
	if !reflect.DeepEqual(counted.FacetCounts, wantCounts) {
		t.Errorf("facet_counts under repo=acme/widgets = %v, want %v", counted.FacetCounts, wantCounts)
	}
	searched := getDeliveryTimeline(t, handler, window+"&repo=acme/widgets&q=api")
	if got := searched.FacetCounts["repo"]; !reflect.DeepEqual(got, map[string]int{"acme/widgets": 1}) {
		t.Errorf("facet_counts.repo under q=api = %v, want only acme/widgets#1's repository", got)
	}
	wantColors := map[string]map[string]int{
		"repo":         {"acme/widgets": 2, "acme/other": 2},
		"author":       {"octocat": 2, "hubot": 2},
		"priority":     {"P0": 1, "P2": 1, "__no_priority__": 1, "__no_issue__": 1},
		"component":    {"ACME/api": 2, "__no_component__": 1, "__no_issue__": 1},
		"parent_agent": {"__no_session__": 4},
	}
	if !reflect.DeepEqual(counted.ColorCounts, wantColors) {
		t.Errorf("color_counts = %v, want %v (no facet applied)", counted.ColorCounts, wantColors)
	}
}

// TestGetDeliveryRunListsEveryJob proves GET /api/v1/delivery/runs/{id} answers one deploy
// repository run with every job it ran, by start (a job that never started last), and refuses a
// run id that is not a positive integer or names no run.
func TestGetDeliveryRunListsEveryJob(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := t.Context()
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/runs/7", nil, "alice"); response.Code != http.StatusNotFound {
		t.Fatalf("unconfigured: status = %d, body = %s, want 404 DELIVERY_NOT_CONFIGURED", response.Code, response.Body.String())
	}
	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	failure := delivery.DeliveryRunConclusionFailure
	if err := delivery.UpsertRun(ctx, database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: 7, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "abc", HeadCommitAt: start,
		StartedAt: start, CompletedAt: new(start.Add(time.Hour)), Conclusion: &failure, URL: "https://github.com/acme/widgets/actions/runs/7",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	jobs := []delivery.DeliveryRunJob{
		{Name: "release", Conclusion: new(delivery.DeliveryJobConclusionSkipped)},
		{Name: "test", StartedAt: new(start.Add(10 * time.Minute)), CompletedAt: new(start.Add(20 * time.Minute)), Conclusion: new(delivery.DeliveryJobConclusionFailure)},
		{Name: "build", StartedAt: new(start), CompletedAt: new(start.Add(5 * time.Minute)), Conclusion: new(delivery.DeliveryJobConclusionSuccess)},
	}
	for i := range jobs {
		jobs[i].Repo, jobs[i].RunID = "acme/widgets", 7
	}
	if err := delivery.UpsertRunJobs(ctx, database.Pool, "acme/widgets", 7, jobs); err != nil {
		t.Fatalf("seed jobs: %v", err)
	}

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/runs/7", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var detail delivery.DeliveryRunDetailView
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	names := make([]string, len(detail.Jobs))
	for i, job := range detail.Jobs {
		names[i] = job.Name
	}
	if detail.ID != 7 || detail.URL != "https://github.com/acme/widgets/actions/runs/7" || !slices.Equal(names, []string{"build", "test", "release"}) {
		t.Fatalf("run = %+v, want run 7 with build, test, release in start order", detail)
	}
	if test := detail.Jobs[1]; test.Conclusion == nil || *test.Conclusion != delivery.DeliveryJobConclusionFailure ||
		test.StartedAt == nil || !test.StartedAt.Equal(start.Add(10*time.Minute)) || test.CompletedAt == nil {
		t.Fatalf("test job = %+v, want its failure with both timings", test)
	}

	for target, want := range map[string]int{
		"/api/v1/delivery/runs/8":   http.StatusNotFound,
		"/api/v1/delivery/runs/abc": http.StatusBadRequest,
		"/api/v1/delivery/runs/0":   http.StatusBadRequest,
	} {
		if response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); response.Code != want {
			t.Errorf("GET %s: status = %d, body = %s, want %d", target, response.Code, response.Body.String(), want)
		}
	}
}

// TestGetDeliveryTimelineSurfacesReconcileErrorOnFreshness proves a recorded reconcile failure
// (delivery_settings.last_error) reaches the API's freshness object by name, not just the
// database -- a stale-but-healthy pass and a failing one must be distinguishable on the wire.
func TestGetDeliveryTimelineSurfacesReconcileErrorOnFreshness(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := t.Context()
	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "widgets-release / widgets-release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	if err := delivery.RecordReconcileError(ctx, database.Pool, "the installation lacks Actions: read on acme/widgets"); err != nil {
		t.Fatalf("seed reconcile error: %v", err)
	}

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Freshness.LastError == nil || *body.Freshness.LastError != "the installation lacks Actions: read on acme/widgets" {
		t.Fatalf("freshness.last_error = %v, want the recorded permission failure by name", body.Freshness.LastError)
	}
}

// TestNewDeliveryRowGivesEveryColourFacetAValue: deliveryColorCounts colours a row by its first
// value under each colour-by facet, so newDeliveryRow must give each of them one, a placeholder
// where the pull request has none (no issue, no session).
func TestNewDeliveryRowGivesEveryColourFacetAValue(t *testing.T) {
	bare := newDeliveryRow(delivery.DeliveryPRView{Repo: "acme/widgets", Author: "octocat", Components: []string{}})
	for _, facet := range deliveryColorFacets {
		if len(bare.values[facet]) == 0 {
			t.Fatalf("a pull request with no issue and no session has no %s value: %v", facet, bare.values)
		}
	}
	if got := bare.values["rework"]; len(got) != 1 || got[0] != deliveryReworkValue {
		t.Fatalf("rework = %v, want [%s] for a pull request that is not rework", got, deliveryReworkValue)
	}
}
