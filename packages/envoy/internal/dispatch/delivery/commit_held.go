package delivery

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// commit_held.go answers one question about one commit: whether a repository holds it at all.
// It is not a default-branch answer, which default_branch.go owns alone, and it keeps nothing:
// GitHub is asked each time, by the one caller that asks before it stores a check result
// (api/results.go).

// RepositoryHoldsCommit answers whether repoFull holds sha, through the App: it mints the
// repository's installation token, through the repositoryRead prologue commitOnBranch and
// repositoryDefaultBranch share, and makes githubapp.Client.HasCommit's read.
//
// A false with no error is GitHub's 422, the one answer that proves the repository does not hold
// the commit. Every other failure is the error, which says only that Dispatch cannot tell: a
// malformed repository name, a deployment with no App key, a repository the App cannot read, a
// rate limit, a 5xx, a transport failure.
func RepositoryHoldsCommit(ctx context.Context, client *githubapp.Client, repoFull, sha string) (bool, error) {
	owner, repo, token, err := repositoryRead(ctx, client, repoFull)
	if err != nil {
		return false, err
	}
	return client.HasCommit(ctx, token, owner, repo, sha)
}
