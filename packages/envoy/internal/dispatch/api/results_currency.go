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
// is written back are delivery's (delivery/default_branch.go, delivery.HeadAttributedTo), and
// what lives here is one memo per read, so a tab listing twenty results against a handful of
// commits costs a handful of round trips.

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
// of the results. Across reads, a repository's default branch and a commit's answer are held by
// delivery (server.defaultBranches and default_branch_commits).
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
	c.heads[key] = atHead
	return atHead, true
}

// repositoryOf is the repository the check's issue resolves to: its own, the nearest one above
// it, or its project's default (refs.IssueRepository); nil when none resolves or the read
// failed, which is the unknown answer either way.
func (c *currencyReader) repositoryOf(ctx context.Context, issueKey string) *string {
	if repo, memoized := c.repos[issueKey]; memoized {
		return repo
	}
	repo, err := refs.IssueRepository(ctx, c.server.deps.Store.Pool, issueKey)
	if err != nil {
		slog.Warn("dispatch: resolve the repository of a check's issue", "issue", issueKey, "error", err)
		repo = nil
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
