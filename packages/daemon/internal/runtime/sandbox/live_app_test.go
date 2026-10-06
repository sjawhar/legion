//go:build e2e

// The implement App the Stage 4a harness mints provisioning tokens from. The rig is live_test.go.

package sandbox

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// implementTokens is the harness's ProvisionTokens: the implement App's installation token, minted
// in-process from the App's private key.
type implementTokens struct{ apps *appauth.Manager }

func (t implementTokens) Token(ctx context.Context, owner string) (string, error) {
	lease, err := t.apps.Token(ctx, appauth.Implement, owner)
	return lease.Token, err
}

// resolveApp reads the implement App's private key from its file, a PEM block only its owner can
// read (the script refuses any other mode), and mints the App's installation token for the run's
// repository owner. The key stays in this process's memory.
func (r *liveRig) resolveApp() error {
	raw, err := os.ReadFile(r.env.appKeyFile)
	if err != nil {
		return fmt.Errorf("reading the implement App's key: %w", err)
	}
	pem := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(pem, "-----BEGIN") {
		return fmt.Errorf("the implement App's key file %s holds no PEM block (-----BEGIN …)", r.env.appKeyFile)
	}
	apps := appauth.New(config.GitHubApps{Implement: config.GitHubApp{AppID: r.env.appID, PrivateKey: pem}}, appauth.Options{})
	owner := r.env.repo.Owner()
	lease, err := apps.Token(r.ctx, appauth.Implement, owner)
	if err != nil {
		return fmt.Errorf("minting the implement App's installation token for %s: %w", owner, err)
	}
	r.tokens, r.identity = implementTokens{apps: apps}, lease.Identity
	return nil
}
