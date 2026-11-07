package sandbox

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// refusedProjectRepos is the project's mapping for the tests below: the repository every test
// spec works, under the owner its roles write to, and one repository of a second owner, which a
// bundle holds one read-only directory for.
func refusedProjectRepos() []ghrepo.Repository {
	return []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion-smoke"), ghrepo.MustParse("acme/widgets")}
}

// refusingOwnerTokens answers every lease naming a repository of owner with the refusal GitHub
// gives a request naming a repository its installation does not reach (422,
// appauth.ErrNarrowingRefused) while refuse holds, and an ordinary lease for every other owner
// and once refuse is cleared. app, when set, is the one App role that refuses, the other minting
// as readily as ever: two Apps' installations answer for themselves.
type refusingOwnerTokens struct {
	refuse *atomic.Bool
	owner  string
	app    appauth.AppRole
}

func (t refusingOwnerTokens) Token(_ context.Context, role appauth.AppRole, repo ghrepo.Repository) (appauth.Lease, error) {
	return t.lease(role, repo.Owner(), repo.Name())
}

func (t refusingOwnerTokens) ReadToken(_ context.Context, role appauth.AppRole, owner string, repos []string) (appauth.Lease, error) {
	return t.lease(role, owner, strings.Join(repos, "+"))
}

func (t refusingOwnerTokens) lease(role appauth.AppRole, owner, repos string) (appauth.Lease, error) {
	if t.refuse.Load() && strings.EqualFold(owner, t.owner) && (t.app == "" || t.app == role) {
		return appauth.Lease{}, fmt.Errorf("the %s App on %s (%s): %w", role, owner, repos, appauth.ErrNarrowingRefused)
	}
	return appauth.Lease{Token: "ghs_" + string(role) + "_" + strings.ToLower(owner) + "_" + repos, ExpiresAt: staticCredentialExpiry}, nil
}

// withRefusedOwner gives the rig the daemon's own three gh answers (appauth.GitHubCredential,
// appauth.ControllerCredential and appauth.GitHubDirs) over refusedProjectRepos, the three
// sharing one record of what their mints found GitHub refuses, as internal/daemon's
// sandboxRuntime builds them for a project. GitHub refuses every lease of owner while refuse
// holds, whichever App asks.
func withRefusedOwner(owner string, refuse *atomic.Bool) rigOption {
	tokens := refusingOwnerTokens{refuse: refuse, owner: owner}
	return withOptions(func(o *Options) {
		left := appauth.NewLeftOut()
		o.GitHubCredential = appauth.GitHubCredential(tokens, refusedProjectRepos, ghrepo.Renames{}, left, o.Log)
		o.ControllerCredential = appauth.ControllerCredential(tokens, refusedProjectRepos, ghrepo.Renames{}, left, o.Log)
		o.GitHubDirs = func(repo ghrepo.Repository) []string {
			return appauth.GitHubDirs(refusedProjectRepos, ghrepo.Renames{}, left, repo)
		}
	})
}

// ghDriftReasons is why loc's pod has drifted that name role's gh volume: what an update round
// over that pod would be opened for (PodDrift, driftedInPod, movedGhDirs).
func ghDriftReasons(t *testing.T, g *rig, loc runtime.Locator, role claim.Role) []string {
	t.Helper()
	name := roleVolume(ghVolume, role)
	var named []string
	for _, reason := range driftOf(t, g.ctx, g.r, loc) {
		if strings.Contains(reason, name) {
			named = append(named, reason)
		}
	}
	return named
}

