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
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// ErrOwnerOutsideProject is GitHubCredential's refusal of a controller asked for a default owner
// that is neither one of the project's nor a configured rename's alias owner.
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
//
// A configured repository rename (renames, `repository_renames`) whose `to` the project owns is
// one pair of owners, not two: the bundle carries one entry for both, minted for the owner the
// repository moved to, written into that owner's directory and into the owner it moved from's, and
// that one lease is the write one whenever either of them is the bundle's Default. A pod's
// GH_CONFIG_DIR is fixed at its launch from the owner its issue's record names, which over a
// transfer is still the old one, so the directory a live pod reads has to carry the token for the
// repository as GitHub holds it now; the directory's name authorises nothing, the token in it
// does. Such an alias owner is a default the controller may ask for too. Where the owner the
// repository moved from is itself an owner of the project, its own entry wins — its repositories
// are the project's and its token is its own — which is a conflict the mapping's load warns of,
// once per load, where the owners that decide it are read (daemon's logRenameOwnerConflicts).
func GitHubCredential(tokens Tokens, owners func() []string, renames ghrepo.Renames) runtime.GitHubCredential {
	return func(ctx context.Context, role claim.Role, owner string) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(role)
		owner = strings.ToLower(owner)
		writer := owner
		all := map[string]bool{}
		for _, o := range owners() {
			all[strings.ToLower(o)] = true
		}
		// aliases is each owner a configured rename moved a repository away from, by the owner it
		// moved to: the pair the bundle holds one entry for, and the daemon's one rule for whose
		// installation holds a renamed repository now (ghrepo.Renames.OwnerAliases, which the
		// mint sites of every GitHub call read too). Its conflicts are left to the mapping's load,
		// so this is a pure function of its inputs and may be built per call.
		aliases, _ := renames.OwnerAliases(func(name string) bool { return all[name] })
		if role == claim.RoleController {
			writer = ""
			if _, alias := aliases[owner]; owner != "" && !all[owner] && !alias {
				return ghconfig.Bundle{}, fmt.Errorf("the controller's default owner %q %w", owner, ErrOwnerOutsideProject)
			}
		}
		if owner != "" {
			all[owner] = true
		}
		for from := range aliases {
			all[from] = true
		}
		names := slices.Sorted(maps.Keys(all))
		if len(names) == 0 {
			return ghconfig.Bundle{}, fmt.Errorf("the %s App for %s: %w", appRole, role, runtime.ErrNoGitHubOwner)
		}
		// mintFor is the owner a directory's lease is minted for: its own, or, for an alias, the
		// owner its repository moved to, whose installation holds that repository now.
		mintFor := func(name string) string {
			if to, alias := aliases[name]; alias {
				return to
			}
			return name
		}
		// writer is the empty owner for the controller, which writes to none, and mintFor answers
		// it unchanged, since no alias is keyed by it: write is then an owner no target equals.
		write := mintFor(writer)
		bundle := ghconfig.Bundle{Default: owner, Owners: make(map[string]ghconfig.Rendered, len(names))}
		if owner == "" {
			bundle.Default = names[0]
		}
		// A pair's two directories hold one entry, so its lease is minted once however many
		// directories carry it; with no alias every directory mints its own, as it always has. The
		// target alone keys them: whether a target's lease is the write one is `target == write`,
		// so one target is never both kinds.
		entries := map[string]ghconfig.Rendered{}
		for _, name := range names {
			target := mintFor(name)
			mint, how := tokens.ReadToken, "read-only "
			if target == write {
				mint, how = tokens.Token, ""
			}
			entry, held := entries[target]
			if !held {
				lease, err := mint(ctx, appRole, target)
				if err != nil {
					return ghconfig.Bundle{}, fmt.Errorf("mint the %s%s App lease for %s on %s: %w", how, appRole, role, target, err)
				}
				entry = ghconfig.Render(lease.Token, string(appRole), lease.ExpiresAt)
				entries[target] = entry
			}
			bundle.Owners[name] = entry
		}
		return bundle, nil
	}
}
