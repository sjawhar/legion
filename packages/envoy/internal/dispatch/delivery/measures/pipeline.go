package measures

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// The Pipeline page's numbers (LEGION-567 slice 3): how long PR checks, the deploy workflow's
// staging end-to-end gate and its staging and production deploys take, and how often each ends in
// success, per UTC day over the last seven days, beside the merge-to-production and
// open-to-merge lead times per merge day. Ported from the prototype's
// collector/src/collector/pipeline.py (the run and job timings) and web/src/lib/pipeline.ts (the
// lead times), formula by formula. Every timing buckets by the UTC day its run started (a job by
// its run's start). Only results that ended in success or failure are timed; a cancelled one is
// counted beside them, and anything else (skipped, still running) is not counted at all.

// ShownDayCount is how many UTC days the Pipeline page shows, ending today (pipeline.py:19).
const ShownDayCount = 7

// SlowestJobs is how many of the PR-checks workflow's jobs the slowest-jobs table ranks.
const SlowestJobs = 5

// SlowestMinResults is how many timed results a job needs in the shown days to be ranked, so a
// one-task matrix entry with a handful of results is not (pipeline.py:27-29).
const SlowestMinResults = 50

// FailingWindow is how far back the failing-jobs table looks from now (pipeline.py:30).
const FailingWindow = 24 * time.Hour

// Outcome is a run or job conclusion as the Pipeline counts it.
type Outcome string

const (
	OutcomeSuccess   Outcome = "success"
	OutcomeFailure   Outcome = "failure"
	OutcomeCancelled Outcome = "cancelled"
)

// OutcomeOf is a conclusion as the Pipeline counts it (pipeline.py:38-47): success; failure for
// failure or timed_out; cancelled; anything else (skipped, startup_failure, still running) is not
// counted, and the second result is false.
func OutcomeOf[C ~string](conclusion *C) (Outcome, bool) {
	if conclusion == nil {
		return "", false
	}
	switch string(*conclusion) {
	case "success":
		return OutcomeSuccess, true
	case "failure", "timed_out":
		return OutcomeFailure, true
	case "cancelled":
		return OutcomeCancelled, true
	}
	return "", false
}

// timed is an outcome the Pipeline times: success or failure.
func (o Outcome) timed() bool { return o == OutcomeSuccess || o == OutcomeFailure }

// Cell is one day's, or the shown days', results of one stage.
type Cell struct {
	N             int      `json:"n"`              // timed results (success or failure)
	MedianMinutes *float64 `json:"median_minutes"` // one decimal, as the prototype rounds
	P90Minutes    *float64 `json:"p90_minutes"`
	Success       int      `json:"success"`
	Failure       int      `json:"failure"`
	Cancelled     int      `json:"cancelled"`
	SuccessRate   *float64 `json:"success_rate"` // success/(success+failure), four decimals, nil when both are 0
}

// Day is one shown UTC day's Cell; Partial is today's.
type Day struct {
	Day     string `json:"day"` // YYYY-MM-DD, UTC
	Partial bool   `json:"partial"`
	Cell
}

// Timing is one stage per shown day, oldest first, and over all of them.
type Timing struct {
	Days  []Day `json:"days"`
	Total Cell  `json:"total"`
}

// Sample is one result of one stage: the UTC day its run started, its outcome, and its minutes
// when timed (nil otherwise).
type Sample struct {
	Day     string
	Outcome Outcome
	Minutes *float64
}

// JobTiming is one PR-checks job's timing, by its name.
type JobTiming struct {
	Name string `json:"name"`
	Timing
}

// DeployJobTiming is one deploy-workflow job's timing, by the job names it follows, joined by
// " or ".
type DeployJobTiming struct {
	Job string `json:"job"`
	Timing
}

// FailingJob is one PR-checks job that failed in the last FailingWindow: how many of its results
// there failed, out of how many, and the run of its latest failure.
type FailingJob struct {
	Name         string    `json:"name"`
	Failures     int       `json:"failures"`
	Results      int       `json:"results"`
	LastFailedAt time.Time `json:"last_failed_at"`
	LastRunURL   string    `json:"last_run_url"`
}

// LeadTimeCell is one day's, or the shown days', lead times in minutes, unrounded as the
// prototype's web side leaves them.
type LeadTimeCell struct {
	N             int      `json:"n"`
	MedianMinutes *float64 `json:"median_minutes"`
	P90Minutes    *float64 `json:"p90_minutes"`
	MaxMinutes    *float64 `json:"max_minutes"`
}

