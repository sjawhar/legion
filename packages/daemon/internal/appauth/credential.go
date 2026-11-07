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
// nothing — the token in it does. A call that names no repository at all, which a tmux tree pane
// whose claim the daemon records no repository for makes, gets every directory read-only and the
// first owner the mint answered for, sorted, as its default, since it writes to no repository; a
// project with no owner at all to mint for is runtime.ErrNoGitHubOwner. Every controller bundle,
// the launch's and the API route's both, is asked for by owner instead (ControllerCredential).
//
// A read lease GitHub refuses the narrowing of costs only the repositories it names: those are
// dropped and the rest of the bundle is minted and served (mintBundle), each dropped repository
// said once on log. The write lease is not softened — a role cannot work its own repository
// without it — so its refusal fails the whole bundle.
//
// A directory whose every repository is refused is left out of the bundle and recorded in left
// (LeftOut), which GitHubDirs reads, so the drift check never asks a pod for a directory no mint
// produces. The one directory that cannot be left out is the bundle's own default, which the
// caller named: that is ErrNarrowingRefused, naming the owner and the repositories the lease
// would have reached, since a bundle whose GH_CONFIG_DIR holds no token is no bundle to launch
// a role on.
//
// A Manager holds every lease and re-mints it as it nears its expiry, so a launch and every
// refresh ask GitHub only when a lease turns over, and one lease is minted per scope however many
// directories carry it: a configured rename's two owner directories hold the one lease, as do the
// repository directories under them.
func GitHubCredential(tokens Tokens, repos func() []ghrepo.Repository, renames ghrepo.Renames, left *LeftOut, log *slog.Logger) runtime.GitHubCredential {
	return func(ctx context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(role)
		layout, err := bundleDirs(repos, renames, repo)
		if err != nil {
			return ghconfig.Bundle{}, err
		}
		if len(layout.owners) == 0 {
			return ghconfig.Bundle{}, fmt.Errorf("the %s App for %s: %w", appRole, role, runtime.ErrNoGitHubOwner)
		}
		return mintBundle(ctx, tokens, appRole, role, strings.ToLower(repo.Owner()), layout, left, log)
	}
}

// ControllerCredential is the controller's own bundle, and the one producer of it: a controller's
// launch asks for it, and the API's controller route serves `legion controller start` from it
// (internal/api's controllerGitHubCredential). It is the review App's
// read-only lease for every owner of the project, each narrowed to the repositories that owner
// serves, with no repository directory under any of them, since a controller writes to none and
// reads them all. Its default is the owner it is asked for — the one its launch recorded, which
// its GH_CONFIG_DIR names, and which over a configured rename may be an alias owner — or the
// first owner the mint answered for, sorted, when it is asked for none.
//
// An owner outside the project is refused (ErrOwnerOutsideProject) before anything is minted: the
// controller asks for its default over the API, and a default is no authority to read another
// owner's repositories. A project with no owner at all is runtime.ErrNoGitHubOwner, which the
// route answers as a daemon with nothing to act as. An owner whose every repository GitHub
// refuses the narrowing for is left out of the bundle, as it is of a role's, and recorded in
// left (mintBundle, LeftOut); the one it is asked for cannot be left out, and is
// ErrNarrowingRefused naming that owner and its repositories, since the controller's
// GH_CONFIG_DIR names that directory and nothing else.
func ControllerCredential(tokens Tokens, repos func() []ghrepo.Repository, renames ghrepo.Renames, left *LeftOut, log *slog.Logger) runtime.ControllerCredential {
	return func(ctx context.Context, defaultOwner string) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(claim.RoleController)
		layout, err := bundleDirs(repos, renames, ghrepo.Repository{})
		if err != nil {
			return ghconfig.Bundle{}, err
		}
		owner := strings.ToLower(defaultOwner)
		if owner != "" && !layout.owners[owner] {
			return ghconfig.Bundle{}, fmt.Errorf("the controller's default owner %q %w", owner, ErrOwnerOutsideProject)
		}
		if len(layout.owners) == 0 {
			return ghconfig.Bundle{}, fmt.Errorf("the %s App for %s: %w", appRole, claim.RoleController, runtime.ErrNoGitHubOwner)
		}
		return mintBundle(ctx, tokens, appRole, claim.RoleController, owner, layout, left, log)
	}
}

