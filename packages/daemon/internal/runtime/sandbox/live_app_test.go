//go:build e2e

// The implement App the Stage 4a harness mints provisioning tokens from. The rig is live_test.go.

package sandbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// implementTokens is the harness's ProvisionTokens: the implement App's installation token, from
// the token command the script hands the harness (resolveApp), so the harness holds no App key.
// repos are the run's repositories, the first the run's own; a token names the one it is minted
// for, as every mint of the daemon's does.
type implementTokens struct {
	apps  *appauth.Manager
	repos []ghrepo.Repository
}

func (t implementTokens) Token(ctx context.Context, repo ghrepo.Repository) (string, error) {
	lease, err := t.apps.Token(ctx, appauth.Implement, repo)
	return lease.Token, err
}

// Credential is the harness's GitHubCredential: every role's gh bundle renders the implement App's
// tokens, the one App the harness holds — its full lease for the repository of owner (the run's
// repository's owner when owner is empty, the controller's launch), and its read-only lease,
// narrowed to that owner's repository, for every other owner of the run's repositories. Stage 4a
// proves the files reach each role, follow the lease and pick the owner a command names, not which
// App a role writes as; Stage 4b's daemon hands each role its own.
func (t implementTokens) Credential(ctx context.Context, _ claim.Role, owner string) (ghconfig.Bundle, error) {
	owner = strings.ToLower(owner)
	if owner == "" {
		owner = strings.ToLower(t.repos[0].Owner())
	}
	bundle := ghconfig.Bundle{Default: owner, Owners: map[string]ghconfig.Rendered{}}
	for _, repo := range t.repos {
		name := strings.ToLower(repo.Owner())
		if _, done := bundle.Owners[name]; done {
			continue
		}
		lease, err := t.apps.ReadToken(ctx, appauth.Implement, name, []string{repo.Name()})
		if name == owner {
			lease, err = t.apps.Token(ctx, appauth.Implement, repo)
		}
		if err != nil {
			return ghconfig.Bundle{}, fmt.Errorf("mint the implement App's lease for %s: %w", repo, err)
		}
		bundle.Owners[name] = ghconfig.Render(lease.Token, string(appauth.Implement), lease.ExpiresAt)
	}
	return bundle, nil
}

// ProjectOwners is the harness's GitHubOwners (Options.GitHubOwners): the owners of the run's
// repositories as appauth.GitHubOwners reads them for a deployment that configures no rename,
// lowercased and sorted, which is the set every bundle Credential renders holds a directory for.
func (t implementTokens) ProjectOwners() []string {
	return appauth.GitHubOwners(func() []string {
		owners := make([]string, 0, len(t.repos))
		for _, repo := range t.repos {
			owners = append(owners, repo.Owner())
		}
		return owners
	}, ghrepo.Renames{})
}

// resolveApp mints the implement App's tokens through the token command the script hands the
// harness (LEGION_E2E_IMPLEMENT_APP_TOKEN_COMMAND): lib/github-apps.sh's own app_token_command,
// which asks the secrets broker's github-app secret for a release of installation tokens, so the
// broker client's argv lives in the lib alone, as real_github_test.go's liveProofApps runs the same
// lib's output. The token manager runs it in token mode (appauth's TokenCommand) for each of the
// run's two repositories' owners, and with those tokens reads each repository's default branch as
// GitHub names it, the base every launch on it provisions at. No key reaches the harness.
func (r *liveRig) resolveApp() (err error) {
	apps := appauth.New(config.GitHubApps{Implement: config.GitHubApp{AppID: r.env.appID, TokenCommand: r.env.appTokenCommand}}, appauth.Options{})
	// The rig owns the manager once it is stored in r.tokens, and closes it in close; until then a
	// failure closes it here, so a token command still running never outlives the harness.
	defer func() {
		if err != nil {
			apps.Close()
		}
	}()
	branches := make([]string, 2)
	var identity runtime.GitIdentity
	for i, repo := range []ghrepo.Repository{r.env.repo, r.env.repoSecondOwner} {
		lease, err := apps.Token(r.ctx, appauth.Implement, repo)
		if err != nil {
			return fmt.Errorf("minting the implement App's installation token for %s: %w", repo.Owner(), err)
		}
		if branches[i], err = ghbranch.DefaultBranch(r.ctx, githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI("", repo)}, repo); err != nil {
			return err
		}
		if i == 0 {
			identity = lease.Identity
		}
	}
	r.tokens = implementTokens{apps: apps, repos: []ghrepo.Repository{r.env.repo, r.env.repoSecondOwner}}
	r.identity, r.defaultBranch, r.secondDefaultBranch = identity, branches[0], branches[1]
	return nil
}
