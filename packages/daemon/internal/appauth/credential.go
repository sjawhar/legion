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
// It is kept per App role, and read for the App roles of the pod an answer is about alone
// (left, GitHubDirs): a pod's projection is its roles' directory sets intersected (sandbox's
// gitHubBundles), so a directory one of its own Apps does not reach is in no such pod's
// projection, however readily the other App mints it — while what the other App's mint found
// says nothing about a pod that does not run it. The project controller's pod runs the review
// App alone: were the implement App's record read for it too, a controller launched before an
// owner was served would never be found to lack that owner's directory, its gh would hold no
// token for an owner the review App reaches, and in a project with no live issue pod nothing
// would mint for the implement App to clear the record (INFRA-513).
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

// left reports that the last mint of dir as one of roles — the App roles of the pod the answer
// is for — left it out of that role's bundle, so no pod of those Apps created now projects it
// (GitHubDirs). A pod running one of them is told nothing by what the mints of an App it does
// not run found.
func (l *LeftOut) left(dir string, roles []AppRole) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, role := range roles {
		if l.dirs[role][dir] {
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
// roles are the App roles of the pod that pod projection belongs to, which is what the record of
// the directories a mint found GitHub refuses is read for (left, LeftOut): an issue pod runs
// both Apps and projects what both their bundles hold, so a directory either App is refused is
// in no projection of it, while the project controller's pod runs the review App alone and is
// handed every directory that App mints, whatever the implement App's installation reaches
// (sandbox's movedGhDirs, which maps its pod's kind's roles through AppRoleFor).
//
// It and GitHubCredential are built from one description of the bundle (bundleDirs), so the set a
// pod is compared against and the set a pod would be handed cannot drift apart. It mints nothing
// and calls GitHub not at all, reading the mapping, the configured renames and what the mints
// before it recorded, since the drift check reads it on every evaluation: a
// repository whose name no directory may carry is left out of both (bundleDirs), and a directory
// GitHub refuses every lease of is left out of both as well — of the bundle by the mint that met
// the refusal, and of this answer by the record that mint wrote, so a pod lacking a directory no
// mint produces is no pod to replace.
// A bundle no launch could write — an owner directory ghconfig refuses a name for, which is what
// GitHubCredential fails on — is no set to compare a pod against and answers none, leaving every
// pod where it is.
func GitHubDirs(repos func() []ghrepo.Repository, renames ghrepo.Renames, left *LeftOut, roles []AppRole, repo ghrepo.Repository) []string {
	layout, err := bundleDirs(repos, renames, repo)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(layout.dirs))
	for _, dir := range layout.dirs {
		if left.left(dir.name, roles) {
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

// sayDirectoryLeftOut says on log, at warn, that owner's directory is left out of role's gh
// bundle though every repository it names answered a probe of its own: GitHub refuses the lease
// over them together (dirEntry's second round), so there is no one token the directory could
// hold. It is said each time it happens rather than once for the life of the process, as
// sayLeftOut's lines are: no repository of the set is refused on its own, so none of them is
// what the record is keyed by, and an answer that disagrees with itself this way is worth a line
// at every bundle until it stops. So it repeats at every launch and every refresh while GitHub
// keeps answering that way, which is the point: nothing else says that the installation's two
// answers cannot both be true, and the line stops the moment one of them changes. It carries no
// token.
func sayDirectoryLeftOut(log *slog.Logger, role AppRole, owner string, repos []string) {
	if log == nil {
		log = slog.Default()
	}
	log.Warn("appauth: an owner's directory is left out of the role's gh bundle",
		"app", string(role), "owner", owner, "repositories", strings.Join(repos, ", "),
		"because", "each of them answered a token of its own, and GitHub refused the one naming them together")
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
// said once on log (sayLeftOut). A narrowing refused over those too sends the probes back to
// GitHub past the leases cached for them, and a refusal past that round costs the directory and
// no more (dirEntry): a directory whose every repository is refused is left out of the bundle, as
// is one ghconfig holds no name for (layout.unnamed), where failing would cost the role every
// owner and the launch. Every other failure of a read mint fails the bundle as a write mint's
// does.
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
// lease the directory holds is minted over those (probeReached, overReached). The Manager caches
// a lease by the repositories it reaches (leaseScope), so the lease over a single repository that
// answered is the one its own ask already minted, and a directory of one repository is not asked
// for twice. A write lease is never softened, and no other failure is read as a refusal of what
// it named.
//
// What a probe is worth is what decides the rest. An answer served from the Manager's cache says
// a token naming that one repository was minted within the hour, not that GitHub reaches the
// repository now, and those single-repository leases are nearly always cached: every issue pod's
// `<owner>/<repo>` directory mints one, for both Apps, as does the provisioning adapter
// (internal/daemon's kubernetes.go). So a probe can call a repository reached that the
// installation has just dropped, and the ask over the repositories that answered is then refused
// as the first ask was.
//
// That last ask is made past any refusal the Manager remembers for their scope (RefusalForgetter,
// narrowingRefusalWindow), since serving a memory met before those repositories answered would
// cost the directory a call to GitHub it must make. Where it is refused all the same, the probes
// are made once more, after dropping what the Manager holds for each of those one-repository read
// scopes, the cached lease and the remembered refusal both (ReadScopeForgetter): a lease over a
// repository that left the installation reaches nothing, so each probe of this round is GitHub's
// answer now, and the directory holds a lease over what answers then. One extra round, never a
// loop: a second refusal of the ask over them leaves the directory out of the bundle, as every
// repository being refused does, rather than failing the bundle the write lease and the launch
// are in for repositories GitHub reaches (mintBundle, INFRA-513).
//
// A Tokens that caches nothing and remembers nothing implements neither optional interface and is
// asked for neither: its probes are GitHub's answer already, so the second round repeats the
// first and answers what the first answered.
func dirEntry(ctx context.Context, tokens Tokens, appRole AppRole, hold dirLease, log *slog.Logger) (entry ghconfig.Rendered, reached bool, err error) {
	lease, err := hold.mint(ctx, tokens, appRole)
	if err == nil {
		return ghconfig.Render(lease.Token, string(appRole), lease.ExpiresAt), true, nil
	}
	if hold.write || !NarrowingRefused(err) {
		return ghconfig.Rendered{}, false, err
	}
	if len(hold.repos) == 1 {
		sayLeftOut(log, appRole, hold.owner, hold.repos[0], refusedNarrowing)
		return ghconfig.Rendered{}, false, nil
	}
	answered, only, err := probeReached(ctx, tokens, appRole, hold, hold.repos, log)
	if err != nil {
		return ghconfig.Rendered{}, false, err
	}
	lease, held, err := overReached(ctx, tokens, appRole, hold, answered, only)
	if NarrowingRefused(err) {
		if forgetter, ok := tokens.(ReadScopeForgetter); ok {
			for _, repo := range answered {
				forgetter.ForgetReadScope(appRole, hold.owner, []string{repo})
			}
		}
		if answered, only, err = probeReached(ctx, tokens, appRole, hold, answered, log); err != nil {
			return ghconfig.Rendered{}, false, err
		}
		if lease, held, err = overReached(ctx, tokens, appRole, hold, answered, only); NarrowingRefused(err) {
			sayDirectoryLeftOut(log, appRole, hold.owner, answered)
			return ghconfig.Rendered{}, false, nil
		}
	}
	if err != nil {
		return ghconfig.Rendered{}, false, err
	}
	if !held {
		return ghconfig.Rendered{}, false, nil
	}
	return ghconfig.Render(lease.Token, string(appRole), lease.ExpiresAt), true, nil
}

// probeReached asks for the directory's read lease over one of repos at a time, which is the one
// way the repositories the installation reaches are told apart: GitHub refuses a request whole
// over any one repository it does not reach, so a refusal here names that repository alone, and
// each one refused is said once on log (sayLeftOut). It answers the repositories that answered
// and the last lease it was given, which is the directory's own where that one is all of them.
// Any other failure is the caller's to fail on: it is no answer about what the lease named.
func probeReached(ctx context.Context, tokens Tokens, appRole AppRole, hold dirLease, repos []string, log *slog.Logger) ([]string, Lease, error) {
	answered := make([]string, 0, len(repos))
	// The lease of the one repository that answered, where that is all of them: no other probed
	// lease is of use, and a token is held no longer than it is needed.
	var only Lease
	for _, repo := range repos {
		one, err := hold.over([]string{repo}).mint(ctx, tokens, appRole)
		switch {
		case NarrowingRefused(err):
			sayLeftOut(log, appRole, hold.owner, repo, refusedNarrowing)
			continue
		case err != nil:
			return nil, Lease{}, err
		}
		answered = append(answered, repo)
		only = one
	}
	return answered, only, nil
}

// overReached is the lease the directory holds over the repositories that answered the probes,
// and held, whether there is one to hold: none where no repository answered, the probe's own
// lease where one did — the Manager caches a lease by what it reaches, so that ask is already
// made — and otherwise the ask over them together, made past the refusal the Manager remembers
// for their scope. A refusal of that ask is answered as it is, ErrNarrowingRefused and all, for
// the caller to probe again past the cache (dirEntry).
func overReached(ctx context.Context, tokens Tokens, appRole AppRole, hold dirLease, answered []string, only Lease) (lease Lease, held bool, err error) {
	switch len(answered) {
	case 0:
		return Lease{}, false, nil
	case 1:
		return only, true, nil
	}
	if forgetter, ok := tokens.(RefusalForgetter); ok {
		forgetter.ForgetNarrowingRefusal(appRole, hold.owner, answered)
	}
	if lease, err = hold.over(answered).mint(ctx, tokens, appRole); err != nil {
		return Lease{}, false, err
	}
	return lease, true, nil
}
