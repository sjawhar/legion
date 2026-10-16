package delivery

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ordertest"
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

func jobNames(jobs []DeliveryRunJob) []string {
	names := make([]string, len(jobs))
	for i, job := range jobs {
		names[i] = job.Name
	}
	return names
}

// TestAFailedRunReadsTheSameInEveryListingOrder: a run's failed jobs come by when each started (a
// job that never started last, then by name), and its root failing job among jobs that finished
// together is the first by name, in every order its jobs are listed in. GitHub's listing order, and
// the order a store read answers rows tied on its order in, are neither of them the run's: whatever
// a run's jobs show must not move with them, or the live Delivery hub sends the run again.
func TestAFailedRunReadsTheSameInEveryListingOrder(t *testing.T) {
	base := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	t.Run("failed jobs started and finished together", func(t *testing.T) {
		jobs := []DeliveryRunJob{
			{Name: "setup", StartedAt: new(base), CompletedAt: new(base.Add(6 * time.Minute)), Conclusion: new(DeliveryJobConclusionFailure)},
			{Name: "lint", StartedAt: new(base.Add(time.Minute)), CompletedAt: new(base.Add(5 * time.Minute)), Conclusion: new(DeliveryJobConclusionFailure)},
			{Name: "build", StartedAt: new(base.Add(time.Minute)), CompletedAt: new(base.Add(5 * time.Minute)), Conclusion: new(DeliveryJobConclusionFailure)},
			{Name: "verdict", StartedAt: new(base.Add(2 * time.Minute)), CompletedAt: new(base.Add(3 * time.Minute)), Conclusion: new(DeliveryJobConclusionFailure)},
			{Name: "unit", Conclusion: new(DeliveryJobConclusionFailure)},
			{Name: "deploy", StartedAt: new(base), CompletedAt: new(base.Add(time.Minute)), Conclusion: new(DeliveryJobConclusionSuccess)},
		}
		wantFailed := []string{"setup", "build", "lint", "verdict", "unit"}
		ordertest.EveryOrder(jobs, func(listed []DeliveryRunJob) {
			if got := jobNames(FailedJobs(listed)); !slices.Equal(got, wantFailed) {
				t.Fatalf("FailedJobs of jobs listed %q = %q, want %q", jobNames(listed), got, wantFailed)
			}
			if got := RootFailingJob(listed); got == nil || got.Name != "build" {
				t.Fatalf("RootFailingJob of jobs listed %q = %+v, want build, the first by name of the two finishing first", jobNames(listed), got)
			}
		})
	})
	t.Run("no failed job has finished", func(t *testing.T) {
		jobs := []DeliveryRunJob{
			{Name: "integration", Conclusion: new(DeliveryJobConclusionFailure)},
			{Name: "e2e", Conclusion: new(DeliveryJobConclusionFailure)},
			{Name: "smoke", Conclusion: new(DeliveryJobConclusionFailure)},
		}
		ordertest.EveryOrder(jobs, func(listed []DeliveryRunJob) {
			if got := RootFailingJob(listed); got == nil || got.Name != "e2e" {
				t.Fatalf("RootFailingJob of jobs listed %q = %+v, want e2e, the first by name", jobNames(listed), got)
			}
		})
	})
}
