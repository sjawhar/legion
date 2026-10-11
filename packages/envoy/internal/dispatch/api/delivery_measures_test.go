package api

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// measuresFixture is the delivery measures package's testdata/dora-fixture.json (the prototype's
// web/fixtures/dataset.json with its names replaced): the settings, the four issues, the 14
// population pull requests and the 8 deploy runs with their production job.
type measuresFixture struct {
	Settings struct {
		DeployRepo           string   `json:"deploy_repo"`
		DeployWorkflowPath   string   `json:"deploy_workflow_path"`
		ProductionJobName    string   `json:"production_job_name"`
		PRChecksWorkflowPath string   `json:"pr_checks_workflow_path"`
		PopulationAuthors    []string `json:"population_authors"`
	} `json:"settings"`
	Issues []struct {
		Key      string  `json:"key"`
		Title    string  `json:"title"`
		Status   string  `json:"status"`
		Priority *int    `json:"priority"`
		Claimed  bool    `json:"claimed"`
		Route    *string `json:"route"`
	} `json:"issues"`
	PRs []struct {
		Repo          string     `json:"repo"`
		Number        int        `json:"number"`
		Title         string     `json:"title"`
		Author        string     `json:"author"`
		CreatedAt     time.Time  `json:"created_at"`
		MergedAt      time.Time  `json:"merged_at"`
		FirstCommitAt *time.Time `json:"first_commit_at"`
		Additions     int        `json:"additions"`
		Deletions     int        `json:"deletions"`
		Rework        bool       `json:"rework"`
		Issue         *string    `json:"issue"`
	} `json:"prs"`
	Runs []struct {
		RunID        int64                           `json:"run_id"`
		HeadSHA      string                          `json:"head_sha"`
		HeadCommitAt time.Time                       `json:"head_commit_at"`
		StartedAt    time.Time                       `json:"started_at"`
		CompletedAt  *time.Time                      `json:"completed_at"`
		Conclusion   *delivery.DeliveryRunConclusion `json:"conclusion"`
		HeadBranch   *string                         `json:"head_branch"`
		Event        *string                         `json:"event"`
		Production   *struct {
			Conclusion  delivery.DeliveryJobConclusion `json:"conclusion"`
			CompletedAt time.Time                      `json:"completed_at"`
		} `json:"production"`
		FailedJobs []struct {
			Name        string    `json:"name"`
			CompletedAt time.Time `json:"completed_at"`
		} `json:"failed_jobs"`
	} `json:"runs"`
}

const measuresFixtureWindow = "from=2026-08-30T00:00:00Z&to=2026-09-27T20:00:00Z"

