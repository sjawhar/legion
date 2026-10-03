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
	"github.com/sjawhar/legion/daemon/internal/requiredchecks"
)

// githubRemote is a GitHub repository's clone URL as a workspace's origin names it.
var githubRemote = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:)([^/\s]+/[^/\s]+?)(?:\.git)?/?$`)

// readyChecks refuses a READY whose head GitHub will not merge for its checks: every check the
// base branch requires - its rulesets' required status checks and its branch protection's
// (requiredchecks.Required) - must have succeeded on the pull request's head, judged by the rule
// the workflow's checks verdict judges by too (classify.Judge). A head reports none of them when
// its push skipped CI when it should not have (legion push's rule), or when the pull request
// conflicts with its base, since GitHub starts no pull_request run for a pull request it cannot
// merge; it is refused here, naming the head, the check and which of the two GitHub shows, rather
// than left for GitHub to block the human merge.
//
// A base branch that requires no check has nothing to refuse, and READY is published. It says so
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
	github := requiredchecks.GitHub{Token: credential.Token, API: requiredchecks.RepositoryAPI(os.Getenv("LEGION_GITHUB_API_URL"), repository)}
	var pull struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
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
	if len(required) == 0 {
		fmt.Fprintf(stdout, "[handoff] no check is required on %q of %s, so READY was published without reading the head's checks\n", pull.Base.Ref, repository)
		return nil
	}
	results, err := headCheckResults(ctx, github, pull.Head.SHA)
	if err != nil {
		return err
	}
	head := pull.Head.SHA
	if len(head) > 12 {
		head = head[:12]
	}
	for _, check := range classify.Judge(required, results) {
		switch name := check.Name; {
		case check.Result == classify.Missing && pull.MergeableState == "dirty":
			return fmt.Errorf("head %s of pull request #%d has no result for the required check %q: the pull request conflicts with %s, and GitHub starts no pull_request CI for a pull request it cannot merge; tell the architect", head, issue.PullRequest.Number, name, pull.Base.Ref)
		case check.Result == classify.Missing:
			return fmt.Errorf("head %s of pull request #%d has no result for the required check %q: its push may have skipped CI when it should not have; tell the architect", head, issue.PullRequest.Number, name)
		case check.Result == classify.Pending:
			return fmt.Errorf("the required check %q is still running on head %s of pull request #%d: wait for it to finish", name, head, issue.PullRequest.Number)
		case check.Red():
			return fmt.Errorf("the required check %q ended %s on head %s of pull request #%d", name, check.Result, head, issue.PullRequest.Number)
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
func headCheckResults(ctx context.Context, github requiredchecks.GitHub, sha string) (map[string]string, error) {
	results := map[string]string{}
	for page := 1; ; page++ {
		var runs struct {
			TotalCount int `json:"total_count"`
			CheckRuns  []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := github.Get(ctx, fmt.Sprintf("/commits/%s/check-runs?per_page=100&page=%d", sha, page), &runs); err != nil {
			return nil, err
		}
		for _, run := range runs.CheckRuns {
			switch {
			case run.Status != "completed":
				results[run.Name] = classify.Pending
			case run.Conclusion == "success" || run.Conclusion == "neutral" || run.Conclusion == "skipped":
				results[run.Name] = classify.Success
			default:
				results[run.Name] = run.Conclusion
			}
		}
		if len(runs.CheckRuns) == 0 || page*100 >= runs.TotalCount {
			break
		}
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
