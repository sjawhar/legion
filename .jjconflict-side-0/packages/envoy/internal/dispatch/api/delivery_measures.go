package api

import (
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
// request: the window's population read once (readDeliveryPopulation) and measured by
// deliveryMeasuresCompute, the two halves the live hub calls too.
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
	population, err := s.readDeliveryPopulation(r.Context(), settings, from, to, query, false)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, deliveryMeasuresCompute(settings, population, from, to, time.Now().UTC()))
}

// measuresPRs is the derived pull requests that pass the search and facets, as the measures
// package reads them. A partial row's lead-time fields are not yet knowable, so it is skipped
// rather than read as zero.
func (d deliveryDerived) measuresPRs() []measures.PR {
	prs := make([]measures.PR, 0, len(d.rows))
	for _, row := range d.rows {
		view := row.view
		if !row.matches(d.selection, "") || view.CreatedAt == nil || view.MergedAt == nil {
			continue
		}
		prs = append(prs, measures.PR{
			ID: view.ID, CreatedAt: *view.CreatedAt, MergedAt: *view.MergedAt, FirstCommitAt: view.FirstCommitAt,
			DeployRunID: view.DeployRun, DeployedAt: view.DeployedAt, Rework: view.Rework,
		})
	}
	return prs
}

// deliveryMeasuresCompute is GET /api/v1/delivery/measures' body for [from, to) from a population
// read for that window or narrowed to it. It reads nothing; now is only computed_at.
func deliveryMeasuresCompute(settings delivery.DeliverySettings, pop deliveryPopulation, from, to, now time.Time) deliveryMeasuresResponse {
	return deliveryMeasuresOver(settings, pop, pop.derive(settings), from, to, now)
}

// deliveryMeasuresOver measures [from, to) over population and what derive computed from it.
// Facets narrow the pull-request measures (lead time, rework share, the per-PR failure rate)
// exactly as they narrow the timeline's prs[]; deploys and runs follow the window alone: every
// deploy-workflow run on main started in [from, to), each a deploy when its production job
// succeeded. A deploy's shipped-PR count is every window pull request whose first shipping apply is
// that run, before the facets (derived.shipped), so deploys_with_prs belongs to the deploy, not the
// filter. The measures route and the timeline both answer through it, so the two never disagree.
func deliveryMeasuresOver(settings delivery.DeliverySettings, pop deliveryPopulation, derived deliveryDerived, from, to, now time.Time) deliveryMeasuresResponse {
	applies := delivery.ProductionApplies(pop.windowRuns, pop.windowJobs, settings.Production()).Applies
	reached := map[int64]bool{}
	for _, apply := range applies {
		reached[apply.RunID] = true
	}
	runs := make([]measures.Run, len(pop.windowRuns))
	for i, run := range pop.windowRuns {
		runs[i] = measures.Run{
			ID: run.RunID, StartedAt: run.StartedAt, Conclusion: run.Conclusion,
			ReachedProduction: reached[run.RunID], ShippedPRs: len(derived.shipped[run.RunID]),
		}
	}
	unowned := pop.unownedP0
	if unowned == nil {
		unowned = []delivery.UnownedIssue{}
	}
	result := measures.Compute(derived.measuresPRs(), runs, nil, measures.Window{From: from, To: to})
	return deliveryMeasuresResponse{
		Window:      delivery.DeliveryWindowView{From: from, To: to},
		ComputedAt:  now,
		Measures:    result,
		FlagsSource: deliveryFlagsSourceNone,
		UnownedP0:   unowned,
		Targets:     measures.DefaultTargets,
		Status:      measures.KPIStatus(result, len(unowned), measures.DefaultTargets),
		Freshness:   derived.freshness,
	}
}
