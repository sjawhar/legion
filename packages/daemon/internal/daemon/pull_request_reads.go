package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// followHead is the required-checks pass's check of pr's head against pull, GitHub's own read of
// it. The daemon otherwise moves a recorded head only on the pull_request synchronize webhook
// (and a READY the merger reports), and a webhook lost, or waiting behind hours of the GitHub
// consumer's backlog, leaves reviews, READY and the CI verdict judged against an old head. When
// GitHub shows the pull request open at another head than the record holds now (the pass listed
// its records when it started, and a webhook may have moved this one since), it applies the
// intake.PullRequestSynchronized that webhook would have carried, with GitHub's updated_at as its
// clock, so the workflow decides it as it decides the webhook's (workflow's
// pullRequestSynchronized), behind the same fence: a read older than the record's clock moves
// nothing, and a webhook arriving later that is older than this read moves nothing either; one of
// the same second runs as a redelivery does, moving nothing.
//
// Before the synchronize it applies the push that reached the new head when GitHub shows it
// changed only .legion/ (handoffPush): the push webhook waits in the same backlog, and without it a
// reviewer's approval followed by its handoff push would read as not covering the head
// (classify.ApprovalStands), a round stuck for nothing. A push that may change code is left to its
// webhook, as when a synchronize beats its push: a code head with no known push is judged as such,
// and the push's own handler settles what only the pusher decides (classify.ApplyPush's planned
// mark and take-back).
//
// It returns the record as the pass goes on with it: as it stands when the head did not move, and
// otherwise as the synchronize left it, so the mergeability, review and required-check reads after
// it judge the head GitHub shows. An error, or a record no longer open, ends this pull request's
// part of the pass; the next pass reads it again.
func (w *workflowRuntime) followHead(ctx context.Context, github githubrest.Client, pr record.PullRequest, pull githubPullRequest) (record.PullRequest, error) {
	if pull.State != "open" || pull.Head.SHA == "" || pull.Head.SHA == pr.HeadSHA {
		return pr, nil
	}
	current, err := w.openRecord(ctx, pr)
	if err != nil || current.HeadSHA == pull.Head.SHA {
		return current, err
	}
	pr = current
	read, cancel := context.WithTimeout(ctx, requiredChecksRead)
	push, handoff, err := handoffPush(read, github, pr, pull.Head.SHA)
	cancel()
	if err != nil {
		return pr, err
	}
	w.log.Warn("workflow: GitHub shows a pull request at a head the daemon has not recorded; applying the synchronize its webhook would",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "recorded", pr.HeadSHA, "head", pull.Head.SHA, "updated_at", pull.UpdatedAt, "handoffOnly", handoff)
	if handoff {
		eventID := fmt.Sprintf("push-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
		if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, push, w.handlers...); err != nil {
			return pr, fmt.Errorf("apply the handoff push to %s: %w", pull.Head.SHA, err)
		}
	}
	eventID := fmt.Sprintf("head-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
	fact := intake.PullRequestSynchronized{Repo: pr.Repo, Number: pr.Number, Branch: pr.Branch, HeadSHA: pull.Head.SHA, Body: pull.Body, UpdatedAt: pull.UpdatedAt}
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		return pr, fmt.Errorf("apply the synchronize to %s: %w", pull.Head.SHA, err)
	}
	return w.openRecord(ctx, pr)
}

// openRecord is pr's issue's pull request record as it stands now, which must still be pr's and
// open.
func (w *workflowRuntime) openRecord(ctx context.Context, pr record.PullRequest) (record.PullRequest, error) {
	var current *record.PullRequest
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		current, err = w.records.PullRequest(ctx, tx, pr.Issue)
		return err
	}); err != nil {
		return pr, fmt.Errorf("read the pull request record of %s: %w", pr.Issue, err)
	}
	if current == nil || current.State != record.PullRequestOpen || current.Number != pr.Number {
		return pr, fmt.Errorf("%s records pull request #%d open no longer", pr.Issue, pr.Number)
	}
	return *current, nil
}

// githubCompare is what GET /compare/{base}...{head} says of the commits from base to head: ahead
// when head is base with commits on top, and the files they change, a renamed one under both its
// names, up to compareFilesCap.
type githubCompare struct {
	Status string `json:"status"`
	Files  []struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename"`
	} `json:"files"`
}

// compareFilesCap is the most files GitHub lists in a comparison: a list that long may be cut.
const compareFilesCap = 300

// handoffPush is the intake.Push that took pr from its recorded head to head, and true, when GitHub
// shows head is the recorded head with commits on top that change only .legion/
// (classify.ClassifyPush), as the push webhook would have carried it. Every other comparison - one
// that changes code, a rewritten history, a list GitHub may have cut, or a recorded head GitHub no
// longer has - is false, and the push is left to its webhook. The push names no pusher: the pusher
// decides only a code push's planned mark and whether a push takes back its head's counted attempt,
// which a handoff-only push takes back whoever pushed it (classify.ApplyPush).
func handoffPush(ctx context.Context, github githubrest.Client, pr record.PullRequest, head string) (intake.Push, bool, error) {
	var compare githubCompare
	if err := github.Get(ctx, fmt.Sprintf("/compare/%s...%s", url.PathEscape(pr.HeadSHA), url.PathEscape(head)), &compare); err != nil {
		var answer *githubrest.Answer
		if errors.As(err, &answer) && answer.Status == http.StatusNotFound {
			return intake.Push{}, false, nil
		}
		return intake.Push{}, false, fmt.Errorf("compare %s with %s: %w", pr.HeadSHA, head, err)
	}
	if compare.Status != "ahead" || len(compare.Files) == 0 || len(compare.Files) >= compareFilesCap {
		return intake.Push{}, false, nil
	}
	paths := make([]string, 0, len(compare.Files))
	for _, file := range compare.Files {
		paths = append(paths, file.Filename)
		if file.PreviousFilename != "" {
			paths = append(paths, file.PreviousFilename)
		}
	}
	changed, whole, unforced := strings.Join(paths, "\n"), "false", "false"
	if !classify.ClassifyPush(classify.PushPayload{ChangedPaths: &changed, ChangedPathsTruncated: &whole}).HandoffOnly {
		return intake.Push{}, false, nil
	}
	return intake.Push{Repo: pr.Repo, Branch: pr.Branch, Before: pr.HeadSHA, After: head, ChangedPaths: &changed, Truncated: &whole, Forced: &unforced}, true, nil
}

