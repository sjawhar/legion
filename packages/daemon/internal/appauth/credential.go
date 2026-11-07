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
// over tokens: a bundle with a directory for every owner of the project (GitHubOwners — the owner
// of every repository served answers, with every owner a resolving rename moved a repository away
// from beside them — and the request's own owner when it falls outside that set), each rendered as
// gh's hosts.yml and config.yml, named for the App and its expiry so a log can say whose token it
// wrote, and the request's own directory the bundle's default.
//
// A request's own owner is its issue's recorded repository's when repo names one, which is what a
// workflow role passes, and owner otherwise, which is what the controller passes and what a
// workflow role whose claim names no repository has always passed. A role writes through that one
// directory and reads through every other.
//
// The directory a role writes through is named for the owner its record keeps, and the token in
// it is minted for the owner of the repository GitHub holds that record's repository under now
// (ghrepo.Renames.Serving, read against the repositories the project serves), which is the
// recorded owner's own wherever no configured pair resolves. A pod's GH_CONFIG_DIR is fixed at
// its launch from the owner its issue's record names, and a transfer does not move it, so the
// directory a live pod reads has to carry the token for the repository as GitHub holds it now:
// the directory's name authorises nothing, the token in it does.
//
// Which read lease every other directory carries is the other half of that one rule. An owner the
// project serves a repository of under its own name carries its own read lease, whatever a pair
// says of some other repository of that owner — its repositories are the project's and its token
// is its own. An owner that only a resolving rename puts in the bundle serves no repository of
// its own, so it carries the read lease of the owner that serves the moved repository now
// (projectOwners' aliases), which is the only token that reads what a record under that owner's
// name points at. Both are decided per repository: a rename puts no owner in the bundle whose
// moved repository the project does not serve, since such a pair resolves for nobody.
//
// The controller writes to no owner: every directory is read-only, its own directory the default
// when it names one (a refresh asks for the default the controller's launch recorded, which its
// GH_CONFIG_DIR names) and the first owner, sorted, otherwise; an owner outside the project is
// refused (ErrOwnerOutsideProject) before anything is minted, since the controller asks for its
// default over the API and a default is no authority to read another owner's repositories. The
// runtimes, and the controller route `legion controller start` fetches, call it; a Manager holds
// every lease and re-mints it as it nears its expiry, so a launch and every refresh ask GitHub
// only when a lease turns over.
func GitHubCredential(tokens Tokens, served func() []ghrepo.Repository, renames ghrepo.Renames) runtime.GitHubCredential {
	return func(ctx context.Context, role claim.Role, repo ghrepo.Repository, owner string) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(role)
		// own is the owner the request's own record names: its repository's when it carries one,
		// and the owner it asks for when it carries no repository at all.
		own := strings.ToLower(owner)
		if !repo.IsZero() {
			own = strings.ToLower(repo.Owner())
		}
		// The served repositories are read once, so the owner set, the aliases and the rename the
		// write lease is minted through are one view of a mapping another goroutine may be
		// replacing, rather than three reads of it.
		repos := served()
		owners, aliases := projectOwners(repos, renames)
		all := directories(owners, aliases)
		// writeDir is the one directory the role writes through, named for own, and writeTarget
		// the owner that directory's lease is minted for. The controller writes through none, so
		// its writeDir is the empty owner, which no directory is named for.
		writeDir, writeTarget := own, own
		if role == claim.RoleController {
			writeDir, writeTarget = "", ""
			if own != "" && !all[own] {
				return ghconfig.Bundle{}, fmt.Errorf("the controller's default owner %q %w", own, ErrOwnerOutsideProject)
			}
		} else if !repo.IsZero() {
			writeTarget = strings.ToLower(renames.Serving(repo, func(candidate ghrepo.Repository) bool {
				return ghrepo.Serves(repos, candidate)
			}).Owner())
		}
		if own != "" {
			all[own] = true
		}
		names := slices.Sorted(maps.Keys(all))
		if len(names) == 0 {
			return ghconfig.Bundle{}, fmt.Errorf("the %s App for %s: %w", appRole, role, runtime.ErrNoGitHubOwner)
		}
		bundle := ghconfig.Bundle{Default: own, Owners: make(map[string]ghconfig.Rendered, len(names))}
		if own == "" {
			bundle.Default = names[0]
		}
		// Each lease is minted once, keyed by the owner it is for and its kind, so a pair's two
		// directories ask GitHub once between them while a deployment with no pair mints one lease
		// per directory, as it always has. The kind is half the key because one owner can need
		// both at once: where a pair resolves, the owner that serves the moved repository holds
		// the write lease in the recorded owner's directory and its own read lease in its own.
		entries := map[bundleLease]ghconfig.Rendered{}
		for _, name := range names {
			want := bundleLease{owner: name}
			switch to, alias := aliases[name]; {
			case name == writeDir:
				want = bundleLease{owner: writeTarget, write: true}
			case alias && !owners[name]:
				want.owner = to
			}
			entry, held := entries[want]
			if !held {
				mint, how := tokens.ReadToken, "read-only "
				if want.write {
					mint, how = tokens.Token, ""
				}
				lease, err := mint(ctx, appRole, want.owner)
				if err != nil {
					return ghconfig.Bundle{}, fmt.Errorf("mint the %s%s App lease for %s on %s: %w", how, appRole, role, want.owner, err)
				}
				entry = ghconfig.Render(lease.Token, string(appRole), lease.ExpiresAt)
				entries[want] = entry
			}
			bundle.Owners[name] = entry
		}
		return bundle, nil
	}
}

