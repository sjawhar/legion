package delivery

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestProductionApplies(t *testing.T) {
	const jobName = "production-apply / production-apply"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	runs := []model.DeliveryRun{
		{Repo: "acme/widgets", RunID: 1, HeadCommitAt: base},
		{Repo: "acme/widgets", RunID: 2, HeadCommitAt: base.Add(time.Hour)},
		{Repo: "acme/widgets", RunID: 3, HeadCommitAt: base.Add(2 * time.Hour)},
	}
	jobsByRun := map[int64][]model.DeliveryRunJob{
		// run 1 has no matching job at all -- skipped
		1: {{Name: "other-job", Conclusion: new(model.DeliveryConclusionSuccess), CompletedAt: new(base)}},
		// run 2's matching job failed -- skipped
		2: {{Name: jobName, Conclusion: new(model.DeliveryConclusionFailure), CompletedAt: new(base.Add(time.Hour))}},
		// run 3's matching job succeeded -- included
		3: {{Name: jobName, Conclusion: new(model.DeliveryConclusionSuccess), CompletedAt: new(base.Add(2 * time.Hour))}},
	}

	applies := ProductionApplies(runs, jobsByRun, jobName)
	if len(applies) != 1 {
		t.Fatalf("expected 1 apply, got %d", len(applies))
	}
	if applies[0].RunID != 3 {
		t.Errorf("expected apply for run 3, got run %d", applies[0].RunID)
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
		pr := model.DeliveryPullRequest{Repo: "acme/other", MergedAt: new(mergedAt)}
		got := ContainingRun(pr, "acme/widgets", applies)
		if got != nil {
			t.Fatalf("expected nil for PR outside deploy repo, got %+v", got)
		}
	})

	t.Run("inside deploy repo delegates to FirstShippingApply", func(t *testing.T) {
		pr := model.DeliveryPullRequest{Repo: "acme/widgets", MergedAt: new(mergedAt)}
		got := ContainingRun(pr, "acme/widgets", applies)
		if got == nil || got.RunID != 1 {
			t.Fatalf("expected apply for run 1, got %+v", got)
		}
	})
}

func TestDeployedStatus(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	apply := &Apply{RunID: 1, CompletedAt: base, HeadCommitAt: base}

	t.Run("deployed in deploy repo", func(t *testing.T) {
		pr := model.DeliveryPullRequest{Repo: "acme/widgets"}
		if got := DeployedStatus(pr, "acme/widgets", apply); got != "deployed" {
			t.Errorf("got %q, want deployed", got)
		}
	})

	t.Run("waiting in deploy repo with nil apply", func(t *testing.T) {
		pr := model.DeliveryPullRequest{Repo: "acme/widgets"}
		if got := DeployedStatus(pr, "acme/widgets", nil); got != "waiting" {
			t.Errorf("got %q, want waiting", got)
		}
	})

	t.Run("not_tracked outside deploy repo even with nil apply", func(t *testing.T) {
		pr := model.DeliveryPullRequest{Repo: "acme/other"}
		if got := DeployedStatus(pr, "acme/widgets", nil); got != "not_tracked" {
			t.Errorf("got %q, want not_tracked (repo check independent of apply nilness)", got)
		}
	})
}
