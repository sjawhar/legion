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

// ErrOwnerOutsideProject is ControllerCredential's refusal of a controller asked for a default
// owner that is neither one of the project's nor a configured rename's alias owner.
var ErrOwnerOutsideProject = errors.New("is not an owner of the project's repositories")

// GitHubCredential is the one function every role's gh files come from (runtime.GitHubCredential)
// over tokens: a bundle whose directories (GitHubDirs, built from the same description) each hold
// one lease of the role's App, rendered as gh's hosts.yml and config.yml and named for the App and
// its expiry so a log can say whose token it wrote. Every lease names the repositories it reaches
// and no others, so a directory's token is never wider than the directory's own name says:
//
//   - the owner directory of the owner whose installation holds the role's own repository holds
//     the write lease for that one repository (Tokens.Token), and each other repository the
//     mapping serves under that owner has a directory of its own, `<owner>/<repo>`, holding a
//     read-only lease (Tokens.ReadToken) naming that repository alone;
//   - every other owner's directory holds one read-only lease over the repositories the mapping
//     serves under that owner.
//
// Default is the issue's own owner's directory, which GH_CONFIG_DIR names, lowercased: the owner
// the daemon's record names, which over a configured transfer is still the one the repository
// moved from. A pod's GH_CONFIG_DIR is fixed at its launch, and the directory's name authorises
// nothing — the token in it does. A call that names no repository at all, which the Sandbox's
// controller launch makes, gets every directory read-only and the first owner, sorted, as its
// default, since a controller writes to no repository; a project with no owner at all to mint for
// is runtime.ErrNoGitHubOwner. The controller route of the API asks for its own bundle by owner
// instead (ControllerCredential).
//
// A Manager holds every lease and re-mints it as it nears its expiry, so a launch and every
// refresh ask GitHub only when a lease turns over, and one lease is minted per scope however many
// directories carry it: a configured rename's two owner directories hold the one lease, as do the
// repository directories under them.
func GitHubCredential(tokens Tokens, repos func() []ghrepo.Repository, renames ghrepo.Renames) runtime.GitHubCredential {
	return func(ctx context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(role)
		dirs, owners, err := bundleDirs(repos, renames, repo)
		if err != nil {
			return ghconfig.Bundle{}, err
		}
		if len(owners) == 0 {
			return ghconfig.Bundle{}, fmt.Errorf("the %s App for %s: %w", appRole, role, runtime.ErrNoGitHubOwner)
		}
		dflt := strings.ToLower(repo.Owner())
		if dflt == "" {
			dflt = firstOwner(owners)
		}
		return mintBundle(ctx, tokens, appRole, role, dflt, dirs)
	}
}

// ControllerCredential is the controller's own bundle, which the API's controller route serves
// `legion controller start` (internal/api's controllerGitHubCredential): the review App's
// read-only lease for every owner of the project, each narrowed to the repositories that owner
// serves, with no repository directory under any of them, since a controller writes to none and
// reads them all. Its default is the owner it is asked for — the one its launch recorded, which
// its GH_CONFIG_DIR names, and which over a configured rename may be an alias owner — or the
// first owner, sorted, when it is asked for none.
//
// An owner outside the project is refused (ErrOwnerOutsideProject) before anything is minted: the
// controller asks for its default over the API, and a default is no authority to read another
// owner's repositories. A project with no owner at all is runtime.ErrNoGitHubOwner, which the
// route answers as a daemon with nothing to act as.
func ControllerCredential(tokens Tokens, repos func() []ghrepo.Repository, renames ghrepo.Renames) runtime.ControllerCredential {
	return func(ctx context.Context, defaultOwner string) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(claim.RoleController)
		dirs, owners, err := bundleDirs(repos, renames, ghrepo.Repository{})
		if err != nil {
			return ghconfig.Bundle{}, err
		}
		owner := strings.ToLower(defaultOwner)
		if owner != "" && !owners[owner] {
			return ghconfig.Bundle{}, fmt.Errorf("the controller's default owner %q %w", owner, ErrOwnerOutsideProject)
		}
		if len(owners) == 0 {
			return ghconfig.Bundle{}, fmt.Errorf("the %s App for %s: %w", appRole, claim.RoleController, runtime.ErrNoGitHubOwner)
		}
		if owner == "" {
			owner = firstOwner(owners)
		}
		return mintBundle(ctx, tokens, appRole, claim.RoleController, owner, dirs)
	}
}

