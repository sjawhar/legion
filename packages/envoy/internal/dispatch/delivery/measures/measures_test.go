package measures

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// fixtureFile is testdata/dora-fixture.json: the prototype's web/fixtures/dataset.json with its
// names replaced, carrying two expectation sets per PR and run. expected.prototype is the deploy
// the prototype's fixture states (its own dora.test.ts inputs); expected.stored is the deploy the
// prototype's containment derives from the fixture's runs, which is what Dispatch derives from the
// stored rows (containment.go FirstShippingApply).
type fixtureFile struct {
	Window struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"window"`
	PRs []struct {
		Repo          string     `json:"repo"`
		Number        int        `json:"number"`
		CreatedAt     time.Time  `json:"created_at"`
		MergedAt      time.Time  `json:"merged_at"`
		FirstCommitAt *time.Time `json:"first_commit_at"`
		Rework        bool       `json:"rework"`
		Expected      struct {
			Prototype fixtureDeploy `json:"prototype"`
			Stored    fixtureDeploy `json:"stored"`
		} `json:"expected"`
	} `json:"prs"`
	Runs []struct {
		RunID      int64                           `json:"run_id"`
		StartedAt  time.Time                       `json:"started_at"`
		Conclusion *delivery.DeliveryRunConclusion `json:"conclusion"`
		Production *struct {
			Conclusion string `json:"conclusion"`
		} `json:"production"`
		ExpectedPRs       []string `json:"expected_prs"`
		ExpectedPRsStored []string `json:"expected_prs_stored"`
	} `json:"runs"`
}

type fixtureDeploy struct {
	DeployRun  *int64     `json:"deploy_run"`
	DeployedAt *time.Time `json:"deployed_at"`
}

// fixtureFlags are the prototype fixture's four failed-change flags (web/fixtures/dataset.json
// "flags"), names replaced as the fixture's are. The fixture file carries none, since slice 2
// stores no flags (decision 1); these prove the change-failure formulas on the prototype's inputs.
var fixtureFlags = []Flag{
	{Broken: "acme/widgets#101", Fix: "acme/widgets#102", Kind: "fix", State: "confirmed"},
	{Broken: "acme/widgets#104", Fix: "acme/widgets#105", Kind: "revert", State: "confirmed"},
	{Broken: "acme/widgets#106", Fix: "acme/widgets#107", Kind: "fix", State: "pending"},
	{Broken: "acme/widgets#111", Fix: "acme/widgets#112", Kind: "fix", State: "rejected"},
}

func readFixture(t *testing.T) fixtureFile {
	t.Helper()
	data, err := os.ReadFile("testdata/dora-fixture.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture fixtureFile
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fixture
}

// loadFixture builds Compute's inputs from the fixture as the prototype's own dora.test.ts reads
// it: each PR's deploy as the fixture states it, and each run's ShippedPRs from expected_prs.
func loadFixture(t *testing.T) ([]PR, []Run, Window) {
	return buildFixture(t, false)
}

// storedFixture builds the same inputs with each PR's deploy as containment derives it from the
// fixture's runs (expected.stored, expected_prs_stored): the numbers Task 3's API test reads back
// from Postgres.
func storedFixture(t *testing.T) ([]PR, []Run, Window) {
	return buildFixture(t, true)
}

func buildFixture(t *testing.T, stored bool) ([]PR, []Run, Window) {
	t.Helper()
	fixture := readFixture(t)
	prs := make([]PR, 0, len(fixture.PRs))
	for _, pr := range fixture.PRs {
		deploy := pr.Expected.Prototype
		if stored {
			deploy = pr.Expected.Stored
		}
		prs = append(prs, PR{
			ID:            pr.Repo + "#" + strconv.Itoa(pr.Number),
			CreatedAt:     pr.CreatedAt,
			MergedAt:      pr.MergedAt,
			FirstCommitAt: pr.FirstCommitAt,
			DeployRunID:   deploy.DeployRun,
			DeployedAt:    deploy.DeployedAt,
			Rework:        pr.Rework,
		})
	}
	runs := make([]Run, 0, len(fixture.Runs))
	for _, run := range fixture.Runs {
		shipped := run.ExpectedPRs
		if stored {
			shipped = run.ExpectedPRsStored
		}
		runs = append(runs, Run{
			ID:                run.RunID,
			StartedAt:         run.StartedAt,
			Conclusion:        run.Conclusion,
			ReachedProduction: run.Production != nil && run.Production.Conclusion == "success",
			ShippedPRs:        len(shipped),
		})
	}
	return prs, runs, Window{From: fixture.Window.From, To: fixture.Window.To}
}

const tolerance = 1e-9

func near(got, want float64) bool { return math.Abs(got-want) <= tolerance }

func assertFloat(t *testing.T, name string, got, want float64) {
	t.Helper()
	if !near(got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func assertOptional(t *testing.T, name string, got *float64, want *float64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s = %v, want %v", name, show(got), show(want))
	case !near(*got, *want):
		t.Errorf("%s = %v, want %v", name, *got, *want)
	}
}

func show(v *float64) any {
	if v == nil {
		return "nil"
	}
	return *v
}

func assertSpread(t *testing.T, name string, got Spread, median, p90, max float64) {
	t.Helper()
	assertOptional(t, name+".median_minutes", got.MedianMinutes, &median)
	assertOptional(t, name+".p90_minutes", got.P90Minutes, &p90)
	assertOptional(t, name+".max_minutes", got.MaxMinutes, &max)
}

func dayOf(t *testing.T, daily []DailyPoint, day string) DailyPoint {
	t.Helper()
	for _, point := range daily {
		if point.Day == day {
			return point
		}
	}
	t.Fatalf("daily has no %s", day)
	return DailyPoint{}
}

func assertDay(t *testing.T, point DailyPoint, deploys, concluded, reached, cancelled int, rate *float64) {
	t.Helper()
	if point.Deploys != deploys || point.Concluded != concluded || point.ReachedProduction != reached || point.Cancelled != cancelled {
		t.Errorf("%s = {deploys %d, concluded %d, reached %d, cancelled %d}, want {%d, %d, %d, %d}",
			point.Day, point.Deploys, point.Concluded, point.ReachedProduction, point.Cancelled, deploys, concluded, reached, cancelled)
	}
	assertOptional(t, point.Day+".run_success_rate", point.RunSuccessRate, rate)
}

// Expected values below are the prototype's own computeDora output on the same fixture:
//
//	cd ~/proto/delivery-timeline/web && bun /tmp/plan567s23/dump-dora.ts
//
// and its web/src/lib/dora.test.ts asserts the same numbers (`bun test src/lib/dora.test.ts`).

func TestComputeMatchesThePrototypeOnItsFixture(t *testing.T) {
	prs, runs, window := loadFixture(t)
	result := Compute(prs, runs, fixtureFlags, window)

	frequency := result.DeployFrequency
	if frequency.SuccessfulDeploys != 5 || frequency.DeploysWithPRs != 5 {
		t.Errorf("deploy_frequency = {%d, %d}, want {5, 5}", frequency.SuccessfulDeploys, frequency.DeploysWithPRs)
	}
	assertFloat(t, "deploy_frequency.per_day", frequency.PerDay, 0.17341040462427745)
	assertFloat(t, "deploy_frequency.with_prs_per_day", frequency.WithPRsPerDay, 0.17341040462427745)

	assertSpread(t, "merge_to_production", result.LeadTime.MergeToProduction, 60, 1014, 1140)
	assertSpread(t, "first_commit_to_production", result.LeadTime.FirstCommitToProduction, 255, 1494, 1620)
	assertSpread(t, "opened_to_production", result.LeadTime.OpenedToProduction, 210, 1416, 1500)
	assertSpread(t, "opened_to_merge", result.LeadTime.OpenedToMerge, 330, 462, 1080)

	perPR := result.ChangeFailureRate.PerPR
	if perPR.Confirmed != 2 || perPR.Pending != 1 || perPR.Rejected != 1 || perPR.Reverts != 1 || perPR.Total != 14 {
		t.Errorf("per_pr = %+v, want {confirmed 2, pending 1, rejected 1, reverts 1, total 14}", perPR)
	}
	assertFloat(t, "per_pr.rate", perPR.Rate, 2.0/14)
	perDeploy := result.ChangeFailureRate.PerDeploy
	if perDeploy.Confirmed != 2 || perDeploy.Pending != 0 || perDeploy.Rejected != 1 || perDeploy.Reverts != 1 ||
		perDeploy.Total != 5 || perDeploy.ConfirmedOrPending != 2 {
		t.Errorf("per_deploy = %+v, want {confirmed 2, pending 0, rejected 1, reverts 1, total 5, confirmed_or_pending 2}", perDeploy)
	}
	assertFloat(t, "per_deploy.rate", perDeploy.Rate, 0.4)
	assertFloat(t, "per_deploy.upper_bound_rate", perDeploy.UpperBoundRate, 0.4)

	assertOptional(t, "time_to_restore.median_minutes", result.TimeToRestore.MedianMinutes, new(750.0))
	assertFloat(t, "rework_share", result.ReworkShare, 4.0/14)

	success := result.DeployRunSuccess
	if success.Concluded != 7 || success.ReachedProduction != 5 || success.Cancelled != 1 {
		t.Errorf("deploy_run_success = %+v, want {7, 5, 1}", success)
	}
	assertOptional(t, "deploy_run_success.rate", success.Rate, new(5.0/7))

	daily := result.Daily
	if len(daily) != 29 {
		t.Fatalf("len(daily) = %d, want 29", len(daily))
	}
	if daily[0].Day != "2026-08-30" || daily[0].Partial {
		t.Errorf("daily[0] = {%s, partial %t}, want {2026-08-30, false}", daily[0].Day, daily[0].Partial)
	}
	if daily[28].Day != "2026-09-27" || !daily[28].Partial {
		t.Errorf("daily[28] = {%s, partial %t}, want {2026-09-27, true}", daily[28].Day, daily[28].Partial)
	}
	deploys, concluded := 0, 0
	for _, point := range daily {
		deploys += point.Deploys
		concluded += point.Concluded
	}
	if deploys != frequency.SuccessfulDeploys || concluded != success.Concluded {
		t.Errorf("daily sums = {deploys %d, concluded %d}, want the totals {%d, %d}", deploys, concluded, frequency.SuccessfulDeploys, success.Concluded)
	}
	assertDay(t, dayOf(t, daily, "2026-09-05"), 1, 1, 1, 0, new(1.0))
	assertDay(t, dayOf(t, daily, "2026-09-11"), 0, 1, 0, 0, new(0.0))
	assertDay(t, dayOf(t, daily, "2026-09-19"), 0, 0, 0, 1, nil)
	assertDay(t, dayOf(t, daily, "2026-09-01"), 0, 0, 0, 0, nil)
}

// TestComputeOnStoredContainment runs the fixture with each PR's deploy derived from the fixture's
// runs, as Dispatch derives it from stored rows; the numbers are the prototype's computeDora over
// the prototype's own containment of the same fixture (`bun /tmp/plan567s23/dump-dora-stored.ts`
// from the prototype's web/), so Task 3's API test and this package agree on one set.
func TestComputeOnStoredContainment(t *testing.T) {
	prs, runs, window := storedFixture(t)
	result := Compute(prs, runs, nil, window)

	if result.DeployFrequency.SuccessfulDeploys != 5 || result.DeployFrequency.DeploysWithPRs != 5 {
		t.Errorf("deploy_frequency = %+v, want {5, 5}", result.DeployFrequency)
	}
	assertFloat(t, "deploy_frequency.per_day", result.DeployFrequency.PerDay, 0.17341040462427745)
	assertSpread(t, "merge_to_production", result.LeadTime.MergeToProduction, 510, 2736, 3840)
	assertSpread(t, "first_commit_to_production", result.LeadTime.FirstCommitToProduction, 1380, 2988, 4380)
	assertSpread(t, "opened_to_production", result.LeadTime.OpenedToProduction, 1260, 2874, 4320)
	assertSpread(t, "opened_to_merge", result.LeadTime.OpenedToMerge, 330, 462, 1080)
	assertFloat(t, "rework_share", result.ReworkShare, 4.0/14)
	if s := result.DeployRunSuccess; s.Concluded != 7 || s.ReachedProduction != 5 || s.Cancelled != 1 {
		t.Errorf("deploy_run_success = %+v, want {7, 5, 1}", s)
	}
}

func TestComputeWithNoFlagsReportsZeroFailuresAndNoRestoreTime(t *testing.T) {
	prs, runs, window := loadFixture(t)
	result := Compute(prs, runs, nil, window)

	if got, want := result.ChangeFailureRate.PerPR, (FlagCounts{Total: 14}); got != want {
		t.Errorf("per_pr = %+v, want %+v", got, want)
	}
	if got, want := result.ChangeFailureRate.PerDeploy, (DeployFlagCounts{FlagCounts: FlagCounts{Total: 5}}); got != want {
		t.Errorf("per_deploy = %+v, want %+v", got, want)
	}
	if result.TimeToRestore.MedianMinutes != nil {
		t.Errorf("time_to_restore.median_minutes = %v, want nil", *result.TimeToRestore.MedianMinutes)
	}
}

// TestUpperBoundCountsEachDeployOnce is dora.test.ts:95-113: #108 shipped in run 504, which no
// confirmed flag touches, so the bound gains a deploy; #105 shipped in run 503, already counted
// through confirmed #104, so it does not count twice.
func TestUpperBoundCountsEachDeployOnce(t *testing.T) {
	prs, runs, window := loadFixture(t)
	flags := append(append([]Flag{}, fixtureFlags...),
		Flag{Broken: "acme/widgets#108", Fix: "acme/widgets#109", Kind: "fix", State: "pending"},
		Flag{Broken: "acme/widgets#105", Fix: "acme/widgets#107", Kind: "fix", State: "pending"},
	)
	perDeploy := Compute(prs, runs, flags, window).ChangeFailureRate.PerDeploy
	assertFloat(t, "per_deploy.rate", perDeploy.Rate, 0.4)
	if perDeploy.Pending != 2 || perDeploy.ConfirmedOrPending != 3 {
		t.Errorf("per_deploy = {pending %d, confirmed_or_pending %d}, want {2, 3}", perDeploy.Pending, perDeploy.ConfirmedOrPending)
	}
	assertFloat(t, "per_deploy.upper_bound_rate", perDeploy.UpperBoundRate, 0.6)
}

// TestACancelledRunWhoseProductionJobSucceededIsADeployButNotARunSuccess is dora.test.ts:122-137.
func TestACancelledRunWhoseProductionJobSucceededIsADeployButNotARunSuccess(t *testing.T) {
	prs, runs, window := loadFixture(t)
	runs = append(runs, Run{
		ID:                509,
		StartedAt:         time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC),
		Conclusion:        new(delivery.DeliveryRunConclusionCancelled),
		ReachedProduction: true,
	})
	result := Compute(prs, runs, fixtureFlags, window)
	if result.DeployFrequency.SuccessfulDeploys != 6 {
		t.Errorf("successful_deploys = %d, want 6", result.DeployFrequency.SuccessfulDeploys)
	}
	if s := result.DeployRunSuccess; s.Concluded != 7 || s.ReachedProduction != 5 || s.Cancelled != 2 {
		t.Errorf("deploy_run_success = %+v, want {7, 5, 2}", s)
	}
	assertOptional(t, "deploy_run_success.rate", result.DeployRunSuccess.Rate, new(5.0/7))
	assertDay(t, dayOf(t, result.Daily, "2026-09-20"), 1, 0, 0, 1, nil)
}

// windowed narrows the fixture to a window as the store's queries do: runs started in [From, To)
// and PRs merged in it. Compute filters nothing itself.
func windowed(prs []PR, runs []Run, window Window) ([]PR, []Run) {
	var keptPRs []PR
	for _, pr := range prs {
		if !pr.MergedAt.Before(window.From) && pr.MergedAt.Before(window.To) {
			keptPRs = append(keptPRs, pr)
		}
	}
	var keptRuns []Run
	for _, run := range runs {
		if !run.StartedAt.Before(window.From) && run.StartedAt.Before(window.To) {
			keptRuns = append(keptRuns, run)
		}
	}
	return keptPRs, keptRuns
}

type dayRow struct {
	day     string
	partial bool
	deploys int
}

func dayRows(daily []DailyPoint) []dayRow {
	rows := make([]dayRow, len(daily))
	for i, point := range daily {
		rows[i] = dayRow{point.Day, point.Partial, point.Deploys}
	}
	return rows
}

func assertDayRows(t *testing.T, got []DailyPoint, want []dayRow) {
	t.Helper()
	rows := dayRows(got)
	if len(rows) != len(want) {
		t.Fatalf("daily = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("daily[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

// TestDailySeriesFollowsABrushedWindow is dora.test.ts:155-166.
func TestDailySeriesFollowsABrushedWindow(t *testing.T) {
	allPRs, allRuns, _ := loadFixture(t)
	window := Window{From: time.Date(2026, 9, 5, 12, 30, 0, 0, time.UTC), To: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
	prs, runs := windowed(allPRs, allRuns, window)
	result := Compute(prs, runs, fixtureFlags, window)
	assertDayRows(t, result.Daily, []dayRow{
		{"2026-09-05", true, 0},
		{"2026-09-06", false, 1},
		{"2026-09-07", false, 0},
		{"2026-09-08", false, 0},
		{"2026-09-09", true, 1},
	})
	assertFloat(t, "deploy_frequency.per_day", result.DeployFrequency.PerDay, 0.5026178010471204)
}

// TestAWindowEndingAtMidnightHasNoEmptyTrailingDay is dora.test.ts:168-176.
func TestAWindowEndingAtMidnightHasNoEmptyTrailingDay(t *testing.T) {
	allPRs, allRuns, _ := loadFixture(t)
	window := Window{From: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)}
	prs, runs := windowed(allPRs, allRuns, window)
	assertDayRows(t, Compute(prs, runs, fixtureFlags, window).Daily, []dayRow{
		{"2026-09-06", false, 1},
		{"2026-09-07", false, 0},
		{"2026-09-08", false, 0},
	})
}

// TestQuantileIsLinearBetweenRanks is the prototype's collector/tests/test_pipeline.py:52-57.
func TestQuantileIsLinearBetweenRanks(t *testing.T) {
	values := []float64{10, 20, 30, 40, 60}
	assertFloat(t, "Quantile(0.9)", Quantile(values, 0.9), 52)
	assertFloat(t, "Quantile(0.5)", Quantile(values, 0.5), 30)
	assertFloat(t, "Quantile of one", Quantile([]float64{7}, 0.9), 7)
}

func optionalBool(b *bool) any {
	if b == nil {
		return "nil"
	}
	return *b
}

func assertStatus(t *testing.T, got, want Status) {
	t.Helper()
	pairs := []struct {
		name      string
		got, want *bool
	}{
		{"change_failure_rate", got.ChangeFailureRate, want.ChangeFailureRate},
		{"change_failure_rate_upper_bound", got.ChangeFailureRateUpperBound, want.ChangeFailureRateUpperBound},
		{"merge_to_production_median", got.MergeToProductionMedian, want.MergeToProductionMedian},
		{"merge_to_production", got.MergeToProduction, want.MergeToProduction},
		{"opened_to_merge", got.OpenedToMerge, want.OpenedToMerge},
		{"deploy_run_success", got.DeployRunSuccess, want.DeployRunSuccess},
	}
	for _, pair := range pairs {
		if (pair.got == nil) != (pair.want == nil) || (pair.got != nil && *pair.got != *pair.want) {
			t.Errorf("%s = %v, want %v", pair.name, optionalBool(pair.got), optionalBool(pair.want))
		}
	}
	if got.DeploysPerDay != want.DeploysPerDay {
		t.Errorf("deploys_per_day = %t, want %t", got.DeploysPerDay, want.DeploysPerDay)
	}
	if got.UnownedP0 != want.UnownedP0 {
		t.Errorf("unowned_p0 = %t, want %t", got.UnownedP0, want.UnownedP0)
	}
}

// atTargets is the fixture's result with every KPI set exactly to its target value
// (dora.test.ts:193-219).
func atTargets(base Result) Result {
	result := base
	result.DeployFrequency.PerDay = DefaultTargets.DeploysPerDay
	result.ChangeFailureRate.PerDeploy.Total = 100
	result.ChangeFailureRate.PerDeploy.Rate = DefaultTargets.ChangeFailureRate
	result.ChangeFailureRate.PerDeploy.UpperBoundRate = DefaultTargets.ChangeFailureRate
	merge := DefaultTargets.MergeToProductionMinutes
	result.LeadTime.MergeToProduction = Spread{MedianMinutes: &merge, P90Minutes: &merge, MaxMinutes: &merge}
	opened := DefaultTargets.OpenedToMergeMedianMinutes
	result.LeadTime.OpenedToMerge = Spread{MedianMinutes: &opened, P90Minutes: &opened, MaxMinutes: &opened}
	result.DeployRunSuccess = DeployRunSuccess{Concluded: 10, ReachedProduction: 9, Rate: new(DefaultTargets.DeployRunSuccessRate)}
	return result
}

// TestKPIStatus is dora.test.ts:190-290.
func TestKPIStatus(t *testing.T) {
	prs, runs, window := loadFixture(t)
	base := Compute(prs, runs, fixtureFlags, window)

	t.Run("the fixture misses every target", func(t *testing.T) {
		assertStatus(t, KPIStatus(base, 1, DefaultTargets), Status{
			DeploysPerDay:               false,
			ChangeFailureRate:           new(false),
			ChangeFailureRateUpperBound: new(false),
			MergeToProductionMedian:     new(false),
			MergeToProduction:           new(false),
			OpenedToMerge:               new(false),
			DeployRunSuccess:            new(false),
			UnownedP0:                   false,
		})
	})

	t.Run("'at least' targets are met at the target; 'under' targets are missed at it", func(t *testing.T) {
		assertStatus(t, KPIStatus(atTargets(base), 0, DefaultTargets), Status{
			DeploysPerDay:               true,
			ChangeFailureRate:           new(false),
			ChangeFailureRateUpperBound: new(false),
			MergeToProductionMedian:     new(false),
			MergeToProduction:           new(false),
			OpenedToMerge:               new(false),
			DeployRunSuccess:            new(true),
			UnownedP0:                   true,
		})
	})

	t.Run("just under an 'under' target is met, and the maximum decides merge to production", func(t *testing.T) {
		result := atTargets(base)
		result.ChangeFailureRate.PerDeploy.Rate = 0.0499
		result.ChangeFailureRate.PerDeploy.UpperBoundRate = 0.08
		result.LeadTime.MergeToProduction = Spread{MedianMinutes: new(20.0), P90Minutes: new(40.0), MaxMinutes: new(44.9)}
		result.LeadTime.OpenedToMerge = Spread{MedianMinutes: new(59.9), P90Minutes: new(59.9), MaxMinutes: new(59.9)}
		assertStatus(t, KPIStatus(result, 0, DefaultTargets), Status{
			DeploysPerDay:               true,
			ChangeFailureRate:           new(true),
			ChangeFailureRateUpperBound: new(false),
			MergeToProductionMedian:     new(true),
			MergeToProduction:           new(true),
			OpenedToMerge:               new(true),
			DeployRunSuccess:            new(true),
			UnownedP0:                   true,
		})
	})

	t.Run("a window with nothing to measure reports no status rather than met", func(t *testing.T) {
		result := atTargets(base)
		result.ChangeFailureRate.PerDeploy.Total = 0
		result.ChangeFailureRate.PerDeploy.Rate = 0
		result.ChangeFailureRate.PerDeploy.UpperBoundRate = 0
		result.LeadTime.MergeToProduction = Spread{}
		result.LeadTime.OpenedToMerge = Spread{}
		result.DeployRunSuccess = DeployRunSuccess{Cancelled: 3}
		assertStatus(t, KPIStatus(result, 0, DefaultTargets), Status{
			DeploysPerDay: true,
			UnownedP0:     true,
		})
	})
}
