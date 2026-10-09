package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/requiredchecks"
)

// githubRemote is a GitHub repository's clone URL as a workspace's origin names it.
var githubRemote = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:)([^/\s]+/[^/\s]+?)(?:\.git)?/?$`)

// readyChecks refuses a READY whose head GitHub will not merge for its checks, or whose merge would
// carry the issue's handoffs onto the base branch. A pull request already merged has nothing left to
// gate: a person merged it before READY, which the workflow takes on to the production check, and no
// commit can change its head, so READY is published without reading it. Otherwise the head must not
// hold .legion/<issue>/ (headCarries), which retro's last commit removes (dispatch://LEGION-605): a
// squash merge commits the head merged into the base, and nothing on the base branch reads a
// handoff. A read GitHub fails is refused as GitHub's failure, to retry, not the head's. Then every
// check the base branch requires - its
// rulesets' required status checks and its branch protection's - must have succeeded on the pull
// request's head, and every workflow its rulesets require must have a run for the head that
// succeeded (requiredchecks.Required, requiredchecks.Workflows), judged by the rule the workflow's
// checks verdict judges by too (classify.Judge). A head reports none of them when its push skipped
// CI when it should not have (legion push's rule), or when the pull request conflicts with its base,
// since GitHub starts no pull_request run for a pull request it cannot merge; it is refused here,
// naming the head, the check or workflow, and the conflict once GitHub shows it, rather than left for
// GitHub to block the human merge. A required workflow another repository defines (an organization
// ruleset can require one) never matches a run here, since a run is matched in the repository that
// defines the workflow and belongs to the one it ran for; its refusal names that repository instead.
//
// A base branch that requires no check has no check to refuse, and READY is published once the head
// carries no handoffs. It says so on stdout rather than reading like a head whose every required
// check was read and passed: a private repository on the free plan can define no ruleset, so this is
// the ordinary state of the smoke sandbox, and a merger there has no check-based gate on the head at
// all.
func readyChecks(ctx context.Context, workspace string, issue paneIssue, stdout io.Writer) error {
	if issue.PullRequest == nil || issue.PullRequest.Number <= 0 {
		return fmt.Errorf("the daemon records no pull request for %s", issue.Key)
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
		Merged         bool   `json:"merged"`
	}
	if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", issue.PullRequest.Number), &pull); err != nil {
		return err
	}
	head := pull.Head.SHA
	if len(head) > 12 {
		head = head[:12]
	}
	number := issue.PullRequest.Number
	if pull.Merged {
		fmt.Fprintf(stdout, "[handoff] pull request #%d is already merged, so READY was published without reading its head's checks or handoffs\n", number)
		return nil
	}
	dir := filepath.ToSlash(handoffFile(issue.Key, "")) + "/"
	switch carries, err := headCarries(ctx, github, dir, pull.Head.SHA); {
	case err != nil:
		return fmt.Errorf("GitHub's read of %s at head %s of pull request #%d failed, so whether the head still carries it is unknown: complete again; this is GitHub's failure, not the head's: %w", dir, head, number, err)
	case carries:
		return fmt.Errorf("head %s of pull request #%d still carries %s, this issue's handoffs, which its merge would carry onto the base branch: retro's last commit removes them, so the issue goes back to retro; tell the architect", head, number, dir)
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
	var workflows []requiredchecks.WorkflowRun
	if len(required.Workflows) > 0 {
		if workflows, err = requiredchecks.Workflows(ctx, github, pull.Head.SHA, required.Workflows); err != nil {
			return err
		}
	}
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
	for _, workflow := range workflows {
		if workflow.Result == classify.Missing && workflow.RepositoryID != pull.Base.Repo.ID {
			return fmt.Errorf("the required workflow %q is defined in repository %d, not in pull request #%d's own (%d): Legion matches a workflow's runs only in the repository that defines it, so it found no run of it on head %s and cannot confirm it passed; tell the architect", workflow.Path, workflow.RepositoryID, number, pull.Base.Repo.ID, head)
		}
		if err := refusal(workflow.Standing(), "required workflow", "no run of"); err != nil {
			return err
		}
	}
	return nil
}

// headCarries is whether head holds dir, a directory relative to the repository root: GitHub's
// contents read of that path at head answers 404 when it does not. Any other failure of the read is
// an error, which leaves the answer unknown.
func headCarries(ctx context.Context, github githubrest.Client, dir, head string) (bool, error) {
	err := github.Get(ctx, "/contents/"+strings.TrimSuffix(dir, "/")+"?ref="+url.QueryEscape(head), nil)
	var answer *githubrest.Answer
	if errors.As(err, &answer) && answer.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
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
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	runs, err := githubrest.GetListPages[run](ctx, github, "/commits/"+sha+"/check-runs", "check_runs")
	if err != nil {
		return nil, err
	}
	results := map[string]string{}
	// GitHub documents no ordering for this list (unlike the combined-status endpoint below,
	// whose docs guarantee "the most recent status for each context"), and a cancelled run
	// superseded by a concurrency group's newer run can list after the newer run's own success
	// (pr-title.yaml's `edited` re-trigger after a concurrency-group cancellation is one way this
	// happens). Keeping whichever run a name is seen last in the list would then let a cancelled
	// run overwrite a later success. Check run IDs are assigned at creation and never reused, so
	// the highest ID per name is always its most recent run, regardless of list order.
	latestID := map[string]int64{}
	for _, run := range runs {
		if prevID, seen := latestID[run.Name]; seen && prevID >= run.ID {
			continue
		}
		latestID[run.Name] = run.ID
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