// GitHubDirs is the directory names a gh bundle built now for repo would hold, sorted: the answer
// the Sandbox runtime's drift check reads a running pod's gh projection against
// (sandbox.Options.GitHubDirs), since a pod's projection is fixed at the directories of its
// creation. A zero repo is the controller's owner-only set, the bundle its pod was launched with.
//
// It and GitHubCredential are built from one description of the bundle (bundleDirs), so the set a
// pod is compared against and the set a pod would be handed cannot drift apart. It mints nothing
// and calls GitHub not at all, reading the mapping and the configured renames alone, since the
// drift check reads it on every evaluation. A bundle no launch could write — a directory name
// ghconfig refuses, which is what GitHubCredential fails on — is no set to compare a pod against
// and answers none, leaving every pod where it is.
func GitHubDirs(repos func() []ghrepo.Repository, renames ghrepo.Renames, repo ghrepo.Repository) []string {
	dirs, _, err := bundleDirs(repos, renames, repo)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		names = append(names, dir.name)
	}
	return names
}

// bundleDir is one directory of a gh bundle: name, its path under the bundle's root, and the one
// lease it holds.
type bundleDir struct {
	name string
	hold dirLease
}

// dirLease is the ask that mints one directory's lease: the write lease for repo, the one
// repository a role works, or a read-only lease over owner's repos. Both name the repositories
// they reach, which is what keeps a directory's token no wider than its name.
type dirLease struct {
	write bool
	repo  ghrepo.Repository
	owner string
	repos []string
}

// scope is what two directories hold the one lease under: the lease's kind and what it reaches,
// as the Manager keys its cache (leaseScope), so the grouping here and the Manager's are the one
// rule. A configured rename's two owner directories share one scope, as do the repository
// directories under them.
func (d dirLease) scope(role AppRole) scope {
	if d.write {
		return leaseScope(false, role, d.repo.Owner(), []string{d.repo.Name()})
	}
	return leaseScope(true, role, d.owner, d.repos)
}

// mint asks tokens for the lease, as the App role.
func (d dirLease) mint(ctx context.Context, tokens Tokens, role AppRole) (Lease, error) {
	if d.write {
		return tokens.Token(ctx, role, d.repo)
	}
	return tokens.ReadToken(ctx, role, d.owner, d.repos)
}

// where names what the lease reaches, for a refusal to say which mint failed: the repository a
// write lease names, and the owner and repositories a read lease does. It carries no token, a
// refusal being about the ask.
func (d dirLease) where() string {
	if d.write {
		return d.repo.String()
	}
	return d.owner + " (" + strings.Join(d.repos, ", ") + ")"
}

// firstOwner is the first of the bundle's owner directories, sorted: the default of a bundle
// whose caller named no owner of its own. Both callers refuse an empty set before they ask.
func firstOwner(owners map[string]bool) string {
	return slices.Min(slices.Collect(maps.Keys(owners)))
}

