package tmux

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// gitHubRefreshInterval is how often refreshGitHubCredentials walks the tracked panes. The daemon's
// function (Options.GitHubCredential) holds each App's lease and re-mints it as the lease nears its
// expiry, so most ticks render the hosts.yml the pane's directory already holds and write nothing;
// a minute keeps the time a role's gh runs on a token the daemon has already replaced short against
// an installation token's hour, at one file read per tracked pane.
const gitHubRefreshInterval = time.Minute

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
// each pane's role from its claim token (claim.Token.Role), skips only a token that names no
// role, renders the credential, and rewrites hosts.yml only when it differs (ghconfig.Write),
// recreating a directory that is gone. A render or a write that fails is logged and the pane
// keeps its last token until the next tick. A runtime with no credential function
// (Options.GitHubCredential nil: a daemon with no GitHub Apps) wrote no pane any gh files, so it
// has nothing to refresh.
func (r *Runtime) refreshGitHubCredentials(ctx context.Context) {
	if r.gitHubCredential == nil {
		return
	}
	for _, entry := range r.trackedProcesses() {
		token := entry.locator.Claim
		role, ok := token.Role()
		if !ok {
			continue
		}
		rendered, err := r.gitHubCredential(ctx, role)
		if err != nil {
			r.log.Warn("tmux runtime: github credential refresh failed", "claim", token, "role", role, "err", err)
			continue
		}
		changed, err := ghconfig.Write(runtime.GHConfigDir(r.stateDir, token), rendered)
		if err != nil {
			r.log.Warn("tmux runtime: github credential refresh failed", "claim", token, "role", role, "err", fmt.Errorf("write the github credential: %w", err))
			continue
		}
		if changed {
			r.log.Info("tmux runtime: github credential refreshed", "claim", token, "role", role, "app", rendered.App, "expiresAt", rendered.ExpiresAt)
		}
	}
}
