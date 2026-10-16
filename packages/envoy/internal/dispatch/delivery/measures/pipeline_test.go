package measures

import (
	"math"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/ordertest"
)

// The rows below are the ones the prototype's collector/tests/test_pipeline.py builds in code
// (`_ci`, `_job`, `_deploy`, GENERATED = 2026-10-02T12:00:00Z), with every job name replaced by a
// placeholder: a name is a label in every formula, so the numbers do not change. The expected
// values are the prototype's own pipeline.build output on those rows:
//
//	~/proto/delivery-timeline/collector/.venv/bin/python /tmp/plan567s23/dump-pipeline.py
//	~/proto/delivery-timeline/collector/.venv/bin/python /tmp/plan567s23/dump-failing.py
//
// and `cd collector && .venv/bin/python -m pytest tests/test_pipeline.py -q` passes the same
// assertions. The lead-time rows are the prototype's web/src/lib/pipeline.test.ts.

var (
	pipelineNow  = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	pipelineDays = []string{"2026-09-26", "2026-09-27", "2026-09-28", "2026-09-29", "2026-09-30", "2026-10-01", "2026-10-02"}
)

// at is the prototype test's _ts: a time on 2026-09-<day> (a day past 30 rolls into October).
func at(dayOfMonth, hour int, minute float64) time.Time {
	return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(dayOfMonth-1)*day + time.Duration(hour)*time.Hour + time.Duration(minute*float64(time.Minute)))
}

// ciJob is the prototype test's _job: a job of a PR-checks run that took minutes.
type ciJob struct {
	name       string
	minutes    float64
	conclusion string
}

func job(name string, minutes float64, conclusion ...string) ciJob {
	result := "success"
	if len(conclusion) > 0 {
		result = conclusion[0]
	}
	return ciJob{name: name, minutes: minutes, conclusion: result}
}

// ciRuns accumulates PR-checks runs and their job rows, as the prototype test's _ci builds them.
type ciRuns struct {
	runs []delivery.DeliveryRun
	jobs map[int64][]delivery.DeliveryRunJob
}

func runURL(id int64) string {
	return "https://github.com/acme/widgets/actions/runs/" + strconv.FormatInt(id, 10)
}

// add is _ci(run_id, day, minutes, conclusion, jobs, hour): a run started at hour on dayOfMonth
// that completed minutes later, each job starting with the run and lasting its minutes rounded to
// the second, as the prototype stores a job's seconds. No jobs is a run with no stored job rows.
func (c *ciRuns) add(id int64, dayOfMonth int, minutes float64, conclusion string, hour int, jobs ...ciJob) {
	if c.jobs == nil {
		c.jobs = map[int64][]delivery.DeliveryRunJob{}
	}
	started, completed := at(dayOfMonth, hour, 0), at(dayOfMonth, hour, minutes)
	c.runs = append(c.runs, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: id, Kind: delivery.DeliveryRunKindPRChecks,
		StartedAt: started, CompletedAt: &completed, Conclusion: new(delivery.DeliveryRunConclusion(conclusion)), URL: runURL(id),
	})
	for _, j := range jobs {
		c.jobs[id] = append(c.jobs[id], delivery.DeliveryRunJob{
			Repo: "acme/widgets", RunID: id, Name: j.name, StartedAt: &started,
			CompletedAt: new(started.Add(time.Duration(math.Round(j.minutes*60)) * time.Second)),
			Conclusion:  new(delivery.DeliveryJobConclusion(j.conclusion)),
		})
	}
}

func assertCell(t *testing.T, name string, got Cell, n int, median, p90 *float64, success, failure, cancelled int, rate *float64) {
	t.Helper()
	if got.N != n || got.Success != success || got.Failure != failure || got.Cancelled != cancelled {
		t.Errorf("%s = {n %d, success %d, failure %d, cancelled %d}, want {%d, %d, %d, %d}",
			name, got.N, got.Success, got.Failure, got.Cancelled, n, success, failure, cancelled)
	}
	assertOptional(t, name+".median_minutes", got.MedianMinutes, median)
	assertOptional(t, name+".p90_minutes", got.P90Minutes, p90)
	assertOptional(t, name+".success_rate", got.SuccessRate, rate)
}

