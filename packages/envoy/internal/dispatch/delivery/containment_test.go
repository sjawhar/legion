package delivery

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ordertest"
)

func TestProductionApplies(t *testing.T) {
	const jobName = "widgets-release / widgets-release"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	runs := []DeliveryRun{
		{Repo: "acme/widgets", RunID: 1, HeadCommitAt: base},
		{Repo: "acme/widgets", RunID: 2, HeadCommitAt: base.Add(time.Hour)},
		{Repo: "acme/widgets", RunID: 3, HeadCommitAt: base.Add(2 * time.Hour)},
	}
	jobsByRun := map[int64][]DeliveryRunJob{
		// run 1 has no matching job at all -- skipped
		1: {{Name: "other-job", Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base)}},
		// run 2's matching job failed -- skipped
		2: {{Name: jobName, Conclusion: new(DeliveryJobConclusionFailure), CompletedAt: new(base.Add(time.Hour))}},
		// run 3's matching job succeeded -- included
		3: {{Name: jobName, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base.Add(2 * time.Hour))}},
	}

	judged := ProductionApplies(runs, jobsByRun, Production{JobNames: []string{jobName}})
	if len(judged.Applies) != 1 {
		t.Fatalf("expected 1 apply, got %d", len(judged.Applies))
	}
	if judged.Applies[0].RunID != 3 {
		t.Errorf("expected apply for run 3, got run %d", judged.Applies[0].RunID)
	}
}

