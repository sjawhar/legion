// Deploy containment (CONTRACT.md "Sources" > "Containment", ported from the delivery-timeline
// prototype's collector/src/collector/containment.py). A population pull request in the
// configured deploy repository ships with the earliest-completing successful production deploy
// whose run GitHub created after the PR's merge (DeliveryRun.Created). Creation, not the head
// commit's timestamp, decides it: a repository that merges through GitHub's merge queue writes the
// merge commit before GitHub records the merge, so the run that shipped a pull request can have a
// head commit timed before its merged_at, while a push run is created only once its push has
// landed. Unlike the prototype, which matches a job name against a set of alternate spellings of
// one job (APPLY_JOBS), this schema configures one exact job name
// (delivery_settings.production_job_name), so matching is a direct equality check against one
// job's name within a run rather than set membership.
package delivery

import (
	"sort"
	"strings"
	"time"
)

// Apply is one successful production deploy: the run that shipped it, when it completed, and when
// GitHub created the run (DeliveryRun.Created: what FirstShippingApply compares a PR's merge time
// against).
type Apply struct {
	RunID       int64
	CompletedAt time.Time
	CreatedAt   time.Time
}

// UnmatchedRun is a successfully concluded run that holds no production job and is not shown to
// have skipped it, with the name of every job it holds: a misconfigured or renamed
// production_job_name otherwise produces zero deploys forever, silently, so each caller warns
// about it, naming every job the run actually has (the Python prototype's own
// `successful_applies` raises for this case).
type UnmatchedRun struct {
	Run      DeliveryRun
	JobNames []string
}

// ProductionApplies is every successful production deploy among runs, and every successfully
// concluded run among them that holds no production job. For each run, the job named
// settings.production_job_name among jobsByRun[run.RunID] must exist, have concluded with
// Conclusion == DeliveryJobConclusionSuccess, and have a non-nil CompletedAt; a run with no such
// job, or whose matching job did not succeed, is not an apply. A run that concluded successfully
// overall but holds no job by that name is an UnmatchedRun, unless it skipped its production job
// (skippedCaller) and the name is shown to be a real job's: some run of runs holds a job by
// exactly that name. Without that, a typo whose first part happens to name a job a run skipped
// (`staging-apply / production-apply` beside a skipped `staging-apply`) would read as a skipped
// call in every run and never be named. The cost is that a listing in which no run holds the
// production job, such as a stretch with no production deploy, sets apart each of its successful
// runs. Applies are sorted by CompletedAt ascending; unmatched runs keep runs' order.
func ProductionApplies(runs []DeliveryRun, jobsByRun map[int64][]DeliveryRunJob, productionJobName string) ([]Apply, []UnmatchedRun) {
	var applies []Apply
	type missing struct {
		run     DeliveryRun
		skipped bool
	}
	var missed []missing
	nameIsReal := false
	for _, run := range runs {
		var match *DeliveryRunJob
		skipped := false
		for i, job := range jobsByRun[run.RunID] {
			if job.Name == productionJobName {
				match = &jobsByRun[run.RunID][i]
				break
			}
			skipped = skipped || skippedCaller(job, productionJobName)
		}
		if match == nil {
			if run.Conclusion != nil && *run.Conclusion == DeliveryRunConclusionSuccess {
				missed = append(missed, missing{run: run, skipped: skipped})
			}
			continue
		}
		nameIsReal = true
		if match.Conclusion == nil || *match.Conclusion != DeliveryJobConclusionSuccess {
			continue
		}
		if match.CompletedAt == nil {
			continue
		}
		applies = append(applies, Apply{
			RunID:       run.RunID,
			CompletedAt: *match.CompletedAt,
			CreatedAt:   run.Created(),
		})
	}

	// A run of the listing holding the configured job shows the name is real, so a run that
	// skipped its caller skipped that job rather than matching a typo by chance.
	var unmatched []UnmatchedRun
	for _, m := range missed {
		if m.skipped && nameIsReal {
			continue
		}
		names := make([]string, 0, len(jobsByRun[m.run.RunID]))
		for _, job := range jobsByRun[m.run.RunID] {
			names = append(names, job.Name)
		}
		unmatched = append(unmatched, UnmatchedRun{Run: m.run, JobNames: names})
	}

	sort.Slice(applies, func(i, j int) bool {
		return applies[i].CompletedAt.Before(applies[j].CompletedAt)
	})
	return applies, unmatched
}

// skippedCaller reports whether job could be a skipped call of a reusable workflow that holds the
// production job. GitHub names a reusable workflow's job `<caller> / <job>`, but when it skips the
// calling job it runs none of the called workflow's jobs and lists the call under its caller's
// name alone. So a production job named `production-apply / production-apply` appears in a run
// that skipped it as a skipped `production-apply`, and one nested deeper,
// `deploy / production / apply`, as a skipped `deploy / production` (or `deploy`, when the outer
// call was skipped). The name alone cannot tell such a call from an unrelated skipped job that
// shares the first part of a typo, which is why ProductionApplies also asks that some run hold
// the configured job.
func skippedCaller(job DeliveryRunJob, productionJobName string) bool {
	return job.Conclusion != nil && *job.Conclusion == DeliveryJobConclusionSkipped &&
		strings.HasPrefix(productionJobName, job.Name+" / ")
}

// FirstShippingApply is the earliest-completing apply (by CompletedAt) among applies GitHub created
// strictly after mergedAt, or nil if none qualifies. The boundary is exclusive: GitHub times both
// to the second, and a run created in the merge's own second can be an earlier push's, whose head
// does not hold the merge.
func FirstShippingApply(mergedAt time.Time, applies []Apply) *Apply {
	var best *Apply
	for i, apply := range applies {
		if !apply.CreatedAt.After(mergedAt) {
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