// TestOutcome is test_pipeline.py:69-73: a time-out is a failure, and a result that is neither
// success, failure nor cancelled (skipped, still running) is not counted.
func TestOutcome(t *testing.T) {
	cases := []struct {
		conclusion *string
		want       Outcome
		counted    bool
	}{
		{new("success"), OutcomeSuccess, true},
		{new("failure"), OutcomeFailure, true},
		{new("timed_out"), OutcomeFailure, true},
		{new("cancelled"), OutcomeCancelled, true},
		{new("startup_failure"), "", false},
		{new("skipped"), "", false},
		{nil, "", false},
	}
	for _, c := range cases {
		got, counted := OutcomeOf(c.conclusion)
		name := "<nil>"
		if c.conclusion != nil {
			name = *c.conclusion
		}
		if got != c.want || counted != c.counted {
			t.Errorf("OutcomeOf(%s) = (%q, %v), want (%q, %v)", name, got, counted, c.want, c.counted)
		}
	}
}

// TestRound1MatchesPython pins the rounding to Python's round(x, n), which rounds the decimal
// value a float holds half to even: 189 s is 3.15 minutes, which Python rounds to 3.1 where a
// float multiply by ten would give 3.2. Each want is the prototype venv's round() of the input.
func TestRound1MatchesPython(t *testing.T) {
	cases := []struct {
		value  float64
		places int
		want   float64
	}{
		{3.15, 1, 3.1},
		{189.0 / 60, 1, 3.1},
		{2.675, 2, 2.67},
		{0.25, 1, 0.2},
		{0.35, 1, 0.3},
		{56.5, 1, 56.5},
		{85.65, 1, 85.7},
		{6.0 / 7, 4, 0.8571},
	}
	for _, c := range cases {
		if got := roundTo(c.value, c.places); got != c.want {
			t.Errorf("roundTo(%v, %d) = %v, want %v", c.value, c.places, got, c.want)
		}
	}
}

// TestComputeTimingGivesEachDayAndTheWeekAMedianP90AndSuccessRate is test_pipeline.py:37-66.
func TestComputeTimingGivesEachDayAndTheWeekAMedianP90AndSuccessRate(t *testing.T) {
	var samples []Sample
	for _, minutes := range []float64{10, 20, 30, 40} {
		samples = append(samples, Sample{Day: "2026-10-02", Outcome: OutcomeSuccess, Minutes: new(minutes)})
	}
	samples = append(samples,
		Sample{Day: "2026-10-02", Outcome: OutcomeFailure, Minutes: new(60.0)},
		Sample{Day: "2026-10-02", Outcome: OutcomeCancelled},
		Sample{Day: "2026-10-01", Outcome: OutcomeSuccess, Minutes: new(15.0)},
		Sample{Day: "2026-10-01", Outcome: OutcomeSuccess, Minutes: new(25.0)},
		// Before the first shown day.
		Sample{Day: "2026-09-25", Outcome: OutcomeFailure, Minutes: new(500.0)},
	)
	timing := ComputeTiming(samples, pipelineDays)

	if len(timing.Days) != 7 {
		t.Fatalf("days = %d, want 7", len(timing.Days))
	}
	for i, d := range timing.Days {
		if d.Day != pipelineDays[i] || d.Partial != (i == 6) {
			t.Errorf("days[%d] = {%s, partial %v}, want {%s, partial %v}", i, d.Day, d.Partial, pipelineDays[i], i == 6)
		}
	}
	assertCell(t, "days[0]", timing.Days[0].Cell, 0, nil, nil, 0, 0, 0, nil)
	// p90 lies between the two nearest ranks, as d3's quantileSorted puts it: (5 - 1) * 0.9 = 3.6.
	assertCell(t, "days[6]", timing.Days[6].Cell, 5, new(30.0), new(52.0), 4, 1, 1, new(0.8))
	assertCell(t, "days[5]", timing.Days[5].Cell, 2, new(20.0), new(24.0), 2, 0, 0, new(1.0))
	assertCell(t, "total", timing.Total, 7, new(25.0), new(48.0), 6, 1, 1, new(0.8571))
}

