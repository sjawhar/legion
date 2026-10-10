package delivery

import (
	"testing"
	"time"
)

func TestIsSummaryOrGuardJob(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"verdict", true},
		{"Verdict", true},
		{"ci-verdict", true},
		{"weekly-review / page-oncall", true},
		{"Notify-Slack", true},
		{"flow / build-recovered", true},
		{"flow / rerun-refusal", true},
		{"notify", false},             // no trailing hyphen: the prototype's rule is a prefix match on "notify-"
		{"ci-verdict-summary", false}, // contains "verdict" but does not end with it -- a suffix match, not substring
		{"build", false},
		{"widgets-release / widgets-release", false},
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
		jobs := []DeliveryRunJob{
			{Name: "build", Conclusion: new(DeliveryJobConclusionFailure), CompletedAt: new(base.Add(2 * time.Hour))},
			{Name: "lint", Conclusion: new(DeliveryJobConclusionFailure), CompletedAt: new(base.Add(1 * time.Hour))},
			{Name: "verdict", Conclusion: new(DeliveryJobConclusionFailure), CompletedAt: new(base)},
		}
		got := RootFailingJob(jobs)
		if got == nil || got.Name != "lint" {
			t.Fatalf("expected lint (earliest non-guard), got %+v", got)
		}
	})

	t.Run("falls back to the earliest-finishing guard job when every failed job is a guard job", func(t *testing.T) {
		jobs := []DeliveryRunJob{
			{Name: "Notify-Slack", Conclusion: new(DeliveryJobConclusionFailure), CompletedAt: new(base.Add(time.Hour))},
			{Name: "verdict", Conclusion: new(DeliveryJobConclusionFailure), CompletedAt: new(base)},
		}
		got := RootFailingJob(jobs)
		if got == nil || got.Name != "verdict" {
			t.Fatalf("expected verdict (earliest guard job), got %+v", got)
		}
	})

	t.Run("returns nil for a run with no failed job", func(t *testing.T) {
		jobs := []DeliveryRunJob{
			{Name: "build", Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base)},
		}
		if got := RootFailingJob(jobs); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("ignores non-failed jobs' timing entirely", func(t *testing.T) {
		jobs := []DeliveryRunJob{
			{Name: "quick-success", Conclusion: new(DeliveryJobConclusionSuccess), CompletedAt: new(base)},
			{Name: "slow-failure", Conclusion: new(DeliveryJobConclusionFailure), CompletedAt: new(base.Add(time.Hour))},
		}
		got := RootFailingJob(jobs)
		if got == nil || got.Name != "slow-failure" {
			t.Fatalf("expected slow-failure (the only failed job), got %+v", got)
		}
	})
}
