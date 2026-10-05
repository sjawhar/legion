// Pipeline-failure detection (CONTRACT.md's dataset schema section on runs[].failed_jobs and
// runs[].root_failing_job): a failed run's root-cause job is the earliest-finishing failed job
// that isn't a summary/guard job such as a verdict or notify step, falling back to the earliest
// failed job overall.
package delivery

import (
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// FailedJobs is every job in jobs with Conclusion == model.DeliveryConclusionFailure.
func FailedJobs(jobs []model.DeliveryRunJob) []model.DeliveryRunJob {
	var failed []model.DeliveryRunJob
	for _, job := range jobs {
		if job.Conclusion != nil && *job.Conclusion == model.DeliveryConclusionFailure {
			failed = append(failed, job)
		}
	}
	return failed
}

// IsSummaryOrGuardJob reports whether a job name names a non-root-cause roll-up/notification step
// rather than a genuine failing step: case-insensitively containing "verdict" or "notify" (the two
// guard-job shapes CONTRACT.md names as examples). Exported so the reconcile/API layer and its
// tests can reason about the same rule.
func IsSummaryOrGuardJob(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "verdict") || strings.Contains(lower, "notify")
}

// earliestFinishing returns the earliest-finishing job among jobs (nil CompletedAt sorts last),
// or nil when jobs is empty.
func earliestFinishing(jobs []model.DeliveryRunJob) *model.DeliveryRunJob {
	var best *model.DeliveryRunJob
	for i, job := range jobs {
		if best == nil {
			best = &jobs[i]
			continue
		}
		if job.CompletedAt == nil {
			continue
		}
		if best.CompletedAt == nil || job.CompletedAt.Before(*best.CompletedAt) {
			best = &jobs[i]
		}
	}
	return best
}

// RootFailingJob is the earliest-finishing (by CompletedAt; nil CompletedAt sorts last) job among
// FailedJobs(jobs) that is not IsSummaryOrGuardJob, or, if every failed job is a guard job, the
// earliest-finishing failed job overall. Returns nil when jobs has no failed job at all.
func RootFailingJob(jobs []model.DeliveryRunJob) *model.DeliveryRunJob {
	failed := FailedJobs(jobs)
	if len(failed) == 0 {
		return nil
	}

	var nonGuard []model.DeliveryRunJob
	for _, job := range failed {
		if !IsSummaryOrGuardJob(job.Name) {
			nonGuard = append(nonGuard, job)
		}
	}

	if len(nonGuard) > 0 {
		return earliestFinishing(nonGuard)
	}
	return earliestFinishing(failed)
}