// TestPRChecksTimesWholeRunsFromStartToCompletion is test_pipeline.py:76-91: a run's wall time
// from its start to its completion, a cancelled run counted beside the timed ones, a run before
// the shown days dropped, and a run with no job rows (run 2) timed all the same.
func TestPRChecksTimesWholeRunsFromStartToCompletion(t *testing.T) {
	var c ciRuns
	c.add(1, 32, 20, "success", 8)
	c.add(2, 32, 93, "success", 8)
	c.add(3, 32, 5, "cancelled", 8)
	c.add(4, 31, 30, "failure", 8)
	c.add(5, 24, 30, "success", 8)
	run, _, _ := PRChecks(c.runs, c.jobs, pipelineDays, pipelineNow, "")

	assertCell(t, "run.days[6]", run.Days[6].Cell, 2, new(56.5), new(85.7), 2, 0, 1, new(1.0))
	assertCell(t, "run.days[5]", run.Days[5].Cell, 1, new(30.0), new(30.0), 0, 1, 0, new(0.0))
	assertCell(t, "run.total", run.Total, 3, new(30.0), new(80.4), 2, 1, 1, new(0.6667))
}

// TestSlowestJobsRankBySevenDayMedianOnceAJobHasEnoughResults is test_pipeline.py:94-111: a job
// ranks once it has 50 timed results in the shown days, by its seven-day median, slowest first.
func TestSlowestJobsRankBySevenDayMedianOnceAJobHasEnoughResults(t *testing.T) {
	var c ciRuns
	for i := range SlowestMinResults {
		infra := "success"
		if i == 0 {
			infra = "failure"
		}
		c.add(int64(100+i), 27+i%6, 95, "success", 8,
			job("build / unit", float64(90+i)), job("lint", 5), job("infra / plan", 17, infra))
	}
	// Slow but rare: one result short of enough to rank.
	for i := range SlowestMinResults - 1 {
		c.add(int64(1000+i), 30, 130, "success", 8, job("tasks / run (one task)", 120))
	}
	_, slowest, _ := PRChecks(c.runs, c.jobs, pipelineDays, pipelineNow, "")

	names := make([]string, len(slowest))
	for i, j := range slowest {
		names[i] = j.Name
	}
	if want := []string{"build / unit", "infra / plan", "lint"}; !slices.Equal(names, want) {
		t.Fatalf("slowest = %v, want %v", names, want)
	}
	assertCell(t, "build / unit total", slowest[0].Total, 50, new(114.5), new(134.1), 50, 0, 0, new(1.0))
	assertCell(t, "infra / plan total", slowest[1].Total, 50, new(17.0), new(17.0), 49, 1, 0, new(0.98))
	assertCell(t, "lint total", slowest[2].Total, 50, new(5.0), new(5.0), 50, 0, 0, new(1.0))
}

// failingFixture is test_pipeline.py:114-137's runs with the roll-up checks-complete, plus the two
// rows the prototype never stores and Dispatch does (/tmp/plan567s23/dump-failing.py): a skipped
// docs job on run 3, and a fifth run cancelled at 11:10 on Oct 2 with a failed build / unit job.
func failingFixture() ciRuns {
	var c ciRuns
	c.add(1, 32, 30, "failure", 8, job("build / unit", 29, "failure"), job("checks-complete", 0.5, "failure"), job("lint", 5))
	c.add(2, 32, 30, "failure", 10, job("build / unit", 29, "failure"), job("lint", 5, "failure"))
	c.add(3, 31, 30, "success", 13, job("build / unit", 29), job("lint", 5), job("docs", 0, "skipped"))
	// Ended before the last 24 hours.
	c.add(4, 31, 30, "failure", 6, job("typecheck", 4, "failure"))
	c.add(5, 32, 10, "cancelled", 11, job("build / unit", 9, "failure"))
	return c
}

// TestFailingJobsCoverTheLast24HoursWithoutTheRollup is test_pipeline.py:114-137 and
// dump-failing.py: the roll-up and a skipped job add no result, a cancelled run's failed job adds
// no failure, and a failure before the last 24 hours is absent.
func TestFailingJobsCoverTheLast24HoursWithoutTheRollup(t *testing.T) {
	c := failingFixture()
	run, _, failing := PRChecks(c.runs, c.jobs, pipelineDays, pipelineNow, "checks-complete")

	lastFailed := time.Date(2026, 10, 2, 10, 30, 0, 0, time.UTC)
	want := []FailingJob{
		{Name: "build / unit", Failures: 2, Results: 3, LastFailedAt: lastFailed, LastRunURL: runURL(2)},
		{Name: "lint", Failures: 1, Results: 3, LastFailedAt: lastFailed, LastRunURL: runURL(2)},
	}
	if len(failing) != len(want) {
		t.Fatalf("failing = %+v, want %+v", failing, want)
	}
	for i := range want {
		if failing[i].Name != want[i].Name || failing[i].Failures != want[i].Failures || failing[i].Results != want[i].Results ||
			!failing[i].LastFailedAt.Equal(want[i].LastFailedAt) || failing[i].LastRunURL != want[i].LastRunURL {
			t.Errorf("failing[%d] = %+v, want %+v", i, failing[i], want[i])
		}
	}
	assertCell(t, "run.days[6]", run.Days[6].Cell, 2, new(30.0), new(30.0), 0, 2, 1, new(0.0))
	assertCell(t, "run.total", run.Total, 4, new(30.0), new(30.0), 1, 3, 1, new(0.25))
}

