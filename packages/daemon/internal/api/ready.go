package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/requiredchecks"
)

// refusal is a completion's refusal before its fact is applied: the status, code and sentence
// writeFailure answers with. Nothing is recorded under the completion's key, so the corrected call
// at the same head is applied rather than answered HANDOFF_ALREADY_RECORDED.
type refusal struct {
	status        int
	code, message string
}

func (r *refusal) write(w http.ResponseWriter) { writeFailure(w, r.status, r.code, r.message) }

// githubReadFailed is the refusal of a completion GitHub failed to answer a read for: GitHub's
// failure, not the head's, so the worker completes again. Nothing retries in the daemon; a GitHub
// outage holds every completion until it ends, as it holds every webhook-driven transition.
func githubReadFailed(err error) *refusal {
	return &refusal{http.StatusBadGateway, "GITHUB_READ_FAILED", fmt.Sprintf("%v; GitHub's failure, not the head's: complete again", err)}
}

// readyChecks refuses a READY whose head GitHub will not merge for its checks, or whose merge would
// carry the issue's handoffs onto the base branch. A pull request already merged has nothing left to
// gate: a person merged it before READY, which the workflow takes on to the production check, and no
// commit can change its head, so READY is published without reading it. Otherwise the head must not
// hold .legion/<issue>/ (headCarries), which retro's last commit removes (dispatch://LEGION-605): a
// squash merge commits the head merged into the base, and nothing on the base branch reads a
// handoff. A read GitHub fails is refused as GitHub's failure, to retry, not the head's. Then every
// check the base branch requires - its rulesets' required status checks and its branch
// protection's - must have succeeded on the pull request's head, and every workflow its rulesets
// require must have a run for the head that succeeded (requiredchecks.Required,
// requiredchecks.Workflows), judged by the rule the workflow's checks verdict judges by too
// (classify.Judge). A head reports none of them when its push skipped CI when it should not have
// (a `skip-checks: true` trailer on a push that touched more than .legion/), or when the pull
// request conflicts with its base, since GitHub starts no pull_request run for a pull request it
// cannot merge; it is refused here, naming the head, the check or workflow, and the conflict once
// GitHub shows it, rather than left for GitHub to block the human merge. A required workflow
// another repository defines (an organization ruleset can require one) never matches a run here,
// since a run is matched in the repository that defines the workflow and belongs to the one it ran
// for; its refusal names that repository instead.
//
// A base branch that requires no check has no check to refuse, and READY is published once the head
// carries no handoffs. The note says so rather than reading like a head whose every required check
// was read and passed: a private repository on the free plan can define no ruleset, so this is the
// ordinary state of the smoke sandbox, and a merger there has no check-based gate on the head at
// all. The note is the completion's answer (HandoffCompleteResponse.Note).
func readyChecks(ctx context.Context, github githubrest.Client, repository ghrepo.Repository, issue string, pr *record.PullRequest) (note string, refused *refusal) {
	if pr == nil || pr.Number <= 0 {
		return "", &refusal{http.StatusConflict, "NO_PULL_REQUEST", fmt.Sprintf("%s has no pull request recorded", issue)}
	}
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
	if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", pr.Number), &pull); err != nil {
		return "", githubReadFailed(err)
	}
	head := shortSHA(pull.Head.SHA)
	number := pr.Number
	if pull.Merged {
		return fmt.Sprintf("pull request #%d is already merged, so READY was published without reading its head's checks or handoffs", number), nil
	}
	dir := handoffDir(issue)
	switch carries, err := headCarries(ctx, github, dir, pull.Head.SHA); {
	case err != nil:
		return "", &refusal{http.StatusBadGateway, "GITHUB_READ_FAILED", fmt.Sprintf("GitHub's read of %s/ at head %s of pull request #%d failed, so whether the head still carries it is unknown: complete again; this is GitHub's failure, not the head's: %v", dir, head, number, err)}
	case carries:
		return "", &refusal{http.StatusConflict, "READY_HEAD_CARRIES_HANDOFFS", fmt.Sprintf("head %s of pull request #%d still carries %s/, this issue's handoffs, which its merge would carry onto the base branch: retro's last commit removes them, so the issue goes back to retro; tell the architect", head, number, dir)}
	}
	// A pull request GitHub cannot merge gets no pull_request run and no merge: it is refused by
	// name, before its checks are read, since a missing check would otherwise be reported as the
	// cause. GitHub reports the conflict as mergeable_state "dirty" (computed shortly after each
	// push; "unknown" while it computes, which the checks below then judge as they stand).
	if pull.MergeableState == "dirty" {
		return "", &refusal{http.StatusConflict, "READY_HEAD_CONFLICTS", fmt.Sprintf("head %s of pull request #%d conflicts with its base %s, which GitHub cannot merge and starts no pull_request CI for: the implementer brings %s into the branch with a forward merge; tell the architect", head, number, pull.Base.Ref, pull.Base.Ref)}
	}
	required, err := requiredchecks.Required(ctx, github, pull.Base.Ref)
	if err != nil {
		return "", githubReadFailed(err)
	}
	if len(required.Checks) == 0 && len(required.Workflows) == 0 {
		return fmt.Sprintf("no check is required on %q of %s, so READY was published without reading the head's checks", pull.Base.Ref, repository), nil
	}
	var results map[string]string
	if len(required.Checks) > 0 {
		if results, err = headCheckResults(ctx, github, pull.Head.SHA); err != nil {
			return "", githubReadFailed(err)
		}
	}
	var workflows []requiredchecks.WorkflowRun
	if len(required.Workflows) > 0 {
		if workflows, err = requiredchecks.Workflows(ctx, github, pull.Head.SHA, required.Workflows); err != nil {
			return "", githubReadFailed(err)
		}
	}
	// notGreen is READY's refusal for one required check or workflow's standing on the head, nil
	// when it succeeded there.
	notGreen := func(check classify.Standing, what, missing string) *refusal {
		var message string
		switch name := check.Name; {
		case check.Result == classify.Missing:
			message = fmt.Sprintf("head %s of pull request #%d has %s the %s %q: its push may have skipped CI when it should not have; tell the architect", head, number, missing, what, name)
		case check.Result == classify.Pending:
			message = fmt.Sprintf("the %s %q is still running on head %s of pull request #%d: wait for it to finish", what, name, head, number)
		case check.Red():
			message = fmt.Sprintf("the %s %q ended %s on head %s of pull request #%d", what, name, check.Result, head, number)
		default:
			return nil
		}
		return &refusal{http.StatusConflict, "READY_CHECKS_NOT_GREEN", message}
	}
	for _, check := range classify.Judge(required.Checks, results) {
		if refused := notGreen(check, "required check", "no result for"); refused != nil {
			return "", refused
		}
	}
	for _, workflow := range workflows {
		if workflow.Result == classify.Missing && workflow.RepositoryID != pull.Base.Repo.ID {
			return "", &refusal{http.StatusConflict, "READY_CHECKS_NOT_GREEN", fmt.Sprintf("the required workflow %q is defined in repository %d, not in pull request #%d's own (%d): Legion matches a workflow's runs only in the repository that defines it, so it found no run of it on head %s and cannot confirm it passed; tell the architect", workflow.Path, workflow.RepositoryID, number, pull.Base.Repo.ID, head)}
		}
		if refused := notGreen(workflow.Standing(), "required workflow", "no run of"); refused != nil {
			return "", refused
		}
	}
	return "", nil
}

// shortSHA is sha as a refusal names it, its first twelve characters.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// headCarries is whether head holds dir, a directory relative to the repository root: GitHub's
// contents read of that path at head answers 404 when it does not. Any other failure of the read is
// an error, which leaves the answer unknown.
func headCarries(ctx context.Context, github githubrest.Client, dir, head string) (bool, error) {
	err := github.Get(ctx, "/contents/"+dir+"?ref="+url.QueryEscape(head), nil)
	var answer *githubrest.Answer
	if errors.As(err, &answer) && answer.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// handoffDir is the directory issue's handoffs live under, relative to the repository root, with no
// trailing slash: .legion/<issue>/<phase>.json is written there by each file-backed phase's role,
// and retro's last commit removes the directory from the head a human merges, so none reaches main
// (dispatch://LEGION-605). READY reads whether the head still carries it (headCarries); the daemon
// reads nothing inside it.
func handoffDir(issue string) string { return ".legion/" + issue }

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
