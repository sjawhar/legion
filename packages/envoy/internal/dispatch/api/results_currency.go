package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// results_currency.go says whether each recorded result still speaks for the work as it stands.
// It holds no policy of its own: what GitHub is asked, how long an answer is believed and what
// is written back are delivery's (delivery/default_branch.go, delivery/commit_pull_reads.go,
// delivery.HeadAttributedTo, delivery.ReadThroughCommitPulls), and what lives here is one memo
// per read, so a tab listing twenty results against a handful of commits costs a handful of
// round trips.

// currencyReader answers the currency of every result of one read of one issue's results.
//
// A `before_merge` result is current while its commit is the head of an open pull request
// attributed to the check's own issue, or the final head of a merged one, and the check's words
// have not changed since: a push that moves the head makes it older with no write of
// Dispatch's, and rewording the check makes its own results older and no other's. The issue a
// result was recorded on counts for nothing here, so a child's pull request never makes a
// result against its parent's check current; a child carrying part of a parent's requirement
// proves it with a check of its own whose `serves` names that requirement.
//
// An `after_merge` or `default_branch` result is current while its commit is on the default
// branch of the check's issue's resolved repository and the words match.
//
// Where Dispatch cannot see the repository the check's issue resolves to — it resolves to none,
// the App is installed on none that covers it, or a read failed — the answer is unknown and the
// commit is shown with neither mark, whatever the check's `when`: nothing can have taught
// Dispatch that commit's pull request, so "no head" is no evidence that the work has moved on.
// Dispatch's own failed read is unknown for the same reason: a database hiccup must not make a
// reviewer refuse an approval.
//
// The repository each issue resolves to, whether the App can read it, the pull request heads of
// each issue-and-commit pair and GitHub's answer about each commit are each read once per read
// of the results. Across reads, a repository's default branch, a commit's answer and the last
// read-through of a commit's pull requests are held by delivery (server.defaultBranches,
// default_branch_commits and server.commitPulls).
type currencyReader struct {
	server   *server
	repos    map[string]*string
	readable map[string]bool
	heads    map[[2]string]bool
	commits  map[[2]string]commitAnswer
}

// commitAnswer is what delivery said about one commit: whether it is on the default branch, and
// whether Dispatch could say at all.
type commitAnswer struct {
	onDefault bool
	known     bool
}

func newCurrencyReader(s *server) *currencyReader {
	return &currencyReader{
		server: s, repos: map[string]*string{}, readable: map[string]bool{},
		heads: map[[2]string]bool{}, commits: map[[2]string]commitAnswer{},
	}
}

func (c *currencyReader) of(ctx context.Context, check model.CheckBlock, result model.CheckResult) string {
	if check.When == checkWhenBeforeMerge {
		atHead, known := c.headOf(ctx, result)
		switch {
		case !known:
			return model.CurrencyUnknown
		case atHead:
			if check.SHA256 == result.CheckSHA256 {
				return model.CurrencyCurrent
			}
			return model.CurrencyOlder
		case !c.repositoryReadable(ctx, result.CheckIssueKey):
			return model.CurrencyUnknown
		}
		return model.CurrencyOlder
	}
	repo := c.repositoryOf(ctx, result.CheckIssueKey)
	if repo == nil {
		return model.CurrencyUnknown
	}
	onDefault, known := c.onDefaultBranch(ctx, *repo, result.CommitSHA)
	if !known {
		return model.CurrencyUnknown
	}
	if onDefault && check.SHA256 == result.CheckSHA256 {
		return model.CurrencyCurrent
	}
	return model.CurrencyOlder
}

// headOf reports whether a result's commit is the head of a pull request attributed to the
// check's own issue, and whether Dispatch's own read could say at all.
//
// A commit that heads no such row may be one Dispatch has never been told a pull request for at
// all: the read-through a record makes (results.go's readThroughPullRequests) ran before the
// pull request was opened, a deployment holding no delivery settings binds no `pull_request`
// durable and so consumes no such event ever, and the reconcile's head pass only re-reads rows
// that already exist. So a "no" is read through once and asked again (readThroughHead), which
// is how a result recorded at a commit before any pull request carried it stops reading `older`
// for ever (dispatch://INFRA-1240).
func (c *currencyReader) headOf(ctx context.Context, result model.CheckResult) (bool, bool) {
	key := [2]string{result.CheckIssueKey, result.CommitSHA}
	if atHead, memoized := c.heads[key]; memoized {
		return atHead, true
	}
	atHead, err := delivery.HeadAttributedTo(ctx, c.server.deps.Store.Pool, result.CheckIssueKey, result.CommitSHA)
	if err != nil {
		slog.Warn("dispatch: read a result's pull request head", "issue", result.CheckIssueKey, "error", err)
		return false, false
	}
	if !atHead && c.readThroughHead(ctx, result) {
		atHead, err = delivery.HeadAttributedTo(ctx, c.server.deps.Store.Pool, result.CheckIssueKey, result.CommitSHA)
		if err != nil {
			slog.Warn("dispatch: read a result's pull request head", "issue", result.CheckIssueKey, "error", err)
			return false, false
		}
	}
	c.heads[key] = atHead
	return atHead, true
}

