package delivery

import (
	"fmt"
	"strings"
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

	applies := ProductionApplies(runs, jobsByRun, jobName)
	if len(applies) != 1 {
		t.Fatalf("expected 1 apply, got %d", len(applies))
	}
	if applies[0].RunID != 3 {
		t.Errorf("expected apply for run 3, got run %d", applies[0].RunID)
	}
}

// TestProductionAppliesWarnsOnlyForARunOnMain: a successful run with no job named
// production_job_name is logged only on main, where a missing production job means the
// configured name is wrong. A run on another branch can skip its production job (a dev-only run,
// whose skipped job GitHub names after the calling job alone), and a run whose branch is not yet
// recorded is not known to be on main, so neither is logged. None of the three is a deploy.
func TestProductionAppliesWarnsOnlyForARunOnMain(t *testing.T) {
	const jobName = "production-apply / production-apply"
	logs := captureLogs(t)
	success := DeliveryRunConclusionSuccess
	runs := []DeliveryRun{
		{Repo: "acme/widgets", RunID: 1, HeadBranch: new("test/dev-only"), Conclusion: &success},
		{Repo: "acme/widgets", RunID: 2, Conclusion: &success},
		{Repo: "acme/widgets", RunID: 3, HeadBranch: new("main"), Conclusion: &success},
	}
	skipped := map[int64][]DeliveryRunJob{}
	for _, run := range runs {
		skipped[run.RunID] = []DeliveryRunJob{{Repo: run.Repo, RunID: run.RunID, Name: "production-apply", Conclusion: new(DeliveryJobConclusionSkipped)}}
	}
	if applies := ProductionApplies(runs, skipped, jobName); len(applies) != 0 {
		t.Fatalf("applies = %+v, want none: a skipped production job is no deploy", applies)
	}
	var warned []any
	for _, record := range logs() {
		if record["level"] == "WARN" && strings.Contains(fmt.Sprint(record["msg"]), "no job matching the configured production_job_name") {
			warned = append(warned, record["run_id"])
		}
	}
	if len(warned) != 1 || warned[0] != float64(3) {
		t.Fatalf("warned for runs %v, want only run 3 (on main)", warned)
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
