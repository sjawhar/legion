package delivery

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// commit_held.go answers one question about one commit: whether a repository holds it at all.
// It is not a default-branch answer, which default_branch.go owns alone, and it keeps nothing:
// GitHub is asked each time, by the one caller that asks before it stores a check result
// (api/results.go).

// RepositoryHoldsCommit answers whether repoFull holds sha, through the App: it mints the
// repository's installation token and makes githubapp.Client.HasCommit's read, exactly as
// commitOnBranch mints and reads for its own question.
//
// A false with no error is GitHub's 422, the one answer that proves the repository does not hold
// the commit. Every other failure is the error, which says only that Dispatch cannot tell: a
// malformed repository name, a deployment with no App key, a repository the App cannot read, a
// rate limit, a 5xx, a transport failure.
func RepositoryHoldsCommit(ctx context.Context, client *githubapp.Client, repoFull, sha string) (bool, error) {
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return false, err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return false, fmt.Errorf("mint installation token for %s: %w", repoFull, err)
	}
	return client.HasCommit(ctx, token, owner, repo, sha)
}
