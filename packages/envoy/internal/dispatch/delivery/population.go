// Population and rework rules (LEGION-294, ported from the delivery-timeline prototype's
// collector/src/collector/population.py; CONTRACT.md "Population and window"). These are pure
// functions over data already gathered from GitHub -- no Postgres, no NATS, no HTTP here.
package delivery

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// TimeWindow is a half-open [Start, End) interval.
type TimeWindow struct {
	Start, End time.Time
}

// Contains reports whether t falls inside the window: Start <= t && t < End.
func (w TimeWindow) Contains(t time.Time) bool {
	return !t.Before(w.Start) && t.Before(w.End)
}

// RawPullRequest is everything the population and rework rules need, gathered from GitHub (the
// webhook envelope and/or the completing fetch) BEFORE the decision to store anything -- a PR
// that fails IsPopulationPR is never written to delivery_pull_requests at all.
type RawPullRequest struct {
	Repo     string
	Number   int
	Title    string
	Author   string
	Labels   []string // from the GitHub pulls API response; only meaningful for the configured deploy repo
	MergedAt time.Time
}

// fixTitle matches a rework title: starts with fix, revert or hotfix, case-insensitive, word
// boundary.
var fixTitle = regexp.MustCompile(`(?i)^(fix|revert|hotfix)\b`)

// revertTitle matches a revert title: starts with revert, case-insensitive, word boundary.
var revertTitle = regexp.MustCompile(`(?i)^revert\b`)

// IsPopulationPR reports whether pr belongs to LEGION-294's population: pr.Repo is not one of
// settings.ExcludedRepos, pr.Author is one of settings.PopulationAuthors (exact string match --
// the caller is responsible for passing author strings in GitHub's own form, [bot] suffix
// included where GitHub adds one, matching settings.PopulationAuthors entries exactly), pr.MergedAt
// is inside window, and pr is not a task PR per IsTaskPR for the configured deploy repo.
func IsPopulationPR(pr RawPullRequest, settings DeliverySettings, window TimeWindow) (bool, error) {
	for _, excluded := range settings.ExcludedRepos {
		if pr.Repo == excluded {
			return false, nil
		}
	}

	authorInPopulation := false
	for _, author := range settings.PopulationAuthors {
		if pr.Author == author {
			authorInPopulation = true
			break
		}
	}
	if !authorInPopulation {
		return false, nil
	}

	if !window.Contains(pr.MergedAt) {
		return false, nil
	}

	isTask, err := IsTaskPR(pr.Repo, pr.Number, pr.Labels, settings)
	if err != nil {
		return false, err
	}
	return !isTask, nil
}

// IsTaskPR reports LEGION-294's task-PR rule, scoped to the configured deploy repository: true
// when "task" is among pr's labels, false when "non-task" is, and an error naming the PR when it
// is a deploy-repo PR with NEITHER label (an unclassified PR -- surfacing this loudly is the
// point: the Python reference's is_task_pr raises for exactly this case rather than guessing). A
// PR outside the deploy repository is never a task PR and never errors.
//
// The Python reference this is ported from (the weekly-review skill's common.is_task_pr) has a
// second branch for a different, file-path-based task rule on a second repository; LEGION-294's
// own population rule defines "task PR" only for the configured deploy repository, so that second
// branch is not ported here -- naming the gap rather than silently porting an unrequested second
// repository's rule or silently dropping it without a trace.
func IsTaskPR(repo string, number int, labels []string, settings DeliverySettings) (bool, error) {
	if repo != settings.DeployRepo {
		return false, nil
	}

	hasTask := false
	hasNonTask := false
	for _, label := range labels {
		switch label {
		case "task":
			hasTask = true
		case "non-task":
			hasNonTask = true
		}
	}

	if hasTask {
		return true, nil
	}
	if hasNonTask {
		return false, nil
	}

	return false, fmt.Errorf("delivery: unclassified pull request %s#%d has neither task nor non-task label", repo, number)
}

// IsRework reports the rework (fix/revert/hotfix) title rule: title starts with fix, revert or
// hotfix, case-insensitive, word boundary. Rework PRs are never counted as delivered value.
func IsRework(title string) bool {
	return fixTitle.MatchString(strings.TrimSpace(title))
}

// RevertKind is "revert" when title matches ^revert\b (case-insensitive), else "fix" (a hotfix
// title flags as "fix": hotfix is a fix that shipped fast, not a distinct flag kind).
func RevertKind(title string) string {
	if revertTitle.MatchString(strings.TrimSpace(title)) {
		return "revert"
	}
	return "fix"
}