// LeadTimeDay is one shown UTC merge day's LeadTimeCell; Partial is today's.
type LeadTimeDay struct {
	Day     string `json:"day"`
	Partial bool   `json:"partial"`
	LeadTimeCell
}

// LeadTimeTiming is one lead time per shown merge day, oldest first, and over all of them.
type LeadTimeTiming struct {
	Days  []LeadTimeDay `json:"days"`
	Total LeadTimeCell  `json:"total"`
}

// ShownDays is the ShownDayCount UTC days ending on now's, oldest first (pipeline.py:54-57).
func ShownDays(now time.Time) []string {
	first := WindowStart(now)
	days := make([]string, ShownDayCount)
	for i := range days {
		days[i] = first.AddDate(0, 0, i).Format(time.DateOnly)
	}
	return days
}

// WindowStart is midnight UTC of the first shown day (pipeline.py:60-63).
func WindowStart(now time.Time) time.Time {
	return midnight(now).AddDate(0, 0, -(ShownDayCount - 1))
}

// roundTo is Python's round(x, places), which the prototype's collector applies: it rounds the
// decimal value x holds, half to even, as strconv's 'f' formatting does. A float multiply would
// not (189 s is 3.15 minutes, 3.149999… as a float, which Python rounds to 3.1 and
// math.RoundToEven(x*10)/10 to 3.2).
func roundTo(x float64, places int) float64 {
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', places, 64), 64)
	if err != nil {
		panic("measures: a formatted float does not parse: " + err.Error())
	}
	return rounded
}

// cell is samples' Cell (pipeline.py:80-92): the timed results' median and p90 to one decimal,
// each outcome counted, and the success rate to four.
func cell(samples []Sample) Cell {
	var result Cell
	var timedMinutes []float64
	for _, sample := range samples {
		switch sample.Outcome {
		case OutcomeSuccess:
			result.Success++
		case OutcomeFailure:
			result.Failure++
		case OutcomeCancelled:
			result.Cancelled++
		}
		if sample.Outcome.timed() && sample.Minutes != nil {
			timedMinutes = append(timedMinutes, *sample.Minutes)
		}
	}
	result.N = len(timedMinutes)
	if len(timedMinutes) > 0 {
		slices.Sort(timedMinutes)
		result.MedianMinutes = new(roundTo(Quantile(timedMinutes, 0.5), 1))
		result.P90Minutes = new(roundTo(Quantile(timedMinutes, 0.9), 1))
	}
	if decided := result.Success + result.Failure; decided > 0 {
		result.SuccessRate = new(roundTo(float64(result.Success)/float64(decided), 4))
	}
	return result
}

// ComputeTiming is samples per shown day and over all of them (pipeline.py:95-106): a sample whose
// day is not shown is dropped, and the last day is partial.
func ComputeTiming(samples []Sample, days []string) Timing {
	index := make(map[string]int, len(days))
	for i, d := range days {
		index[d] = i
	}
	byDay := make([][]Sample, len(days))
	var shown []Sample
	for _, sample := range samples {
		if i, ok := index[sample.Day]; ok {
			byDay[i] = append(byDay[i], sample)
			shown = append(shown, sample)
		}
	}
	timing := Timing{Days: make([]Day, len(days)), Total: cell(shown)}
	for i, d := range days {
		timing.Days[i] = Day{Day: d, Partial: i == len(days)-1, Cell: cell(byDay[i])}
	}
	return timing
}

// utcDay is t's UTC day, YYYY-MM-DD.
func utcDay(t time.Time) string {
	return t.UTC().Format(time.DateOnly)
}

// elapsedMinutes is end - start in minutes, or nil when either is unknown.
func elapsedMinutes(start, end *time.Time) *float64 {
	if start == nil || end == nil {
		return nil
	}
	return new(minutesBetween(*start, *end))
}

// jobTimings is each PR-checks job's timing by name (pipeline.py:121-135): its samples come only
// from runs that ended in success or failure, whose jobs the prototype fetches, and only from
// jobs that did, the only ones it stores (github_source.py:287-299). A job's minutes are its
// duration rounded to the whole second, as the prototype stores its seconds.
func jobTimings(runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob, days []string) map[string]Timing {
	samples := map[string][]Sample{}
	for _, run := range runs {
		if outcome, ok := OutcomeOf(run.Conclusion); !ok || !outcome.timed() {
			continue
		}
		runDay := utcDay(run.StartedAt)
		for _, job := range jobsByRun[run.RunID] {
			outcome, ok := OutcomeOf(job.Conclusion)
			if !ok || !outcome.timed() {
				continue
			}
			var minutes *float64
			if job.StartedAt != nil && job.CompletedAt != nil {
				minutes = new(math.Round(job.CompletedAt.Sub(*job.StartedAt).Seconds()) / 60)
			}
			samples[job.Name] = append(samples[job.Name], Sample{Day: runDay, Outcome: outcome, Minutes: minutes})
		}
	}
	timings := make(map[string]Timing, len(samples))
	for name, jobSamples := range samples {
		timings[name] = ComputeTiming(jobSamples, days)
	}
	return timings
}

