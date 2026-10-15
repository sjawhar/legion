package api

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// deliveryPipelineResponse is GET /api/v1/delivery/pipeline: the Pipeline page's seven UTC days
// ending on `at`, the PR-checks workflow's run timing with its slowest and recently failing jobs,
// the deploy workflow's staging end-to-end, staging deploy and production deploy jobs (the first
// two null when the settings name none), the merge-to-production and open-to-merge lead times per
// merge day, the targets the two lead-time cards are judged against, and the timeline's freshness.
// Mirrors packages/contracts/src/dispatch-api.ts's DeliveryPipelineResponse exactly. It lives here
// beside deliveryMeasuresResponse, since the measures package imports delivery.
type deliveryPipelineResponse struct {
	Days              []string                       `json:"days"`
	ComputedAt        time.Time                      `json:"computed_at"`
	PRChecks          deliveryPipelinePRChecks       `json:"pr_checks"`
	StagingE2E        *measures.DeployJobTiming      `json:"staging_e2e"`
	StagingApply      *measures.DeployJobTiming      `json:"staging_apply"`
	ProductionApply   measures.DeployJobTiming       `json:"production_apply"`
	MergeToProduction measures.LeadTimeTiming        `json:"merge_to_production"`
	OpenedToMerge     measures.LeadTimeTiming        `json:"opened_to_merge"`
	Targets           measures.Targets               `json:"targets"`
	Freshness         delivery.DeliveryFreshnessView `json:"freshness"`
}

const (
	pipelineProductionApplyCard measures.DeployJobCardID = "production_apply"
	pipelineStagingE2ECard      measures.DeployJobCardID = "staging_e2e"
	pipelineStagingApplyCard    measures.DeployJobCardID = "staging_apply"
)

// deliveryPipelinePRChecks is the PR-checks workflow's part of the Pipeline page: its whole runs'
// timing, its slowest jobs and the jobs that failed in the last 24 hours, both lists never null.
type deliveryPipelinePRChecks struct {
	Run         measures.Timing       `json:"run"`
	SlowestJobs []measures.JobTiming  `json:"slowest_jobs"`
	FailingJobs []measures.FailingJob `json:"failing_jobs"`
}

// getDeliveryPipeline answers GET /api/v1/delivery/pipeline, computed from stored facts on
// request. Its clock is `at` (RFC3339, default now): the seven shown days end on at's UTC day, and
// the failing-jobs table looks back 24 hours from at, so a test or a screenshot pins "today" to a
// fixture's day. Runs are read by start over the whole of the shown days, today included past at.
// The PR checks are the pull_request runs; the deploy jobs are read from the push runs on main
// (decision 6); the lead times are every population pull request merged in the shown days,
// before any facet, since the Pipeline page follows neither the timeline's window nor its facets.
func (s *server) getDeliveryPipeline(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	at, err := parseOptionalRFC3339(r.URL.Query(), "at", time.Now().UTC())
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	at = at.UTC()
	settings, ok := s.deliverySettings(w, r)
	if !ok {
		return
	}
	population, err := s.readPipelinePopulation(r.Context(), settings, at)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, deliveryPipelineCompute(settings, population, at, time.Now().UTC()))
}

// pipelinePopulation is every read the Pipeline page's answer for one `at` makes: the PR-checks
// pull_request runs and the deploy push runs on main started in the shown days, each with its
// jobs, and the population pull requests merged in them (their lead times).
type pipelinePopulation struct {
	checks, deploys delivery.RunsWithJobs
	leadTimes       deliveryPopulation
}

// readPipelinePopulation reads the Pipeline page's runs and pull requests for the shown days
// ending on at's UTC day. Runs are read by start over the whole of the shown days, today included
// past at.
func (s *server) readPipelinePopulation(ctx context.Context, settings delivery.DeliverySettings, at time.Time) (pipelinePopulation, error) {
	pool := s.deps.Store.Pool
	from := measures.WindowStart(at)
	to := from.AddDate(0, 0, measures.ShownDayCount)

	// The two run reads are independent, so they run side by side, each on a hold mark of its own
	// (store.ForConcurrentRead), as readDeliveryPopulation runs its reads.
	var pop pipelinePopulation
	reads, readsCtx := errgroup.WithContext(ctx)
	for _, read := range []struct {
		into                *delivery.RunsWithJobs
		kind                delivery.DeliveryRunKind
		branch, githubEvent string
	}{
		{&pop.checks, delivery.DeliveryRunKindPRChecks, "", "pull_request"},
		{&pop.deploys, delivery.DeliveryRunKindDeploy, "main", "push"},
	} {
		readCtx, err := store.ForConcurrentRead(readsCtx)
		if err != nil {
			_ = reads.Wait()
			return pipelinePopulation{}, err
		}
		reads.Go(func() (err error) {
			*read.into, err = delivery.ListRunsWithJobsStartedIn(readCtx, pool, settings.DeployRepo, read.kind, read.branch, read.githubEvent, from, to)
			return err
		})
	}
	if err := reads.Wait(); err != nil {
		return pipelinePopulation{}, err
	}
	var err error
	if pop.leadTimes, err = s.readDeliveryRows(ctx, settings, from, to, url.Values{}, false, false); err != nil {
		return pipelinePopulation{}, err
	}
	return pop, nil
}