// seedMeasuresFixture writes the fixture's settings, issues, pull requests, runs and jobs through
// the same store calls intake and reconcile use, so Dispatch derives each PR's deploy from the
// stored rows (containment.go FirstShippingApply) as it does in production.
func seedMeasuresFixture(t *testing.T, database *store.Store) measuresFixture {
	t.Helper()
	ctx := t.Context()
	pool := database.Pool
	data, err := os.ReadFile("../delivery/measures/testdata/dora-fixture.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture measuresFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	if _, err := delivery.PutSettings(ctx, pool, delivery.DeliverySettings{
		DeployRepo: fixture.Settings.DeployRepo, DeployWorkflowPath: fixture.Settings.DeployWorkflowPath,
		ProductionJobName: fixture.Settings.ProductionJobName, PRChecksWorkflowPath: fixture.Settings.PRChecksWorkflowPath,
		PopulationAuthors: fixture.Settings.PopulationAuthors,
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if _, err := pool.Exec(ctx, `insert into projects (key, name) values ('ACME', 'Acme') on conflict do nothing`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	for _, issue := range fixture.Issues {
		number := strings.TrimPrefix(issue.Key, "ACME-")
		var claimedBy *string
		if issue.Claimed {
			claim := `{"kind":"session","id":"01a1-test-session"}`
			claimedBy = &claim
		}
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, priority, route, claimed_by, claimed_at, created_by, rank)
			values ($1, 'ACME', $2::int, $3, $4, $5, $6, $7::jsonb, case when $7::jsonb is null then null else now() end, '{"kind":"system","id":"test"}', 'U' || $2)
		`, issue.Key, number, issue.Title, issue.Status, issue.Priority, issue.Route, claimedBy); err != nil {
			t.Fatalf("seed issue %s: %v", issue.Key, err)
		}
	}

	for _, pr := range fixture.PRs {
		createdAt, mergedAt := pr.CreatedAt, pr.MergedAt
		additions, deletions := pr.Additions, pr.Deletions
		if err := delivery.UpsertPullRequest(ctx, pool, delivery.DeliveryPullRequest{
			Repo: pr.Repo, Number: pr.Number, Title: pr.Title, Author: pr.Author,
			URL:       "https://github.com/" + pr.Repo + "/pull/" + strconv.Itoa(pr.Number),
			CreatedAt: &createdAt, MergedAt: &mergedAt, FirstCommitAt: pr.FirstCommitAt,
			Additions: &additions, Deletions: &deletions, Rework: pr.Rework, IssueKey: pr.Issue,
		}); err != nil {
			t.Fatalf("seed PR %s#%d: %v", pr.Repo, pr.Number, err)
		}
	}

	for _, run := range fixture.Runs {
		if err := delivery.UpsertRun(ctx, pool, delivery.DeliveryRun{
			Repo: fixture.Settings.DeployRepo, RunID: run.RunID, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: run.HeadSHA,
			HeadCommitAt: run.HeadCommitAt, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt, Conclusion: run.Conclusion,
			HeadBranch: run.HeadBranch, Event: run.Event,
			URL: "https://github.com/" + fixture.Settings.DeployRepo + "/actions/runs/" + strconv.FormatInt(run.RunID, 10),
		}); err != nil {
			t.Fatalf("seed run %d: %v", run.RunID, err)
		}
		jobs := map[string]delivery.DeliveryRunJob{}
		for _, failed := range run.FailedJobs {
			completedAt := failed.CompletedAt
			jobs[failed.Name] = delivery.DeliveryRunJob{
				Repo: fixture.Settings.DeployRepo, RunID: run.RunID, Name: failed.Name,
				CompletedAt: &completedAt, Conclusion: new(delivery.DeliveryJobConclusionFailure),
			}
		}
		if run.Production != nil {
			completedAt, conclusion := run.Production.CompletedAt, run.Production.Conclusion
			jobs[fixture.Settings.ProductionJobName] = delivery.DeliveryRunJob{
				Repo: fixture.Settings.DeployRepo, RunID: run.RunID, Name: fixture.Settings.ProductionJobName,
				CompletedAt: &completedAt, Conclusion: &conclusion,
			}
		}
		if len(jobs) == 0 {
			continue
		}
		list := make([]delivery.DeliveryRunJob, 0, len(jobs))
		for _, job := range jobs {
			list = append(list, job)
		}
		if err := delivery.UpsertRunJobs(ctx, pool, fixture.Settings.DeployRepo, run.RunID, list); err != nil {
			t.Fatalf("seed jobs of run %d: %v", run.RunID, err)
		}
	}
	return fixture
}

func getDeliveryMeasures(t *testing.T, handler http.Handler, query string) deliveryMeasuresResponse {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/measures?"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET measures?%s: status = %d, body = %s", query, response.Code, response.Body.String())
	}
	var body deliveryMeasuresResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode measures?%s: %v, body=%s", query, err, response.Body.String())
	}
	return body
}

func assertMeasuresFloat(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func assertMeasuresSpread(t *testing.T, name string, got measures.Spread, median, p90, max float64) {
	t.Helper()
	for _, field := range []struct {
		name string
		got  *float64
		want float64
	}{{"median_minutes", got.MedianMinutes, median}, {"p90_minutes", got.P90Minutes, p90}, {"max_minutes", got.MaxMinutes, max}} {
		if field.got == nil {
			t.Errorf("%s.%s = null, want %v", name, field.name, field.want)
			continue
		}
		assertMeasuresFloat(t, name+"."+field.name, *field.got, field.want)
	}
}

func seedMeasuresSettings(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := delivery.PutSettings(t.Context(), database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "widgets-release / widgets-release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
}

func TestGetDeliveryMeasuresWithoutSettingsIs404(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/measures", nil, "alice")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"DELIVERY_NOT_CONFIGURED"`) {
		t.Fatalf("status = %d, body = %s, want 404 DELIVERY_NOT_CONFIGURED", response.Code, response.Body.String())
	}
}

func TestGetDeliveryMeasuresRejectsMalformedWindow(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedMeasuresSettings(t, database)
	for _, target := range []string{
		"/api/v1/delivery/measures?from=not-a-date",
		"/api/v1/delivery/measures?to=not-a-date",
		"/api/v1/delivery/measures?from=2024-01-02T00:00:00Z&to=2024-01-01T00:00:00Z",
		"/api/v1/delivery/measures?from=2020-01-01T00:00:00Z&to=2024-01-01T00:00:00Z",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_QUERY"`) {
			t.Errorf("GET %s: status = %d, body = %s, want 400 INVALID_QUERY", target, response.Code, response.Body.String())
		}
	}
}