// TestFailingJobsWithoutARollupLeaveNothingOut: with no roll-up job set, the job that only reports
// the others' result is tallied like any other.
func TestFailingJobsWithoutARollupLeaveNothingOut(t *testing.T) {
	c := failingFixture()
	_, _, failing := PRChecks(c.runs, c.jobs, pipelineDays, pipelineNow, "")
	names := make([]string, len(failing))
	for i, j := range failing {
		names[i] = j.Name
	}
	if want := []string{"build / unit", "checks-complete", "lint"}; !slices.Equal(names, want) {
		t.Fatalf("failing names = %v, want %v", names, want)
	}
}

// TestFailingJobsStopAtNow: the failing-jobs table covers the 24 hours before now and nothing
// after it, so a pinned `at` in the past never counts a run that completed later that day.
func TestFailingJobsStopAtNow(t *testing.T) {
	c := failingFixture()
	// Completed at 12:30 on Oct 2, half an hour after pipelineNow.
	c.add(6, 32, 20, "failure", 12, job("build / unit", 19, "failure"), job("lint", 5, "failure"), job("typecheck", 3, "failure"))
	_, _, failing := PRChecks(c.runs, c.jobs, pipelineDays, pipelineNow, "checks-complete")

	byName := map[string]FailingJob{}
	for _, f := range failing {
		byName[f.Name] = f
	}
	if unit := byName["build / unit"]; unit.Failures != 2 || unit.Results != 3 || unit.LastRunURL != runURL(2) {
		t.Errorf("build / unit = %+v, want 2 failures of 3 results, last in run 2 (run 6 completed after now)", unit)
	}
	if _, listed := byName["typecheck"]; listed {
		t.Errorf("typecheck is listed, but its only failure in the window is run 6's, after now: %+v", failing)
	}
}

// TestAFailingJobsLastRunIsTheSameInEveryRunOrder: of the runs that completed in one instant, a
// failing job's last run is the one with the higher run id, in every order the runs are listed in
// (ListRunsStartedIn answers runs that started together by run id, but runs that completed together
// can have started apart).
func TestAFailingJobsLastRunIsTheSameInEveryRunOrder(t *testing.T) {
	var c ciRuns
	// Runs 8 and 9 both complete at 10:30 on Oct 2; run 9 started earlier. Run 7 completes first.
	c.add(7, 32, 10, "failure", 9, job("build / unit", 9, "failure"))
	c.add(8, 32, 30, "failure", 10, job("build / unit", 29, "failure"))
	c.add(9, 32, 90, "failure", 9, job("build / unit", 89, "failure"))
	lastFailed := time.Date(2026, 10, 2, 10, 30, 0, 0, time.UTC)
	ordertest.EveryOrder(c.runs, func(runs []delivery.DeliveryRun) {
		ids := make([]int64, len(runs))
		for i, run := range runs {
			ids[i] = run.RunID
		}
		_, _, failing := PRChecks(runs, c.jobs, pipelineDays, pipelineNow, "")
		if len(failing) != 1 || failing[0].Failures != 3 || !failing[0].LastFailedAt.Equal(lastFailed) || failing[0].LastRunURL != runURL(9) {
			t.Fatalf("failing jobs of runs listed %v = %+v, want build / unit failing 3 times, last in run 9 at %s", ids, failing, lastFailed)
		}
	})
}

