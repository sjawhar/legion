// Package requiredchecks reads what a pull request's base branch requires before GitHub merges into
// it (Required) - its required status checks and the workflows its rulesets require to succeed -
// and how each required workflow's run stands on a head (Workflows): the one reader both the
// merger's READY (`legion handoff complete --ready`) and the daemon's workflow use, since only what
// the base branch requires decides whether CI is red at a head (classify.Judge). What each judges
// the required checks against is its own: READY the head's check runs and commit statuses as GitHub
// reports them, the workflow the Envoy settlement that stands for the head.
package requiredchecks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// Set is what a base branch requires: Checks, every required status check name of the rulesets
// that apply to it and of its branch protection, and Workflows, every workflow its rulesets'
// workflows rules require to succeed. Each is sorted and holds each once.
type Set struct {
	Checks    []string
	Workflows []Workflow
}

// Workflow is one workflow a ruleset's workflows rule requires: the file at Path in the repository
// whose id is RepositoryID. GitHub merges a pull request only once that workflow's run for its head
// succeeded, so the rule's ref and sha, which pick the version of the file that runs, do not decide
// it.
type Workflow struct {
	Path         string `json:"path"`
	RepositoryID int64  `json:"repository_id"`
}

// Required is what base requires: the required status checks and required workflows of the
// rulesets that apply to it, every page of them, and the required status checks of its branch
// protection. A repository whose plan has no rulesets has none of the first (rulesetsUnavailable).
// An empty answer is a base that requires nothing. A branch GitHub calls protected but answers
// with no protection summary is an error rather than a branch that requires nothing, so a reader
// never fails open on an answer it cannot read.
func Required(ctx context.Context, github githubrest.Client, base string) (Set, error) {
	// A branch name's slashes stay path segments, as GitHub's branch routes take them.
	branch := strings.ReplaceAll(url.PathEscape(base), "%2F", "/")
	type rule struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
			Workflows []Workflow `json:"workflows"`
		} `json:"parameters"`
	}
	rules, err := githubrest.GetPages[rule](ctx, github, "/rules/branches/"+branch)
	if err != nil && !rulesetsUnavailable(err) {
		return Set{}, err
	}
	var branchAnswer struct {
		Protected  bool `json:"protected"`
		Protection *struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
				Checks   []struct {
					Context string `json:"context"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if err := github.Get(ctx, "/branches/"+branch, &branchAnswer); err != nil {
		return Set{}, err
	}
	if branchAnswer.Protected && branchAnswer.Protection == nil {
		return Set{}, fmt.Errorf("GitHub calls %s protected but answered GET /branches/%s with no protection summary", base, branch)
	}
	set := Set{Checks: []string{}, Workflows: []Workflow{}}
	for _, rule := range rules {
		switch rule.Type {
		case "required_status_checks":
			for _, check := range rule.Parameters.RequiredStatusChecks {
				set.Checks = append(set.Checks, check.Context)
			}
		case "workflows":
			set.Workflows = append(set.Workflows, rule.Parameters.Workflows...)
		}
	}
	if protection := branchAnswer.Protection; protection != nil {
		set.Checks = append(set.Checks, protection.RequiredStatusChecks.Contexts...)
		for _, check := range protection.RequiredStatusChecks.Checks {
			set.Checks = append(set.Checks, check.Context)
		}
	}
	slices.Sort(set.Checks)
	set.Checks = slices.Compact(set.Checks)
	slices.SortFunc(set.Workflows, func(a, b Workflow) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.RepositoryID, b.RepositoryID))
	})
	set.Workflows = slices.Compact(set.Workflows)
	return set, nil
}

// WorkflowRun is how one required workflow (Workflow) stands on a head: its latest run's Result, as
// classify judges a check's, and which run that is, its id (Run) and attempt (Attempt), both zero
// when the head has no run of it. A re-run keeps its run's id and raises its attempt.
type WorkflowRun struct {
	Workflow
	Result  string
	Run     int64
	Attempt int
}

// Standing is the run's result as classify judges a required check's, named by the workflow's path.
func (r WorkflowRun) Standing() classify.Standing {
	return classify.Standing{Name: r.Path, Result: r.Result}
}

// Workflows is how each required workflow's run stands on sha, one WorkflowRun for each workflow in
// workflows, in order: the latest run (the highest id; a re-run keeps its run's id and reports its
// newest attempt) that a pull_request or pull_request_target event started for that file of that
// repository. A run that ended success, neutral or skipped is classify.Success, as GitHub counts a
// check run for a required check; one still queued or running is classify.Pending; one that ended
// any other way is red with its conclusion (failure, cancelled, timed_out, action_required,
// startup_failure); and a workflow the head has no such run of is classify.Missing. A run lives in
// the repository it ran for, so a workflow another repository defines never matches one, and is
// Missing. It reads the head's runs whatever workflows names, so a caller with no workflow required
// does not call it.
func Workflows(ctx context.Context, github githubrest.Client, sha string, workflows []Workflow) ([]WorkflowRun, error) {
	type run struct {
		ID         int64  `json:"id"`
		Attempt    int    `json:"run_attempt"`
		Path       string `json:"path"`
		Event      string `json:"event"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Repository struct {
			ID int64 `json:"id"`
		} `json:"repository"`
	}
	runs, err := githubrest.GetListPages[run](ctx, github, "/actions/runs?head_sha="+url.QueryEscape(sha), "workflow_runs")
	if err != nil {
		return nil, err
	}
	standings := make([]WorkflowRun, 0, len(workflows))
	for _, workflow := range workflows {
		var latest *run
		for i, candidate := range runs {
			if candidate.Path == workflow.Path && candidate.Repository.ID == workflow.RepositoryID &&
				(candidate.Event == "pull_request" || candidate.Event == "pull_request_target") && (latest == nil || candidate.ID > latest.ID) {
				latest = &runs[i]
			}
		}
		standing := WorkflowRun{Workflow: workflow, Result: classify.Missing}
		if latest != nil {
			standing.Result, standing.Run, standing.Attempt = classify.RunResult(latest.Status, latest.Conclusion), latest.ID, latest.Attempt
		}
		standings = append(standings, standing)
	}
	return standings, nil
}

// rulesetsUnavailable is GitHub's answer to the rulesets read of a private repository whose plan
// has no rulesets: 403, its message ending "make this repository public to enable this feature"
// (docs/solutions/legion/controller-gate-2-required-checks-live-reads.md, observed on
// sjawhar/legion-smoke). Such a repository can define no ruleset, so no ruleset requires a check.
func rulesetsUnavailable(err error) bool {
	var answer *githubrest.Answer
	return errors.As(err, &answer) && answer.Status == http.StatusForbidden && strings.Contains(answer.Body, "make this repository public to enable this feature")
}