// TestProductionAppliesSetsApartOnlyARunWithNoProductionJob: a successfully concluded run holding
// no job by the configured production job name is set apart for its callers' warning, unless the
// production job was skipped, which GitHub lists under the name of the calling job it skipped, and
// another run of the same listing holds a job by the configured name, which shows the name is a
// real job's and not a typo. testdata/dev-only-run-jobs.json is GitHub's job list for a main run of
// a deploy workflow whose dispatch applied dev alone: its production job, a reusable workflow's
// (`production-apply / production-apply`), is skipped and listed as the bare `production-apply`,
// beside a skipped `staging-apply`. It is decoded and mapped exactly as reconcileRun stores a run's
// jobs.
func TestProductionAppliesSetsApartOnlyARunWithNoProductionJob(t *testing.T) {
	const jobName = "production-apply / production-apply"
	data, err := os.ReadFile("testdata/dev-only-run-jobs.json")
	if err != nil {
		t.Fatalf("read the recorded job list: %v", err)
	}
	fetched, _, err := DecodeJobsPage(data)
	if err != nil {
		t.Fatalf("decode the recorded job list: %v", err)
	}
	devOnly := RunJobs("acme/widgets", 1, fetched, Production{})

	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	success := DeliveryRunConclusionSuccess
	runs := make([]DeliveryRun, 5)
	for i := range runs {
		runs[i] = DeliveryRun{Repo: "acme/widgets", RunID: int64(i + 1), HeadCommitAt: base.Add(time.Duration(i) * time.Hour), Conclusion: &success}
	}
	setApart := func(runs []DeliveryRun, jobsByRun map[int64][]DeliveryRunJob, jobName string) ([]Apply, []int64) {
		judged := ProductionApplies(runs, jobsByRun, Production{JobNames: []string{jobName}})
		ids := []int64{}
		for _, run := range judged.Unmatched {
			ids = append(ids, run.Run.RunID)
		}
		return judged.Applies, ids
	}
	jobsByRun := map[int64][]DeliveryRunJob{
		// The dev-only run: its production job skipped under the caller's name. No deploy, and,
		// beside run 4, no warning.
		1: devOnly,
		// A renamed job: neither spelling of the configured one.
		2: {{Name: "prod-apply / prod-apply", Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base)}},
		// The caller's name, but a job that ran: the configured name is not the job this run ran.
		3: {{Name: "production-apply", Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base)}},
		// The configured job, succeeded: a deploy, which shows the configured name is real.
		4: {{Name: jobName, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base.Add(4 * time.Hour))}},
		// A skipped job whose name starts the configured one without ending at a " / ".
		5: {{Name: "production", Conclusion: new(DeliveryJobConclusionSkipped)}},
	}
	applies, ids := setApart(runs, jobsByRun, jobName)
	if len(applies) != 1 || applies[0].RunID != 4 {
		t.Fatalf("applies = %+v, want run 4's alone", applies)
	}
	if !slices.Equal(ids, []int64{2, 3, 5}) {
		t.Fatalf("runs set apart = %v, want [2 3 5]: never the dev-only run 1 nor the deploy 4", ids)
	}
	if unmatched := ProductionApplies(runs, jobsByRun, Production{JobNames: []string{jobName}}).Unmatched; !slices.Equal(unmatched[0].JobNames, []string{"prod-apply / prod-apply"}) {
		t.Errorf("run 2's job names = %v, want every job it holds", unmatched[0].JobNames)
	}

	// A typo whose first part names a job the run skipped: `staging-apply` is skipped in the
	// dev-only run, and no run of the listing holds a `staging-apply / production-apply`, so
	// nothing shows the name is real and the run is set apart, beside the deploy 4 as alone.
	for _, listing := range [][]DeliveryRun{{runs[0]}, {runs[0], runs[3]}} {
		if _, ids := setApart(listing, jobsByRun, "staging-apply / production-apply"); !slices.Equal(ids, []int64{1, 4}[:len(listing)]) {
			t.Fatalf("a typo'd name over runs %d..: runs set apart = %v, want every run of the listing", listing[0].RunID, ids)
		}
	}

	// A listing in which every run skipped production: nothing shows the configured name is real,
	// so each run is set apart, the price of not mistaking a typo for a skipped call.
	everySkipped := map[int64][]DeliveryRunJob{1: devOnly, 2: RunJobs("acme/widgets", 2, fetched, Production{})}
	if _, ids := setApart(runs[:2], everySkipped, jobName); !slices.Equal(ids, []int64{1, 2}) {
		t.Fatalf("a listing with no production job: runs set apart = %v, want [1 2]", ids)
	}

	// A nested reusable call GitHub skipped is listed by the names down to the skipped call, as a
	// dispatch-only deploy run lists `dispatch-apply / staging-e2e`; a run that ran the call shows
	// the configured name is real.
	const nestedName = "dispatch-apply / staging-e2e / staging-e2e"
	nested := map[int64][]DeliveryRunJob{
		1: {
			{Name: "dispatch-apply / dispatch-apply", Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base)},
			{Name: "dispatch-apply / staging-e2e", Conclusion: new(DeliveryJobConclusionSkipped)},
		},
		2: {{Name: nestedName, Conclusion: new(DeliveryJobConclusionFailure), CompletedAt: new(base)}},
	}
	if applies, ids := setApart(runs[:2], nested, nestedName); len(applies) != 0 || len(ids) != 0 {
		t.Fatalf("a nested call skipped: applies = %+v, set apart = %v, want neither", applies, ids)
	}
}

// TestProductionAppliesMatchesEveryListedName proves a renamed production job's old and new
// names both count while an unmatched run and a skipped caller keep their existing safeguards.
func TestProductionAppliesMatchesEveryListedName(t *testing.T) {
	const oldName, newName = "production-apply / production-apply", "production / production-apply"
	production := Production{JobNames: []string{newName, oldName}}
	base := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	success := DeliveryRunConclusionSuccess
	runs := make([]DeliveryRun, 4)
	for i := range runs {
		runs[i] = DeliveryRun{Repo: "acme/widgets", RunID: int64(i + 1), HeadCommitAt: base.Add(time.Duration(i) * time.Hour), Conclusion: &success}
	}
	jobsByRun := map[int64][]DeliveryRunJob{
		1: {{Name: oldName, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base.Add(time.Hour))}},
		2: {{Name: newName, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base.Add(2 * time.Hour))}},
		3: {{Name: "prod / apply", Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base)}},
		4: {{Name: "production", Conclusion: new(DeliveryJobConclusionSkipped)}},
	}
	judged := ProductionApplies(runs, jobsByRun, production)
	var applied []int64
	for _, apply := range judged.Applies {
		applied = append(applied, apply.RunID)
	}
	if !slices.Equal(applied, []int64{1, 2}) {
		t.Fatalf("applies = %v, want runs 1 (old name) and 2 (new name)", applied)
	}
	if len(judged.Unmatched) != 1 || judged.Unmatched[0].Run.RunID != 3 || !slices.Equal(judged.Unmatched[0].JobNames, []string{"prod / apply"}) {
		t.Fatalf("unmatched = %+v, want run 3 alone, naming its job", judged.Unmatched)
	}
	alone := ProductionApplies([]DeliveryRun{runs[0], runs[3]}, jobsByRun, production)
	if len(alone.Unmatched) != 1 || alone.Unmatched[0].Run.RunID != 4 {
		t.Fatalf("unmatched without a run of the new name = %+v, want run 4", alone.Unmatched)
	}
}

