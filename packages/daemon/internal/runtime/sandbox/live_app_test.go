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
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// implementTokens is the harness's ProvisionTokens: the implement App's installation token,
// minted in-process from the key the devbox's secret store holds (Sami's ruling on ask e3943412).
// owner is the run's repository owner, whose installation the token is for.
type implementTokens struct {
	apps  *appauth.Manager
	owner string
}

func (t implementTokens) Token(ctx context.Context, owner string) (string, error) {
	lease, err := t.apps.Token(ctx, appauth.Implement, owner)
	return lease.Token, err
}

// Credential is the harness's GitHubCredential: every role's gh files render the implement App's
// token, the one App the harness holds. Stage 4a proves the files reach each role's container and
// follow the lease, not which App a role writes as; Stage 4b's daemon hands each role its own.
func (t implementTokens) Credential(ctx context.Context, _ claim.Role) (ghconfig.Rendered, error) {
	lease, err := t.apps.Token(ctx, appauth.Implement, t.owner)
	if err != nil {
		return ghconfig.Rendered{}, err
	}
	return ghconfig.Render(lease.Token, string(appauth.Implement), lease.ExpiresAt), nil
}

// resolveApp reads the implement App's key from the secret store, handed to one child process
// through the agent-tier `secrets <KEY> -- …` injection, and mints the App's installation token for
// the run's repository owner; the key stays in this process's memory.
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
	owner := r.env.repo.Owner()
	lease, err := apps.Token(r.ctx, appauth.Implement, owner)
	if err != nil {
		return fmt.Errorf("minting the implement App's installation token for %s: %w", owner, err)
	}
	r.tokens, r.identity = implementTokens{apps: apps, owner: owner}, lease.Identity
	return nil
}
