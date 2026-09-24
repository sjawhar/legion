//go:build e2e

// The implement App the Stage 4a harness mints provisioning tokens from. The rig is live_test.go.

package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// implementTokens is the harness's ProvisionTokens: the implement App's installation token,
// minted in-process from the key the devbox's secret store holds (Sami's ruling on ask e3943412).
type implementTokens struct{ apps *appauth.Manager }

func (t implementTokens) Token(ctx context.Context, owner string) (string, error) {
	lease, err := t.apps.Token(ctx, appauth.Implement, owner)
	return lease.Token, err
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
	pem, err := decodeKey(stdout.String())
	if err != nil {
		return fmt.Errorf("%s: %w", r.env.appKeyName, err)
	}
	apps := appauth.New(config.GitHubApps{Implement: config.GitHubApp{AppID: r.env.appID, PrivateKey: pem}}, appauth.Options{})
	owner, _, _ := strings.Cut(r.env.repo, "/")
	lease, err := apps.Token(r.ctx, appauth.Implement, owner)
	if err != nil {
		return fmt.Errorf("minting the implement App's installation token for %s: %w", owner, err)
	}
	r.tokens, r.identity = implementTokens{apps: apps}, lease.Identity
	return nil
}

// decodeKey decodes the stored base64 of a PEM key, URL-safe or standard, padded or not.
func decodeKey(encoded string) (string, error) {
	cleaned := strings.NewReplacer("-", "+", "_", "/", "\n", "", "\r", "", " ", "").Replace(strings.TrimSpace(encoded))
	cleaned = strings.TrimRight(cleaned, "=")
	raw, err := base64.RawStdEncoding.DecodeString(cleaned)
	if err != nil {
		return "", errors.New("not base64")
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("-----BEGIN")) {
		return "", errors.New("does not decode to a PEM block")
	}
	return string(bytes.TrimSpace(raw)), nil
}