// readThroughHead teaches Dispatch the pull requests GitHub names for a result's commit
// (delivery.ReadThroughCommitPulls) and reports whether it wrote any row, which is the only case
// worth asking the stored heads again for.
//
// Three things come first, so no GitHub call is wasted. A deployment holding no App key can ask
// nothing. A repository Dispatch cannot see is the unknown answer whatever GitHub would say, and
// it is the test the `of` switch makes next, so it decides here first and costs nothing twice
// (repositoryReadable's memo); it also answers false for an issue resolving to no repository,
// which is why the repository read below has one. And one commit is read through at most once
// every delivery.CommitPullReadStaleAfter, however many reads ask (server.commitPulls).
//
// The read is against the canonical repository the check's issue resolves to (repositoryOf), the
// same pair headOf asks the stored rows about. A read that fails leaves the currency exactly as
// the stored rows say, which is what a result reads with no read-through at all: a GitHub call
// that did not land is no evidence either way.
func (c *currencyReader) readThroughHead(ctx context.Context, result model.CheckResult) bool {
	if c.server.deps.GitHub == nil || !c.repositoryReadable(ctx, result.CheckIssueKey) {
		return false
	}
	repo := c.repositoryOf(ctx, result.CheckIssueKey)
	now := time.Now()
	if !c.server.commitPulls.ReadDue(*repo, result.CommitSHA, now) {
		return false
	}
	c.server.commitPulls.RecordRead(*repo, result.CommitSHA, now)
	written, err := delivery.ReadThroughCommitPulls(
		ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, *repo, result.CommitSHA, now)
	if err != nil {
		slog.Info("dispatch: GitHub named no pull request for a result's commit",
			"repo", *repo, "commit", result.CommitSHA, "error", err)
		return false
	}
	return written > 0
}

// repositoryOf is the repository the check's issue resolves to: its own, the nearest one above
// it, or its project's default (refs.IssueRepository), under the canonical name that repository
// is called today (refs.CanonicalRepo); nil when none resolves or either read failed, which is
// the unknown answer either way.
//
// The canonical name is taken here, before anything asks GitHub, because everything downstream
// takes this answer: repositoryReadable, onDefaultBranch and the read-through a recorded result
// makes (results.go's readThroughPullRequests). An issue recorded under a name that was renamed
// away would otherwise have the App asked about that name, which GitHub answers 404 for, and
// every result of that issue would read unknown. A repository with no rename is its own
// canonical name, so this costs one read and changes no answer for it.
func (c *currencyReader) repositoryOf(ctx context.Context, issueKey string) *string {
	if repo, memoized := c.repos[issueKey]; memoized {
		return repo
	}
	repo, err := refs.IssueRepository(ctx, c.server.deps.Store.Pool, issueKey)
	if err != nil {
		slog.Warn("dispatch: resolve the repository of a check's issue", "issue", issueKey, "error", err)
		repo = nil
	}
	if repo != nil {
		canonical, err := refs.CanonicalRepo(ctx, c.server.deps.Store.Pool, *repo)
		if err != nil {
			slog.Warn("dispatch: canonical name of a check's repository", "issue", issueKey, "repo", *repo, "error", err)
			repo = nil
		} else {
			repo = &canonical
		}
	}
	c.repos[issueKey] = repo
	return repo
}

// repositoryReadable reports whether Dispatch can see the repository the check's issue resolves
// to: it resolves to one, and the App answered that repository's default branch.
func (c *currencyReader) repositoryReadable(ctx context.Context, issueKey string) bool {
	repo := c.repositoryOf(ctx, issueKey)
	if repo == nil {
		return false
	}
	if readable, memoized := c.readable[*repo]; memoized {
		return readable
	}
	readable := c.server.defaultBranches.RepositoryReadable(ctx, c.server.deps.GitHub, *repo, time.Now())
	c.readable[*repo] = readable
	return readable
}

// onDefaultBranch is delivery's answer about one commit, read once per read of the results.
func (c *currencyReader) onDefaultBranch(ctx context.Context, repo, sha string) (bool, bool) {
	memo := [2]string{repo, sha}
	if answer, memoized := c.commits[memo]; memoized {
		return answer.onDefault, answer.known
	}
	onDefault, known := c.server.defaultBranches.CommitOnDefaultBranch(
		ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, repo, sha, time.Now())
	c.commits[memo] = commitAnswer{onDefault: onDefault, known: known}
	return onDefault, known
}
