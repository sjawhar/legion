package api

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// pipelineAt pins "today" to the fixture's last day, as the e2e seed and the docs screenshot do.
const pipelineAt = "2026-10-02T12:00:00Z"

// pipelineFixtureTime is the prototype's collector/tests/test_pipeline.py _ts: a time on
// 2026-09-<day> (a day past 30 rolls into October).
func pipelineFixtureTime(day, hour int, minute float64) time.Time {
	return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(day-1)*24*time.Hour + time.Duration(hour)*time.Hour + time.Duration(minute*float64(time.Minute)))
}

type pipelineFixtureJob struct {
	name       string
	conclusion string // "" while running
	start      time.Time
	end        time.Time
}

// seedPipelineRun writes one run and its jobs through the store calls intake and reconcile use.
func seedPipelineRun(t *testing.T, pool *store.Pool, run delivery.DeliveryRun, jobs []pipelineFixtureJob) {
	t.Helper()
	run.Repo = "acme/widgets"
	run.HeadSHA = "s" + strconv.FormatInt(run.RunID, 10)
	run.HeadCommitAt = run.StartedAt
	run.URL = "https://github.com/acme/widgets/actions/runs/" + strconv.FormatInt(run.RunID, 10)
	if err := delivery.UpsertRun(t.Context(), pool, run); err != nil {
		t.Fatalf("seed run %d: %v", run.RunID, err)
	}
	if len(jobs) == 0 {
		return
	}
	rows := make([]delivery.DeliveryRunJob, len(jobs))
	for i, job := range jobs {
		rows[i] = delivery.DeliveryRunJob{
			Repo: "acme/widgets", RunID: run.RunID, Name: job.name, StartedAt: new(job.start), CompletedAt: new(job.end),
		}
		if job.conclusion != "" {
			rows[i].Conclusion = new(delivery.DeliveryJobConclusion(job.conclusion))
		}
	}
	if err := delivery.UpsertRunJobs(t.Context(), pool, "acme/widgets", run.RunID, rows); err != nil {
		t.Fatalf("seed jobs of run %d: %v", run.RunID, err)
	}
}

