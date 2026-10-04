package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/requiredchecks"
)

// githubRemote is a GitHub repository's clone URL as a workspace's origin names it.
var githubRemote = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:)([^/\s]+/[^/\s]+?)(?:\.git)?/?$`)

// readyChecks refuses a READY whose head GitHub will not merge for its checks: every check the
// base branch requires - its rulesets' required status checks and its branch protection's - must
// have succeeded on the pull request's head, and every workflow its rulesets require must have a
// run for the head that succeeded (requiredchecks.Required, requiredchecks.Workflows), judged by
// the rule the workflow's checks verdict judges by too (classify.Judge). A head reports none of
// them when its push skipped CI when it should not have (legion push's rule), or when the pull
// request conflicts with its base, since GitHub starts no pull_request run for a pull request it
// cannot merge; it is refused here, naming the head, the check or workflow, and the conflict once
// GitHub shows it, rather than left for GitHub to block the human merge. A required workflow
// another repository defines (an organization ruleset can require one) never matches a run here,
// since a run is matched in the repository that defines the workflow and belongs to the one it ran
// for; its refusal names that repository instead.
//
// A base branch that requires nothing has nothing to refuse, and READY is published. It says so
// on stdout rather than reading like a head whose every required check was read and passed: a
// private repository on the free plan can define no ruleset, so this is the ordinary state of the
// smoke sandbox, and a merger there has no check-based gate on the head at all.
func readyChecks(ctx context.Context, workspace string, issue paneIssue, stdout io.Writer) error {
	if issue.PullRequest == nil || issue.PullRequest.Number <= 0 {
		return fmt.Errorf("the daemon records no pull request for %s", os.Getenv("LEGION_ISSUE"))
	}
	repository, err := workspaceRepository(workspace)
	if err != nil {
		return err
	}
	response, err := redeemGrant(ctx, "/legion/v1/gh-token")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var credential githubTokenResponse
	if err := json.NewDecoder(response.Body).Decode(&credential); err != nil || credential.Token == "" {
		return fmt.Errorf("the daemon returned no GitHub token")
	}
	github := githubrest.Client{Token: credential.Token, API: githubrest.RepositoryAPI(os.Getenv("LEGION_GITHUB_API_URL"), repository)}
	var pull struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref  string `json:"ref"`
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"base"`
		// MergeableState is "dirty" while the pull request conflicts with its base.
		MergeableState string `json:"mergeable_state"`
	}
	if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", issue.PullRequest.Number), &pull); err != nil {
		return err
	}
	required, err := requiredchecks.Required(ctx, github, pull.Base.Ref)
	if err != nil {
		return err
	}
	if len(required.Checks) == 0 && len(required.Workflows) == 0 {
		fmt.Fprintf(stdout, "[handoff] no check is required on %q of %s, so READY was published without reading the head's checks\n", pull.Base.Ref, repository)
		return nil
	}
	var results map[string]string
	if len(required.Checks) > 0 {
		if results, err = headCheckResults(ctx, github, pull.Head.SHA); err != nil {
			return err
		}
	}
	var workflows []classify.Standing
	if len(required.Workflows) > 0 {
		if workflows, err = requiredchecks.Workflows(ctx, github, pull.Head.SHA, required.Workflows); err != nil {
			return err
		}
	}
	head := pull.Head.SHA
	if len(head) > 12 {
		head = head[:12]
	}
	number := issue.PullRequest.Number
	// refusal is READY's refusal for one required check or workflow's standing on the head, nil when
	// it succeeded there.
	refusal := func(check classify.Standing, what, missing string) error {
		switch name := check.Name; {
		case check.Result == classify.Missing && pull.MergeableState == "dirty":
			return fmt.Errorf("head %s of pull request #%d has %s the %s %q: the pull request conflicts with %s, and GitHub starts no pull_request CI for a pull request it cannot merge; tell the architect", head, number, missing, what, name, pull.Base.Ref)
		case check.Result == classify.Missing:
			return fmt.Errorf("head %s of pull request #%d has %s the %s %q: its push may have skipped CI when it should not have, or the pull request conflicts with %s and GitHub started no pull_request CI for it; tell the architect", head, number, missing, what, name, pull.Base.Ref)
		case check.Result == classify.Pending:
			return fmt.Errorf("the %s %q is still running on head %s of pull request #%d: wait for it to finish", what, name, head, number)
		case check.Red():
			return fmt.Errorf("the %s %q ended %s on head %s of pull request #%d", what, name, check.Result, head, number)
		}
		return nil
	}
	for _, check := range classify.Judge(required.Checks, results) {
		if err := refusal(check, "required check", "no result for"); err != nil {
			return err
		}
	}
	for i, workflow := range workflows {
		if defined := required.Workflows[i].RepositoryID; workflow.Result == classify.Missing && defined != pull.Base.Repo.ID {
			return fmt.Errorf("the required workflow %q is defined in repository %d, not in pull request #%d's own (%d): Legion matches a workflow's runs only in the repository that defines it, so it found no run of it on head %s and cannot confirm it passed; tell the architect", workflow.Name, defined, number, pull.Base.Repo.ID, head)
		}
		if err := refusal(workflow, "required workflow", "no run of"); err != nil {
			return err
		}
	}
	return nil
}

// workspaceRepository is the GitHub repository the workspace's origin names.
func workspaceRepository(workspace string) (ghrepo.Repository, error) {
	remotes, err := pushJJ(os.Getenv("LEGION_JJ_PATH"), workspace, "git", "remote", "list")
	if err != nil {
		return ghrepo.Repository{}, err
	}
	for _, line := range strings.Split(remotes, "\n") {
		name, remote, _ := strings.Cut(line, " ")
		if name != "origin" {
			continue
		}
		match := githubRemote.FindStringSubmatch(strings.TrimSpace(remote))
		if match == nil {
			return ghrepo.Repository{}, fmt.Errorf("the workspace's origin %q is not a GitHub repository", remote)
		}
		return ghrepo.Parse("the workspace's origin", match[1])
	}
	return ghrepo.Repository{}, fmt.Errorf("the workspace has no origin remote")
}

// headCheckResults is each check and commit status reported on sha, by name: success, pending, or
// the failing conclusion or state (classify.Judge's results). A check run that ended neutral or
// skipped counts as a success, as GitHub counts it for a required check.
func headCheckResults(ctx context.Context, github githubrest.Client, sha string) (map[string]string, error) {
	type run struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	runs, err := githubrest.GetListPages[run](ctx, github, "/commits/"+sha+"/check-runs", "check_runs")
	if err != nil {
		return nil, err
	}
	results := map[string]string{}
	for _, run := range runs {
		results[run.Name] = classify.RunResult(run.Status, run.Conclusion)
	}
	var combined struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := github.Get(ctx, fmt.Sprintf("/commits/%s/status?per_page=100", sha), &combined); err != nil {
		return nil, err
	}
	for _, status := range combined.Statuses {
		results[status.Context] = status.State
	}
	return results, nil
}
