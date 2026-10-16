// Pipeline-failure detection (CONTRACT.md's dataset schema section on runs[].failed_jobs and
// runs[].root_failing_job): a failed run's root-cause job is the earliest-finishing failed job
// that isn't a summary/guard job such as a verdict or notify step, falling back to the earliest
// failed job overall.
package delivery

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

// compareRunJobs is the order a run's jobs are read in (ListRunJobs, ListRunJobsForRuns): by when
// each started, a job that never started last, then by name in byte order. A run never repeats a
// name, so no two of its jobs compare equal, and the order is the run's whatever order GitHub listed
// them in or a table scan answered them in. GET /api/v1/delivery/runs/{id} lists a run's jobs in it.
func compareRunJobs(a, b DeliveryRunJob) int {
	return compareOptionalTime(a.StartedAt, b.StartedAt, a.Name, b.Name)
}

// compareOptionalTime orders by aTime and bTime, a nil time last, then by aName and bName in byte
// order: the one rule a run's jobs are ordered by, whether by start or by finish.
func compareOptionalTime(aTime, bTime *time.Time, aName, bName string) int {
	switch {
	case aTime == nil && bTime != nil:
		return 1
	case aTime != nil && bTime == nil:
		return -1
	case aTime != nil && !aTime.Equal(*bTime):
		return aTime.Compare(*bTime)
	}
	return cmp.Compare(aName, bName)
}

// FailedJobs is every job in jobs with Conclusion == DeliveryJobConclusionFailure, in
// compareRunJobs' order whatever order jobs holds them in.
func FailedJobs(jobs []DeliveryRunJob) []DeliveryRunJob {
	var failed []DeliveryRunJob
	for _, job := range jobs {
		if job.Conclusion != nil && *job.Conclusion == DeliveryJobConclusionFailure {
			failed = append(failed, job)
		}
	}
	slices.SortFunc(failed, compareRunJobs)
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
// the first by name among jobs that finished together or never did, or nil when jobs is empty.
func earliestFinishing(jobs []DeliveryRunJob) *DeliveryRunJob {
	var best *DeliveryRunJob
	for i := range jobs {
		if best == nil || finishesBefore(jobs[i], *best) {
			best = &jobs[i]
		}
	}
	return best
}

// finishesBefore reports whether a finished before b (nil CompletedAt last), or, finishing
// together or neither finished, whether a's name comes first.
func finishesBefore(a, b DeliveryRunJob) bool {
	return compareOptionalTime(a.CompletedAt, b.CompletedAt, a.Name, b.Name) < 0
}

// RootFailingJob is the earliest-finishing (by CompletedAt; nil CompletedAt sorts last, ties by
// name) job among FailedJobs(jobs) that is not IsSummaryOrGuardJob, or, if every failed job is a
// guard job, the earliest-finishing failed job overall. Returns nil when jobs has no failed job at
// all.
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