// TestProductionAppliesDoesNotSetApartASkippedCallerOfEverySuccessfulListedName proves each
// successful listed job demonstrates its name is real, even when Production.Job selects another
// listed job as the run's deploy.
func TestProductionAppliesDoesNotSetApartASkippedCallerOfEverySuccessfulListedName(t *testing.T) {
	const selectedName, otherName = "production / deploy", "production-apply / deploy"
	base := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	success := DeliveryRunConclusionSuccess
	runs := []DeliveryRun{
		{Repo: "acme/widgets", RunID: 1, HeadCommitAt: base, Conclusion: &success},
		{Repo: "acme/widgets", RunID: 2, HeadCommitAt: base.Add(time.Hour), Conclusion: &success},
	}
	jobsByRun := map[int64][]DeliveryRunJob{
		// Production.Job selects selectedName, but otherName still succeeded in this run.
		1: {
			{Name: selectedName, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base)},
			{Name: otherName, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base.Add(time.Hour))},
		},
		// GitHub lists the skipped call to otherName under its bare caller name.
		2: {{Name: "production-apply", Conclusion: new(DeliveryJobConclusionSkipped)}},
	}

	judged := ProductionApplies(runs, jobsByRun, Production{JobNames: []string{selectedName, otherName}})
	if len(judged.Applies) != 1 || !judged.Applies[0].CompletedAt.Equal(base) {
		t.Fatalf("applies = %+v, want selectedName's earlier apply", judged.Applies)
	}
	if len(judged.Unmatched) != 0 {
		t.Fatalf("unmatched = %+v, want none: otherName was proven real by run 1", judged.Unmatched)
	}
}

// TestProductionAppliesCountsOnlyAJobThatApplied proves the optional apply-step rule distinguishes
// a stored historical job from a stood-down job and names a configured step that is absent.
func TestProductionAppliesCountsOnlyAJobThatApplied(t *testing.T) {
	const name, step = "production / production-apply", "Run ./.github/actions/apply"
	production := Production{JobNames: []string{name}, ApplyStep: new(step)}
	base := time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC)
	success := DeliveryRunConclusionSuccess
	job := func(applyStep *string, stepConclusion *DeliveryJobConclusion) []DeliveryRunJob {
		return []DeliveryRunJob{{Name: name, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base), ApplyStep: applyStep, ApplyStepConclusion: stepConclusion}}
	}
	runs := make([]DeliveryRun, 5)
	for i := range runs {
		runs[i] = DeliveryRun{Repo: "acme/widgets", RunID: int64(i + 1), HeadCommitAt: base, Conclusion: &success}
	}
	jobsByRun := map[int64][]DeliveryRunJob{
		1: job(new(step), new(DeliveryJobConclusionSuccess)),
		2: job(new(step), new(DeliveryJobConclusionSkipped)),
		3: job(new(step), nil),
		4: job(nil, nil),
		5: job(new("Run something else"), new(DeliveryJobConclusionSkipped)),
	}
	judged := ProductionApplies(runs, jobsByRun, production)
	var applied []int64
	for _, apply := range judged.Applies {
		applied = append(applied, apply.RunID)
	}
	if !slices.Equal(applied, []int64{1, 4, 5}) {
		t.Fatalf("applies = %v, want 1 (applied), 4 (stored before steps) and 5 (read for another step)", applied)
	}
	if len(judged.StepMissing) != 1 || judged.StepMissing[0].RunID != 3 {
		t.Fatalf("step-missing = %+v, want run 3 alone", judged.StepMissing)
	}
	if len(judged.Unmatched) != 0 {
		t.Fatalf("unmatched = %+v, want none: every run holds the production job", judged.Unmatched)
	}
	if got := production.Conclusion(jobsByRun[2][0]); got == nil || *got != DeliveryJobConclusionSkipped {
		t.Fatalf("the stood-down job's conclusion = %v, want skipped", got)
	}
}

