package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// gitHubRefreshInterval is how often refreshGitHubCredentials walks the tracked panes. The daemon's
// function (Options.GitHubCredential) holds each App's lease and re-mints it as the lease nears its
// expiry, so most ticks render the hosts.yml the pane's directory already holds and write nothing;
// a minute keeps the time a role's gh runs on a token the daemon has already replaced short against
// an installation token's hour, at one file read per tracked pane.
const gitHubRefreshInterval = time.Minute

// writeGHConfig brings dir, a claim's GH_CONFIG_DIR (runtime.GHConfigDir), to rendered: the
// directory made 0700 if it is gone, config.yml written once when absent or different, and
// hosts.yml replaced — a temporary file in the directory (0600, as os.CreateTemp makes it) renamed
// over it, so the pane's gh never reads a half-written file — only when its content differs. It
// reports whether hosts.yml changed, which is what the refresher logs.
func writeGHConfig(dir string, rendered ghconfig.Rendered) (changed bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return false, err
	}
	config := filepath.Join(dir, ghconfig.ConfigFile)
	if current, err := os.ReadFile(config); err != nil || string(current) != rendered.Config {
		if err := os.WriteFile(config, []byte(rendered.Config), 0o600); err != nil {
			return false, err
		}
	}
	hosts := filepath.Join(dir, ghconfig.HostsFile)
	current, err := os.ReadFile(hosts)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err == nil && bytes.Equal(current, []byte(rendered.Hosts)) {
		return false, nil
	}
	temporary, err := os.CreateTemp(dir, "."+ghconfig.HostsFile+"-")
	if err != nil {
		return false, err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.WriteString(rendered.Hosts); err != nil {
		_ = temporary.Close()
		return false, err
	}
	if err := temporary.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(temporaryPath, hosts); err != nil {
		return false, err
	}
	return true, nil
}

// refreshGitHubCredentialsEvery runs refreshGitHubCredentials every interval until ctx ends: the
// sweep's lifetime (Observe), so panes are refreshed exactly while they are watched.
func (r *Runtime) refreshGitHubCredentialsEvery(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.refreshGitHubCredentials(ctx)
		}
	}
}

// refreshGitHubCredentials brings the gh files of every tracked pane to the current render of its
// role's GitHub credential, so the pane's plain gh and the clone's `gh auth git-credential` helper
// read the token the daemon holds now. It walks a snapshot of the watch (trackedProcesses), takes
// each pane's role from its claim token (claim.Token.Role), skips a token that names no role and
// the controller, which has no App, renders the credential, and rewrites hosts.yml only when it
// differs (writeGHConfig), recreating a directory that is gone. A render or a write that fails is
// logged and the pane keeps its last token until the next tick. A runtime with no credential
// function (Options.GitHubCredential nil: a daemon with no GitHub Apps) wrote no pane any gh
// files, so it has nothing to refresh.
func (r *Runtime) refreshGitHubCredentials(ctx context.Context) {
	if r.gitHubCredential == nil {
		return
	}
	for _, entry := range r.trackedProcesses() {
		token := entry.locator.Claim
		role, ok := token.Role()
		if !ok || role == claim.RoleController {
			continue
		}
		rendered, err := r.gitHubCredential(ctx, role)
		if err != nil {
			r.log.Warn("tmux runtime: github credential refresh failed", "claim", token, "role", role, "err", err)
			continue
		}
		changed, err := writeGHConfig(runtime.GHConfigDir(r.stateDir, token), rendered)
		if err != nil {
			r.log.Warn("tmux runtime: github credential refresh failed", "claim", token, "role", role, "err", fmt.Errorf("write the github credential: %w", err))
			continue
		}
		if changed {
			r.log.Info("tmux runtime: github credential refreshed", "claim", token, "role", role, "app", rendered.App, "expiresAt", rendered.ExpiresAt)
		}
	}
}
