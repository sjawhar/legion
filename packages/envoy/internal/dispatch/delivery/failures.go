// Pipeline-failure detection (CONTRACT.md's dataset schema section on runs[].failed_jobs and
// runs[].root_failing_job): a failed run's root-cause job is the earliest-finishing failed job
// that isn't a summary/guard job such as a verdict or notify step, falling back to the earliest
// failed job overall.
package delivery

import (
	"strings"
)

// FailedJobs is every job in jobs with Conclusion == DeliveryJobConclusionFailure.
func FailedJobs(jobs []DeliveryRunJob) []DeliveryRunJob {
	var failed []DeliveryRunJob
	for _, job := range jobs {
		if job.Conclusion != nil && *job.Conclusion == DeliveryJobConclusionFailure {
			failed = append(failed, job)
		}
	}
	return failed
}

// IsSummaryOrGuardJob reports whether a job name names a non-root-cause roll-up/notification step
// rather than a genuine failing step -- ported from the prototype's own `is_summary_or_guard_job`
// (~/proto/delivery-timeline/collector/src/collector/failures.py), not the broader "contains"
// check an earlier revision of this function used (which wrongly flagged any job merely
// mentioning "verdict" anywhere in its name, and never recognized "recovered"/"rerun-refusal"/
// "page-" at all). The rule inspects only the job's own last "/"-segment (GitHub displays a
// reusable-workflow job as "<workflow> / <job>"; the guard check runs on <job>, not the whole
// string): a suffix match on "verdict", "recovered" or "rerun-refusal", or a prefix match on
// "notify-" or "page-", case-insensitive.
func IsSummaryOrGuardJob(name string) bool {
	lower := strings.ToLower(name)
	segment := lower
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		segment = strings.TrimSpace(lower[idx+1:])
	}
	for _, suffix := range []string{"verdict", "recovered", "rerun-refusal"} {
		if strings.HasSuffix(segment, suffix) {
			return true
		}
	}
	for _, prefix := range []string{"notify-", "page-"} {
		if strings.HasPrefix(segment, prefix) {
			return true
		}
	}
	return false
}

// earliestFinishing returns the earliest-finishing job among jobs (nil CompletedAt sorts last),
// or nil when jobs is empty.
func earliestFinishing(jobs []DeliveryRunJob) *DeliveryRunJob {
	var best *DeliveryRunJob
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
func RootFailingJob(jobs []DeliveryRunJob) *DeliveryRunJob {
	failed := FailedJobs(jobs)
	if len(failed) == 0 {
		return nil
	}

	var nonGuard []DeliveryRunJob
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