// TestGetDeliveryMeasuresIsAgentReadable: an agent optimizing against the measures reads them
// with its bearer, as a person's browser does; an anonymous caller reads nothing.
func TestGetDeliveryMeasuresIsAgentReadable(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedMeasuresSettings(t, database)
	target := "/api/v1/delivery/measures?" + measuresFixtureWindow
	if response := agentRequest(t, handler, http.MethodGet, target, nil, "agent-token"); response.Code != http.StatusOK {
		t.Fatalf("agent bearer: status = %d, body = %s, want 200", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodGet, target, nil, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status = %d, body = %s, want 401", response.Code, response.Body.String())
	}
}

// TestGetDeliveryTimelineCarriesTheMeasuresOfItsWindow: the timeline's read carries, as
// `measures`, exactly what GET /api/v1/delivery/measures answers for the same window, search and
// facets, so the Delivery page reads the population once when no brush narrows the measures.
// Only computed_at, the moment each answer was computed, may differ.
func TestGetDeliveryTimelineCarriesTheMeasuresOfItsWindow(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedMeasuresFixture(t, database)
	for _, query := range []string{
		measuresFixtureWindow,
		measuresFixtureWindow + "&repo=acme/widgets",
		measuresFixtureWindow + "&issue=ACME-101&q=facet",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?"+query, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET timeline?%s: status = %d, body = %s", query, response.Code, response.Body.String())
		}
		var timeline struct {
			Measures *deliveryMeasuresResponse `json:"measures"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &timeline); err != nil {
			t.Fatalf("decode timeline?%s: %v", query, err)
		}
		if timeline.Measures == nil {
			t.Fatalf("timeline?%s carries no measures", query)
		}
		got, want := *timeline.Measures, getDeliveryMeasures(t, handler, query)
		if got.ComputedAt.IsZero() {
			t.Errorf("timeline?%s: measures.computed_at is zero", query)
		}
		got.ComputedAt, want.ComputedAt = time.Time{}, time.Time{}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("timeline?%s measures = %+v, want the measures route's %+v", query, got, want)
		}
	}
}

// TestGetDeliveryMeasuresComputesTheFixture writes the fixture to Postgres and reads the measures
// back. The expected numbers are the stored path's: the prototype's own computeDora over the PRs
// with each deploy derived by the prototype's own containment from the fixture's runs
// (`bun /tmp/plan567s23/dump-dora-stored.ts` from the prototype's web/), which is what Dispatch
// derives from the stored rows; measures_test.go's TestComputeOnStoredContainment holds the same
// set.
func TestGetDeliveryMeasuresComputesTheFixture(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	fixture := seedMeasuresFixture(t, database)

	body := getDeliveryMeasures(t, handler, measuresFixtureWindow)
	result := body.Measures
	if f := result.DeployFrequency; f.SuccessfulDeploys != 5 || f.DeploysWithPRs != 5 {
		t.Errorf("deploy_frequency = %+v, want {5, 5}", f)
	}
	assertMeasuresFloat(t, "deploy_frequency.per_day", result.DeployFrequency.PerDay, 0.17341040462427745)
	assertMeasuresFloat(t, "deploy_frequency.with_prs_per_day", result.DeployFrequency.WithPRsPerDay, 0.17341040462427745)
	assertMeasuresSpread(t, "merge_to_production", result.LeadTime.MergeToProduction, 510, 2736, 3840)
	assertMeasuresSpread(t, "first_commit_to_production", result.LeadTime.FirstCommitToProduction, 1380, 2988, 4380)
	assertMeasuresSpread(t, "opened_to_production", result.LeadTime.OpenedToProduction, 1260, 2874, 4320)
	assertMeasuresSpread(t, "opened_to_merge", result.LeadTime.OpenedToMerge, 330, 462, 1080)
	assertMeasuresFloat(t, "rework_share", result.ReworkShare, 4.0/14)
	if s := result.DeployRunSuccess; s.Concluded != 7 || s.ReachedProduction != 5 || s.Cancelled != 1 || s.Rate == nil {
		t.Errorf("deploy_run_success = %+v, want {7, 5, 1, 5/7}", s)
	} else {
		assertMeasuresFloat(t, "deploy_run_success.rate", *s.Rate, 5.0/7)
	}
	if len(result.Daily) != 29 {
		t.Errorf("len(daily) = %d, want 29", len(result.Daily))
	}

	if body.FlagsSource != "none" {
		t.Errorf("flags_source = %q, want none", body.FlagsSource)
	}
	if got := result.ChangeFailureRate.PerPR; got.Confirmed != 0 || got.Total != 14 {
		t.Errorf("change_failure_rate.per_pr = %+v, want no confirmed flags of 14", got)
	}
	if result.TimeToRestore.MedianMinutes != nil {
		t.Errorf("time_to_restore.median_minutes = %v, want null", *result.TimeToRestore.MedianMinutes)
	}
	if len(body.UnownedP0) != 1 || body.UnownedP0[0] != (delivery.UnownedIssue{Key: "ACME-103", Title: "Production deploy gate flakes"}) {
		t.Errorf("unowned_p0 = %+v, want [{ACME-103 Production deploy gate flakes}]", body.UnownedP0)
	}
	if body.Status.UnownedP0 {
		t.Error("status.unowned_p0 = true, want false (ACME-103 has no owner)")
	}
	if body.Targets != measures.DefaultTargets {
		t.Errorf("targets = %+v, want %+v", body.Targets, measures.DefaultTargets)
	}
	if body.Status.DeploysPerDay || body.Status.MergeToProduction == nil || *body.Status.MergeToProduction {
		t.Errorf("status = %+v, want the fixture to miss deploys a day and merge to production", body.Status)
	}
	if body.Status.ChangeFailureRate == nil {
		// flags_source none still has 5 successful deploys to judge the per-deploy rate over.
		t.Error("status.change_failure_rate = null, want a verdict over 5 deploys")
	}
	timeline := getDeliveryTimeline(t, handler, measuresFixtureWindow)
	if body.Freshness != timeline.Freshness {
		t.Errorf("freshness = %+v, want the timeline's %+v", body.Freshness, timeline.Freshness)
	}
	if !body.Window.From.Equal(time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)) || !body.Window.To.Equal(time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)) {
		t.Errorf("window = %+v, want the requested one", body.Window)
	}
	if body.ComputedAt.IsZero() {
		t.Error("computed_at is zero")
	}

	// The repository facet narrows the PR-derived measures; deploys_with_prs is the deploy's.
	repo := getDeliveryMeasures(t, handler, measuresFixtureWindow+"&repo=acme/widgets").Measures
	assertMeasuresFloat(t, "repo facet rework_share", repo.ReworkShare, 4.0/13)
	assertMeasuresSpread(t, "repo facet opened_to_merge", repo.LeadTime.OpenedToMerge, 360, 468, 1080)
	if repo.DeployFrequency.DeploysWithPRs != 5 {
		t.Errorf("repo facet deploys_with_prs = %d, want 5", repo.DeployFrequency.DeploysWithPRs)
	}

	// The deployed facet narrows PRs, never runs.
	deployed := getDeliveryMeasures(t, handler, measuresFixtureWindow+"&deployed=deployed").Measures
	assertMeasuresSpread(t, "deployed facet opened_to_merge", deployed.LeadTime.OpenedToMerge, 300, 474, 1080)
	assertMeasuresFloat(t, "deployed facet rework_share", deployed.ReworkShare, 1.0/3)
	if deployed.DeployFrequency.SuccessfulDeploys != 5 {
		t.Errorf("deployed facet successful_deploys = %d, want 5", deployed.DeployFrequency.SuccessfulDeploys)
	}

	// The issue facet keeps #103, #109 and #113; deploys_with_prs still counts every deploy that
	// shipped a window PR (only runs 503 and 504 ship an ACME-101 PR, so a faceted count would be 2).
	issue := getDeliveryMeasures(t, handler, measuresFixtureWindow+"&issue=ACME-101").Measures
	assertMeasuresSpread(t, "issue facet opened_to_merge", issue.LeadTime.OpenedToMerge, 360, 360, 360)
	assertMeasuresFloat(t, "issue facet rework_share", issue.ReworkShare, 0)
	if issue.DeployFrequency.DeploysWithPRs != 5 {
		t.Errorf("issue facet deploys_with_prs = %d, want 5", issue.DeployFrequency.DeploysWithPRs)
	}

	// A ninth run started in the window whose production job succeeded, on branch feature/x: not
	// a deploy (LEGION-294's "a successful production job on main"), and every number stays as it
	// was. Its head commit is after acme/widgets#113's merge, so on main it would ship #113 too.
	ninthStarted := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	ninthDone := time.Date(2026, 9, 20, 10, 30, 0, 0, time.UTC)
	if err := delivery.UpsertRun(t.Context(), database.Pool, delivery.DeliveryRun{
		Repo: fixture.Settings.DeployRepo, RunID: 509, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "aaa109",
		HeadCommitAt: time.Date(2026, 9, 20, 9, 50, 0, 0, time.UTC), StartedAt: ninthStarted, CompletedAt: &ninthDone,
		Conclusion: new(delivery.DeliveryRunConclusionSuccess), URL: "https://github.com/acme/widgets/actions/runs/509",
		HeadBranch: new("feature/x"), Event: new("push"),
	}); err != nil {
		t.Fatalf("seed run 509: %v", err)
	}
	if err := delivery.UpsertRunJobs(t.Context(), database.Pool, fixture.Settings.DeployRepo, 509, []delivery.DeliveryRunJob{{
		Repo: fixture.Settings.DeployRepo, RunID: 509, Name: fixture.Settings.ProductionJobName,
		CompletedAt: &ninthDone, Conclusion: new(delivery.DeliveryJobConclusionSuccess),
	}}); err != nil {
		t.Fatalf("seed run 509's production job: %v", err)
	}
	ninth := getDeliveryMeasures(t, handler, measuresFixtureWindow)
	if !reflect.DeepEqual(ninth.Measures, body.Measures) {
		t.Errorf("with run 509 on feature/x, measures = %+v, want them unchanged: %+v", ninth.Measures, body.Measures)
	}
	for _, pr := range getDeliveryTimeline(t, handler, measuresFixtureWindow).PRs {
		if pr.ID == "acme/widgets#113" && pr.DeployedStatus != delivery.DeployedStatusWaiting {
			t.Errorf("acme/widgets#113 deployed_status = %q with run 509 on feature/x, want waiting", pr.DeployedStatus)
		}
	}
	for _, run := range getDeliveryTimeline(t, handler, measuresFixtureWindow).Runs {
		if run.ID == 509 {
			t.Errorf("the timeline's runs[] lists run 509 on feature/x, want only runs on main")
		}
	}
}
