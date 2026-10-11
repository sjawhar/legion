// Package measures is the delivery measures' pure computation (LEGION-567 slice 2): deploys a day,
// lead time, change failure rate, time to restore, rework share, deploy-run success and their
// per-UTC-day series, judged against Sami's KPI targets. It ports the prototype's
// web/src/lib/dora.ts formula by formula, and each function cites the prototype lines it ports. It
// reads no settings, applies no facet and filters no window: the caller hands it the population
// PRs that survived the facets and the deploy runs its store query selected, and Compute counts
// exactly those.
package measures

import (
	"math"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

const day = 24 * time.Hour

// Window is [From, To): half-open, as GET /api/v1/delivery/timeline's window is.
type Window struct{ From, To time.Time }

// PR is what Compute needs of one population pull request, already facet-filtered.
type PR struct {
	ID            string // "owner/repo#N"
	CreatedAt     time.Time
	MergedAt      time.Time
	FirstCommitAt *time.Time
	DeployRunID   *int64
	DeployedAt    *time.Time // the production job's CompletedAt (delivery.Apply.CompletedAt)
	Rework        bool
}

// Run is one deploy-workflow run on main started in the window (the store query applies the
// branch and window; Compute filters nothing).
type Run struct {
	ID                int64
	StartedAt         time.Time
	Conclusion        *delivery.DeliveryRunConclusion
	ReachedProduction bool // the production job concluded success (delivery.ProductionApplies found it)
	ShippedPRs        int  // population PRs merged in the window whose first shipping apply is this run, before facets
}

// Flag is one failed-change flag against a population PR: the PR that broke production, the PR
// that fixed or reverted it, and whether the dora role holder has confirmed it.
type Flag struct {
	Broken, Fix string // PR ids
	Kind        string // "revert" | "fix"
	State       string // "pending" | "confirmed" | "rejected"
}

// Spread is a set of durations' median, 90th percentile and maximum, in minutes; all three are
// nil for an empty set.
type Spread struct {
	MedianMinutes *float64 `json:"median_minutes"`
	P90Minutes    *float64 `json:"p90_minutes"`
	MaxMinutes    *float64 `json:"max_minutes"`
}

// DailyPoint is one UTC day of the window; Partial when the window covers only part of it.
type DailyPoint struct {
	Day               string   `json:"day"` // YYYY-MM-DD, UTC
	Partial           bool     `json:"partial"`
	Deploys           int      `json:"deploys"`
	Concluded         int      `json:"concluded"`
	ReachedProduction int      `json:"reached_production"`
	Cancelled         int      `json:"cancelled"`
	RunSuccessRate    *float64 `json:"run_success_rate"`
}

// FlagCounts is the change failure rate's flags by state, counted against PRs or against deploys.
type FlagCounts struct {
	Confirmed int     `json:"confirmed"`
	Pending   int     `json:"pending"`
	Rejected  int     `json:"rejected"`
	Reverts   int     `json:"reverts"`
	Total     int     `json:"total"`
	Rate      float64 `json:"rate"`
}

// DeployFlagCounts is the per-deploy change failure rate, with the rate it would be if every
// pending flag were confirmed: ConfirmedOrPending counts each deploy once.
type DeployFlagCounts struct {
	FlagCounts
	ConfirmedOrPending int     `json:"confirmed_or_pending"`
	UpperBoundRate     float64 `json:"upper_bound_rate"`
}

// DeployFrequency counts the window's successful production deploys, and those that shipped at
// least one population PR, in total and per day.
type DeployFrequency struct {
	SuccessfulDeploys int     `json:"successful_deploys"`
	DeploysWithPRs    int     `json:"deploys_with_prs"`
	PerDay            float64 `json:"per_day"`
	WithPRsPerDay     float64 `json:"with_prs_per_day"`
}

// LeadTime is the four lead-time spreads: merge, first commit and open to production over the
// deployed PRs, and open to merge over every PR.
type LeadTime struct {
	MergeToProduction       Spread `json:"merge_to_production"`
	FirstCommitToProduction Spread `json:"first_commit_to_production"`
	OpenedToProduction      Spread `json:"opened_to_production"`
	OpenedToMerge           Spread `json:"opened_to_merge"`
}

// ChangeFailureRate is the flags counted against PRs and against deploys.
type ChangeFailureRate struct {
	PerPR     FlagCounts       `json:"per_pr"`
	PerDeploy DeployFlagCounts `json:"per_deploy"`
}

// TimeToRestore is the median minutes from a broken PR's deploy to its fix's, over confirmed
// flags whose two PRs both deployed; nil when there is none.
type TimeToRestore struct {
	MedianMinutes *float64 `json:"median_minutes"`
}

// DeployRunSuccess is the deploy runs that concluded success or failure, how many of those reached
// production, the cancelled runs beside them, and the rate; Rate is nil when none concluded.
type DeployRunSuccess struct {
	Concluded         int      `json:"concluded"`
	ReachedProduction int      `json:"reached_production"`
	Cancelled         int      `json:"cancelled"`
	Rate              *float64 `json:"rate"`
}

// Result is every number the measures panel and GET /api/v1/delivery/measures show for one window
// and population.
type Result struct {
	DeployFrequency   DeployFrequency   `json:"deploy_frequency"`
	LeadTime          LeadTime          `json:"lead_time"`
	ChangeFailureRate ChangeFailureRate `json:"change_failure_rate"`
	TimeToRestore     TimeToRestore     `json:"time_to_restore"`
	ReworkShare       float64           `json:"rework_share"`
	DeployRunSuccess  DeployRunSuccess  `json:"deploy_run_success"`
	Daily             []DailyPoint      `json:"daily"`
}

// Targets are Sami's engineering targets (the prototype's KPI_TARGETS, CONTRACT.md "KPI targets",
// set 2026-09-28): "under" is strictly below, "at least" is >=.
type Targets struct {
	DeploysPerDay              float64 `json:"deploys_per_day"`                // at least
	ChangeFailureRate          float64 `json:"change_failure_rate"`            // under, per deploy
	MergeToProductionMinutes   float64 `json:"merge_to_production_minutes"`    // under, for every change (the max)
	OpenedToMergeMedianMinutes float64 `json:"opened_to_merge_median_minutes"` // under, the median
	DeployRunSuccessRate       float64 `json:"deploy_run_success_rate"`        // at least
	UnownedP0                  int     `json:"unowned_p0"`                     // exactly
}

// DefaultTargets are the targets every measures response is judged against (the prototype's
// web/src/lib/dora.ts:11-18).
var DefaultTargets = Targets{
	DeploysPerDay:              20,
	ChangeFailureRate:          0.05,
	MergeToProductionMinutes:   45,
	OpenedToMergeMedianMinutes: 60,
	DeployRunSuccessRate:       0.9,
	UnownedP0:                  0,
}

// Status is whether each KPI meets its target; nil when the window has nothing to measure.
type Status struct {
	DeploysPerDay               bool  `json:"deploys_per_day"`
	ChangeFailureRate           *bool `json:"change_failure_rate"`
	ChangeFailureRateUpperBound *bool `json:"change_failure_rate_upper_bound"`
	MergeToProductionMedian     *bool `json:"merge_to_production_median"`
	MergeToProduction           *bool `json:"merge_to_production"`
	OpenedToMerge               *bool `json:"opened_to_merge"`
	DeployRunSuccess            *bool `json:"deploy_run_success"`
	UnownedP0                   bool  `json:"unowned_p0"`
}

// Quantile is p's quantile of ascending, non-empty sorted, linear between the two nearest ranks:
// d3's quantileSorted, as the prototype's collector/src/collector/pipeline.py:69-77 writes it.
func Quantile(sorted []float64, p float64) float64 {
	i := float64(len(sorted)-1) * p
	lo := int(math.Floor(i))
	if lo+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + (sorted[lo+1]-sorted[lo])*(i-float64(lo))
}

// SpreadOf is minutes' median, 90th percentile and maximum (the prototype's dora.ts:48-56).
func SpreadOf(minutes []float64) Spread {
	if len(minutes) == 0 {
		return Spread{}
	}
	sorted := slices.Sorted(slices.Values(minutes))
	median := Quantile(sorted, 0.5)
	p90 := Quantile(sorted, 0.9)
	maximum := sorted[len(sorted)-1]
	return Spread{MedianMinutes: &median, P90Minutes: &p90, MaxMinutes: &maximum}
}

// minutesBetween is b - a in minutes, one division of whole nanoseconds, so a duration the
// prototype computes from whole milliseconds (dora.ts:25-27) comes out to the same float.
func minutesBetween(a, b time.Time) float64 {
	return float64(b.Sub(a)) / float64(time.Minute)
}

// concluded is a run that concluded success or failure: the run-success denominator. A cancelled
// run is not a deploy attempt (dora.ts:61-63).
func concluded(run Run) bool {
	return run.Conclusion != nil &&
		(*run.Conclusion == delivery.DeliveryRunConclusionSuccess || *run.Conclusion == delivery.DeliveryRunConclusionFailure)
}

func cancelled(run Run) bool {
	return run.Conclusion != nil && *run.Conclusion == delivery.DeliveryRunConclusionCancelled
}

func ratio(numerator, denominator int) *float64 {
	if denominator == 0 {
		return nil
	}
	rate := float64(numerator) / float64(denominator)
	return &rate
}

func rateOrZero(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func midnight(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// DailySeries is the window's per-UTC-day deploys and run success, oldest first, every day of the
// window present, runs or not (the prototype's dora.ts:83-117). Runs bucket by StartedAt, the field
// the window's query filters on, so the days sum to the window's totals. A window ending exactly
// at midnight covers none of the day that starts then. The prototype's window end is inclusive;
// Dispatch's store query never passes a run that starts at To, so the last-day clamp only ever
// holds a run that starts inside it.
func DailySeries(runs []Run, window Window) []DailyPoint {
	first := midnight(window.From)
	last := midnight(window.To)
	if last.Equal(window.To) {
		last = last.Add(-day)
	}
	if last.Before(first) {
		last = first
	}
	var points []DailyPoint
	for d := first; !d.After(last); d = d.Add(day) {
		points = append(points, DailyPoint{
			Day:     d.Format(time.DateOnly),
			Partial: d.Before(window.From) || d.Add(day-time.Millisecond).After(window.To),
		})
	}
	for _, run := range runs {
		index := min(int(run.StartedAt.Sub(first)/day), len(points)-1)
		point := &points[index]
		if run.ReachedProduction {
			point.Deploys++
		}
		if cancelled(run) {
			point.Cancelled++
		}
		if concluded(run) {
			point.Concluded++
			if run.ReachedProduction {
				point.ReachedProduction++
			}
		}
	}
	for i := range points {
		points[i].RunSuccessRate = ratio(points[i].ReachedProduction, points[i].Concluded)
	}
	return points
}

// Compute is every measure of the window over prs and runs (the prototype's dora.ts:163-266
// computeDora). Every run counts and every PR counts: the caller's query applied the window and
// branch to runs, and the facets to prs.
func Compute(prs []PR, runs []Run, flags []Flag, window Window) Result {
	var result Result

	// Deploy frequency and deploy-run success (dora.ts:169-174, 218-223, 253-258).
	var successful, withPRs, concludedRuns, concludedReached, cancelledRuns int
	for _, run := range runs {
		if run.ReachedProduction {
			successful++
			if run.ShippedPRs > 0 {
				withPRs++
			}
		}
		if concluded(run) {
			concludedRuns++
			if run.ReachedProduction {
				concludedReached++
			}
		}
		if cancelled(run) {
			cancelledRuns++
		}
	}
	windowDays := float64(window.To.Sub(window.From)) / float64(day)
	result.DeployFrequency = DeployFrequency{
		SuccessfulDeploys: successful,
		DeploysWithPRs:    withPRs,
		PerDay:            float64(successful) / windowDays,
		WithPRsPerDay:     float64(withPRs) / windowDays,
	}
	result.DeployRunSuccess = DeployRunSuccess{
		Concluded:         concludedRuns,
		ReachedProduction: concludedReached,
		Cancelled:         cancelledRuns,
		Rate:              ratio(concludedReached, concludedRuns),
	}

	// Lead time (dora.ts:176-184) and rework share (dora.ts:215, 252).
	var mergeToProduction, firstCommitToProduction, openedToProduction, openedToMerge []float64
	byID := make(map[string]PR, len(prs))
	rework := 0
	for _, pr := range prs {
		byID[pr.ID] = pr
		openedToMerge = append(openedToMerge, minutesBetween(pr.CreatedAt, pr.MergedAt))
		if pr.Rework {
			rework++
		}
		if pr.DeployedAt == nil {
			continue
		}
		mergeToProduction = append(mergeToProduction, minutesBetween(pr.MergedAt, *pr.DeployedAt))
		openedToProduction = append(openedToProduction, minutesBetween(pr.CreatedAt, *pr.DeployedAt))
		if pr.FirstCommitAt != nil {
			firstCommitToProduction = append(firstCommitToProduction, minutesBetween(*pr.FirstCommitAt, *pr.DeployedAt))
		}
	}
	result.LeadTime = LeadTime{
		MergeToProduction:       SpreadOf(mergeToProduction),
		FirstCommitToProduction: SpreadOf(firstCommitToProduction),
		OpenedToProduction:      SpreadOf(openedToProduction),
		OpenedToMerge:           SpreadOf(openedToMerge),
	}
	result.ReworkShare = rateOrZero(rework, len(prs))

	// Change failure rate (dora.ts:186-204, 230-250): a flag counts against the population only
	// when its broken PR is in it; per deploy, the deploys that shipped a flagged PR, each once.
	var perPR FlagCounts
	confirmedDeploys := map[int64]bool{}
	pendingDeploys := map[int64]bool{}
	rejectedDeploys := map[int64]bool{}
	var restoreMinutes []float64
	for _, flag := range flags {
		broken, ok := byID[flag.Broken]
		if !ok {
			continue
		}
		var deploys map[int64]bool
		switch flag.State {
		case "confirmed":
			perPR.Confirmed++
			if flag.Kind == "revert" {
				perPR.Reverts++
			}
			deploys = confirmedDeploys
			// Time to restore (dora.ts:206-213, 251).
			if fix, ok := byID[flag.Fix]; ok && broken.DeployedAt != nil && fix.DeployedAt != nil {
				restoreMinutes = append(restoreMinutes, minutesBetween(*broken.DeployedAt, *fix.DeployedAt))
			}
		case "pending":
			perPR.Pending++
			deploys = pendingDeploys
		case "rejected":
			perPR.Rejected++
			deploys = rejectedDeploys
		default:
			continue
		}
		if broken.DeployRunID != nil {
			deploys[*broken.DeployRunID] = true
		}
	}
	perPR.Total = len(prs)
	perPR.Rate = rateOrZero(perPR.Confirmed, len(prs))
	confirmedOrPending := len(confirmedDeploys)
	for run := range pendingDeploys {
		if !confirmedDeploys[run] {
			confirmedOrPending++
		}
	}
	result.ChangeFailureRate = ChangeFailureRate{
		PerPR: perPR,
		PerDeploy: DeployFlagCounts{
			FlagCounts: FlagCounts{
				Confirmed: len(confirmedDeploys),
				Pending:   len(pendingDeploys),
				Rejected:  len(rejectedDeploys),
				Reverts:   perPR.Reverts,
				Total:     successful,
				Rate:      rateOrZero(len(confirmedDeploys), successful),
			},
			ConfirmedOrPending: confirmedOrPending,
			UpperBoundRate:     rateOrZero(confirmedOrPending, successful),
		},
	}
	result.TimeToRestore = TimeToRestore{MedianMinutes: SpreadOf(restoreMinutes).MedianMinutes}

	result.Daily = DailySeries(runs, window)
	return result
}

// KPIStatus judges result and the count of unowned P0 issues against targets (the prototype's
// dora.ts:280-295): "at least" targets are met at the target, "under" targets are missed at it,
// and a measure with nothing to measure is nil rather than met.
func KPIStatus(result Result, unownedP0 int, targets Targets) Status {
	under := func(value *float64, target float64) *bool {
		if value == nil {
			return nil
		}
		met := *value < target
		return &met
	}
	cfr := result.ChangeFailureRate.PerDeploy
	var cfrMet, cfrUpperMet *bool
	if cfr.Total > 0 {
		cfrMet = under(&cfr.Rate, targets.ChangeFailureRate)
		cfrUpperMet = under(&cfr.UpperBoundRate, targets.ChangeFailureRate)
	}
	var runSuccessMet *bool
	if rate := result.DeployRunSuccess.Rate; rate != nil {
		met := *rate >= targets.DeployRunSuccessRate
		runSuccessMet = &met
	}
	merge := result.LeadTime.MergeToProduction
	return Status{
		DeploysPerDay:               result.DeployFrequency.PerDay >= targets.DeploysPerDay,
		ChangeFailureRate:           cfrMet,
		ChangeFailureRateUpperBound: cfrUpperMet,
		MergeToProductionMedian:     under(merge.MedianMinutes, targets.MergeToProductionMinutes),
		MergeToProduction:           under(merge.MaxMinutes, targets.MergeToProductionMinutes),
		OpenedToMerge:               under(result.LeadTime.OpenedToMerge.MedianMinutes, targets.OpenedToMergeMedianMinutes),
		DeployRunSuccess:            runSuccessMet,
		UnownedP0:                   unownedP0 == targets.UnownedP0,
	}
}