// bundleLease is one lease of a bundle: the owner it is minted for and whether it is that owner's
// write lease (Tokens.Token) or its read-only one (Tokens.ReadToken). The kind is part of the key
// because a resolving rename needs one owner's two leases in one bundle — the write lease in the
// directory named for the owner the repository moved away from, and that owner's own read lease
// in its own directory — and a cache keyed by the owner alone would hand one of those directories
// the other's token. It is no relation to the Manager's own cache key (leaseKey, a string over an
// App role and an owner): this one keys the leases of a single bundle.
type bundleLease struct {
	owner string
	write bool
}

// GitHubOwners is the GitHub owners the daemon serves the project with, lowercased, deduplicated
// and sorted: the owner of every repository the project serves (served, the mapping's) with every
// owner a configured rename moved a repository away from beside it, for the pairs that resolve
// against that same set (ghrepo.Renames.OwnerAliases), each of which is a directory of the gh
// bundle a role is handed. It is the project's set alone: the owner a request names is a role's
// own and GitHubCredential adds it there, so nothing a request carries reaches this answer.
//
// It takes the served repositories, not their owners, because a rename resolves per repository:
// whether the owner a pair moved a repository away from is a directory at all turns on the
// project serving the repository that moved, never on it serving some other repository of either
// owner.
//
// GitHubCredential builds its bundle from it, and the Sandbox runtime compares it against the
// owners a running pod's gh projection was fixed at (sandbox.Options.GitHubOwners), so the set a
// pod created now would hold and the set a pod is read against are one computation. It reads the
// mapping and the configured renames and nothing else: no token is minted and GitHub is not
// called.
func GitHubOwners(served func() []ghrepo.Repository, renames ghrepo.Renames) []string {
	return slices.Sorted(maps.Keys(directories(projectOwners(served(), renames))))
}

// projectOwners is the project's half of a bundle, in the two parts GitHubCredential decides each
// directory's lease from: owners, the owner of every repository the project serves under its own
// name, lowercased, and aliases, the owner each configured pair moved a repository away from by
// the owner it moved to, for the pairs that resolve (ghrepo.Renames.OwnerAliases: the pair's `to`
// served and its `from` not).
//
// The two sets overlap, and that overlap is why they are kept apart rather than merged into one
// map of directories: a project that serves another repository of the owner a pair moved a
// repository away from both serves that owner's own repositories and is the pair's alias, so its
// directory carries its own read lease while the pair still resolves for the moved repository
// alone. The pairs are read against repos, one repository at a time (ghrepo.Serves), and never
// against the owners those repositories have.
//
// It is a pure function of its inputs and may be built per call; the mapping behind repos is read
// once, by the caller.
func projectOwners(repos []ghrepo.Repository, renames ghrepo.Renames) (owners map[string]bool, aliases map[string]string) {
	owners = make(map[string]bool, len(repos))
	for _, repo := range repos {
		owners[strings.ToLower(repo.Owner())] = true
	}
	aliases = renames.OwnerAliases(func(repo ghrepo.Repository) bool { return ghrepo.Serves(repos, repo) })
	return owners, aliases
}

// directories is the set of directories the project's own half of a bundle holds: an owner for
// every served repository, and every owner a resolving rename moved a repository away from, which
// a tree recorded before that transfer still names. GitHubCredential adds the request's own owner
// to the set this answers; nothing else does.
func directories(owners map[string]bool, aliases map[string]string) map[string]bool {
	all := maps.Clone(owners)
	for from := range aliases {
		all[from] = true
	}
	return all
}
