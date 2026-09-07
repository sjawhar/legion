package cistore

import (
	"sort"
)

// category buckets a check into one of the rollup groups.
type category int

const (
	catPassed category = iota
	catFailed
	catRunning
	catQueued
	catCancelled
	catSkipped
)

// classify maps a check's (status, conclusion) to a group.
//
// A completed check is judged by its conclusion; an incomplete check by its
// status. All documented GitHub conclusions are handled explicitly; an unknown
// (future/undocumented) completed conclusion is surfaced as failed rather than
// silently passed, so a failure-like new state is not hidden. Unknown statuses
// default to queued.
func classify(c Check) category {
	if c.Status == "completed" {
		switch c.Conclusion {
		case "failure", "timed_out", "action_required", "startup_failure", "stale":
			return catFailed
		case "cancelled":
			return catCancelled
		case "skipped":
			return catSkipped
		case "neutral", "success", "":
			return catPassed
		default:
			return catFailed
		}
	}
	switch c.Status {
	case "in_progress":
		return catRunning
	case "queued", "requested", "waiting", "pending":
		return catQueued
	default:
		return catQueued
	}
}

// StatusGroup is the per-status view: an explicit count plus the full sorted
// list of check names in that status (nothing is collapsed).
type StatusGroup struct {
	Count  int      `json:"count"`
	Checks []string `json:"checks"`
}

// FailingCheck identifies a failed check and its GitHub URL.
type FailingCheck struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Summary is the JSON checks notification body. Every status is always present
// (count 0, empty checks when none) so consumers see a stable schema.
type Summary struct {
	Kind   string `json:"kind"`
	Repo   string `json:"repo"`
	Number string `json:"number"`
	SHA    string `json:"sha"`
	// CheckRuns is the settlement's attempt set: the latest check-run id per
	// check name, sorted by name. Consumers order same-head settlements by it
	// (per-name ids never decrease within a head), then by Generation at an
	// equal set.
	CheckRuns            []CheckRunRef  `json:"check_runs"`
	Generation           uint64         `json:"generation"`
	Snapshot             string         `json:"snapshot"`
	SettledAt            int64          `json:"settled_at,omitempty"`
	SupersededSettlement string         `json:"superseded_settlement,omitempty"`
	Failed               StatusGroup    `json:"failed"`
	Running              StatusGroup    `json:"running"`
	Passed               StatusGroup    `json:"passed"`
	Queued               StatusGroup    `json:"queued"`
	Cancelled            StatusGroup    `json:"cancelled"`
	Skipped              StatusGroup    `json:"skipped"`
	FailingChecks        []FailingCheck `json:"failing_checks"`
}

// CheckRunRef names one check and its latest GitHub check-run id.
type CheckRunRef struct {
	Name string `json:"name"`
	ID   uint64 `json:"id"`
}

// renderSummary derives the stable checks payload. Names within each group are
// sorted; publication owns the settlement timestamp and JSON encoding.
func renderSummary(s State) Summary {
	groups := map[category][]string{}
	failingChecks := make([]FailingCheck, 0)
	checkRuns := make([]CheckRunRef, 0)
	for key, check := range s.Checks {
		name := check.Name
		if name == "" {
			name = key
		}
		if check.CheckRunID > 0 {
			checkRuns = append(checkRuns, CheckRunRef{Name: name, ID: uint64(check.CheckRunID)})
		}
		category := classify(check)
		groups[category] = append(groups[category], name)
		if category == catFailed {
			failingChecks = append(failingChecks, FailingCheck{Name: name, URL: check.URL})
		}
	}
	sort.Slice(failingChecks, func(i, j int) bool {
		if failingChecks[i].Name == failingChecks[j].Name {
			return failingChecks[i].URL < failingChecks[j].URL
		}
		return failingChecks[i].Name < failingChecks[j].Name
	})
	sort.Slice(checkRuns, func(i, j int) bool { return checkRuns[i].Name < checkRuns[j].Name })
	failed := group(groups[catFailed])
	sum := Summary{
		Kind:          "checks",
		Repo:          s.Owner + "/" + s.Repo,
		Number:        s.Number,
		SHA:           s.SHA,
		CheckRuns:     checkRuns,
		Generation:    s.Generation,
		Snapshot:      s.Hash(),
		Failed:        failed,
		Running:       group(groups[catRunning]),
		Passed:        group(groups[catPassed]),
		Queued:        group(groups[catQueued]),
		Cancelled:     group(groups[catCancelled]),
		Skipped:       group(groups[catSkipped]),
		FailingChecks: failingChecks,
	}
	if s.EmittedCount > 0 {
		sum.SupersededSettlement = "true"
	}
	return sum
}

// group builds a StatusGroup from a name list: sorted names, explicit count, and
// a non-nil slice so JSON marshals an empty group's checks as [] not null.
func group(names []string) StatusGroup {
	if names == nil {
		names = []string{}
	}
	sort.Strings(names)
	return StatusGroup{Count: len(names), Checks: names}
}
