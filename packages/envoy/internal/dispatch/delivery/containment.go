// Deploy containment (CONTRACT.md "Sources" > "Containment", ported from the delivery-timeline
// prototype's collector/src/collector/containment.py). A population pull request in the
// configured deploy repository ships with the first successful production deploy whose head
// commit timestamp is at or after the PR's merge. Unlike the prototype, which matches a job name
// against a set of alternate spellings of one job (APPLY_JOBS), this schema configures one exact
// job name (delivery_settings.production_job_name), so matching is a direct equality check
// against one job's name within a run rather than set membership.
package delivery

import (
	"sort"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
// Conclusion == model.DeliveryConclusionSuccess, and have a non-nil CompletedAt; a run with no such
// job, or whose matching job did not succeed, is not an apply. Sorted by CompletedAt ascending.
func ProductionApplies(runs []model.DeliveryRun, jobsByRun map[int64][]model.DeliveryRunJob, productionJobName string) []Apply {
	var applies []Apply
	for _, run := range runs {
		var match *model.DeliveryRunJob
		for i, job := range jobsByRun[run.RunID] {
			if job.Name == productionJobName {
				match = &jobsByRun[run.RunID][i]
				break
			}
		}
		if match == nil {
			continue
		}
		if match.Conclusion == nil || *match.Conclusion != model.DeliveryConclusionSuccess {
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
func ContainingRun(pr model.DeliveryPullRequest, deployRepo string, applies []Apply) *Apply {
	if pr.Repo != deployRepo {
		return nil
	}
	if pr.MergedAt == nil {
		return nil
	}
	return FirstShippingApply(*pr.MergedAt, applies)
}

// DeployedStatus is the PR's deploy state: "not_tracked" for any PR outside the deploy repository
// (there is nothing watching it -- never "waiting", which would inflate a count that should only
// ever shrink to zero as deploys catch up); "deployed" when apply is non-nil; "waiting" otherwise.
func DeployedStatus(pr model.DeliveryPullRequest, deployRepo string, apply *Apply) string {
	if pr.Repo != deployRepo {
		return "not_tracked"
	}
	if apply != nil {
		return "deployed"
	}
	return "waiting"
}
