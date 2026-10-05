package delivery

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestIsSummaryOrGuardJob(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"verdict", true},
		{"Verdict", true},
		{"notify", true},
		{"Notify-Slack", true},
		{"ci-verdict-summary", true},
		{"build", false},
		{"production-apply / production-apply", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSummaryOrGuardJob(tt.name); got != tt.want {
				t.Errorf("IsSummaryOrGuardJob(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestRootFailingJob(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("picks earliest-finishing non-guard failed job over a later one", func(t *testing.T) {
		jobs := []model.DeliveryRunJob{
			{Name: "build", Conclusion: new(model.DeliveryConclusionFailure), CompletedAt: new(base.Add(2 * time.Hour))},
			{Name: "lint", Conclusion: new(model.DeliveryConclusionFailure), CompletedAt: new(base.Add(1 * time.Hour))},
			{Name: "verdict", Conclusion: new(model.DeliveryConclusionFailure), CompletedAt: new(base)},
		}
		got := RootFailingJob(jobs)
		if got == nil || got.Name != "lint" {
			t.Fatalf("expected lint (earliest non-guard), got %+v", got)
		}
	})

	t.Run("falls back to the earliest-finishing guard job when every failed job is a guard job", func(t *testing.T) {
		jobs := []model.DeliveryRunJob{
			{Name: "notify-slack", Conclusion: new(model.DeliveryConclusionFailure), CompletedAt: new(base.Add(time.Hour))},
			{Name: "verdict", Conclusion: new(model.DeliveryConclusionFailure), CompletedAt: new(base)},
		}
		got := RootFailingJob(jobs)
		if got == nil || got.Name != "verdict" {
			t.Fatalf("expected verdict (earliest guard job), got %+v", got)
		}
	})

	t.Run("returns nil for a run with no failed job", func(t *testing.T) {
		jobs := []model.DeliveryRunJob{
			{Name: "build", Conclusion: new(model.DeliveryConclusionSuccess), CompletedAt: new(base)},
		}
		if got := RootFailingJob(jobs); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("ignores non-failed jobs' timing entirely", func(t *testing.T) {
		jobs := []model.DeliveryRunJob{
			{Name: "quick-success", Conclusion: new(model.DeliveryConclusionSuccess), CompletedAt: new(base)},
			{Name: "slow-failure", Conclusion: new(model.DeliveryConclusionFailure), CompletedAt: new(base.Add(time.Hour))},
		}
		got := RootFailingJob(jobs)
		if got == nil || got.Name != "slow-failure" {
			t.Fatalf("expected slow-failure (the only failed job), got %+v", got)
		}
	})
}
