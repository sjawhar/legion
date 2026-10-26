package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// followHead is the required-checks pass's check of pr's head against pull, GitHub's own read of
// it. The daemon otherwise moves a recorded head only on the pull_request synchronize webhook
// (and a READY the merger reports), and a webhook lost, or waiting behind hours of the GitHub
// consumer's backlog, leaves reviews, READY and the CI verdict judged against an old head. When
// GitHub shows the pull request open at another head it applies the intake.PullRequestSynchronized
// that webhook would have carried, with GitHub's updated_at as its clock, so the workflow decides
// it as it decides the webhook's (workflow's pullRequestSynchronized), behind the same fence: a
// read older than the record's clock moves nothing, and the webhook arriving later is older than
// this read and moves nothing either. The push of the new head still comes only by webhook, so in
// reviewing the round is asked again once that push is known, as when a synchronize beats its push.
//
// It returns the record as the pass goes on with it: pr itself when the head did not move, and
// otherwise the record the synchronize left, read again, so the mergeability, review and
// required-check reads after it judge the head GitHub shows. An error, or a record no longer open
// after the synchronize, ends this pull request's part of the pass; the next pass reads it again.
func (w *workflowRuntime) followHead(ctx context.Context, pr record.PullRequest, pull githubPullRequest) (record.PullRequest, error) {
	if pull.State != "open" || pull.Head.SHA == "" || pull.Head.SHA == pr.HeadSHA {
		return pr, nil
	}
	w.log.Warn("workflow: GitHub shows a pull request at a head the daemon has not recorded; applying the synchronize its webhook would",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "recorded", pr.HeadSHA, "head", pull.Head.SHA, "updated_at", pull.UpdatedAt)
	eventID := fmt.Sprintf("head-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
	fact := intake.PullRequestSynchronized{Repo: pr.Repo, Number: pr.Number, Branch: pr.Branch, HeadSHA: pull.Head.SHA, Body: pull.Body, UpdatedAt: pull.UpdatedAt}
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		return pr, fmt.Errorf("apply the synchronize to %s: %w", pull.Head.SHA, err)
	}
	var current *record.PullRequest
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		current, err = w.records.PullRequest(ctx, tx, pr.Issue)
		return err
	}); err != nil {
		return pr, fmt.Errorf("read the pull request record the synchronize left: %w", err)
	}
	if current == nil || current.State != record.PullRequestOpen || current.Number != pr.Number {
		return pr, errors.New("after the synchronize the issue records this pull request open no longer")
	}
	return *current, nil
}

// readUnrecordedPullRequests is the required-checks pass's look for a pull request the daemon has
// no record of. An implementer's completion moves its issue to testing only once the issue's pull
// request is recorded (workflow's implementationReady), and only the opened or reopened webhook
// records it, so one lost, or waiting behind the GitHub consumer's backlog, holds the issue in
// implementing with its pull request open on GitHub. For each issue in implementing whose
// implementer recorded its handoff and that has no open pull request recorded
// (record.Store.HandedOffWithoutPullRequest) it lists the open pull requests on the issue's branch
// (claim.IssueBranch) and applies the intake.PullRequestOpened that webhook would have carried -
// a reopen when the record holds the same pull request closed - which the workflow decides as it
// decides the webhook's (workflow's pullRequestOpened): it records the pull request and moves the
// issue on. An issue whose implementer has not completed costs no read. ctx bounds the pass; a
// failed read is logged and the next pass asks again, and a rate-limit answer ends the look there.
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
		prior := recorded[issue.Key]
		fact := intake.PullRequestOpened{Repo: issue.GitHubRepo, Number: pull.Number, Branch: branch, HeadSHA: pull.Head.SHA, Body: pull.Body,
			URL: pull.HTMLURL, UpdatedAt: pull.UpdatedAt, Reopened: prior != nil && prior.Number == pull.Number}
		w.log.Warn("workflow: GitHub holds an open pull request for an issue whose implementer completed, and the daemon has none recorded; applying the opened its webhook would",
			"repo", issue.GitHubRepo, "pullRequest", pull.Number, "issue", issue.Key, "head", pull.Head.SHA, "reopened", fact.Reopened, "updated_at", pull.UpdatedAt)
		eventID := fmt.Sprintf("opened-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, issue.GitHubRepo, pull.Number, time.Now().UnixNano())
		if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
			w.log.Warn("apply an open pull request read from GitHub", "repo", issue.GitHubRepo, "pullRequest", pull.Number, "issue", issue.Key, "error", err)
		}
	}
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
