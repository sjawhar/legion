package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// default_branch.go owns every answer about a repository's default branch: whether the App can
// read the repository at all, and whether one commit sits on that branch. One place holds the
// GitHub reads, the default_branch_commits table and how long each answer is believed, so a
// change to any of the three is a change here and nowhere else. Its one caller is the results
// read (api/results_currency.go), which keeps a memo for the span of one request and no policy.

// DefaultBranchCommit is what Dispatch last learned about one commit of one repository:
// whether it sits on the default branch, and when that was read.
type DefaultBranchCommit struct {
	OnDefault bool
	CheckedAt time.Time
}

// DefaultBranchCommitStaleAfter is how long a "no" is believed. A "yes" is kept for good: a
// commit on the default branch never leaves it. A repository's default branch, and the empty
// answer that says the App cannot read it, are believed for the same span.
const DefaultBranchCommitStaleAfter = 5 * time.Minute

// DefaultBranches holds each repository's default branch across reads. The Verification tab
// refetches the results on every result.recorded, and a read holding an `after_merge` or
// `default_branch` check, or a `before_merge` result at a commit heading no pull request
// Dispatch knows, would otherwise spend a GET /repos/{owner}/{repo} each time. Its zero value
// is an empty cache, and the server holds one for the process: the answers are a repository's,
// not a request's, and every goroutine reading results shares them.
type DefaultBranches struct {
	mu    sync.Mutex
	known map[string]cachedBranch
}

// cachedBranch is one repository's default branch and when it was read. An empty branch is
// "Dispatch cannot say", held as long as any other answer so an unreadable repository is not
// asked about on every read.
type cachedBranch struct {
	branch string
	readAt time.Time
}

// RepositoryReadable reports whether the App can read repoFull right now: it answered that
// repository's default branch, within DefaultBranchCommitStaleAfter of now. It is false for a
// repository the App is not installed on, one its token is refused, a deployment holding no App
// key, and a read that failed; all of them mean Dispatch cannot see the repository, which is
// the unknown currency rather than a verdict about the work.
func (c *DefaultBranches) RepositoryReadable(ctx context.Context, client *githubapp.Client, repoFull string, now time.Time) bool {
	return c.branchOf(ctx, client, repoFull, now) != ""
}

// CommitOnDefaultBranch answers whether sha sits on repoFull's default branch, and whether
// Dispatch could say at all. What default_branch_commits holds answers first while it still
// stands — a yes for good, a no for DefaultBranchCommitStaleAfter — and GitHub is read, and its
// answer written back, otherwise. A repository the App cannot read is the unknown answer, as is
// a read that failed.
func (c *DefaultBranches) CommitOnDefaultBranch(
	ctx context.Context,
	pool *store.Pool,
	client *githubapp.Client,
	repoFull, sha string,
	now time.Time,
) (onDefault, known bool) {
	stored, found, err := readDefaultBranchCommit(ctx, pool, repoFull, sha)
	if err != nil {
		slog.Warn("dispatch: read a commit's default-branch answer", "repo", repoFull, "commit", sha, "error", err)
	}
	if found && (stored.OnDefault || now.Sub(stored.CheckedAt) < DefaultBranchCommitStaleAfter) {
		return stored.OnDefault, true
	}
	branch := c.branchOf(ctx, client, repoFull, now)
	if branch == "" {
		return false, false
	}
	answer, err := commitOnBranch(ctx, client, repoFull, branch, sha)
	if err != nil {
		if !cannotSeeRepository(err) {
			slog.Warn("dispatch: ask whether a commit is on the default branch", "repo", repoFull, "commit", sha, "error", err)
		}
		return false, false
	}
	if err := writeDefaultBranchCommit(ctx, pool, repoFull, sha, answer, now); err != nil {
		slog.Warn("dispatch: record a commit's default-branch answer", "repo", repoFull, "commit", sha, "error", err)
	}
	return answer, true
}

