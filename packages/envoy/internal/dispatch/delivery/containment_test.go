package delivery

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"
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

	applies, _ := ProductionApplies(runs, jobsByRun, jobName)
	if len(applies) != 1 {
		t.Fatalf("expected 1 apply, got %d", len(applies))
	}
	if applies[0].RunID != 3 {
		t.Errorf("expected apply for run 3, got run %d", applies[0].RunID)
	}
}

// TestProductionAppliesSetsApartOnlyARunWithNoProductionJob: a successfully concluded run holding
// no job by the configured production job name is set apart for its callers' warning, unless the
// production job was skipped, which GitHub lists under the name of the calling job it skipped.
// testdata/dev-only-run-jobs.json is GitHub's job list for a main run of a deploy workflow whose
// dispatch applied dev alone: its production job, a reusable workflow's
// (`production-apply / production-apply`), is skipped and listed as the bare `production-apply`.
// It is decoded and mapped exactly as reconcileRun stores a run's jobs.
func TestProductionAppliesSetsApartOnlyARunWithNoProductionJob(t *testing.T) {
	const jobName = "production-apply / production-apply"
	data, err := os.ReadFile("testdata/dev-only-run-jobs.json")
	if err != nil {
		t.Fatalf("read the recorded job list: %v", err)
	}
	var payload jobsPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode the recorded job list: %v", err)
	}
	devOnly := make([]DeliveryRunJob, len(payload.Jobs))
	for i, job := range payload.Jobs {
		devOnly[i] = DeliveryRunJob{Name: job.Name, StartedAt: job.StartedAt, CompletedAt: job.CompletedAt, Conclusion: mapJobConclusion(job.Conclusion)}
	}

	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	success := DeliveryRunConclusionSuccess
	runs := make([]DeliveryRun, 5)
	for i := range runs {
		runs[i] = DeliveryRun{Repo: "acme/widgets", RunID: int64(i + 1), HeadCommitAt: base.Add(time.Duration(i) * time.Hour), Conclusion: &success}
	}
	jobsByRun := map[int64][]DeliveryRunJob{
		// The dev-only run: its production job skipped under the caller's name. No deploy, no warning.
		1: devOnly,
		// A renamed job: neither spelling of the configured one.
		2: {{Name: "prod-apply / prod-apply", Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base)}},
		// The caller's name, but a job that ran: the configured name is not the job this run ran.
		3: {{Name: "production-apply", Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base)}},
		// The configured job, succeeded: a deploy.
		4: {{Name: jobName, Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base.Add(4 * time.Hour))}},
		// A skipped job whose name starts the configured one without ending at a " / ".
		5: {{Name: "production", Conclusion: new(DeliveryJobConclusionSkipped)}},
	}

	applies, unmatched := ProductionApplies(runs, jobsByRun, jobName)
	if len(applies) != 1 || applies[0].RunID != 4 {
		t.Fatalf("applies = %+v, want run 4's alone", applies)
	}
	var ids []int64
	for _, run := range unmatched {
		ids = append(ids, run.Run.RunID)
	}
	if !slices.Equal(ids, []int64{2, 3, 5}) {
		t.Fatalf("runs set apart = %v, want [2 3 5]: never the dev-only run 1 nor the deploy 4", ids)
	}
	if !slices.Equal(unmatched[0].JobNames, []string{"prod-apply / prod-apply"}) {
		t.Errorf("run 2's job names = %v, want every job it holds", unmatched[0].JobNames)
	}

	// A nested reusable call GitHub skipped is listed by the names down to the skipped call, as a
	// dispatch-only deploy run lists `dispatch-apply / staging-e2e`.
	nested := map[int64][]DeliveryRunJob{1: {
		{Name: "dispatch-apply / dispatch-apply", Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base)},
		{Name: "dispatch-apply / staging-e2e", Conclusion: new(DeliveryJobConclusionSkipped)},
	}}
	if applies, unmatched := ProductionApplies(runs[:1], nested, "dispatch-apply / staging-e2e / staging-e2e"); len(applies) != 0 || len(unmatched) != 0 {
		t.Fatalf("a nested call skipped: applies = %+v, set apart = %+v, want neither", applies, unmatched)
	}
}

func TestFirstShippingApply(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mergedAt := base

	t.Run("exact boundary counts (>=)", func(t *testing.T) {
		applies := []Apply{
			{RunID: 1, CompletedAt: base.Add(time.Hour), HeadCommitAt: mergedAt},
		}
		got := FirstShippingApply(mergedAt, applies)
		if got == nil || got.RunID != 1 {
			t.Fatalf("expected apply at exact boundary to qualify, got %+v", got)
		}
	})

	t.Run("one microsecond before does not count", func(t *testing.T) {
		applies := []Apply{
			{RunID: 1, CompletedAt: base.Add(time.Hour), HeadCommitAt: mergedAt.Add(-time.Microsecond)},
		}
		got := FirstShippingApply(mergedAt, applies)
		if got != nil {
			t.Fatalf("expected no qualifying apply, got %+v", got)
		}
	})

	t.Run("earliest completing wins, not earliest head commit", func(t *testing.T) {
		applies := []Apply{
			{RunID: 1, CompletedAt: base.Add(3 * time.Hour), HeadCommitAt: mergedAt},
			{RunID: 2, CompletedAt: base.Add(1 * time.Hour), HeadCommitAt: mergedAt.Add(2 * time.Hour)},
		}
		got := FirstShippingApply(mergedAt, applies)
		if got == nil || got.RunID != 2 {
			t.Fatalf("expected earliest-completing apply (run 2), got %+v", got)
		}
	})

	t.Run("no qualifying apply returns nil", func(t *testing.T) {
		applies := []Apply{
			{RunID: 1, CompletedAt: base, HeadCommitAt: mergedAt.Add(-time.Hour)},
		}
		got := FirstShippingApply(mergedAt, applies)
		if got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})
}

func TestContainingRun(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mergedAt := base
	applies := []Apply{
		{RunID: 1, CompletedAt: base.Add(time.Hour), HeadCommitAt: mergedAt},
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
	apply := &Apply{RunID: 1, CompletedAt: base, HeadCommitAt: base}

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