// readUnrecordedPullRequests is the required-checks pass's look for a pull request the daemon has
// no record of. An implementer's completion moves its issue to testing only once the issue's pull
// request is recorded (workflow's implementationReady), and only the opened or reopened webhook
// records it, so one lost, or waiting behind the GitHub consumer's backlog, holds the issue in
// implementing with its pull request open on GitHub. For each issue in implementing whose
// implementer recorded its handoff and that has no open or merged pull request recorded
// (record.Store.HandedOffWithoutPullRequest) it lists the open pull requests on the issue's branch
// (claim.IssueBranch) and applies the intake.PullRequestOpened that webhook would have carried,
// naming the repository as the webhook's fact would (heardSpelling) - a reopen when the record
// holds the same pull request closed - which the workflow decides as it decides the webhook's
// (workflow's pullRequestOpened): it records the pull request and moves the issue on. An issue
// whose implementer has not completed costs no read. ctx bounds the pass; a failed read is logged
// and the next pass asks again, and a rate-limit answer ends the look there.
func (w *workflowRuntime) readUnrecordedPullRequests(ctx context.Context) {
	var waiting []record.Issue
	recorded := map[string]*record.PullRequest{}
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		if waiting, err = w.records.HandedOffWithoutPullRequest(ctx, tx, w.dispatchProject); err != nil {
			return err
		}
		for _, issue := range waiting {
			if recorded[issue.Key], err = w.records.PullRequest(ctx, tx, issue.Key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		if ctx.Err() == nil {
			w.log.Warn("list the issues whose implementer completed with no pull request recorded", "error", err)
		}
		return
	}
	for _, issue := range waiting {
		branch := claim.IssueBranch(issue.Key)
		read, cancel := context.WithTimeout(ctx, requiredChecksRead)
		pull, found, err := w.openPullRequestOn(read, issue.GitHubRepo, branch)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			attributes := []any{"repo", issue.GitHubRepo, "issue", issue.Key, "branch", branch, "error", err}
			var answer *githubrest.Answer
			if errors.As(err, &answer) {
				attributes = append(attributes, "status", answer.Status)
			}
			if answer != nil && answer.RateLimited {
				w.log.Error("list the open pull requests on an issue's branch: GitHub's rate limit; this pass looks up no more", attributes...)
				return
			}
			w.log.Error("list the open pull requests on an issue's branch; a later pass looks again", attributes...)
			continue
		}
		if !found {
			continue
		}
		prior, repo := recorded[issue.Key], w.heardSpelling(issue.GitHubRepo)
		fact := intake.PullRequestOpened{Repo: repo, Number: pull.Number, Branch: branch, HeadSHA: pull.Head.SHA, Body: pull.Body,
			URL: pull.HTMLURL, UpdatedAt: pull.UpdatedAt, Reopened: prior != nil && prior.Number == pull.Number}
		w.log.Warn("workflow: GitHub holds an open pull request for an issue whose implementer completed, and the daemon has none recorded; applying the opened its webhook would",
			"repo", repo, "pullRequest", pull.Number, "issue", issue.Key, "head", pull.Head.SHA, "reopened", fact.Reopened, "updated_at", pull.UpdatedAt)
		eventID := fmt.Sprintf("opened-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, repo, pull.Number, time.Now().UnixNano())
		if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
			w.log.Warn("apply an open pull request read from GitHub", "repo", repo, "pullRequest", pull.Number, "issue", issue.Key, "error", err)
		}
	}
}

// heardSpelling is repo as GitHub intake hears it, the spelling a webhook's fact names it by and a
// pull request is recorded and looked up under: a served repository as the mapping names it, and
// any other as its record keeps GitHub's spelling (heardRepositories).
func (w *workflowRuntime) heardSpelling(repo string) string {
	if w.repositories != nil {
		for _, served := range w.repositories.Accepted() {
			if strings.EqualFold(served.String(), repo) {
				return served.String()
			}
		}
	}
	return repo
}

// openPullRequestOn is the open pull request of repo whose head is branch in repo itself (GET
// /pulls?head=<owner>:<branch>&state=open), and whether there is one. The workflow keeps one pull
// request per branch and assumes one is open at a time, so of two it takes the first GitHub lists.
func (w *workflowRuntime) openPullRequestOn(ctx context.Context, repo, branch string) (githubPullRequest, bool, error) {
	repository, err := ghrepo.Parse("the issue's repository", repo)
	if err != nil {
		return githubPullRequest{}, false, err
	}
	github, err := w.implementClient(ctx, repository)
	if err != nil {
		return githubPullRequest{}, false, err
	}
	var pulls []githubPullRequest
	query := url.Values{"head": {repository.Owner() + ":" + branch}, "state": {"open"}}
	if err := github.Get(ctx, "/pulls?"+query.Encode(), &pulls); err != nil {
		return githubPullRequest{}, false, err
	}
	if len(pulls) == 0 {
		return githubPullRequest{}, false, nil
	}
	return pulls[0], true, nil
}