// PRChecks is the PR-checks workflow's Pipeline numbers (pipeline.py:113-167): run, every run's
// wall time from its start to its completion and its outcome; slowest, the SlowestJobs jobs with
// SlowestMinResults timed results by their seven-day median, slowest first, then by name; and
// failing, every job that failed in a run completed in the FailingWindow up to now (a run that
// completed after now, which a pinned `at` in the past can read, is not counted), most failures
// first, then by name. The failing tally reads only runs that ended in success or failure (a
// cancelled run's jobs are not the prototype's to count) and only their jobs that did, and leaves
// out rollupJob (none when empty) and every summary or guard job (delivery.IsSummaryOrGuardJob).
// The caller passes the shown days' runs, each with its event and branch already selected.
func PRChecks(runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob, days []string, now time.Time, rollupJob string) (run Timing, slowest []JobTiming, failing []FailingJob) {
	runSamples := make([]Sample, 0, len(runs))
	for _, r := range runs {
		outcome, ok := OutcomeOf(r.Conclusion)
		if !ok {
			continue
		}
		sample := Sample{Day: utcDay(r.StartedAt), Outcome: outcome}
		if outcome.timed() {
			sample.Minutes = elapsedMinutes(&r.StartedAt, r.CompletedAt)
		}
		runSamples = append(runSamples, sample)
	}
	run = ComputeTiming(runSamples, days)

	slowest = []JobTiming{}
	for name, timing := range jobTimings(runs, jobsByRun, days) {
		if timing.Total.N >= SlowestMinResults {
			slowest = append(slowest, JobTiming{Name: name, Timing: timing})
		}
	}
	slices.SortFunc(slowest, func(a, b JobTiming) int {
		// A ranked job has SlowestMinResults timed results, so its median is set.
		if c := cmp.Compare(*b.Total.MedianMinutes, *a.Total.MedianMinutes); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	slowest = slowest[:min(len(slowest), SlowestJobs)]

	return run, slowest, failingJobs(runs, jobsByRun, now, rollupJob)
}

// failingJobs is PRChecks' failing tally (pipeline.py:144-157).
func failingJobs(runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob, now time.Time, rollupJob string) []FailingJob {
	horizon := now.Add(-FailingWindow)
	var recent []delivery.DeliveryRun
	for _, run := range runs {
		outcome, ok := OutcomeOf(run.Conclusion)
		if ok && outcome.timed() && run.CompletedAt != nil && !run.CompletedAt.Before(horizon) && !run.CompletedAt.After(now) {
			recent = append(recent, run)
		}
	}
	// In completion order, so each job's last failure is its latest.
	slices.SortStableFunc(recent, func(a, b delivery.DeliveryRun) int {
		return a.CompletedAt.Compare(*b.CompletedAt)
	})
	tallies := map[string]*FailingJob{}
	for _, run := range recent {
		for _, job := range jobsByRun[run.RunID] {
			outcome, ok := OutcomeOf(job.Conclusion)
			if !ok || !outcome.timed() || (rollupJob != "" && job.Name == rollupJob) || delivery.IsSummaryOrGuardJob(job.Name) {
				continue
			}
			tally, seen := tallies[job.Name]
			if !seen {
				tally = &FailingJob{Name: job.Name}
				tallies[job.Name] = tally
			}
			tally.Results++
			if outcome == OutcomeFailure {
				tally.Failures++
				tally.LastFailedAt = *run.CompletedAt
				tally.LastRunURL = run.URL
			}
		}
	}
	failing := []FailingJob{}
	for _, tally := range tallies {
		if tally.Failures > 0 {
			failing = append(failing, *tally)
		}
	}
	slices.SortFunc(failing, func(a, b FailingJob) int {
		if c := cmp.Compare(b.Failures, a.Failures); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return failing
}

// DeployJobCardID identifies one Pipeline card within a DeployJobs call.
type DeployJobCardID string

// DeployJobCard is one Pipeline card, identified by CardID. Its embedded production rule gives every
// name it follows (a renamed job's old and new names, so history and new runs both count) and, for
// the production card, the apply step whose success makes a successful job a deploy.
type DeployJobCard struct {
	CardID DeployJobCardID
	delivery.Production
}

// DeployJobs is each card's timing keyed by its CardID, read from runs in one pass
// (pipeline.py:170-185): each followed job's outcome (its conclusion as the card's
// delivery.Production reads it, so a job that applied nothing is not counted), its minutes from its
// start to its completion when timed, and the day its run started. The caller passes the deploy
// workflow's push runs on main started in the shown days. A run without a followed job, or whose job
// is not counted, adds nothing; every card gets a timing, all of its days empty when no run lists
// its jobs, and its job field is its names joined by " or ". A run adds no more than one sample to a
// card, from its Production.Job. A name two cards follow counts for the first.
func DeployJobs(runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob, cards []DeployJobCard, days []string) map[DeployJobCardID]DeployJobTiming {
	samples := make([][]Sample, len(cards))
	cardOf := map[string]int{}
	for i, card := range cards {
		for _, name := range card.JobNames {
			if _, taken := cardOf[name]; !taken {
				cardOf[name] = i
			}
		}
	}
	cardJobs := make([][]delivery.DeliveryRunJob, len(cards))
	for _, run := range runs {
		for i := range cardJobs {
			cardJobs[i] = cardJobs[i][:0]
		}
		for _, job := range jobsByRun[run.RunID] {
			if i, followed := cardOf[job.Name]; followed {
				cardJobs[i] = append(cardJobs[i], job)
			}
		}
		runDay := utcDay(run.StartedAt)
		for i, jobs := range cardJobs {
			job := cards[i].Job(jobs)
			if job == nil {
				continue
			}
			outcome, ok := OutcomeOf(cards[i].Conclusion(*job))
			if !ok {
				continue
			}
			sample := Sample{Day: runDay, Outcome: outcome}
			if outcome.timed() {
				sample.Minutes = elapsedMinutes(job.StartedAt, job.CompletedAt)
			}
			samples[i] = append(samples[i], sample)
		}
	}
	timings := make(map[DeployJobCardID]DeployJobTiming, len(cards))
	for i, card := range cards {
		timings[card.CardID] = DeployJobTiming{Job: strings.Join(card.JobNames, " or "), Timing: ComputeTiming(samples[i], days)}
	}
	return timings
}

// leadTimeCell is minutes' count, median, p90 and longest (pipeline.ts:21-29), unrounded.
func leadTimeCell(minutes []float64) LeadTimeCell {
	cell := LeadTimeCell{N: len(minutes)}
	if len(minutes) == 0 {
		return cell
	}
	sorted := slices.Sorted(slices.Values(minutes))
	cell.MedianMinutes = new(Quantile(sorted, 0.5))
	cell.P90Minutes = new(Quantile(sorted, 0.9))
	cell.MaxMinutes = new(sorted[len(sorted)-1])
	return cell
}

// leadTimeTiming is byDay's cells, one per shown day, and the cell of all of them.
func leadTimeTiming(byDay [][]float64, days []string) LeadTimeTiming {
	timing := LeadTimeTiming{Days: make([]LeadTimeDay, len(days))}
	var all []float64
	for i, d := range days {
		timing.Days[i] = LeadTimeDay{Day: d, Partial: i == len(days)-1, LeadTimeCell: leadTimeCell(byDay[i])}
		all = append(all, byDay[i]...)
	}
	timing.Total = leadTimeCell(all)
	return timing
}

// LeadTimes is the two lead-time cards (pipeline.ts:31-58), each PR bucketed by its UTC merge day:
// mergeToProduction, the DORA lead time, over the PRs that deployed (one still waiting has none
// yet), and openedToMerge over every PR, deployed or not.
func LeadTimes(prs []PR, days []string) (mergeToProduction, openedToMerge LeadTimeTiming) {
	index := make(map[string]int, len(days))
	for i, d := range days {
		index[d] = i
	}
	merged := make([][]float64, len(days))
	opened := make([][]float64, len(days))
	for _, pr := range prs {
		i, ok := index[utcDay(pr.MergedAt)]
		if !ok {
			continue
		}
		opened[i] = append(opened[i], minutesBetween(pr.CreatedAt, pr.MergedAt))
		if pr.DeployedAt != nil {
			merged[i] = append(merged[i], minutesBetween(pr.MergedAt, *pr.DeployedAt))
		}
	}
	return leadTimeTiming(merged, days), leadTimeTiming(opened, days)
}
