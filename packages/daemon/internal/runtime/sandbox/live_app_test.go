//go:build e2e

// The implement App the Stage 4a harness mints provisioning tokens from. The rig is live_test.go.

package sandbox

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// implementTokens is the harness's ProvisionTokens: the implement App's installation token
// narrowed to reading the one repository it is minted for, from the token command the script
// hands the harness (resolveApp), so the harness holds no App key. repos are the run's
// repositories, the first the run's own; a token names the one it is minted for, as every mint
// of the daemon's does. left is the one record of what those mints find GitHub refuses, which
// the bundles write and the directory set reads, as the daemon's three share one
// (appauth.LeftOut, internal/daemon's sandboxRuntime).
type implementTokens struct {
	apps  *appauth.Manager
	repos []ghrepo.Repository
	left  *appauth.LeftOut
}

func (t implementTokens) Token(ctx context.Context, repo ghrepo.Repository) (string, error) {
	// No fallback to the owner's own write token, which the daemon's adapter has: the App is
	// this run's own, so a refusal here is a misconfigured proof and must fail the run loudly.
	lease, err := t.apps.ReadToken(ctx, appauth.Implement, repo.Owner(), []string{repo.Name()})
	return lease.Token, err
}

// Credential is the harness's GitHubCredential: the daemon's own bundle
// (appauth.GitHubCredential) over the run's repositories, with every lease minted as the
// implement App, the one App the harness holds — its write lease for the repository the role
// works, a lease narrowed to that one repository for each other repository of its owner, and a
// read-only lease per other owner over the repositories it serves. Every role is rendered as the
// implementer's, so Stage 4a proves the files reach each role, follow the lease and pick the
// directory a command names, not which App a role writes as; Stage 4b's daemon hands each role
// its own.
func (t implementTokens) Credential(ctx context.Context, _ claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
	return appauth.GitHubCredential(t.apps, t.repositories, ghrepo.Renames{}, t.left, slog.Default())(ctx, claim.RoleImplementer, repo)
}

// ControllerCredential is the harness's ControllerCredential (Options.ControllerCredential): the
// controller's own bundle by default owner (appauth.ControllerCredential), which a controller
// pod's refresh asks for the owner its Secret records. It mints as the implement App, the one App
// the harness holds, where the daemon's own mints as the review App (implementApp).
func (t implementTokens) ControllerCredential(ctx context.Context, defaultOwner string) (ghconfig.Bundle, error) {
	return appauth.ControllerCredential(implementApp{t.apps}, t.repositories, ghrepo.Renames{}, t.left, slog.Default())(ctx, defaultOwner)
}

// implementApp mints every lease as the implement App, whichever App role asks for one: the
// harness holds that App alone.
type implementApp struct{ apps appauth.Tokens }

func (a implementApp) Token(ctx context.Context, _ appauth.AppRole, repo ghrepo.Repository) (appauth.Lease, error) {
	return a.apps.Token(ctx, appauth.Implement, repo)
}

func (a implementApp) ReadToken(ctx context.Context, _ appauth.AppRole, owner string, repos []string) (appauth.Lease, error) {
	return a.apps.ReadToken(ctx, appauth.Implement, owner, repos)
}

// ProjectDirs is the harness's GitHubDirs (Options.GitHubDirs): the directories a bundle built
// now for repo would hold (appauth.GitHubDirs), which is what Credential renders, so the two
// answers move together as the daemon's own do.
func (t implementTokens) ProjectDirs(repo ghrepo.Repository, roles []appauth.AppRole) []string {
	return appauth.GitHubDirs(t.repositories, ghrepo.Renames{}, t.left, roles, repo)
}

// repositories is the run's repositories, as the daemon's mapping would serve them.
func (t implementTokens) repositories() []ghrepo.Repository { return t.repos }

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
	r.tokens = implementTokens{apps: apps, repos: []ghrepo.Repository{r.env.repo, r.env.repoSecondOwner}, left: appauth.NewLeftOut()}
	r.identity, r.defaultBranch, r.secondDefaultBranch = identity, branches[0], branches[1]
	return nil
}