// seedPipelineFixture writes Task 11's rows (the prototype's test_pipeline.py fixtures with the
// placeholder job names): the failing-jobs PR-checks runs as pull_request runs, the slowest-jobs
// runs' 50 build / unit results, the four push deploy runs on main and the workflow_dispatch one,
// plus the two rows a selection rule must leave out: a merge_group PR-checks run and a push run on
// feature/x, each with a successful named job.
func seedPipelineFixture(t *testing.T, database *store.Store, settings delivery.DeliverySettings) {
	t.Helper()
	pool := database.Pool
	if _, err := delivery.PutSettings(t.Context(), pool, settings, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	prRun := func(id int64, day, hour int, minutes float64, conclusion, event string, jobs ...pipelineFixtureJob) {
		started := pipelineFixtureTime(day, hour, 0)
		seedPipelineRun(t, pool, delivery.DeliveryRun{
			RunID: id, Kind: delivery.DeliveryRunKindPRChecks, PRNumber: new(7), StartedAt: started,
			CompletedAt: new(pipelineFixtureTime(day, hour, minutes)), Conclusion: new(delivery.DeliveryRunConclusion(conclusion)),
			Event: new(event),
		}, jobs)
	}
	prJob := func(day, hour int, name string, minutes float64, conclusion string) pipelineFixtureJob {
		return pipelineFixtureJob{name: name, conclusion: conclusion, start: pipelineFixtureTime(day, hour, 0), end: pipelineFixtureTime(day, hour, minutes)}
	}
	// test_pipeline.py:114-137 and dump-failing.py.
	prRun(1, 32, 8, 30, "failure", "pull_request",
		prJob(32, 8, "build / unit", 29, "failure"), prJob(32, 8, "checks-complete", 0.5, "failure"), prJob(32, 8, "lint", 5, "success"))
	prRun(2, 32, 10, 30, "failure", "pull_request",
		prJob(32, 10, "build / unit", 29, "failure"), prJob(32, 10, "lint", 5, "failure"))
	prRun(3, 31, 13, 30, "success", "pull_request",
		prJob(31, 13, "build / unit", 29, "success"), prJob(31, 13, "lint", 5, "success"), prJob(31, 13, "docs", 0, "skipped"))
	prRun(4, 31, 6, 30, "failure", "pull_request", prJob(31, 6, "typecheck", 4, "failure"))
	prRun(5, 32, 11, 10, "cancelled", "pull_request", prJob(32, 11, "build / unit", 9, "failure"))
	// Enough build / unit results to rank among the slowest jobs (test_pipeline.py:94-111's shape).
	for i := range 50 {
		prRun(int64(100+i), 27+i%5, 8, 95, "success", "pull_request", prJob(27+i%5, 8, "build / unit", float64(90+i), "success"))
	}
	// A merge_group run is not a PR check of a pull request (decision 6): uncounted.
	prRun(900, 32, 9, 20, "success", "merge_group", prJob(32, 9, "build / unit", 19, "success"))

	deploy := func(id int64, branch, event string, jobs map[string][2]any) {
		var rows []pipelineFixtureJob
		jobStart := pipelineFixtureTime(32, 9, 0)
		rows = append(rows, pipelineFixtureJob{name: "staging-e2e / inventory", conclusion: "skipped", start: jobStart, end: jobStart})
		for name, result := range jobs {
			rows = append(rows, pipelineFixtureJob{name: name, conclusion: result[0].(string), start: jobStart, end: pipelineFixtureTime(32, 9, result[1].(float64))})
		}
		seedPipelineRun(t, pool, delivery.DeliveryRun{
			RunID: id, Kind: delivery.DeliveryRunKindDeploy, StartedAt: pipelineFixtureTime(32, 8, 0),
			CompletedAt: new(pipelineFixtureTime(32, 12, 0)), Conclusion: new(delivery.DeliveryRunConclusionSuccess),
			HeadBranch: new(branch), Event: new(event),
		}, rows)
	}
	// test_pipeline.py:161-177.
	deploy(501, "main", "push", map[string][2]any{
		"staging-release / apply": {"success", 7.0}, "staging-e2e / run": {"success", 40.0}, "widgets-release / widgets-release": {"success", 9.0},
	})
	deploy(502, "main", "push", map[string][2]any{
		"staging-release / apply": {"success", 8.0}, "staging-e2e / run": {"failure", 70.0}, "widgets-release / widgets-release": {"skipped", 0.0},
	})
	deploy(503, "main", "push", map[string][2]any{"staging-release / apply": {"success", 9.0}, "staging-e2e / run": {"cancelled", 3.0}})
	deploy(504, "main", "push", map[string][2]any{"staging-release / apply": {"", 0.0}})
	deploy(505, "main", "workflow_dispatch", map[string][2]any{"staging-release / apply": {"failure", 99.0}, "staging-e2e / run": {"failure", 99.0}})
	// A push on another branch is not a deploy of main (decision 6): uncounted.
	deploy(506, "feature/x", "push", map[string][2]any{"staging-e2e / run": {"success", 30.0}, "widgets-release / widgets-release": {"success", 9.0}})
}

func pipelineSettings() delivery.DeliverySettings {
	return delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "widgets-release / widgets-release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors:     []string{"octocat"},
		StagingE2EJobName:     new("staging-e2e / run"),
		StagingApplyJobName:   new("staging-release / apply"),
		PRChecksRollupJobName: new("checks-complete"),
	}
}

func getDeliveryPipeline(t *testing.T, handler http.Handler, query string) (map[string]any, deliveryPipelineResponse) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/pipeline?"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET pipeline?%s: status = %d, body = %s", query, response.Code, response.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode pipeline: %v, body=%s", err, response.Body.String())
	}
	var body deliveryPipelineResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode pipeline: %v, body=%s", err, response.Body.String())
	}
	return raw, body
}

