// Deploy containment (CONTRACT.md "Sources" > "Containment", ported from the delivery-timeline
// prototype's collector/src/collector/containment.py). A population pull request in the
// configured deploy repository ships with the first successful production deploy whose head
// commit timestamp is at or after the PR's merge. Unlike the prototype, which matches a job name
// against a set of alternate spellings of one job (APPLY_JOBS), this schema configures one exact
// job name (delivery_settings.production_job_name), so matching is a direct equality check
// against one job's name within a run rather than set membership.
package delivery

import (
	"log/slog"
	"sort"
	"time"
)

// Apply is one successful production deploy: the run that shipped it, when it completed, and the
// run's head commit timestamp (what FirstShippingApply compares a PR's merge time against).
type Apply struct {
	RunID        int64
	CompletedAt  time.Time
	HeadCommitAt time.Time
}

// ProductionApplies is every successful production deploy among runs: for each run, the job named
// settings.production_job_name among jobsByRun[run.RunID] must exist, have concluded with
// Conclusion == DeliveryJobConclusionSuccess, and have a non-nil CompletedAt; a run with no such
// job, or whose matching job did not succeed, is not an apply. A run that *concluded successfully*
// overall but matched no job by that name is logged loudly (a misconfigured or renamed
// production_job_name silently produces zero deploys forever otherwise -- the Python prototype's
// own `successful_applies` raises for exactly this case, naming every job it actually saw).
// Sorted by CompletedAt ascending.
func ProductionApplies(runs []DeliveryRun, jobsByRun map[int64][]DeliveryRunJob, productionJobName string) []Apply {
	var applies []Apply
	for _, run := range runs {
		var match *DeliveryRunJob
		for i, job := range jobsByRun[run.RunID] {
			if job.Name == productionJobName {
				match = &jobsByRun[run.RunID][i]
				break
			}
		}
		if match == nil {
			if run.Conclusion != nil && *run.Conclusion == DeliveryRunConclusionSuccess {
				names := make([]string, 0, len(jobsByRun[run.RunID]))
				for _, job := range jobsByRun[run.RunID] {
					names = append(names, job.Name)
				}
				slog.Warn("dispatch delivery: a successfully concluded run has no job matching the configured production_job_name -- it will never count as a deploy",
					"repo", run.Repo, "run_id", run.RunID, "configured_job_name", productionJobName, "job_names_seen", names)
			}
			continue
		}
		if match.Conclusion == nil || *match.Conclusion != DeliveryJobConclusionSuccess {
			continue
		}
		if match.CompletedAt == nil {
			continue
		}
		applies = append(applies, Apply{
			RunID:        run.RunID,
			CompletedAt:  *match.CompletedAt,
			HeadCommitAt: run.HeadCommitAt,
		})
	}

	sort.Slice(applies, func(i, j int) bool {
		return applies[i].CompletedAt.Before(applies[j].CompletedAt)
	})
	return applies
}

// FirstShippingApply is the earliest-completing apply (by CompletedAt) among applies whose
// HeadCommitAt is at or after mergedAt (inclusive boundary: >=), or nil if none qualifies.
func FirstShippingApply(mergedAt time.Time, applies []Apply) *Apply {
	var best *Apply
	for i, apply := range applies {
		if apply.HeadCommitAt.Before(mergedAt) {
			continue
		}
		if best == nil || apply.CompletedAt.Before(best.CompletedAt) {
			best = &applies[i]
		}
	}
	return best
}

// ContainingRun is FirstShippingApply, but only for PRs in the configured deploy repository --
// a PR elsewhere has no deploy pipeline tracked at all and is always nil.
func ContainingRun(pr DeliveryPullRequest, deployRepo string, applies []Apply) *Apply {
	if pr.Repo != deployRepo {
		return nil
	}
	if pr.MergedAt == nil {
		return nil
	}
	return FirstShippingApply(*pr.MergedAt, applies)
}

// ComputeDeployedStatus is the PR's deploy state: DeployedStatusNotTracked for any PR outside the
// deploy repository (there is nothing watching it -- never "waiting", which would inflate a count
// that should only ever shrink to zero as deploys catch up); DeployedStatusDeployed when apply is
// non-nil; DeployedStatusWaiting otherwise. Named distinctly from the DeployedStatus type (model.go)
// it returns, since Go does not allow a function and a type to share one identifier.
func ComputeDeployedStatus(pr DeliveryPullRequest, deployRepo string, apply *Apply) DeployedStatus {
	if pr.Repo != deployRepo {
		return DeployedStatusNotTracked
	}
	if apply != nil {
		return DeployedStatusDeployed
	}
	return DeployedStatusWaiting
}