// TestARunsProductionJobIsTheSameInEveryListingOrder: among listed jobs that succeeded in one
// instant the run's production job is the first by name, and a run with none that succeeded falls
// back to its first listed job by when each started (a job that never started last, then by name),
// in every order its jobs are listed in.
func TestARunsProductionJobIsTheSameInEveryListingOrder(t *testing.T) {
	const first, second = "production / deploy", "production-apply / deploy"
	production := Production{JobNames: []string{second, first}}
	base := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		jobs []DeliveryRunJob
		want string
	}{
		{
			name: "two listed jobs succeeded together",
			jobs: []DeliveryRunJob{
				{Name: second, StartedAt: new(base), CompletedAt: new(base.Add(time.Hour)), Conclusion: new(DeliveryJobConclusionSuccess)},
				{Name: first, StartedAt: new(base.Add(time.Minute)), CompletedAt: new(base.Add(time.Hour)), Conclusion: new(DeliveryJobConclusionSuccess)},
				{Name: "build", StartedAt: new(base), CompletedAt: new(base.Add(time.Minute)), Conclusion: new(DeliveryJobConclusionSuccess)},
			},
			want: first,
		},
		{
			name: "no listed job succeeded, both started together",
			jobs: []DeliveryRunJob{
				{Name: second, StartedAt: new(base), CompletedAt: new(base.Add(time.Hour)), Conclusion: new(DeliveryJobConclusionFailure)},
				{Name: first, StartedAt: new(base), CompletedAt: new(base.Add(2 * time.Hour)), Conclusion: new(DeliveryJobConclusionFailure)},
				{Name: "build", StartedAt: new(base), CompletedAt: new(base.Add(time.Minute)), Conclusion: new(DeliveryJobConclusionSuccess)},
			},
			want: first,
		},
		{
			name: "no listed job succeeded, one never started",
			jobs: []DeliveryRunJob{
				{Name: first, Conclusion: new(DeliveryJobConclusionSkipped)},
				{Name: second, StartedAt: new(base), CompletedAt: new(base.Add(time.Hour)), Conclusion: new(DeliveryJobConclusionCancelled)},
			},
			want: second,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ordertest.EveryOrder(test.jobs, func(listed []DeliveryRunJob) {
				if got := production.Job(listed); got == nil || got.Name != test.want {
					t.Fatalf("Production.Job of jobs listed %q = %+v, want %s", jobNames(listed), got, test.want)
				}
			})
		})
	}
}

func TestFirstShippingApply(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mergedAt := base

	t.Run("created one second after the merge counts", func(t *testing.T) {
		applies := []Apply{
			{RunID: 1, CompletedAt: base.Add(time.Hour), CreatedAt: mergedAt.Add(time.Second)},
		}
		got := FirstShippingApply(mergedAt, applies)
		if got == nil || got.RunID != 1 {
			t.Fatalf("expected the apply created after the merge to qualify, got %+v", got)
		}
	})

	t.Run("created in the merge's own instant does not count (>)", func(t *testing.T) {
		applies := []Apply{
			{RunID: 1, CompletedAt: base.Add(time.Hour), CreatedAt: mergedAt},
		}
		got := FirstShippingApply(mergedAt, applies)
		if got != nil {
			t.Fatalf("expected no qualifying apply at the exclusive boundary, got %+v", got)
		}
	})

	t.Run("earliest completing wins, not earliest creation", func(t *testing.T) {
		applies := []Apply{
			{RunID: 1, CompletedAt: base.Add(3 * time.Hour), CreatedAt: mergedAt.Add(time.Second)},
			{RunID: 2, CompletedAt: base.Add(1 * time.Hour), CreatedAt: mergedAt.Add(2 * time.Hour)},
		}
		got := FirstShippingApply(mergedAt, applies)
		if got == nil || got.RunID != 2 {
			t.Fatalf("expected earliest-completing apply (run 2), got %+v", got)
		}
	})

	t.Run("no qualifying apply returns nil", func(t *testing.T) {
		applies := []Apply{
			{RunID: 1, CompletedAt: base, CreatedAt: mergedAt.Add(-time.Hour)},
		}
		got := FirstShippingApply(mergedAt, applies)
		if got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})
}

