//go:build e2e

// The implement App the Stage 4a harness mints provisioning tokens from. The rig is live_test.go.

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
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

// implementTokens is the harness's ProvisionTokens: the implement App's installation token,
// minted in-process from the key the devbox's secret store holds (Sami's ruling on ask e3943412).
// owners are the owners of the run's repositories, the first the run's repository's owner.
type implementTokens struct {
	apps   *appauth.Manager
	owners []string
}

func (t implementTokens) Token(ctx context.Context, owner string) (string, error) {
	lease, err := t.apps.Token(ctx, appauth.Implement, owner)
	return lease.Token, err
}

// Credential is the harness's GitHubCredential: every role's gh bundle renders the implement App's
// tokens, the one App the harness holds — its full lease for owner (the run's repository's owner
// when owner is empty, the controller's launch), and its read-only lease for every other owner of
// the run's repositories. Stage 4a proves the files reach each role's container, follow the lease
// and pick the owner a command names, not which App a role writes as; Stage 4b's daemon hands each
// role its own.
func (t implementTokens) Credential(ctx context.Context, _ claim.Role, owner string) (ghconfig.Bundle, error) {
	owner = strings.ToLower(owner)
	if owner == "" {
		owner = strings.ToLower(t.owners[0])
	}
	bundle := ghconfig.Bundle{Default: owner, Owners: map[string]ghconfig.Rendered{}}
	for _, name := range append([]string{owner}, t.owners...) {
		name = strings.ToLower(name)
		if _, done := bundle.Owners[name]; done {
			continue
		}
		mint := t.apps.ReadToken
		if name == owner {
			mint = t.apps.Token
		}
		lease, err := mint(ctx, appauth.Implement, name)
		if err != nil {
			return ghconfig.Bundle{}, fmt.Errorf("mint the implement App's lease for %s: %w", name, err)
		}
		bundle.Owners[name] = ghconfig.Render(lease.Token, string(appauth.Implement), lease.ExpiresAt)
	}
	return bundle, nil
}

// resolveApp reads the implement App's key from the secret store, handed to one child process
// through the agent-tier `secrets <KEY> -- …` injection, mints the App's installation token for
// each of the run's two repositories' owners, and with them reads each repository's default branch
// as GitHub names it, the base every launch on it provisions at; the key stays in this process's
// memory.
func (r *liveRig) resolveApp() error {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(r.ctx, "secrets", r.env.appKeyName, "--", "sh", "-c", `printf %s "$`+r.env.appKeyName+`"`)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reading %s from the secret store: %v: %s", r.env.appKeyName, err, strings.TrimSpace(stderr.String()))
	}
	pem, err := config.DecodePrivateKey(stdout.String())
	if err != nil {
		return fmt.Errorf("%s: %w", r.env.appKeyName, err)
	}
	apps := appauth.New(config.GitHubApps{Implement: config.GitHubApp{AppID: r.env.appID, PrivateKey: pem}}, appauth.Options{})
	branches := make([]string, 2)
	var identity runtime.GitIdentity
	for i, repo := range []ghrepo.Repository{r.env.repo, r.env.repoSecondOwner} {
		lease, err := apps.Token(r.ctx, appauth.Implement, repo.Owner())
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
	r.tokens = implementTokens{apps: apps, owners: []string{r.env.repo.Owner(), r.env.repoSecondOwner.Owner()}}
	r.identity, r.defaultBranch, r.secondDefaultBranch = identity, branches[0], branches[1]
	return nil
}