// branchOf is repoFull's default branch, held for DefaultBranchCommitStaleAfter, and "" when
// Dispatch cannot say.
func (c *DefaultBranches) branchOf(ctx context.Context, client *githubapp.Client, repoFull string, now time.Time) string {
	if held, fresh := c.get(repoFull, now); fresh {
		return held
	}
	branch := ""
	if client != nil {
		read, err := repositoryDefaultBranch(ctx, client, repoFull)
		if err != nil {
			if !cannotSeeRepository(err) {
				slog.Warn("dispatch: read the default branch of a repository", "repo", repoFull, "error", err)
			}
		} else {
			branch = read
		}
	}
	c.put(repoFull, branch, now)
	return branch
}

// get is the branch held for repo and whether that answer still stands at now.
func (c *DefaultBranches) get(repo string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, found := c.known[repo]
	if !found || now.Sub(held.readAt) >= DefaultBranchCommitStaleAfter {
		return "", false
	}
	return held.branch, true
}

// put records what GitHub said about repo's default branch at now.
func (c *DefaultBranches) put(repo, branch string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.known == nil {
		c.known = map[string]cachedBranch{}
	}
	c.known[repo] = cachedBranch{branch: branch, readAt: now}
}

// cannotSeeRepository is repositoryUnreadable — the App is installed on no repository covering
// this one, or its token is refused it — and the deployment holding no App key at all. Each
// means Dispatch cannot see the repository, which is an answer rather than a fault worth
// logging; repositoryUnreadable alone is the reconcile's rule for a row it stamps instead of
// warning about, and a deployment with no key has no rows to stamp.
func cannotSeeRepository(err error) bool {
	return repositoryUnreadable(err) || errors.Is(err, githubapp.ErrNoAppKey)
}

// readDefaultBranchCommit is what default_branch_commits holds for one commit, and whether it
// holds anything at all.
func readDefaultBranchCommit(ctx context.Context, pool *store.Pool, repoFull, sha string) (DefaultBranchCommit, bool, error) {
	var answer DefaultBranchCommit
	err := pool.QueryRow(ctx, `
		select on_default, checked_at from default_branch_commits where repo = $1 and sha = $2
	`, repoFull, sha).Scan(&answer.OnDefault, &answer.CheckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultBranchCommit{}, false, nil
	}
	if err != nil {
		return DefaultBranchCommit{}, false, fmt.Errorf("read whether %s@%s is on the default branch: %w", repoFull, sha, err)
	}
	return answer, true, nil
}

// writeDefaultBranchCommit records GitHub's answer about one commit.
func writeDefaultBranchCommit(ctx context.Context, pool *store.Pool, repoFull, sha string, onDefault bool, now time.Time) error {
	_, err := pool.Exec(ctx, `
		insert into default_branch_commits (repo, sha, on_default, checked_at)
		values ($1, $2, $3, $4)
		on conflict (repo, sha) do update set on_default = excluded.on_default, checked_at = excluded.checked_at
	`, repoFull, sha, onDefault, now)
	if err != nil {
		return fmt.Errorf("record whether %s@%s is on the default branch: %w", repoFull, sha, err)
	}
	return nil
}

// repositoryDefaultBranch is the name of repoFull's default branch, read through the App.
func repositoryDefaultBranch(ctx context.Context, client *githubapp.Client, repoFull string) (string, error) {
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return "", err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("mint installation token for %s: %w", repoFull, err)
	}
	return client.DefaultBranch(ctx, token, owner, repo)
}

// commitOnBranch asks the App whether sha is on branch of repoFull.
func commitOnBranch(ctx context.Context, client *githubapp.Client, repoFull, branch, sha string) (bool, error) {
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return false, err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return false, fmt.Errorf("mint installation token for %s: %w", repoFull, err)
	}
	return client.CommitOnBranch(ctx, token, owner, repo, branch, sha)
}