// LeftOut is the daemon's record of the gh bundle directories a mint found GitHub refuses a lease
// for: one record per project, made by internal/daemon and handed to GitHubCredential,
// ControllerCredential and GitHubDirs alike, so the set a pod's gh projection is compared against
// is the set a mint really produces.
//
// Without it an owner whose every repository GitHub refuses costs the pod more than that owner:
// no bundle holds the directory, so no pod projects it, while the comparison names it at every
// sweep (sandbox's movedGhDirs), an update round asks every worker in the pod to push, the pod is
// replaced, and its new bundle lacks the directory again — for ever (INFRA-513).
//
// It is sticky, not timed: a directory stays recorded left out until a mint of it answers, which
// clears it, and the comparison then names it once and the pod is replaced once, which is right.
// It is kept per App role because a pod's projection is its roles' directory sets intersected
// (sandbox's gitHubBundles): a directory one App's installation does not reach is in no pod's
// projection, however readily the other App mints it.
//
// A nil record holds nothing and names nothing left out: the API's controller route builds a
// bundle of its own per request (internal/api's controllerGitHubCredential), and no pod's
// projection is ever compared against that one.
type LeftOut struct {
	mu   sync.Mutex
	dirs map[AppRole]map[string]bool
}

// NewLeftOut is a record of nothing: every directory the mapping names is served until a mint
// says GitHub refuses it.
func NewLeftOut() *LeftOut { return &LeftOut{dirs: map[AppRole]map[string]bool{}} }

// record holds what role's mint of the directory dir found: left, that GitHub refuses a lease for
// every repository it would hold, or, false, that the mint answered for it, which clears what an
// earlier mint recorded.
func (l *LeftOut) record(role AppRole, dir string, left bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !left {
		delete(l.dirs[role], dir)
		return
	}
	if l.dirs == nil {
		l.dirs = map[AppRole]map[string]bool{}
	}
	if l.dirs[role] == nil {
		l.dirs[role] = map[string]bool{}
	}
	l.dirs[role][dir] = true
}

// left reports that the last mint of dir as some App role left it out of that role's bundle, so
// no pod created now projects it (GitHubDirs).
func (l *LeftOut) left(dir string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, dirs := range l.dirs {
		if dirs[dir] {
			return true
		}
	}
	return false
}