// deliveryPipelineCompute is GET /api/v1/delivery/pipeline's body as of at, from a population read
// for at or narrowed to it. It reads nothing; now is only computed_at.
func deliveryPipelineCompute(settings delivery.DeliverySettings, pop pipelinePopulation, at, now time.Time) deliveryPipelineResponse {
	days := measures.ShownDays(at)
	rollup := ""
	if settings.PRChecksRollupJobName != nil {
		rollup = *settings.PRChecksRollupJobName
	}
	run, slowest, failing := measures.PRChecks(pop.checks.Runs, pop.checks.Jobs, days, at, rollup)
	cards := []measures.DeployJobCard{{
		CardID:     pipelineProductionApplyCard,
		Production: settings.Production(),
	}}
	for _, optional := range []struct {
		cardID measures.DeployJobCardID
		name   *string
	}{
		{pipelineStagingE2ECard, settings.StagingE2EJobName},
		{pipelineStagingApplyCard, settings.StagingApplyJobName},
	} {
		if optional.name == nil {
			continue
		}
		cards = append(cards, measures.DeployJobCard{
			CardID:     optional.cardID,
			Production: delivery.Production{JobNames: []string{*optional.name}},
		})
	}
	timings := measures.DeployJobs(pop.deploys.Runs, pop.deploys.Jobs, cards, days)
	stagingE2E := optionalDeployJobTiming(timings, pipelineStagingE2ECard)
	stagingApply := optionalDeployJobTiming(timings, pipelineStagingApplyCard)
	derived := pop.leadTimes.derive(settings)
	mergeToProduction, openedToMerge := measures.LeadTimes(derived.measuresPRs(), days)
	return deliveryPipelineResponse{
		Days:              days,
		ComputedAt:        now,
		PRChecks:          deliveryPipelinePRChecks{Run: run, SlowestJobs: slowest, FailingJobs: failing},
		StagingE2E:        stagingE2E,
		StagingApply:      stagingApply,
		ProductionApply:   timings[pipelineProductionApplyCard],
		MergeToProduction: mergeToProduction,
		OpenedToMerge:     openedToMerge,
		Targets:           measures.DefaultTargets,
		Freshness:         derived.freshness,
	}
}

func optionalDeployJobTiming(timings map[measures.DeployJobCardID]measures.DeployJobTiming, card measures.DeployJobCardID) *measures.DeployJobTiming {
	timing, exists := timings[card]
	if !exists {
		return nil
	}
	return &timing
}

// narrowedTo is the population for a later `at`: when at's UTC day moved, the runs started before
// the new first shown day leave and the lead times' pull requests narrow to it; the failing-jobs
// table needs nothing taken out, since deliveryPipelineCompute reads its 24 hours from at.
func (p pipelinePopulation) narrowedTo(settings delivery.DeliverySettings, at time.Time) pipelinePopulation {
	start := measures.WindowStart(at)
	startedInShownDays := func(run delivery.DeliveryRun) bool { return !run.StartedAt.Before(start) }
	narrowed := pipelinePopulation{leadTimes: p.leadTimes.narrowedTo(settings, start)}
	narrowed.checks.Runs, narrowed.checks.Jobs = runsWhere(p.checks.Runs, p.checks.Jobs, startedInShownDays)
	narrowed.deploys.Runs, narrowed.deploys.Jobs = runsWhere(p.deploys.Runs, p.deploys.Jobs, startedInShownDays)
	return narrowed
}

// pipelineNextExpiry is the next UTC midnight (measures.ShownDays, pipeline.go:142) or the earliest
// failing-jobs edge, whichever comes first: just past a timed PR-checks run's completion +
// measures.FailingWindow (pipeline.go:33), when it leaves the 24 hours the table counts, or a timed
// run's completion itself when that is after now (a clock ahead of Dispatch's), when it enters them.
func pipelineNextExpiry(pop pipelinePopulation, now time.Time) time.Time {
	next := nextUTCMidnight(now)
	consider := func(at time.Time) {
		if at.After(now) && at.Before(next) {
			next = at
		}
	}
	for _, run := range pop.checks.Runs {
		outcome, ok := measures.OutcomeOf(run.Conclusion)
		if !ok || (outcome != measures.OutcomeSuccess && outcome != measures.OutcomeFailure) || run.CompletedAt == nil {
			continue
		}
		consider(*run.CompletedAt)
		consider(run.CompletedAt.Add(measures.FailingWindow).Add(time.Nanosecond))
	}
	return next
}