func TestGetDeliveryPipelineWithoutSettingsIs404(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/pipeline", nil, "alice")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"DELIVERY_NOT_CONFIGURED"`) {
		t.Fatalf("status = %d, body = %s, want 404 DELIVERY_NOT_CONFIGURED", response.Code, response.Body.String())
	}
}

func TestGetDeliveryPipelineRejectsMalformedAt(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedMeasuresSettings(t, database)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/pipeline?at=yesterday", nil, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_QUERY"`) {
		t.Fatalf("status = %d, body = %s, want 400 INVALID_QUERY", response.Code, response.Body.String())
	}
}

// TestGetDeliveryPipelineIsAgentReadable: an agent reads the Pipeline numbers with its bearer, as a
// person's browser does; an anonymous caller reads nothing.
func TestGetDeliveryPipelineIsAgentReadable(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedMeasuresSettings(t, database)
	target := "/api/v1/delivery/pipeline?at=" + pipelineAt
	if response := agentRequest(t, handler, http.MethodGet, target, nil, "agent-token"); response.Code != http.StatusOK {
		t.Fatalf("agent bearer: status = %d, body = %s, want 200", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodGet, target, nil, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status = %d, body = %s, want 401", response.Code, response.Body.String())
	}
}

// TestGetDeliveryPipelineComputesTheFixture reads Task 11's rows back through the route: the
// numbers are the prototype's (measures/pipeline_test.go cites the command that produced them),
// and the merge_group, workflow_dispatch and feature/x runs are left out by the store's selection.
func TestGetDeliveryPipelineComputesTheFixture(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedPipelineFixture(t, database, pipelineSettings())
	pool := database.Pool

	// Two population pull requests merged today: one deployed 50 minutes later, one still waiting
	// (opened an hour before its merge).
	for _, pr := range []struct {
		number          int
		created, merged time.Time
	}{
		{11, pipelineFixtureTime(32, 7, 0), pipelineFixtureTime(32, 7, 10)},
		{12, pipelineFixtureTime(32, 9, 0), pipelineFixtureTime(32, 10, 0)},
	} {
		if err := delivery.UpsertPullRequest(t.Context(), pool, delivery.DeliveryPullRequest{
			Repo: "acme/widgets", Number: pr.number, Title: "feat", Author: "octocat",
			URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(pr.number), CreatedAt: new(pr.created), MergedAt: new(pr.merged),
			Additions: new(1), Deletions: new(0),
		}); err != nil {
			t.Fatalf("seed PR %d: %v", pr.number, err)
		}
	}

	raw, body := getDeliveryPipeline(t, handler, "at="+pipelineAt)
	if want := []string{"2026-09-26", "2026-09-27", "2026-09-28", "2026-09-29", "2026-09-30", "2026-10-01", "2026-10-02"}; strings.Join(body.Days, ",") != strings.Join(want, ",") {
		t.Fatalf("days = %v, want %v", body.Days, want)
	}
	today := body.PRChecks.Run.Days[6]
	if today.N != 2 || today.Failure != 2 || today.Cancelled != 1 {
		t.Errorf("pr_checks.run.days[6] = %+v, want n 2, failure 2, cancelled 1 (the merge_group run uncounted)", today.Cell)
	}
	if len(body.PRChecks.SlowestJobs) == 0 || body.PRChecks.SlowestJobs[0].Name != "build / unit" {
		t.Errorf("pr_checks.slowest_jobs = %+v, want build / unit first", body.PRChecks.SlowestJobs)
	}
	if len(body.PRChecks.FailingJobs) != 2 {
		t.Fatalf("pr_checks.failing_jobs = %+v, want build / unit and lint", body.PRChecks.FailingJobs)
	}
	failing := body.PRChecks.FailingJobs[0]
	if failing.Name != "build / unit" || failing.Failures != 2 || failing.Results != 3 ||
		!failing.LastFailedAt.Equal(pipelineFixtureTime(32, 10, 30)) || failing.LastRunURL != "https://github.com/acme/widgets/actions/runs/2" {
		t.Errorf("pr_checks.failing_jobs[0] = %+v, want {build / unit, 2, 3, 2026-10-02T10:30:00Z, run 2}", failing)
	}
	if body.StagingE2E == nil {
		t.Fatal("staging_e2e = null, want its timing")
	}
	if e2e := body.StagingE2E.Days[6]; e2e.N != 2 || e2e.MedianMinutes == nil || *e2e.MedianMinutes != 55 || e2e.Cancelled != 1 {
		t.Errorf("staging_e2e.days[6] = %+v, want n 2, median 55, cancelled 1 (the workflow_dispatch and feature/x runs excluded)", e2e.Cell)
	}
	if body.StagingE2E.Job != "staging-e2e / run" {
		t.Errorf("staging_e2e.job = %q", body.StagingE2E.Job)
	}
	if body.StagingApply == nil || body.StagingApply.Days[6].N != 3 {
		t.Errorf("staging_apply = %+v, want n 3 today", body.StagingApply)
	}
	if production := body.ProductionApply.Total; production.N != 1 || production.Success != 1 {
		t.Errorf("production_apply.total = %+v, want n 1 (the feature/x run's success excluded)", production)
	}
	// Run 501's production job completed at 09:09 and shipped PR 11 (merged 07:10, head commit
	// 08:00 after it): merge to production is 119 minutes; PR 12 is still waiting. Open to merge
	// counts both, 10 and 60 minutes.
	if merge := body.MergeToProduction.Days[6]; merge.N != 1 || merge.MaxMinutes == nil || math.Abs(*merge.MaxMinutes-119) > 1e-9 {
		t.Errorf("merge_to_production.days[6] = %+v, want n 1, max 119", merge.LeadTimeCell)
	}
	if opened := body.OpenedToMerge.Total; opened.N != 2 || opened.MedianMinutes == nil || math.Abs(*opened.MedianMinutes-35) > 1e-9 {
		t.Errorf("opened_to_merge.total = %+v, want n 2, median 35", opened)
	}
	if body.Targets.MergeToProductionMinutes != 45 || body.Targets.OpenedToMergeMedianMinutes != 60 {
		t.Errorf("targets = %+v, want the measures' DefaultTargets", body.Targets)
	}
	if _, ok := raw["freshness"]; !ok {
		t.Error("no freshness")
	}

	// A pipeline with no staging end-to-end gate: its card goes, and nothing else changes.
	settings := pipelineSettings()
	settings.StagingE2EJobName = nil
	if _, err := delivery.PutSettings(t.Context(), pool, settings, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("clear staging_e2e_job_name: %v", err)
	}
	raw, without := getDeliveryPipeline(t, handler, "at="+pipelineAt)
	if value, present := raw["staging_e2e"]; !present || value != nil {
		t.Errorf("staging_e2e = %#v (present %v), want null", value, present)
	}
	if without.StagingApply == nil || without.StagingApply.Days[6].N != 3 || without.ProductionApply.Total.N != 1 ||
		without.PRChecks.Run.Days[6].N != 2 || len(without.PRChecks.FailingJobs) != 2 {
		t.Errorf("the rest changed: %+v", without)
	}
}

// TestGetDeliveryPipelineAnswersEmptyListsAsArrays: with no run stored, the lists are [] and the
// optional cards null, never omitted.
func TestGetDeliveryPipelineAnswersEmptyListsAsArrays(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedMeasuresSettings(t, database)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/pipeline?at="+pipelineAt, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	text := response.Body.String()
	for _, want := range []string{`"slowest_jobs":[]`, `"failing_jobs":[]`, `"staging_e2e":null`, `"staging_apply":null`} {
		if !strings.Contains(text, want) {
			t.Errorf("body lacks %s: %s", want, text)
		}
	}
}
