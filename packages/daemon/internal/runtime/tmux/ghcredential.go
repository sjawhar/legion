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

// refreshGitHubCredentials brings the gh bundle of every tracked pane to the current render of its
// role's GitHub credential, so the pane's gh (the `legion gh` wrapper's real gh) and the clone's
// `legion git-credential` helper read the tokens the daemon holds now. It walks a snapshot of the
// watch (trackedProcesses), takes each pane's role from its claim token (claim.Token.Role),
// skipping a token that names no role, and renders the credential for the pane's default owner:
// the one its launch recorded on the tracked entry, or, for a pane adopted after a restart, the one
// its bundle root records (ghconfig.ReadDefault). It rewrites only the owners whose hosts.yml
// differs (ghconfig.WriteBundle), recreating a root that is gone. A default it cannot read, a
// render that fails, or a render whose default is not the pane's — which would leave the pane's
// GH_CONFIG_DIR naming a directory the bundle no longer holds — is logged and the pane keeps its
// last tokens until the next tick. A runtime with no credential function (Options.GitHubCredential
// nil: a daemon with no GitHub Apps) wrote no pane any gh files, so it has nothing to refresh.
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
		failed := func(err error) {
			r.log.Warn("tmux runtime: github credential refresh failed", "claim", token, "role", role, "err", err)
		}
		root := runtime.GHConfigDir(r.stateDir, token)
		owner := entry.ghDefault
		if owner == "" {
			recorded, err := ghconfig.ReadDefault(root)
			if err != nil {
				failed(err)
				continue
			}
			owner = recorded
		}
		bundle, err := r.gitHubCredential(ctx, role, owner)
		if err != nil {
			failed(err)
			continue
		}
		if bundle.Default != owner {
			failed(fmt.Errorf("the credential's default owner %s is not the pane's %s", bundle.Default, owner))
			continue
		}
		changed, err := ghconfig.WriteBundle(root, bundle)
		if err != nil {
			failed(fmt.Errorf("write the github credential: %w", err))
			continue
		}
		if len(changed) > 0 {
			written := bundle.Owners[owner]
			r.log.Info("tmux runtime: github credential refreshed", "claim", token, "role", role, "app", written.App,
				"expiresAt", written.ExpiresAt, "owners", changed)
		}
	}
}