// TestAppliesCompletingTogetherShipTheSameInEveryOrder: two production applies whose jobs completed
// in the same second are ordered by run id, and the lower run id ships a pull request both could,
// in every order the runs are listed in and every order the applies are handed over in. The live
// hub narrows the runs it read as a preset window slides, so the input changes length and order
// with no stored row changed; a pull request's deploy_run and a run's prs must not change with it.
func TestAppliesCompletingTogetherShipTheSameInEveryOrder(t *testing.T) {
	const jobName = "production / production-apply"
	merged := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	completed := merged.Add(30 * time.Minute)
	success := DeliveryRunConclusionSuccess
	runs := []DeliveryRun{
		{RunID: 9, CreatedAt: new(merged.Add(time.Minute)), StartedAt: merged.Add(time.Minute), Conclusion: &success},
		{RunID: 7, CreatedAt: new(merged.Add(2 * time.Minute)), StartedAt: merged.Add(2 * time.Minute), Conclusion: &success},
		{RunID: 8, CreatedAt: new(merged.Add(3 * time.Minute)), StartedAt: merged.Add(3 * time.Minute), Conclusion: &success},
	}
	jobsByRun := map[int64][]DeliveryRunJob{
		9: {{Name: jobName, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(completed)}},
		7: {{Name: jobName, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(completed)}},
		8: {{Name: jobName, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(completed.Add(time.Second))}},
	}
	production := Production{JobNames: []string{jobName}}
	runIDs := func(applies []Apply) []int64 {
		ids := make([]int64, len(applies))
		for i, apply := range applies {
			ids[i] = apply.RunID
		}
		return ids
	}
	t.Run("ProductionApplies", func(t *testing.T) {
		ordertest.EveryOrder(runs, func(listed []DeliveryRun) {
			order := make([]int64, len(listed))
			for i, run := range listed {
				order[i] = run.RunID
			}
			if got := runIDs(ProductionApplies(listed, jobsByRun, production).Applies); !slices.Equal(got, []int64{7, 9, 8}) {
				t.Fatalf("ProductionApplies of runs listed %v = %v, want [7 9 8]: by completion, then run id", order, got)
			}
		})
	})
	t.Run("FirstShippingApply", func(t *testing.T) {
		applies := []Apply{
			{RunID: 9, CompletedAt: completed, CreatedAt: merged.Add(time.Minute)},
			{RunID: 7, CompletedAt: completed, CreatedAt: merged.Add(2 * time.Minute)},
			{RunID: 8, CompletedAt: completed.Add(time.Second), CreatedAt: merged.Add(3 * time.Minute)},
		}
		ordertest.EveryOrder(applies, func(listed []Apply) {
			if got := FirstShippingApply(merged, listed); got == nil || got.RunID != 7 {
				t.Fatalf("FirstShippingApply of applies listed %v = %+v, want run 7, the lower run id of the two completing first", runIDs(listed), got)
			}
		})
	})
}