// GitHubDirs is the directory names a gh bundle built now for repo would hold, sorted: the answer
// the Sandbox runtime's drift check reads a running pod's gh projection against
// (sandbox.Options.GitHubDirs), since a pod's projection is fixed at the directories of its
// creation. A zero repo is the controller's owner-only set, the bundle its pod was launched with.
//
// It and GitHubCredential are built from one description of the bundle (bundleDirs), so the set a
// pod is compared against and the set a pod would be handed cannot drift apart. It mints nothing
// and calls GitHub not at all, reading the mapping, the configured renames and what the mints
// before it recorded (left, LeftOut), since the drift check reads it on every evaluation: a
// repository whose name no directory may carry is left out of both (bundleDirs), and a directory
// GitHub refuses every lease of is left out of both as well — of the bundle by the mint that met
// the refusal, and of this answer by the record that mint wrote, so a pod lacking a directory no
// mint produces is no pod to replace.
// A bundle no launch could write — an owner directory ghconfig refuses a name for, which is what
// GitHubCredential fails on — is no set to compare a pod against and answers none, leaving every
// pod where it is.
func GitHubDirs(repos func() []ghrepo.Repository, renames ghrepo.Renames, left *LeftOut, repo ghrepo.Repository) []string {
	layout, err := bundleDirs(repos, renames, repo)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(layout.dirs))
	for _, dir := range layout.dirs {
		if left.left(dir.name) {
			continue
		}
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

// over is d's read lease narrowed to repos: the ask made again, one repository at a time and then
// over the ones that answered, when GitHub refuses the narrowing of the whole (dirEntry).
func (d dirLease) over(repos []string) dirLease {
	return dirLease{owner: d.owner, repos: repos}
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

// bundleLayout is a role's gh bundle as bundleDirs describes it: dirs, every directory it holds
// with the lease each one carries, owners, the owner directories it holds, and unnamed, the
// repository directories left out because ghconfig holds their name to be no directory's
// (ghconfig.IsDirName), which a mint says once on the log.
type bundleLayout struct {
	dirs    []bundleDir
	owners  map[string]bool
	unnamed []bundleDir
}

// bundleDirs is the one description of a role's gh bundle: every directory it holds, sorted by
// name, with the lease each one carries, and the owner directories the bundle holds — the
// mapping's owners lowercased, every owner a configured rename moved a repository away from
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
// function of its inputs and may be built per call.
//
// A repository whose directory name ghconfig refuses (ghconfig.IsDirName) is left out of the
// bundle and named in unnamed: a repository called hosts.yml or config.yml would want a directory
// where gh keeps the owner's own file of that name, and leaving the one repository out costs the
// role that repository alone, where failing would cost it every repository of every owner. An
// owner directory ghconfig refuses a name for is an error: it carries the owner's whole lease and
// may be the bundle's default, so a bundle is refused rather than written from a name that is not
// one.
func bundleDirs(repos func() []ghrepo.Repository, renames ghrepo.Renames, repo ghrepo.Repository) (bundleLayout, error) {
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
	layout := bundleLayout{owners: maps.Clone(owners)}
	for from := range aliases {
		layout.owners[from] = true
	}
	if recorded := strings.ToLower(repo.Owner()); recorded != "" {
		layout.owners[recorded] = true
	}
	// write is the owner whose installation holds the role's own repository, and so whose
	// directories carry the write lease. It is an owner no directory equals for a call that names
	// no repository: every controller bundle, and a tree pane whose claim the daemon records no
	// repository for, which write to none.
	write := ""
	if !serving.IsZero() {
		write = mintOwner(strings.ToLower(serving.Owner()))
	}
	servingOwner, servingName := strings.ToLower(serving.Owner()), strings.ToLower(serving.Name())
	dirs := make([]bundleDir, 0, len(layout.owners))
	for _, name := range slices.Sorted(maps.Keys(layout.owners)) {
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
		switch {
		case ghconfig.IsDirName(dir.name):
			layout.dirs = append(layout.dirs, dir)
		case dir.hold.write || !strings.Contains(dir.name, "/"):
			return bundleLayout{owners: layout.owners}, fmt.Errorf("the gh bundle directory %q is no lowercased GitHub login or owner/repository", dir.name)
		default:
			layout.unnamed = append(layout.unnamed, dir)
		}
	}
	return layout, nil
}

// leftOutRepository is the key one line of a repository left out of a role's gh bundle is said
// under: the App role, and the owner and repository lowercased as a scope's are.
type leftOutRepository struct {
	role  AppRole
	owner string
	repo  string
}

// saidLeftOut holds every repository already said to be left out of a bundle, so the line is said
// once for the life of the process however many bundles are built after it, as a Manager says a
// grant shortfall once (sayShortfalls). It is the package's own, not a bundle's or a Manager's:
// the API builds a controller's bundle per request (internal/api's controllerGitHubCredential),
// and a repository GitHub refuses a lease for is refused at every one of them.
var (
	saidLeftOutMu sync.Mutex
	saidLeftOut   = map[leftOutRepository]bool{}
)

// refusedNarrowing and refusedName are why a repository is left out of a bundle, as its line says
// it: GitHub refusing a lease that names it, or gh keeping the owner's own file of that name
// where the repository's directory would be.
const (
	refusedNarrowing = "the App's installation does not reach it: GitHub refused a token naming it"
	refusedName      = "gh keeps the owner's own file of that name where the repository's directory would be"
)

// sayLeftOut says on log, at warn, that repo of owner is left out of role's gh bundle and why,
// once per App role, owner and repository for the life of the process (saidLeftOut): the role's
// other repositories are served, and what its bundle does not hold is said once rather than at
// every launch and every refresh. It carries no token, a bundle's shortfall being about the ask.
// A caller that holds no logger of its own (sandbox.Options.Log is one) says it on slog.Default,
// as the daemon's own fallback line does (implementTokens.warnOwnToken).
func sayLeftOut(log *slog.Logger, role AppRole, owner, repo, because string) {
	key := leftOutRepository{role: role, owner: strings.ToLower(strings.TrimSpace(owner)), repo: strings.ToLower(strings.TrimSpace(repo))}
	saidLeftOutMu.Lock()
	said := saidLeftOut[key]
	saidLeftOut[key] = true
	saidLeftOutMu.Unlock()
	if said {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	log.Warn("appauth: a repository the project serves is left out of the role's gh bundle",
		"app", string(role), "owner", owner, "repository", repo, "because", because)
}

// mintBundle is the bundle layout describes: one mint per scope, shared by every directory that
// holds it, each rendered as gh's two files, with dflt its default directory — the owner the
// caller named, or, for a caller that named none, the first owner the mint answered for, sorted
// (bundleDefault).
//
// A write mint that fails fails the whole bundle, naming the role, the App and what the lease
// would have reached, and never the token: a role cannot work its own repository without it. A
// read mint GitHub refuses the narrowing of (ErrNarrowingRefused) costs only the repositories it
// names, which is what a repository dropped from the App's selected repositories, deleted, or
// transferred without a configured rename makes: the lease is asked for again one repository at a
// time, the directory holds a lease over the ones that answered, and every refused repository is
// said once on log (sayLeftOut). A directory whose every repository is refused is left out of the
// bundle, as is one ghconfig holds no name for (layout.unnamed). Every other failure of a read
// mint fails the bundle as a write mint's does.
//
// Every directory of the layout is recorded in left as this mint found it, minted or left out
// (LeftOut), so the drift check's own answer (GitHubDirs) names what a bundle built now holds and
// nothing more.
func mintBundle(ctx context.Context, tokens Tokens, appRole AppRole, role claim.Role, dflt string, layout bundleLayout, left *LeftOut, log *slog.Logger) (ghconfig.Bundle, error) {
	for _, dir := range layout.unnamed {
		sayLeftOut(log, appRole, dir.hold.owner, strings.Join(dir.hold.repos, ", "), refusedName)
	}
	bundle := ghconfig.Bundle{Dirs: make(map[string]ghconfig.Rendered, len(layout.dirs))}
	minted := make(map[scope]ghconfig.Rendered, len(layout.dirs))
	refused := map[scope]bool{}
	for _, dir := range layout.dirs {
		key := dir.hold.scope(appRole)
		entry, held := minted[key]
		if !held && !refused[key] {
			rendered, reached, err := dirEntry(ctx, tokens, appRole, dir.hold, log)
			if err != nil {
				how := "read-only "
				if dir.hold.write {
					how = ""
				}
				return ghconfig.Bundle{}, fmt.Errorf("mint the %s%s App lease for %s on %s: %w", how, appRole, role, dir.hold.where(), err)
			}
			if reached {
				entry, held = rendered, true
				minted[key] = rendered
			} else {
				refused[key] = true
			}
		}
		left.record(appRole, dir.name, !held)
		if !held {
			continue
		}
		bundle.Dirs[dir.name] = entry
	}
	dflt, err := bundleDefault(bundle, appRole, role, dflt, layout)
	if err != nil {
		return ghconfig.Bundle{}, err
	}
	bundle.Default = dflt
	return bundle, nil
}

// bundleDefault is the directory the bundle's GH_CONFIG_DIR names: dflt, the owner the caller
// asked for, or, where it asked for none, the first owner directory the mint answered for,
// sorted (layout.dirs comes sorted by name).
//
// An owner the caller named whose directory no mint answered for is ErrNarrowingRefused, naming
// the owner and the repositories the lease would have reached: the bundle is served from that
// directory, so a bundle without it would be refused later as one whose default holds no gh
// files (ghconfig.Bundle.Validate), long after the ask that could say why. A caller that named
// none and whose every owner was refused is the same refusal over every owner at once.
func bundleDefault(bundle ghconfig.Bundle, appRole AppRole, role claim.Role, dflt string, layout bundleLayout) (string, error) {
	if dflt == "" {
		for _, dir := range layout.dirs {
			if layout.owners[dir.name] {
				if _, held := bundle.Dirs[dir.name]; held {
					return dir.name, nil
				}
			}
		}
		return "", fmt.Errorf("the %s App's gh bundle for %s holds no owner of the project (%s): %w",
			appRole, role, strings.Join(slices.Sorted(maps.Keys(layout.owners)), ", "), ErrNarrowingRefused)
	}
	if _, held := bundle.Dirs[dflt]; held {
		return dflt, nil
	}
	reaches := dflt
	if at := slices.IndexFunc(layout.dirs, func(dir bundleDir) bool { return dir.name == dflt }); at >= 0 {
		reaches = layout.dirs[at].hold.where()
	}
	return "", fmt.Errorf("the %s App's gh bundle for %s holds no directory for its default owner %s: the lease over %s: %w",
		appRole, role, dflt, reaches, ErrNarrowingRefused)
}

// dirEntry is the directory's lease, rendered as gh's two files; reached is false where GitHub
// refuses the narrowing of every repository the lease names, which leaves the directory out of
// the bundle.
//
// A read lease over several repositories that GitHub refuses is asked for again one repository at
// a time: each refusal names that repository alone, so the ones that answered are known, and the
// lease the directory holds is minted over those. The Manager caches a lease by the repositories
// it reaches (leaseScope), so the lease over a single repository that answered is the one its own
// ask already minted, and a directory of one repository is not asked for twice. A write lease is
// never softened, and no other failure is read as a refusal of what it named.
func dirEntry(ctx context.Context, tokens Tokens, appRole AppRole, hold dirLease, log *slog.Logger) (entry ghconfig.Rendered, reached bool, err error) {
	lease, err := hold.mint(ctx, tokens, appRole)
	if err == nil {
		return ghconfig.Render(lease.Token, string(appRole), lease.ExpiresAt), true, nil
	}
	if hold.write || !errors.Is(err, ErrNarrowingRefused) {
		return ghconfig.Rendered{}, false, err
	}
	if len(hold.repos) == 1 {
		sayLeftOut(log, appRole, hold.owner, hold.repos[0], refusedNarrowing)
		return ghconfig.Rendered{}, false, nil
	}
	answered := make([]string, 0, len(hold.repos))
	// The lease of the one repository that answered, where that is all of them: no other probed
	// lease is of use, and a token is held no longer than it is needed.
	var only Lease
	for _, repo := range hold.repos {
		one, err := hold.over([]string{repo}).mint(ctx, tokens, appRole)
		switch {
		case errors.Is(err, ErrNarrowingRefused):
			sayLeftOut(log, appRole, hold.owner, repo, refusedNarrowing)
			continue
		case err != nil:
			return ghconfig.Rendered{}, false, err
		}
		answered = append(answered, repo)
		only = one
	}
	switch len(answered) {
	case 0:
		return ghconfig.Rendered{}, false, nil
	case 1:
		lease = only
	default:
		if lease, err = hold.over(answered).mint(ctx, tokens, appRole); err != nil {
			return ghconfig.Rendered{}, false, err
		}
	}
	return ghconfig.Render(lease.Token, string(appRole), lease.ExpiresAt), true, nil
}