// TestPRChecksJobCellsCountOnlyTimedJobsOfTimedRuns: a job's samples come only from runs that
// ended in success or failure, and only from jobs that did (the prototype never stores the rest,
// github_source.py:287-299, loop.py:241), so the cancelled run's build / unit row and the skipped
// docs job add nothing.
func TestPRChecksJobCellsCountOnlyTimedJobsOfTimedRuns(t *testing.T) {
	c := failingFixture()
	timings := jobTimings(c.runs, c.jobs, pipelineDays)
	unit, ok := timings["build / unit"]
	if !ok {
		t.Fatal("no build / unit timing")
	}
	assertCell(t, "build / unit total", unit.Total, 3, new(29.0), new(29.0), 1, 2, 0, new(0.3333))
	if _, ok := timings["docs"]; ok {
		t.Error("a skipped job has a timing")
	}
}

// deployResult is one named job of a deploy run: its conclusion ("" while it runs) and minutes.
type deployResult struct {
	conclusion string
	minutes    float64
}

// deployRun is the prototype test's _deploy: a push run on main started at 08:00 on dayOfMonth
// whose named jobs started at 09:00 and took the given minutes, beside a skipped job no card
// follows.
func deployRun(id int64, dayOfMonth int, jobs map[string]deployResult) (delivery.DeliveryRun, []delivery.DeliveryRunJob) {
	run := delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: id, Kind: delivery.DeliveryRunKindDeploy,
		StartedAt: at(dayOfMonth, 8, 0), CompletedAt: new(at(dayOfMonth, 12, 0)),
		Conclusion: new(delivery.DeliveryRunConclusionSuccess), URL: runURL(id),
	}
	jobStart := at(dayOfMonth, 9, 0)
	rows := []delivery.DeliveryRunJob{{
		Repo: "acme/widgets", RunID: id, Name: "staging-e2e / inventory",
		StartedAt: &jobStart, CompletedAt: &jobStart, Conclusion: new(delivery.DeliveryJobConclusionSkipped),
	}}
	for name, j := range jobs {
		var jobConclusion *delivery.DeliveryJobConclusion
		if j.conclusion != "" {
			jobConclusion = new(delivery.DeliveryJobConclusion(j.conclusion))
		}
		rows = append(rows, delivery.DeliveryRunJob{
			Repo: "acme/widgets", RunID: id, Name: name,
			StartedAt: &jobStart, CompletedAt: new(at(dayOfMonth, 9, j.minutes)), Conclusion: jobConclusion,
		})
	}
	return run, rows
}

const (
	stagingE2EJob      = "staging-e2e / run"
	stagingApplyJob    = "staging-release / apply"
	productionApplyJob = "widgets-release / widgets-release"
)

// TestDeployJobTimingsFollowEachNamedJob is test_pipeline.py:161-177 with its four push runs: the
// caller's store query already dropped the workflow_dispatch run. One pass over the runs times
// every named job, and a name no run lists still gets its empty days.
func TestDeployJobTimingsFollowEachNamedJob(t *testing.T) {
	var runs []delivery.DeliveryRun
	jobsByRun := map[int64][]delivery.DeliveryRunJob{}
	for id, jobs := range map[int64]map[string]deployResult{
		1: {stagingApplyJob: {"success", 7}, stagingE2EJob: {"success", 40}, productionApplyJob: {"success", 9}},
		2: {stagingApplyJob: {"success", 8}, stagingE2EJob: {"failure", 70}, productionApplyJob: {"skipped", 0}},
		3: {stagingApplyJob: {"success", 9}, stagingE2EJob: {"cancelled", 3}},
		4: {stagingApplyJob: {"", 0}}, // still running: not counted
	} {
		run, rows := deployRun(id, 32, jobs)
		runs = append(runs, run)
		jobsByRun[id] = rows
	}

	timings := DeployJobs(runs, jobsByRun, []DeployJobCard{
		{CardID: "staging_e2e", Production: delivery.Production{JobNames: []string{stagingE2EJob}}},
		{CardID: "staging_apply", Production: delivery.Production{JobNames: []string{stagingApplyJob}}},
		{CardID: "production_apply", Production: delivery.Production{JobNames: []string{productionApplyJob}}},
		{CardID: "never", Production: delivery.Production{JobNames: []string{"never-run / job"}}},
	}, pipelineDays)
	if len(timings) != 4 {
		t.Fatalf("timings = %d, want one per card", len(timings))
	}
	e2e := timings["staging_e2e"]
	if e2e.Job != stagingE2EJob {
		t.Errorf("staging_e2e.job = %q, want %q", e2e.Job, stagingE2EJob)
	}
	assertCell(t, "staging_e2e.days[6]", e2e.Days[6].Cell, 2, new(55.0), new(67.0), 1, 1, 1, new(0.5))
	assertCell(t, "staging_apply.days[6]", timings["staging_apply"].Days[6].Cell, 3, new(8.0), new(8.8), 3, 0, 0, new(1.0))
	assertCell(t, "production_apply.total", timings["production_apply"].Total, 1, new(9.0), new(9.0), 1, 0, 0, new(1.0))
	never := timings["never"]
	if never.Job != "never-run / job" || len(never.Days) != 7 {
		t.Errorf("never-run timing = {job %q, %d days}, want its name and 7 days", never.Job, len(never.Days))
	}
	assertCell(t, "never-run.total", never.Total, 0, nil, nil, 0, 0, 0, nil)
}

