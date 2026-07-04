package cistore

import (
	"encoding/json"
	"sort"
)

// category buckets a check into one of the rollup groups.
type category int

const (
	catPassed category = iota
	catFailed
	catRunning
	catQueued
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
		case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
			return catFailed
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

// Summary is the JSON notification body published to pr.<n>.ci. Every status is
// always present (count 0, empty checks when none) so consumers see a stable
// schema.
type Summary struct {
	Kind    string      `json:"kind"`
	Repo    string      `json:"repo"`
	Number  string      `json:"number"`
	SHA     string      `json:"sha"`
	Failed  StatusGroup `json:"failed"`
	Running StatusGroup `json:"running"`
	Passed  StatusGroup `json:"passed"`
	Queued  StatusGroup `json:"queued"`
	Skipped StatusGroup `json:"skipped"`
}

// RenderSummary produces the JSON notification body for a commit's CI state.
// Pure and deterministic: names within each group are sorted.
func RenderSummary(s State) (string, error) {
	groups := map[category][]string{}
	for name, c := range s.Checks {
		cat := classify(c)
		groups[cat] = append(groups[cat], name)
	}
	sum := Summary{
		Kind:    "ci_summary",
		Repo:    s.Owner + "/" + s.Repo,
		Number:  s.Number,
		SHA:     s.SHA,
		Failed:  group(groups[catFailed]),
		Running: group(groups[catRunning]),
		Passed:  group(groups[catPassed]),
		Queued:  group(groups[catQueued]),
		Skipped: group(groups[catSkipped]),
	}
	buf, err := json.Marshal(sum)
	if err != nil {
		return "", err
	}
	return string(buf), nil
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
