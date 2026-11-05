package refs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The canonical name of a repository is what its repo_projects row's renamed_to names, else the
// name itself, compared case-insensitively. A repository Dispatch holds no mapping for, and one
// whose mapping names no rename, is its own canonical name, so a repository Dispatch does not
// map is unaffected by this rule: it reads exactly as it did before the column existed.
//
// This is the server's one repository-renaming rule, and it is written here once because four
// sites must come to the same answer or the rename is only half known: delivery's attributionHeld
// puts both the issue's resolved repository and the pull request's own through it, the results
// route canonicalises the repository of a check's issue before it asks GitHub anything
// (api/results_currency.go, currencyReader.repositoryOf), and the three writers of a
// pull_request_heads row key the row by it (delivery/pull_request_heads.go:
// RefreshPullRequestHead, ReadThroughCommitPulls and Intake.recordPullRequestHead).
//
// One step is enough: the settings route refuses a pair pointing at a repository that is itself
// renamed, and refuses renaming a repository another row already points at, so no chain and no
// cycle exists to walk (api/repo_projects.go, putRepoProject; migration
// 20261105075455_repo_projects_renamed_to). Dispatch never asks GitHub to resolve a name — for a
// repository renamed away, GitHub answers 404 for its installation, so the old name cannot be
// read at all and the pair is declared, never discovered.

// CanonicalRepoSQL is that rule as a SQL expression over repo, itself an expression naming a
// repository: null where repo is null, so a comparison against it stays null rather than
// becoming true. It reads repo_projects by a lowercased name on both sides, as every comparison
// of a repository here does, because GitHub spells an owner and a repository as their owners do
// (`Acme/Widgets`) while Dispatch stores a mapping's path canonicalised (api's canonicalRepo).
// The expression text is spliced twice, so repo must be a column, a bound parameter or another
// side-effect-free expression.
func CanonicalRepoSQL(repo string) string {
	return `coalesce((select renamed.renamed_to from repo_projects renamed
		where lower(renamed.repo) = lower(` + repo + `)), ` + repo + `)`
}

// CanonicalRepo is that rule read through q for one full `owner/name`: the repository repo was
// renamed to, else repo exactly as it was given. The caller's own spelling comes back unchanged
// where there is no rename, so canonicalising a name never re-cases a stored row's key; where
// there is one, the answer is the pair as the settings route canonicalised it.
func CanonicalRepo(ctx context.Context, q Queryer, repo string) (string, error) {
	if repo == "" {
		return repo, nil
	}
	var renamed string
	err := q.QueryRow(ctx, `
		select renamed_to from repo_projects where lower(repo) = lower($1) and renamed_to is not null
	`, repo).Scan(&renamed)
	if errors.Is(err, pgx.ErrNoRows) {
		return repo, nil
	}
	if err != nil {
		return "", fmt.Errorf("read the canonical name of %s: %w", repo, err)
	}
	return renamed, nil
}
