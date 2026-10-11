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
	response, err := s.deliveryPipelineAt(r.Context(), settings, at)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, response)
}

// deliveryPipelineAt computes the Pipeline page as of at.
func (s *server) deliveryPipelineAt(ctx context.Context, settings delivery.DeliverySettings, at time.Time) (deliveryPipelineResponse, error) {
	pool := s.deps.Store.Pool
	days := measures.ShownDays(at)
	from := measures.WindowStart(at)
	to := from.AddDate(0, 0, measures.ShownDayCount)

	// The two run reads are independent, so they run side by side, each on a hold mark of its own
	// (store.ForConcurrentRead), as filteredDeliveryPullRequests runs its reads.
	var checks, deploys delivery.RunsWithJobs
	reads, readsCtx := errgroup.WithContext(ctx)
	for _, read := range []struct {
		into                *delivery.RunsWithJobs
		kind                delivery.DeliveryRunKind
		branch, githubEvent string
	}{
		{&checks, delivery.DeliveryRunKindPRChecks, "", "pull_request"},
		{&deploys, delivery.DeliveryRunKindDeploy, "main", "push"},
	} {
		readCtx, err := store.ForConcurrentRead(readsCtx)
		if err != nil {
			_ = reads.Wait()
			return deliveryPipelineResponse{}, err
		}
		reads.Go(func() (err error) {
			*read.into, err = delivery.ListRunsWithJobsStartedIn(readCtx, pool, settings.DeployRepo, read.kind, read.branch, read.githubEvent, from, to)
			return err
		})
	}
	if err := reads.Wait(); err != nil {
		return deliveryPipelineResponse{}, err
	}
	population, err := s.filteredDeliveryPullRequests(ctx, settings, from, to, url.Values{}, false)
	if err != nil {
		return deliveryPipelineResponse{}, err
	}

	rollup := ""
	if settings.PRChecksRollupJobName != nil {
		rollup = *settings.PRChecksRollupJobName
	}
	run, slowest, failing := measures.PRChecks(checks.Runs, checks.Jobs, days, at, rollup)
	names := []string{settings.ProductionJobName}
	for _, name := range []*string{settings.StagingE2EJobName, settings.StagingApplyJobName} {
		if name != nil {
			names = append(names, *name)
		}
	}
	deployJobs := measures.DeployJobs(deploys.Runs, deploys.Jobs, names, days)
	optional := func(name *string) *measures.DeployJobTiming {
		if name == nil {
			return nil
		}
		return new(deployJobs[*name])
	}
	mergeToProduction, openedToMerge := measures.LeadTimes(population.measuresPRs(), days)
	return deliveryPipelineResponse{
		Days:              days,
		ComputedAt:        time.Now().UTC(),
		PRChecks:          deliveryPipelinePRChecks{Run: run, SlowestJobs: slowest, FailingJobs: failing},
		StagingE2E:        optional(settings.StagingE2EJobName),
		StagingApply:      optional(settings.StagingApplyJobName),
		ProductionApply:   deployJobs[settings.ProductionJobName],
		MergeToProduction: mergeToProduction,
		OpenedToMerge:     openedToMerge,
		Targets:           measures.DefaultTargets,
		Freshness:         population.freshness,
	}, nil
}