// TestTheProductionCardMergesItsNamesAndCountsOnlyAppliedJobs proves one card follows both names
// of a renamed production job while excluding a job whose configured apply step was skipped.
func TestTheProductionCardMergesItsNamesAndCountsOnlyAppliedJobs(t *testing.T) {
	const oldName, newName, step = "production-apply / production-apply", "production / production-apply", "Run ./.github/actions/apply"
	var runs []delivery.DeliveryRun
	jobsByRun := map[int64][]delivery.DeliveryRunJob{}
	for id, jobs := range map[int64]map[string]deployResult{
		1: {oldName: {"success", 9}},
		2: {newName: {"success", 11}},
		3: {newName: {"success", 1}},
	} {
		run, rows := deployRun(id, 32, jobs)
		for i := range rows {
			if rows[i].Name == newName || rows[i].Name == oldName {
				rows[i].ApplyStep = new(step)
				rows[i].ApplyStepConclusion = new(delivery.DeliveryJobConclusionSuccess)
				if id == 3 {
					rows[i].ApplyStepConclusion = new(delivery.DeliveryJobConclusionSkipped)
				}
			}
		}
		runs = append(runs, run)
		jobsByRun[id] = rows
	}
	timings := DeployJobs(runs, jobsByRun, []DeployJobCard{{CardID: "production_apply", Production: delivery.Production{JobNames: []string{newName, oldName}, ApplyStep: new(step)}}}, pipelineDays)
	if timings["production_apply"].Job != newName+" or "+oldName {
		t.Errorf("job = %q, want both names", timings["production_apply"].Job)
	}
	assertCell(t, "production_apply.total", timings["production_apply"].Total, 2, new(10.0), new(10.8), 2, 0, 0, new(1.0))
}

// TestProductionCardUsesOneAppliedListedJobPerRun proves a run holding both names of a renamed
// production job contributes only its applied job, not its stood-down predecessor, to one card.
func TestProductionCardUsesOneAppliedListedJobPerRun(t *testing.T) {
	const stoodDownName, appliedName, step = "production-apply / production-apply", "production / production-apply", "Run apply"
	started, stoodDownAt, appliedAt := at(32, 8, 0), at(32, 9, 10), at(32, 9, 30)
	run := delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: 5, Kind: delivery.DeliveryRunKindDeploy, StartedAt: started,
		CompletedAt: new(appliedAt), Conclusion: new(delivery.DeliveryRunConclusionSuccess), URL: runURL(5),
	}
	timings := DeployJobs([]delivery.DeliveryRun{run}, map[int64][]delivery.DeliveryRunJob{run.RunID: {
		{Name: stoodDownName, StartedAt: new(at(32, 9, 0)), CompletedAt: new(stoodDownAt), Conclusion: new(delivery.DeliveryJobConclusionSuccess), ApplyStep: new(step), ApplyStepConclusion: new(delivery.DeliveryJobConclusionSkipped)},
		{Name: appliedName, StartedAt: new(at(32, 9, 0)), CompletedAt: new(appliedAt), Conclusion: new(delivery.DeliveryJobConclusionSuccess), ApplyStep: new(step), ApplyStepConclusion: new(delivery.DeliveryJobConclusionSuccess)},
	}}, []DeployJobCard{{
		CardID: "production_apply", Production: delivery.Production{JobNames: []string{stoodDownName, appliedName}, ApplyStep: new(step)},
	}}, pipelineDays)
	assertCell(t, "production_apply.total", timings["production_apply"].Total, 1, new(30.0), new(30.0), 1, 0, 0, new(1.0))
}

