package tmux

import (
	"context"
	"fmt"
	"strings"
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
// skipping a token that names no role, and renders the credential for the pane's write owner: the
// one its launch minted the write token for, recorded on the tracked entry, which is empty for a
// launch on no repository and for the controller, whose bundles are read-only for every owner. For
// a tree pane adopted after a restart, whose launch the runtime never saw, the write owner is the
// daemon's own record of the claim's repository (Options.RepositoryOwner), and an empty answer is a
// claim with none, read-only as its launch was; an adopted controller asks the record nothing.
// Nothing under the pane's gh directory, which the pane's own user can write, decides an owner, and
// a bundle's default owner, the directory GH_CONFIG_DIR names, is never minting authority. It
// rewrites only the owners whose hosts.yml differs (ghconfig.WriteBundle), recreating a root that is
// gone. A record that cannot be read, a render that fails, or a render for a write owner whose
// default is another owner is logged and the pane keeps its last tokens until the next tick. A
// runtime with no credential function (Options.GitHubCredential nil: a daemon with no GitHub Apps)
// wrote no pane any gh files, so it has nothing to refresh.
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
		owner := entry.ghWriter
		if entry.issue == "" && role != claim.RoleController {
			recorded, err := r.repositoryOwner(ctx, token)
			if err != nil {
				failed(fmt.Errorf("the owner of the claim's repository: %w", err))
				continue
			}
			owner = strings.ToLower(recorded)
		}
		bundle, err := r.gitHubCredential(ctx, role, owner)
		if err != nil {
			failed(err)
			continue
		}
		if owner != "" && bundle.Default != owner {
			failed(fmt.Errorf("the credential's default owner %s is not the pane's write owner %s", bundle.Default, owner))
			continue
		}
		root := runtime.GHConfigDir(r.stateDir, token)
		changed, err := ghconfig.WriteBundle(root, bundle)
		if err != nil {
			failed(fmt.Errorf("write the github credential: %w", err))
			continue
		}
		if len(changed) > 0 {
			written := bundle.Owners[bundle.Default]
			r.log.Info("tmux runtime: github credential refreshed", "claim", token, "role", role, "app", written.App,
				"expiresAt", written.ExpiresAt, "owners", changed)
		}
	}
}