// bundleDirs is the one description of a role's gh bundle: every directory it holds, sorted by
// name, with the lease each one carries, and dirOwners, the owner directories the bundle holds —
// the mapping's owners lowercased, every owner a configured rename moved a repository away from
// (ghrepo.Renames.OwnerAliases) beside them, and repo's own recorded owner, which is the role's
// and not the project's. GitHubCredential mints from it and GitHubDirs names it, so neither can
// hold a directory the other does not.
//
// A rename's alias owner is no owner of its own: its directory's lease is minted for the owner
// the repository moved to, whose installation holds it now (mintOwner below), which makes the
// pair's two directories one entry. repo is read as the project serves it now
// (ghrepo.Renames.Serving over the mapping's owners, the rule repositories.serving reads), so the
// write lease names the repository as GitHub holds it rather than as the record does.
//
// The conflict a rename between two owners of the project makes is left to the mapping's load,
// which warns of it once per load (the daemon's logRenameOwnerConflicts), so this is a pure
// function of its inputs and may be built per call. A directory name ghconfig would refuse
// (ghconfig.IsDirName) is an error: a bundle is refused rather than written from a name that is not
// one.
func bundleDirs(repos func() []ghrepo.Repository, renames ghrepo.Renames, repo ghrepo.Repository) (dirs []bundleDir, dirOwners map[string]bool, err error) {
	owners, served := map[string]bool{}, map[string][]string{}
	for _, mapped := range repos() {
		owner := strings.ToLower(mapped.Owner())
		owners[owner] = true
		served[owner] = append(served[owner], strings.ToLower(mapped.Name()))
	}
	// OwnerAliases is asked of the project's own owners, an alias being no owner of it, so it is
	// read before the aliases join the directory names.
	aliases, _ := renames.OwnerAliases(func(name string) bool { return owners[name] })
	serving := renames.Serving(repo, func(name string) bool { return owners[name] })
	for owner, names := range served {
		slices.Sort(names)
		served[owner] = slices.Compact(names)
	}
	// mintOwner is the owner a directory's lease is minted for: its own, or, for an alias, the
	// owner its repository moved to.
	mintOwner := func(name string) string {
		if to, alias := aliases[name]; alias {
			return to
		}
		return name
	}
	dirOwners = maps.Clone(owners)
	for from := range aliases {
		dirOwners[from] = true
	}
	if recorded := strings.ToLower(repo.Owner()); recorded != "" {
		dirOwners[recorded] = true
	}
	// write is the owner whose installation holds the role's own repository, and so whose
	// directories carry the write lease. It is an owner no directory equals for a call that names
	// no repository, the Sandbox's controller launch, which writes to none.
	write := ""
	if !serving.IsZero() {
		write = mintOwner(strings.ToLower(serving.Owner()))
	}
	servingOwner, servingName := strings.ToLower(serving.Owner()), strings.ToLower(serving.Name())
	for _, name := range slices.Sorted(maps.Keys(dirOwners)) {
		target := mintOwner(name)
		if write == "" || target != write {
			dirs = append(dirs, bundleDir{name: name, hold: dirLease{owner: target, repos: served[target]}})
			continue
		}
		dirs = append(dirs, bundleDir{name: name, hold: dirLease{write: true, repo: serving}})
		for _, held := range served[target] {
			if target == servingOwner && held == servingName {
				continue
			}
			dirs = append(dirs, bundleDir{name: name + "/" + held, hold: dirLease{owner: target, repos: []string{held}}})
		}
	}
	slices.SortFunc(dirs, func(a, b bundleDir) int { return strings.Compare(a.name, b.name) })
	for _, dir := range dirs {
		if !ghconfig.IsDirName(dir.name) {
			return nil, dirOwners, fmt.Errorf("the gh bundle directory %q is no lowercased GitHub login or owner/repository", dir.name)
		}
	}
	return dirs, dirOwners, nil
}

// mintBundle is the bundle dirs describe, with dflt its default directory: one mint per scope,
// shared by every directory that holds it, each rendered as gh's two files. A mint that fails
// fails the whole bundle, naming the role, the App and what the lease would have reached, and
// never the token.
func mintBundle(ctx context.Context, tokens Tokens, appRole AppRole, role claim.Role, dflt string, dirs []bundleDir) (ghconfig.Bundle, error) {
	bundle := ghconfig.Bundle{Default: dflt, Dirs: make(map[string]ghconfig.Rendered, len(dirs))}
	minted := make(map[scope]ghconfig.Rendered, len(dirs))
	for _, dir := range dirs {
		key := dir.hold.scope(appRole)
		entry, held := minted[key]
		if !held {
			lease, err := dir.hold.mint(ctx, tokens, appRole)
			if err != nil {
				how := "read-only "
				if dir.hold.write {
					how = ""
				}
				return ghconfig.Bundle{}, fmt.Errorf("mint the %s%s App lease for %s on %s: %w", how, appRole, role, dir.hold.where(), err)
			}
			entry = ghconfig.Render(lease.Token, string(appRole), lease.ExpiresAt)
			minted[key] = entry
		}
		bundle.Dirs[dir.name] = entry
	}
	return bundle, nil
}
