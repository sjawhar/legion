package appauth

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// ownerTokens names each lease for the App, the owner and whether it reads or writes, so a test
// reads which mint answered for which owner.
type ownerTokens struct{ refuse string }

func (t ownerTokens) Token(_ context.Context, role AppRole, owner string) (Lease, error) {
	if owner == t.refuse {
		return Lease{}, errors.New("no installation on " + owner)
	}
	return Lease{Token: string(role) + "-token-for-" + owner, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (t ownerTokens) ReadToken(_ context.Context, role AppRole, owner string) (Lease, error) {
	if owner == t.refuse {
		return Lease{}, errors.New("no installation on " + owner)
	}
	return Lease{Token: string(role) + "-read-for-" + owner, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// bundleTokens is each owner's token in bundle.
func bundleTokens(t *testing.T, bundle ghconfig.Bundle) map[string]string {
	t.Helper()
	tokens := map[string]string{}
	for owner, rendered := range bundle.Owners {
		token, err := ghconfig.TokenFromHosts([]byte(rendered.Hosts))
		if err != nil {
			t.Fatalf("%s's hosts.yml: %v", owner, err)
		}
		tokens[owner] = token
	}
	return tokens
}

// leaseToken is the token ownerTokens mints for want, so a case states what a directory holds as
// the owner the lease was minted for and whether it writes, rather than as a spelling. The fake
// is the one source of both halves: a test that wrote the token out by hand would still pass
// were the kinds swapped.
func leaseToken(t *testing.T, app AppRole, want bundleLease) string {
	t.Helper()
	mint := ownerTokens{}.ReadToken
	if want.write {
		mint = ownerTokens{}.Token
	}
	minted, err := mint(context.Background(), app, want.owner)
	if err != nil {
		t.Fatalf("mint %s's %v: %v", app, want, err)
	}
	return minted.Token
}

// String names a lease as a failure should read it, the test's rendering alone: the owner GitHub
// was asked for it and which of that owner's two leases it is.
func (l bundleLease) String() string {
	kind := "read lease"
	if l.write {
		kind = "write lease"
	}
	return l.owner + "'s " + kind
}

// A role's gh bundle holds every owner of the project: its App's own lease for the owner it writes
// to (its issue's), lowercased, which is the bundle's default, and a lease of the same App narrowed
// to reading for every other owner; an owner outside the project's set still gets its write lease,
// since a role's owner is its issue's repository's, from the daemon's own record. The controller
// writes to no owner: every owner is read-only, as the review App, and the default is the owner it
// is asked for (the one its launch recorded) or the first owner when it is asked for none; an
// owner outside the project is refused (ErrOwnerOutsideProject) and mints nothing, since the
// controller's default is no authority to read another owner's repositories. A mint that fails
// fails the bundle, naming the owner; a project with no owner at all has no bundle.
func TestGitHubCredentialBundlesEveryOwnerOfTheProject(t *testing.T) {
	served := func() []ghrepo.Repository {
		return []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion"), ghrepo.MustParse("Acme/widgets")}
	}
	credential := GitHubCredential(ownerTokens{}, served, ghrepo.Renames{})
	for _, tc := range []struct {
		name    string
		role    claim.Role
		repo    ghrepo.Repository
		owner   string
		def     string
		tokens  map[string]string
		appName string
	}{
		{"an implementer on acme", claim.RoleImplementer, ghrepo.MustParse("Acme/widgets"), "", "acme",
			map[string]string{"acme": "implement-token-for-acme", "sjawhar": "implement-read-for-sjawhar"}, "implement"},
		{"a reviewer on sjawhar", claim.RoleReviewer, ghrepo.MustParse("sjawhar/legion"), "", "sjawhar",
			map[string]string{"acme": "review-read-for-acme", "sjawhar": "review-token-for-sjawhar"}, "review"},
		{"a merger on an owner outside the set", claim.RoleMerger, ghrepo.MustParse("Other/tools"), "", "other",
			map[string]string{"acme": "implement-read-for-acme", "other": "implement-token-for-other", "sjawhar": "implement-read-for-sjawhar"}, "implement"},
		{"the controller", claim.RoleController, ghrepo.Repository{}, "", "acme",
			map[string]string{"acme": "review-read-for-acme", "sjawhar": "review-read-for-sjawhar"}, "review"},
		{"the controller asked for the default its launch recorded", claim.RoleController, ghrepo.Repository{}, "Sjawhar", "sjawhar",
			map[string]string{"acme": "review-read-for-acme", "sjawhar": "review-read-for-sjawhar"}, "review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle, err := credential(context.Background(), tc.role, tc.repo, tc.owner)
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			if bundle.Default != tc.def {
				t.Errorf("Default = %q, want %q", bundle.Default, tc.def)
			}
			if got := bundleTokens(t, bundle); !maps.Equal(got, tc.tokens) {
				t.Errorf("tokens = %v, want %v", got, tc.tokens)
			}
			for owner, rendered := range bundle.Owners {
				if rendered.App != tc.appName || rendered.Config != ghconfig.Config {
					t.Errorf("%s's render = App %q, config %q; want %q and the fixed config", owner, rendered.App, rendered.Config, tc.appName)
				}
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
		})
	}

	refused := GitHubCredential(ownerTokens{refuse: "sjawhar"}, served, ghrepo.Renames{})
	if _, err := refused(context.Background(), claim.RoleImplementer, ghrepo.MustParse("Acme/widgets"), ""); err == nil ||
		!strings.Contains(err.Error(), "sjawhar") || !strings.Contains(err.Error(), string(claim.RoleImplementer)) {
		t.Errorf("a refused read mint = %v, want an error naming the owner and the role", err)
	}
	none := GitHubCredential(ownerTokens{}, func() []ghrepo.Repository { return nil }, ghrepo.Renames{})
	if _, err := none(context.Background(), claim.RoleController, ghrepo.Repository{}, ""); !errors.Is(err, runtime.ErrNoGitHubOwner) {
		t.Errorf("a bundle with no owner = %v, want runtime.ErrNoGitHubOwner", err)
	}
	var minted []bundleLease
	counting := GitHubCredential(countingTokens{minted: &minted}, served, ghrepo.Renames{})
	if _, err := counting(context.Background(), claim.RoleController, ghrepo.Repository{}, "Outside-Project"); !errors.Is(err, ErrOwnerOutsideProject) ||
		!strings.Contains(err.Error(), `"outside-project"`) || len(minted) != 0 {
		t.Errorf("the controller asking for an owner outside the project = %v, minted %v; want ErrOwnerOutsideProject naming it and no mint", err, minted)
	}
}

// legionRename is the pair a deployment configures over sjawhar/legion's transfer to
// cybermonklabs: one repository under two names, the second of which the project serves.
var legionRename = ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")

// stateA, stateB and m0 are the three states that pair is read in. Each serves acme/widgets as
// well, an owner no pair names and no state may lose the directory of.
//
// stateA serves the repository the pair moved to and no repository of sjawhar at all: the
// project as it stood when the transfer was configured. stateB serves sjawhar/oh-my-pi beside
// it — one more repository of the owner the pair moved legion away from, which is the mapping
// entry that turned the live pair off and left 47 trees unable to push for 42 minutes. m0 serves
// another repository of cybermonklabs and neither name of the pair, so the pair resolves for
// nobody.
func stateA() []ghrepo.Repository {
	return []ghrepo.Repository{ghrepo.MustParse("Cybermonklabs/legion"), ghrepo.MustParse("acme/widgets")}
}

func stateB() []ghrepo.Repository {
	return append(stateA(), ghrepo.MustParse("sjawhar/oh-my-pi"))
}

func m0() []ghrepo.Repository {
	return []ghrepo.Repository{ghrepo.MustParse("Cybermonklabs/other"), ghrepo.MustParse("acme/widgets")}
}

// A configured rename is resolved for one repository at a time, against the repositories the
// project serves, so what a role's gh holds turns on its issue's own repository and on nothing
// the project's set of owners says. Each case below is one issue in one state, and states every
// directory of its bundle, the lease each directory holds — the owner GitHub minted it for and
// whether it writes — which directory is the default the role writes through, and every lease
// GitHub was asked for.
//
// The directory named for the owner a record keeps holds the write lease, minted for the owner
// that serves that record's repository now, and the directory of that serving owner holds the
// same lease, the two directories sharing one entry: cybermonklabs' write lease in both the
// sjawhar and the cybermonklabs directory for an issue recorded on sjawhar/legion in states A
// and B alike, since the pair resolves for that repository in both, and sjawhar's own write
// lease in the one directory for an issue recorded on sjawhar/oh-my-pi, which no pair names, and
// for one recorded on sjawhar/legion in m0, where the pair resolves for nobody. Every other
// directory holds a read lease: its own owner's where the project serves a repository of that
// owner under its own name, and the lease of the owner that serves the moved repository where
// only the rename puts the directory there. The controller writes to no owner, so every
// directory of its bundle is read-only and its default is the owner it asks for, or the first
// owner when it asks for none.
func TestGitHubCredentialBundlesARenamedPairAsOneOwner(t *testing.T) {
	renames := ghrepo.MustRenames(legionRename)
	for _, tc := range []struct {
		name string
		// served is the project's mapping, and project the directories that mapping and the
		// pair give on their own (GitHubOwners), which is the set a pod's gh projection is
		// fixed at: the rename adds sjawhar in states A and B and nothing at all in m0.
		served  func() []ghrepo.Repository
		project []string
		role    claim.Role
		repo    ghrepo.Repository
		owner   string
		def     string
		leases  map[string]bundleLease
		minted  []bundleLease
		app     AppRole
	}{
		{
			name: "an issue recorded on the repository as it was, state A", role: claim.RoleImplementer, repo: ghrepo.MustParse("sjawhar/legion"),
			served: stateA, project: []string{"acme", "cybermonklabs", "sjawhar"}, def: "sjawhar", app: Implement,
			leases: map[string]bundleLease{
				"acme":          {owner: "acme"},
				"cybermonklabs": {owner: "cybermonklabs", write: true},
				"sjawhar":       {owner: "cybermonklabs", write: true},
			},
			minted: []bundleLease{{owner: "acme"}, {owner: "cybermonklabs", write: true}},
		},
		{
			name: "an issue recorded on the repository as it was, state B", role: claim.RoleImplementer, repo: ghrepo.MustParse("sjawhar/legion"),
			served: stateB, project: []string{"acme", "cybermonklabs", "sjawhar"}, def: "sjawhar", app: Implement,
			leases: map[string]bundleLease{
				"acme":          {owner: "acme"},
				"cybermonklabs": {owner: "cybermonklabs", write: true},
				"sjawhar":       {owner: "cybermonklabs", write: true},
			},
			minted: []bundleLease{{owner: "acme"}, {owner: "cybermonklabs", write: true}},
		},
		{
			name: "an issue recorded on another repository of the owner it moved from, state B", role: claim.RoleImplementer, repo: ghrepo.MustParse("sjawhar/oh-my-pi"),
			served: stateB, project: []string{"acme", "cybermonklabs", "sjawhar"}, def: "sjawhar", app: Implement,
			leases: map[string]bundleLease{
				"acme":          {owner: "acme"},
				"cybermonklabs": {owner: "cybermonklabs"},
				"sjawhar":       {owner: "sjawhar", write: true},
			},
			minted: []bundleLease{{owner: "acme"}, {owner: "cybermonklabs"}, {owner: "sjawhar", write: true}},
		},
		{
			name: "an issue recorded on the repository as GitHub holds it, state A", role: claim.RoleImplementer, repo: ghrepo.MustParse("Cybermonklabs/legion"),
			served: stateA, project: []string{"acme", "cybermonklabs", "sjawhar"}, def: "cybermonklabs", app: Implement,
			leases: map[string]bundleLease{
				"acme":          {owner: "acme"},
				"cybermonklabs": {owner: "cybermonklabs", write: true},
				"sjawhar":       {owner: "cybermonklabs"},
			},
			minted: []bundleLease{{owner: "acme"}, {owner: "cybermonklabs", write: true}, {owner: "cybermonklabs"}},
		},
		{
			name: "an issue recorded on the repository as it was, with the pair resolving for nobody", role: claim.RoleImplementer, repo: ghrepo.MustParse("sjawhar/legion"),
			served: m0, project: []string{"acme", "cybermonklabs"}, def: "sjawhar", app: Implement,
			leases: map[string]bundleLease{
				"acme":          {owner: "acme"},
				"cybermonklabs": {owner: "cybermonklabs"},
				"sjawhar":       {owner: "sjawhar", write: true},
			},
			minted: []bundleLease{{owner: "acme"}, {owner: "cybermonklabs"}, {owner: "sjawhar", write: true}},
		},
		{
			name: "an issue recorded on another owner of the project, state A", role: claim.RoleImplementer, repo: ghrepo.MustParse("acme/widgets"),
			served: stateA, project: []string{"acme", "cybermonklabs", "sjawhar"}, def: "acme", app: Implement,
			leases: map[string]bundleLease{
				"acme":          {owner: "acme", write: true},
				"cybermonklabs": {owner: "cybermonklabs"},
				"sjawhar":       {owner: "cybermonklabs"},
			},
			minted: []bundleLease{{owner: "acme", write: true}, {owner: "cybermonklabs"}},
		},
		{
			name: "the project controller's own bundle, state A", role: claim.RoleController,
			served: stateA, project: []string{"acme", "cybermonklabs", "sjawhar"}, def: "acme", app: Review,
			leases: map[string]bundleLease{
				"acme":          {owner: "acme"},
				"cybermonklabs": {owner: "cybermonklabs"},
				"sjawhar":       {owner: "cybermonklabs"},
			},
			minted: []bundleLease{{owner: "acme"}, {owner: "cybermonklabs"}},
		},
		{
			name: "the controller asked for the alias owner its launch recorded, state A", role: claim.RoleController, owner: "Sjawhar",
			served: stateA, project: []string{"acme", "cybermonklabs", "sjawhar"}, def: "sjawhar", app: Review,
			leases: map[string]bundleLease{
				"acme":          {owner: "acme"},
				"cybermonklabs": {owner: "cybermonklabs"},
				"sjawhar":       {owner: "cybermonklabs"},
			},
			minted: []bundleLease{{owner: "acme"}, {owner: "cybermonklabs"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minted []bundleLease
			credential := GitHubCredential(countingTokens{minted: &minted}, tc.served, renames)
			bundle, err := credential(context.Background(), tc.role, tc.repo, tc.owner)
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			if got := GitHubOwners(tc.served, renames); !slices.Equal(got, tc.project) {
				t.Errorf("the project's own directories are %v, want %v", got, tc.project)
			}
			if bundle.Default != tc.def {
				t.Errorf("Default = %q, want %q", bundle.Default, tc.def)
			}
			want := make(map[string]string, len(tc.leases))
			for dir, held := range tc.leases {
				want[dir] = leaseToken(t, tc.app, held)
			}
			if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
				t.Errorf("the bundle's directories hold %v, want %v (%v)", got, want, tc.leases)
			}
			if !slices.Equal(minted, tc.minted) {
				t.Errorf("minted %v, want %v: one lease per owner and kind, minted once", minted, tc.minted)
			}
			// Two directories hold one entry exactly when they hold one lease: each render
			// carries the expiry of its own mint, so one entry in both is one mint for both.
			shared := tc.leases["sjawhar"] == tc.leases["cybermonklabs"]
			if held := bundle.Owners["sjawhar"] == bundle.Owners["cybermonklabs"]; held != shared {
				t.Errorf("sjawhar's entry = %+v and cybermonklabs' = %+v, want the two directories to hold one entry = %v",
					bundle.Owners["sjawhar"], bundle.Owners["cybermonklabs"], shared)
			}
			for owner, rendered := range bundle.Owners {
				if rendered.App != string(tc.app) {
					t.Errorf("%s's render = App %q, want %q", owner, rendered.App, tc.app)
				}
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
		})
	}

	// No mint is asked of an owner the project serves no repository of and no rename resolves
	// to. In state A sjawhar is a directory by the rename alone, and that directory carries
	// cybermonklabs' lease, so GitHub is asked nothing about sjawhar at all — which is what a
	// transfer leaves behind: the App's installation on sjawhar can be gone, and a mint asked of
	// it would fail the whole bundle and every role of the issue with it. Here the token source
	// refuses sjawhar as a GitHub with no installation there answers, and the two bundles whose
	// sjawhar directory only the rename puts there are built all the same.
	t.Run("no mint is asked of an owner the project serves no repository of", func(t *testing.T) {
		uninstalled := GitHubCredential(ownerTokens{refuse: "sjawhar"}, stateA, renames)
		for _, tc := range []struct {
			name string
			role claim.Role
			repo ghrepo.Repository
		}{
			{"an issue recorded on the repository as GitHub holds it", claim.RoleImplementer, ghrepo.MustParse("Cybermonklabs/legion")},
			{"the project controller's own bundle", claim.RoleController, ghrepo.Repository{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				bundle, err := uninstalled(context.Background(), tc.role, tc.repo, "")
				if err != nil {
					t.Fatalf("credential: %v, want a bundle built without asking GitHub about sjawhar", err)
				}
				read := bundleLease{owner: "cybermonklabs"}
				if got := bundleTokens(t, bundle)["sjawhar"]; got != leaseToken(t, AppRoleFor(tc.role), read) {
					t.Errorf("sjawhar's directory holds %q, want %v", got, read)
				}
			})
		}
	})

	// A bundle that needs one owner's write lease beside its read lease gets both. An issue
	// recorded on Cybermonklabs/legion writes through cybermonklabs' directory, which has to
	// hold that owner's write lease, while sjawhar's directory — which only the rename puts in
	// the bundle, since state A serves no repository of sjawhar — holds cybermonklabs' read
	// lease: two leases of one owner in one bundle. A cache keyed by the owner alone would hand
	// one of those directories the other's token — a read-only lease where the role pushes,
	// which fails every push of the tree, or a write lease in a directory the role only reads
	// through.
	t.Run("an owner's write lease and its read lease are both minted", func(t *testing.T) {
		var minted []bundleLease
		credential := GitHubCredential(countingTokens{minted: &minted}, stateA, renames)
		bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("Cybermonklabs/legion"), "")
		if err != nil {
			t.Fatalf("credential: %v", err)
		}
		write, read := bundleLease{owner: "cybermonklabs", write: true}, bundleLease{owner: "cybermonklabs"}
		tokens := bundleTokens(t, bundle)
		if tokens["cybermonklabs"] != leaseToken(t, Implement, write) || tokens["sjawhar"] != leaseToken(t, Implement, read) {
			t.Errorf("cybermonklabs' directory holds %q and sjawhar's %q, want %v and %v", tokens["cybermonklabs"], tokens["sjawhar"], write, read)
		}
		if bundle.Owners["sjawhar"] == bundle.Owners["cybermonklabs"] {
			t.Errorf("both directories hold one entry %+v, want cybermonklabs' two leases apart", bundle.Owners["sjawhar"])
		}
		if !slices.Contains(minted, write) || !slices.Contains(minted, read) {
			t.Errorf("minted %v, want %v and %v among them", minted, write, read)
		}
	})

	// both is a project that serves the pair's two repositories, each a repository of its own.
	both := func() []ghrepo.Repository {
		return []ghrepo.Repository{ghrepo.MustParse("Cybermonklabs/legion"), ghrepo.MustParse("sjawhar/legion")}
	}

	// With no pair configured, the owner the repository moved from is an owner like any other:
	// its own read-only lease, minted for itself, as today. Where the project serves the
	// repository the pair moved away from as well as the one it moved to, the pair resolves for
	// nobody and the bundle is that same one: each name is a repository of its own, so each
	// owner's directory holds its own token and neither is an alias of the other.
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
	}{
		{"no pair configured", ghrepo.Renames{}},
		{"the project serves the repository it moved from too", renames},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minted []bundleLease
			credential := GitHubCredential(countingTokens{minted: &minted}, both, tc.renames)
			bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("Cybermonklabs/legion"), "")
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			leases := map[string]bundleLease{"cybermonklabs": {owner: "cybermonklabs", write: true}, "sjawhar": {owner: "sjawhar"}}
			want := make(map[string]string, len(leases))
			for dir, held := range leases {
				want[dir] = leaseToken(t, Implement, held)
			}
			if got := bundleTokens(t, bundle); !maps.Equal(got, want) || bundle.Default != "cybermonklabs" {
				t.Errorf("Default %q and tokens %v, want cybermonklabs and %v", bundle.Default, got, leases)
			}
			if !slices.Equal(minted, []bundleLease{{owner: "cybermonklabs", write: true}, {owner: "sjawhar"}}) {
				t.Errorf("minted %v, want each owner its own lease", minted)
			}
			if _, err := credential(context.Background(), claim.RoleReviewer, ghrepo.MustParse("sjawhar/legion"), ""); err != nil {
				t.Fatalf("credential: %v", err)
			}
		})
	}
}

// countingTokens records every lease GitHub was asked for: the owner it was asked of, and
// whether the request was for that owner's write lease or its read-only one. The kind is half
// the record because one bundle can need both leases of one owner, and because an owner no mint
// is asked of at all is the other half of what a resolving rename has to hold.
type countingTokens struct{ minted *[]bundleLease }

func (t countingTokens) Token(ctx context.Context, role AppRole, owner string) (Lease, error) {
	*t.minted = append(*t.minted, bundleLease{owner: owner, write: true})
	return ownerTokens{}.Token(ctx, role, owner)
}

func (t countingTokens) ReadToken(ctx context.Context, role AppRole, owner string) (Lease, error) {
	*t.minted = append(*t.minted, bundleLease{owner: owner})
	return ownerTokens{}.ReadToken(ctx, role, owner)
}
