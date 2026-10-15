// Deploy containment (CONTRACT.md "Sources" > "Containment", ported from the delivery-timeline
// prototype's collector/src/collector/containment.py). A population pull request in the
// configured deploy repository ships with the earliest-completing successful production deploy
// whose run GitHub created after the PR's merge (DeliveryRun.Created). Creation, not the head
// commit's timestamp, decides it: a repository that merges through GitHub's merge queue writes the
// merge commit before GitHub records the merge, so the run that shipped a pull request can have a
// head commit timed before its merged_at, while a push run is created only once its push has landed.
// Unlike the prototype, which matches alternate spellings of one job (APPLY_JOBS), this schema
// configures a list of exact production job names (delivery_settings.production_job_names).
package delivery

import (
	"slices"
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
// production_job_names setting otherwise produces zero deploys forever, silently, so each caller
// warns about it, naming every job the run actually has (the Python prototype's own
// `successful_applies` raises for this case).
type UnmatchedRun struct {
	Run      DeliveryRun
	JobNames []string
}

// ProductionRuns is ProductionApplies' verdict on a listing of runs: every deploy, sorted by
// CompletedAt; every successfully concluded run that holds no production job and is not shown to have
// skipped it; and every run whose production job succeeded, read for the configured apply step, while
// listing no step of that name (a misnamed step would otherwise count no deploy, silently).
type ProductionRuns struct {
	Applies     []Apply
	Unmatched   []UnmatchedRun
	StepMissing []DeliveryRun
}

// ProductionApplies judges runs by production (Production.Job, Production.Conclusion). A run is a
// deploy when its production job's conclusion as Production.Conclusion reads it is success and the
// job has a CompletedAt. A successful run holding no listed job is unmatched unless it skipped the
// caller of a listed name (skippedCaller) that some run of runs holds exactly: without that, a typo
// whose first part names a job a run skipped would read as a skipped call in every run and never be
// named. The cost is that a listing in which no run holds a listed job, such as a stretch with no
// production deploy, sets apart each of its successful runs. Unmatched runs keep runs' order.
func ProductionApplies(runs []DeliveryRun, jobsByRun map[int64][]DeliveryRunJob, production Production) ProductionRuns {
	var judged ProductionRuns
	type missing struct {
		run        DeliveryRun
		skippedFor []string
	}
	var missed []missing
	real := map[string]bool{}
	for _, run := range runs {
		jobs := jobsByRun[run.RunID]
		match := production.Job(jobs)
		if match == nil {
			if run.Conclusion != nil && *run.Conclusion == DeliveryRunConclusionSuccess {
				m := missing{run: run}
				for _, job := range jobs {
					for _, name := range production.JobNames {
						if skippedCaller(job, name) {
							m.skippedFor = append(m.skippedFor, name)
						}
					}
				}
				missed = append(missed, m)
			}
			continue
		}
		real[match.Name] = true
		if production.ApplyStep != nil && match.Conclusion != nil && *match.Conclusion == DeliveryJobConclusionSuccess &&
			match.ApplyStep != nil && *match.ApplyStep == *production.ApplyStep && match.ApplyStepConclusion == nil {
			judged.StepMissing = append(judged.StepMissing, run)
		}
		conclusion := production.Conclusion(*match)
		if conclusion == nil || *conclusion != DeliveryJobConclusionSuccess || match.CompletedAt == nil {
			continue
		}
		judged.Applies = append(judged.Applies, Apply{RunID: run.RunID, CompletedAt: *match.CompletedAt, CreatedAt: run.Created()})
	}
	for _, m := range missed {
		if slices.ContainsFunc(m.skippedFor, func(name string) bool { return real[name] }) {
			continue
		}
		names := make([]string, 0, len(jobsByRun[m.run.RunID]))
		for _, job := range jobsByRun[m.run.RunID] {
			names = append(names, job.Name)
		}
		judged.Unmatched = append(judged.Unmatched, UnmatchedRun{Run: m.run, JobNames: names})
	}
	sort.Slice(judged.Applies, func(i, j int) bool { return judged.Applies[i].CompletedAt.Before(judged.Applies[j].CompletedAt) })
	return judged
}

// skippedCaller reports whether job could be a skipped call of a reusable workflow that holds a
// listed production job. GitHub names a reusable workflow's job `<caller> / <job>`, but when it
// skips the calling job it runs none of the called workflow's jobs and lists the call under its
// caller's name alone. So a production job named `production-apply / production-apply` appears in
// a run that skipped it as `production-apply`, and one nested deeper, `deploy / production / apply`,
// as `deploy / production` (or `deploy`, when the outer call was skipped). The name alone cannot
// tell such a call from an unrelated skipped job that shares the first part of a typo, which is why
// ProductionApplies also asks that some run hold the listed name.
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
