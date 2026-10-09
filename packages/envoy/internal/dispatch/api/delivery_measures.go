package api

import (
	"context"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// deliveryFlagsSource is flags_source: whether stored failed-change flags fed the change failure
// rate and time to restore. packages/contracts/src/dispatch-api.ts's DeliveryMeasuresResponse
// declares its closed set, "none" | "stored"; this server writes only "none".
type deliveryFlagsSource string

// deliveryFlagsSourceNone is every answer while Dispatch stores no failed-change flags: the change
// failure rate and time to restore are computed from an empty flag set, since a flag counts only
// once the dora role holder confirms it (LEGION-294).
const deliveryFlagsSourceNone deliveryFlagsSource = "none"

// deliveryMeasuresResponse is GET /api/v1/delivery/measures, and the timeline's `measures`: the
// measures for the window over the pull requests the timeline's search and facets select, Sami's
// targets, whether each is met, the open P0 issues with no owner, and the timeline's freshness.
// Mirrors packages/contracts/src/dispatch-api.ts's DeliveryMeasuresResponse exactly. It lives
// here rather than beside the timeline's views in delivery/model.go because the measures package
// imports delivery.
type deliveryMeasuresResponse struct {
	Window      delivery.DeliveryWindowView    `json:"window"`
	ComputedAt  time.Time                      `json:"computed_at"`
	Measures    measures.Result                `json:"measures"`
	FlagsSource deliveryFlagsSource            `json:"flags_source"`
	UnownedP0   []delivery.UnownedIssue        `json:"unowned_p0"`
	Targets     measures.Targets               `json:"targets"`
	Status      measures.Status                `json:"status"`
	Freshness   delivery.DeliveryFreshnessView `json:"freshness"`
}

// getDeliveryMeasures answers GET /api/v1/delivery/measures, computed from stored facts on
// request: the window's population read once (filteredDeliveryPullRequests) and measured by
// deliveryMeasuresFor.
func (s *server) getDeliveryMeasures(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	from, to, err := parseDeliveryWindow(query)
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	settings, ok := s.deliverySettings(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	population, err := s.filteredDeliveryPullRequests(ctx, settings, from, to, query, nil)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	freshness, err := s.deliveryFreshness(ctx, settings)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	response, err := s.deliveryMeasuresFor(ctx, settings, from, to, population, freshness)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, response)
}

// deliveryMeasuresFor measures [from, to) over population, the window's pull requests as
// filteredDeliveryPullRequests read them. Facets narrow the pull-request measures (lead time,
// rework share, the per-PR failure rate) exactly as they narrow the timeline's prs[]; deploys and
// runs follow the window alone: every deploy-workflow run on main started in [from, to), each a
// deploy when its production job succeeded. A deploy's shipped-PR count is every window pull
// request whose first shipping apply is that run, before the facets (population.shipped), so
// deploys_with_prs belongs to the deploy, not the filter. The measures route and the timeline both
// answer through it, so the two never disagree.
func (s *server) deliveryMeasuresFor(ctx context.Context, settings delivery.DeliverySettings, from, to time.Time, population deliveryPopulation, freshness delivery.DeliveryFreshnessView) (deliveryMeasuresResponse, error) {
	pool := s.deps.Store.Pool
	prs := make([]measures.PR, 0, len(population.rows))
	for _, row := range population.rows {
		view := row.view
		// A partial row's lead-time fields are not yet knowable, so it is skipped rather than
		// read as zero.
		if !row.matches(population.selection, "") || view.CreatedAt == nil || view.MergedAt == nil {
			continue
		}
		prs = append(prs, measures.PR{
			ID: view.ID, CreatedAt: *view.CreatedAt, MergedAt: *view.MergedAt, FirstCommitAt: view.FirstCommitAt,
			DeployRunID: view.DeployRun, DeployedAt: view.DeployedAt, Rework: view.Rework,
		})
	}

	// Whether a run reached production is decided on the window's own runs and their jobs, never
	// on the head-commit list containment reads, so every run the window counts is judged.
	windowRuns, err := delivery.ListRunsStartedIn(ctx, pool, settings.DeployRepo, delivery.DeliveryRunKindDeploy, "main", "", from, to)
	if err != nil {
		return deliveryMeasuresResponse{}, err
	}
	runIDs := make([]int64, len(windowRuns))
	for i, run := range windowRuns {
		runIDs[i] = run.RunID
	}
	jobsByRun, err := delivery.ListRunJobsForRuns(ctx, pool, settings.DeployRepo, runIDs)
	if err != nil {
		return deliveryMeasuresResponse{}, err
	}
	reached := map[int64]bool{}
	for _, apply := range delivery.ProductionApplies(windowRuns, jobsByRun, settings.ProductionJobName) {
		reached[apply.RunID] = true
	}
	runs := make([]measures.Run, len(windowRuns))
	for i, run := range windowRuns {
		runs[i] = measures.Run{
			ID: run.RunID, StartedAt: run.StartedAt, Conclusion: run.Conclusion,
			ReachedProduction: reached[run.RunID], ShippedPRs: len(population.shipped[run.RunID]),
		}
	}

	result := measures.Compute(prs, runs, nil, measures.Window{From: from, To: to})
	unowned, err := delivery.ListUnownedP0(ctx, pool)
	if err != nil {
		return deliveryMeasuresResponse{}, err
	}
	return deliveryMeasuresResponse{
		Window:      delivery.DeliveryWindowView{From: from, To: to},
		ComputedAt:  time.Now().UTC(),
		Measures:    result,
		FlagsSource: deliveryFlagsSourceNone,
		UnownedP0:   unowned,
		Targets:     measures.DefaultTargets,
		Status:      measures.KPIStatus(result, len(unowned), measures.DefaultTargets),
		Freshness:   freshness,
	}, nil
}
