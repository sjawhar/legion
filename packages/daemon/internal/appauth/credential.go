package appauth

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// ErrOwnerOutsideProject is GitHubCredential's refusal of a controller asked for a default owner
// that is not one of the project's.
var ErrOwnerOutsideProject = errors.New("is not an owner of the project's repositories")

// GitHubCredential is the one function every role's gh files come from (runtime.GitHubCredential)
// over tokens: a bundle with a directory for every owner of the project (owners, lowercased, and
// owner when it is outside them) — the role's App's lease for owner, the one the role writes to,
// and that App's read-only lease (Tokens.ReadToken) for every other — each rendered as gh's
// hosts.yml and config.yml, named for the App and its expiry so a log can say whose token it
// wrote, owner's directory the default. A role's owner is its issue's repository's, which the
// daemon's own record names. The controller writes to no owner: every owner is read-only, owner's
// directory the default when it names one (a refresh asks for the default the controller's launch
// recorded, which its GH_CONFIG_DIR names) and the first owner, sorted, otherwise; an owner outside
// the project is refused (ErrOwnerOutsideProject) before anything is minted, since the controller
// asks for its default over the API and a default is no authority to read another owner's
// repositories. The runtimes, and the controller route `legion controller start` fetches, call it;
// a Manager holds every lease and re-mints it as it nears its expiry, so a launch and every refresh
// ask GitHub only when a lease turns over.
func GitHubCredential(tokens Tokens, owners func() []string) runtime.GitHubCredential {
	return func(ctx context.Context, role claim.Role, owner string) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(role)
		owner = strings.ToLower(owner)
		writer := owner
		all := map[string]bool{}
		for _, o := range owners() {
			all[strings.ToLower(o)] = true
		}
		if role == claim.RoleController {
			writer = ""
			if owner != "" && !all[owner] {
				return ghconfig.Bundle{}, fmt.Errorf("the controller's default owner %q %w", owner, ErrOwnerOutsideProject)
			}
		}
		if owner != "" {
			all[owner] = true
		}
		names := slices.Sorted(maps.Keys(all))
		if len(names) == 0 {
			return ghconfig.Bundle{}, fmt.Errorf("the %s App for %s: %w", appRole, role, runtime.ErrNoGitHubOwner)
		}
		bundle := ghconfig.Bundle{Default: owner, Owners: make(map[string]ghconfig.Rendered, len(names))}
		if owner == "" {
			bundle.Default = names[0]
		}
		for _, name := range names {
			mint, how := tokens.ReadToken, "read-only "
			if name == writer {
				mint, how = tokens.Token, ""
			}
			lease, err := mint(ctx, appRole, name)
			if err != nil {
				return ghconfig.Bundle{}, fmt.Errorf("mint the %s%s App lease for %s on %s: %w", how, appRole, role, name, err)
			}
			bundle.Owners[name] = ghconfig.Render(lease.Token, string(appRole), lease.ExpiresAt)
		}
		return bundle, nil
	}
}
