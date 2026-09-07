package cistore

import "sort"

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

// Summary is the JSON checks notification body. Every status is always present
// (count 0, empty checks when none) so consumers see a stable schema.
type Summary struct {
	Kind                 string      `json:"kind"`
	Repo                 string      `json:"repo"`
	Number               string      `json:"number"`
	SHA                  string      `json:"sha"`
	SettledAt            int64       `json:"settled_at,omitempty"`
	SupersededSettlement string      `json:"superseded_settlement,omitempty"`
	Failed               StatusGroup `json:"failed"`
	Running              StatusGroup `json:"running"`
	Passed               StatusGroup `json:"passed"`
	Queued               StatusGroup `json:"queued"`
	Cancelled            StatusGroup `json:"cancelled"`
	Skipped              StatusGroup `json:"skipped"`
	FailingChecks        []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"failing_checks"`
}

// renderSummary derives the stable checks payload. Names within each group are
// sorted; publication owns the settlement timestamp and JSON encoding.
func renderSummary(s State) Summary {
	groups := map[category][]string{}
	for name, c := range s.Checks {
		cat := classify(c)
		groups[cat] = append(groups[cat], name)
	}
	failed := group(groups[catFailed])
	failingChecks := make([]struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}, len(failed.Checks))
	for i, name := range failed.Checks {
		failingChecks[i] = struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}{Name: name, URL: s.Checks[name].URL}
	}
	sum := Summary{
		Kind:          "checks",
		Repo:          s.Owner + "/" + s.Repo,
		Number:        s.Number,
		SHA:           s.SHA,
		Failed:        failed,
		Running:       group(groups[catRunning]),
		Passed:        group(groups[catPassed]),
		Queued:        group(groups[catQueued]),
		Cancelled:     group(groups[catCancelled]),
		Skipped:       group(groups[catSkipped]),
		FailingChecks: failingChecks,
	}
	if s.Generation > 0 {
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