// An owner whose every repository GitHub refuses a lease for is left out of every bundle, so no
// pod projects a directory of that name — and the drift check must not ask for one. Were it
// asked for, the daemon would read the pod as drifted at every sweep, open an update round, task
// every worker in the pod to push, replace the pod, and mint the pod that replaces it a bundle
// lacking that directory again: a pod replaced for ever over an owner no mint can produce
// (INFRA-513). The record the mints write and the directory set reads is what keeps the two
// answers one (appauth.LeftOut). Once GitHub answers for that owner again, a mint clears the
// record and the pod is replaced once, which is right.
func TestAnOwnerEveryLeaseIsRefusedForIsNoDriftForThePodWithoutIt(t *testing.T) {
	var refuse atomic.Bool
	refuse.Store(true)
	g := newRig(t, nil, withRefusedOwner("acme", &refuse))
	loc := g.spawn(workerSpec(t))

	if held := ghItems(t, g.pod(loc.Sandbox.Name), claim.RoleTester); !slices.Equal(held, ghItemsFor("sjawhar")) {
		t.Fatalf("the tester's gh volume projects %+v, want sjawhar's two files alone: acme answered for no repository", held)
	}
	for _, evaluation := range []string{"first", "second"} {
		if named := ghDriftReasons(t, g, loc, claim.RoleTester); len(named) != 0 {
			t.Fatalf("the %s evaluation reads %q as drift, want none: no bundle built now holds acme either", evaluation, named)
		}
	}

	refuse.Store(false)
	g.r.refreshGitHubCredentials(g.ctx)
	if named := ghDriftReasons(t, g, loc, claim.RoleTester); len(named) != 1 {
		t.Errorf("PodDrift names %q once acme answers again, want exactly one reason naming its gh volume", named)
	}
}

// The six roles of an issue pod run as two GitHub Apps, and each App's installations answer for
// themselves: one may refuse every lease of an owner the other mints. The pod then projects the
// directories every role's bundle holds and starts, since a projection item naming a key a
// role's Secret lacks would stop the pod from starting; a role whose bundle holds more keeps
// those keys in its own Secret, unprojected, and the issue's next pod projects them once every
// role's bundle holds them.
func TestAPodProjectsTheGhDirectoriesEveryRolesBundleHolds(t *testing.T) {
	var refuse atomic.Bool
	refuse.Store(true)
	tokens := refusingOwnerTokens{refuse: &refuse, owner: "acme", app: appauth.Review}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.GitHubCredential = appauth.GitHubCredential(tokens, refusedProjectRepos, ghrepo.Renames{}, nil, o.Log)
	}))

	loc := g.spawn(workerSpec(t))
	pod := g.pod(loc.Sandbox.Name)
	for _, role := range claim.Roles {
		if held := ghItems(t, pod, role); !slices.Equal(held, ghItemsFor("sjawhar")) {
			t.Errorf("%s's gh volume projects %+v, want sjawhar's two files alone: the review App reaches no acme repository", role, held)
		}
		secret := g.secret(roleSecretName(loc.Sandbox.Name, role))
		_, holdsAcme := secret.Data[GitHubHostsKey("acme")]
		if want := appauth.AppRoleFor(role) == appauth.Implement; holdsAcme != want {
			t.Errorf("%s's Secret holds acme's gh files %v, want %v: each role's Secret holds its own bundle", role, holdsAcme, want)
		}
	}
}

// A launch whose roles share no directory at all still fails: the pod's GH_CONFIG_DIR would name
// a directory no role's gh volume projects, and every role would start unable to reach GitHub.
func TestALaunchWhoseRolesShareNoGhDirectoryFails(t *testing.T) {
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.GitHubCredential = func(ctx context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
			bundle, err := staticCredential(ctx, role, repo)
			if err != nil {
				return bundle, err
			}
			// Each App's installation reaches what the other's does not, so the two bundles
			// share nothing: the review App only acme's repositories, whose directory is then
			// that role's own default, and the implement App only the issue owner's.
			if appauth.AppRoleFor(role) == appauth.Review {
				return ghconfig.Bundle{Default: "acme", Dirs: map[string]ghconfig.Rendered{"acme": bundle.Dirs["acme"]}}, nil
			}
			delete(bundle.Dirs, "acme")
			return bundle, nil
		}
	}))

	_, err := g.r.Spawn(g.ctx, workerSpec(t))
	if err == nil || !strings.Contains(err.Error(), "do not hold the pod's default owner") {
		t.Fatalf("the launch = %v, want a failure naming the directories the roles share and the default they do not hold", err)
	}
}