// TestProductionAppliesReadsTheRunsCreation: an apply carries the run's creation (Created), never
// its head commit's time, which a merge queue writes before the merge it carries is recorded, and
// a row stored before created_at reads its start in its place. Run 1 is the merge queue's: its
// head commit is timed ten minutes before the merge, and GitHub created it two minutes after, so
// it ships the pull request the old head-commit rule handed to run 2. Run 3 is a re-run whose
// start moved past the merge while its creation, and its head, came before it.
func TestProductionAppliesReadsTheRunsCreation(t *testing.T) {
	const jobName = "widgets-release / widgets-release"
	merged := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	success := DeliveryRunConclusionSuccess
	runs := []DeliveryRun{
		{RunID: 1, HeadCommitAt: merged.Add(-10 * time.Minute), CreatedAt: new(merged.Add(2 * time.Minute)), StartedAt: merged.Add(2 * time.Minute), Conclusion: &success},
		{RunID: 2, HeadCommitAt: merged.Add(20 * time.Minute), StartedAt: merged.Add(25 * time.Minute), Conclusion: &success},
		{RunID: 3, HeadCommitAt: merged.Add(-40 * time.Minute), CreatedAt: new(merged.Add(-30 * time.Minute)), StartedAt: merged.Add(15 * time.Minute), Conclusion: &success},
	}
	jobsByRun := map[int64][]DeliveryRunJob{}
	for i, run := range runs {
		jobsByRun[run.RunID] = []DeliveryRunJob{{Name: jobName, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(merged.Add(time.Duration(30+10*i) * time.Minute))}}
	}
	judged := ProductionApplies(runs, jobsByRun, Production{JobNames: []string{jobName}})
	created := map[int64]time.Time{}
	for _, apply := range judged.Applies {
		created[apply.RunID] = apply.CreatedAt
	}
	if !created[1].Equal(merged.Add(2*time.Minute)) || !created[2].Equal(merged.Add(25*time.Minute)) || !created[3].Equal(merged.Add(-30*time.Minute)) {
		t.Fatalf("applies' creation = %v, want run 1's created_at, run 2's start (no created_at), run 3's created_at", created)
	}
	if got := FirstShippingApply(merged, judged.Applies); got == nil || got.RunID != 1 {
		t.Fatalf("FirstShippingApply = %+v, want the merge queue's run 1", got)
	}
}

func TestContainingRun(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mergedAt := base
	applies := []Apply{
		{RunID: 1, CompletedAt: base.Add(time.Hour), CreatedAt: mergedAt.Add(time.Second)},
	}

	t.Run("outside deploy repo is always nil", func(t *testing.T) {
		pr := DeliveryPullRequest{Repo: "acme/other", MergedAt: new(mergedAt)}
		got := ContainingRun(pr, "acme/widgets", applies)
		if got != nil {
			t.Fatalf("expected nil for PR outside deploy repo, got %+v", got)
		}
	})

	t.Run("inside deploy repo delegates to FirstShippingApply", func(t *testing.T) {
		pr := DeliveryPullRequest{Repo: "acme/widgets", MergedAt: new(mergedAt)}
		got := ContainingRun(pr, "acme/widgets", applies)
		if got == nil || got.RunID != 1 {
			t.Fatalf("expected apply for run 1, got %+v", got)
		}
	})
}

func TestComputeDeployedStatus(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	apply := &Apply{RunID: 1, CompletedAt: base, CreatedAt: base}

	t.Run("deployed in deploy repo", func(t *testing.T) {
		pr := DeliveryPullRequest{Repo: "acme/widgets"}
		if got := ComputeDeployedStatus(pr, "acme/widgets", apply); got != DeployedStatusDeployed {
			t.Errorf("got %q, want deployed", got)
		}
	})

	t.Run("waiting in deploy repo with nil apply", func(t *testing.T) {
		pr := DeliveryPullRequest{Repo: "acme/widgets"}
		if got := ComputeDeployedStatus(pr, "acme/widgets", nil); got != DeployedStatusWaiting {
			t.Errorf("got %q, want waiting", got)
		}
	})

	t.Run("not_tracked outside deploy repo even with nil apply", func(t *testing.T) {
		pr := DeliveryPullRequest{Repo: "acme/other"}
		if got := ComputeDeployedStatus(pr, "acme/widgets", nil); got != DeployedStatusNotTracked {
			t.Errorf("got %q, want not_tracked (repo check independent of apply nilness)", got)
		}
	})
}