func assertLeadTimeCell(t *testing.T, name string, got LeadTimeCell, n int, median, p90, maximum *float64) {
	t.Helper()
	if got.N != n {
		t.Errorf("%s.n = %d, want %d", name, got.N, n)
	}
	assertOptional(t, name+".median_minutes", got.MedianMinutes, median)
	assertOptional(t, name+".p90_minutes", got.P90Minutes, p90)
	assertOptional(t, name+".max_minutes", got.MaxMinutes, maximum)
}

func ts(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// TestLeadTimesBucketByMergeDay is the prototype's web/src/lib/pipeline.test.ts:33-73: merge to
// production over deployed PRs and open to merge over every PR, each by its UTC merge day.
func TestLeadTimesBucketByMergeDay(t *testing.T) {
	deployed := func(merged, deployedAt string) PR {
		return PR{CreatedAt: ts(t, merged), MergedAt: ts(t, merged), DeployedAt: new(ts(t, deployedAt))}
	}
	waiting := func(merged string) PR {
		return PR{CreatedAt: ts(t, merged), MergedAt: ts(t, merged)}
	}
	merge, _ := LeadTimes([]PR{
		deployed("2026-10-02T10:00:00Z", "2026-10-02T10:30:00Z"), // 30m today
		deployed("2026-10-02T11:00:00Z", "2026-10-02T11:50:00Z"), // 50m today
		waiting("2026-10-02T11:30:00Z"),                          // no lead time yet
		deployed("2026-10-01T23:50:00Z", "2026-10-02T00:30:00Z"), // 40m, merged Oct 1
		deployed("2026-09-25T12:00:00Z", "2026-09-25T13:00:00Z"), // before the shown days
	}, pipelineDays)
	for i, d := range merge.Days {
		if d.Day != pipelineDays[i] || d.Partial != (i == 6) {
			t.Errorf("merge.days[%d] = {%s, partial %v}", i, d.Day, d.Partial)
		}
	}
	assertLeadTimeCell(t, "merge.days[6]", merge.Days[6].LeadTimeCell, 2, new(40.0), new(48.0), new(50.0))
	assertLeadTimeCell(t, "merge.days[5]", merge.Days[5].LeadTimeCell, 1, new(40.0), new(40.0), new(40.0))
	assertLeadTimeCell(t, "merge.days[0]", merge.Days[0].LeadTimeCell, 0, nil, nil, nil)
	assertLeadTimeCell(t, "merge.total", merge.Total, 3, new(40.0), new(48.0), new(50.0))

	opened := func(created, merged string) PR {
		return PR{CreatedAt: ts(t, created), MergedAt: ts(t, merged)}
	}
	_, openedToMerge := LeadTimes([]PR{
		opened("2026-10-02T09:00:00Z", "2026-10-02T10:00:00Z"), // 60m today, waiting
		opened("2026-10-02T10:00:00Z", "2026-10-02T10:30:00Z"), // 30m today
		opened("2026-10-01T23:00:00Z", "2026-10-02T00:30:00Z"), // 90m, merged Oct 2
		opened("2026-09-25T11:00:00Z", "2026-09-25T12:00:00Z"), // before the shown days
	}, pipelineDays)
	assertLeadTimeCell(t, "opened.days[6]", openedToMerge.Days[6].LeadTimeCell, 3, new(60.0), new(84.0), new(90.0))
	assertLeadTimeCell(t, "opened.days[0]", openedToMerge.Days[0].LeadTimeCell, 0, nil, nil, nil)
	assertLeadTimeCell(t, "opened.total", openedToMerge.Total, 3, new(60.0), new(84.0), new(90.0))
}

// TestShownDaysAndWindowStart is pipeline.py:54-63: the seven UTC days ending today, oldest
// first, and midnight of the first.
func TestShownDaysAndWindowStart(t *testing.T) {
	if got := ShownDays(pipelineNow); !slices.Equal(got, pipelineDays) {
		t.Errorf("ShownDays = %v, want %v", got, pipelineDays)
	}
	if got, want := WindowStart(pipelineNow), time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("WindowStart = %v, want %v", got, want)
	}
	// A clock in another zone names the same UTC days.
	eastern := pipelineNow.In(time.FixedZone("UTC+14", 14*3600))
	if got := ShownDays(eastern); !slices.Equal(got, pipelineDays) {
		t.Errorf("ShownDays(UTC+14) = %v, want %v", got, pipelineDays)
	}
}
