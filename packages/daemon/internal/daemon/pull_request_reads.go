package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
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
// GitHub shows the pull request open at another head than the record holds now (the pass listed
// its records when it started, and a webhook may have moved this one since), it applies the
// intake.PullRequestSynchronized that webhook would have carried, with GitHub's updated_at as its
// clock, so the workflow decides it as it decides the webhook's (workflow's
// pullRequestSynchronized), behind the same fence: a read older than the record's clock moves
// nothing, and a webhook arriving later that is older than this read moves nothing either. The
// fence orders by GitHub's whole seconds, so a clock equal to the record's is not late: a webhook
// of the same second arriving later runs as a redelivery does, and a push of the same second whose
// webhook is applied between this read and its apply is undone by it - the record moves back to
// the head read here, judging it again, until the next pass moves it forward.
//
// A push is left to its webhook, as when a synchronize beats its push: a head with no known push
// is judged as such, and the push's own handler settles what only the pusher decides
// (classify.ApplyPush's planned mark and take-back).
//
// It returns the record as the pass goes on with it: as it stands when the head did not move, and
// otherwise as the synchronize left it, so the mergeability, review and required-check reads after
// it judge the head GitHub shows. On an error, or a record no longer open, it returns the record as
// it last read it, and the pass reads the pull request at that head; the next pass follows it.
func (w *workflowRuntime) followHead(ctx context.Context, pr record.PullRequest, pull githubPullRequest) (record.PullRequest, error) {
	if pull.State != "open" || pull.Head.SHA == "" || pull.Head.SHA == pr.HeadSHA {
		return pr, nil
	}
	current, err := w.openRecord(ctx, pr)
	if err != nil || current.HeadSHA == pull.Head.SHA {
		return current, err
	}
	pr = current
	w.log.Warn("workflow: GitHub shows a pull request at a head the daemon has not recorded; applying the synchronize its webhook would",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "recorded", pr.HeadSHA, "head", pull.Head.SHA, "updated_at", pull.UpdatedAt)
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

// openedWarnEvery bounds readUnrecordedPullRequests's warning to one an hour for each pull request
// it applies: one the workflow does not record (an opened older than the record's clock, a branch
// that names no issue on the repository) is found and applied again every pass.
const openedWarnEvery = time.Hour

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
		if key := fmt.Sprintf("%s#%d", repo, pull.Number); time.Since(w.openedWarned[key]) >= openedWarnEvery {
			if w.openedWarned == nil {
				w.openedWarned = map[string]time.Time{}
			}
			w.openedWarned[key] = time.Now()
			w.log.Warn("workflow: GitHub holds an open pull request for an issue whose implementer completed, and the daemon has none recorded; applying the opened its webhook would",
				"repo", repo, "pullRequest", pull.Number, "issue", issue.Key, "head", pull.Head.SHA, "reopened", fact.Reopened, "updated_at", pull.UpdatedAt)
		}
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
