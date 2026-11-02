package appauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

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
// are the project's and its token is its own — and the conflict is logged once, naming the pair.
func GitHubCredential(tokens Tokens, owners func() []string, renames ghrepo.Renames, log *slog.Logger) runtime.GitHubCredential {
	if log == nil {
		log = slog.Default()
	}
	var conflictMu sync.Mutex
	conflicted := map[ghrepo.Rename]bool{}
	return func(ctx context.Context, role claim.Role, owner string) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(role)
		owner = strings.ToLower(owner)
		writer := owner
		all := map[string]bool{}
		for _, o := range owners() {
			all[strings.ToLower(o)] = true
		}
		// aliases is each owner a configured rename moved a repository away from, by the owner it
		// moved to: the pair the bundle holds one entry for. A rename whose `to` the project does
		// not own is no alias — nothing of it is served — and neither is one inside one owner.
		aliases := map[string]string{}
		for _, pair := range renames.Pairs() {
			from, to := strings.ToLower(pair.From.Owner()), strings.ToLower(pair.To.Owner())
			switch {
			case from == to || !all[to]:
				continue
			case all[from]:
				conflictMu.Lock()
				if !conflicted[pair] {
					conflicted[pair] = true
					log.Warn("appauth: a configured repository rename moved a repository between two owners of this project, so each owner keeps its own gh directory and its own token",
						"from", pair.From.String(), "to", pair.To.String(), "owner", from)
				}
				conflictMu.Unlock()
				continue
			}
			aliases[from] = to
		}
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
