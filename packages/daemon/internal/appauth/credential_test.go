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
	var minted []string
	counting := GitHubCredential(countingTokens{minted: &minted}, served, ghrepo.Renames{})
	if _, err := counting(context.Background(), claim.RoleController, ghrepo.Repository{}, "Outside-Project"); !errors.Is(err, ErrOwnerOutsideProject) ||
		!strings.Contains(err.Error(), `"outside-project"`) || len(minted) != 0 {
		t.Errorf("the controller asking for an owner outside the project = %v, minted %q; want ErrOwnerOutsideProject naming it and no mint", err, minted)
	}
}

// legionRename is the pair a deployment configures over sjawhar/legion's transfer to
// cybermonklabs: one repository under two names, the second of which the project serves.
var legionRename = ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")

// A configured rename whose moved repository the project serves puts the owner it moved away
// from in the bundle beside the owner it moved to, and every token in that sjawhar directory is
// minted for cybermonklabs, the owner whose installation holds the repository now. For the role
// whose record names sjawhar that token is the write lease — a pod launched for an issue recorded
// on sjawhar before the transfer has to write the repository as GitHub holds it now — while
// cybermonklabs' own directory keeps its own read lease, since the project serves a repository of
// that owner under its own name. For a role on any other owner the two directories hold one
// entry, cybermonklabs' read lease, minted once. Every owner outside the pair is minted for
// itself, as it always was. The alias owner is a default the controller may ask for.
func TestGitHubCredentialBundlesARenamedPairAsOneOwner(t *testing.T) {
	served := func() []ghrepo.Repository {
		return []ghrepo.Repository{ghrepo.MustParse("Cybermonklabs/legion"), ghrepo.MustParse("acme/widgets")}
	}
	for _, tc := range []struct {
		name    string
		role    claim.Role
		repo    ghrepo.Repository
		owner   string
		def     string
		tokens  map[string]string
		minted  []string
		shared  bool
		appName string
	}{
		{
			name: "an implementer on the owner the repository moved from", role: claim.RoleImplementer, repo: ghrepo.MustParse("sjawhar/legion"), def: "sjawhar", appName: "implement",
			tokens: map[string]string{"acme": "implement-read-for-acme", "cybermonklabs": "implement-read-for-cybermonklabs", "sjawhar": "implement-token-for-cybermonklabs"},
			minted: []string{"acme", "cybermonklabs", "cybermonklabs"},
		},
		{
			name: "an implementer on the owner it moved to", role: claim.RoleImplementer, repo: ghrepo.MustParse("Cybermonklabs/legion"), def: "cybermonklabs", appName: "implement",
			tokens: map[string]string{"acme": "implement-read-for-acme", "cybermonklabs": "implement-token-for-cybermonklabs", "sjawhar": "implement-read-for-cybermonklabs"},
			minted: []string{"acme", "cybermonklabs", "cybermonklabs"},
		},
		{
			name: "an implementer on another owner of the project", role: claim.RoleImplementer, repo: ghrepo.MustParse("acme/widgets"), def: "acme", shared: true, appName: "implement",
			tokens: map[string]string{"acme": "implement-token-for-acme", "cybermonklabs": "implement-read-for-cybermonklabs", "sjawhar": "implement-read-for-cybermonklabs"},
			minted: []string{"acme", "cybermonklabs"},
		},
		{
			name: "the controller asked for the alias owner its launch recorded", role: claim.RoleController, owner: "Sjawhar", def: "sjawhar", shared: true, appName: "review",
			tokens: map[string]string{"acme": "review-read-for-acme", "cybermonklabs": "review-read-for-cybermonklabs", "sjawhar": "review-read-for-cybermonklabs"},
			minted: []string{"acme", "cybermonklabs"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minted []string
			credential := GitHubCredential(countingTokens{minted: &minted}, served, ghrepo.MustRenames(legionRename))
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
			if !slices.Equal(minted, tc.minted) {
				t.Errorf("minted for %v, want %v: one lease per owner and kind, minted once", minted, tc.minted)
			}
			if shared := bundle.Owners["sjawhar"] == bundle.Owners["cybermonklabs"]; shared != tc.shared {
				t.Errorf("sjawhar's entry = %+v and cybermonklabs' = %+v, want the two directories to hold one entry = %v", bundle.Owners["sjawhar"], bundle.Owners["cybermonklabs"], tc.shared)
			}
			for owner, rendered := range bundle.Owners {
				if rendered.App != tc.appName {
					t.Errorf("%s's render = App %q, want %q", owner, rendered.App, tc.appName)
				}
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
		})
	}

	// both is a project that serves the pair's two repositories, each a repository of its own.
	both := func() []ghrepo.Repository {
		return []ghrepo.Repository{ghrepo.MustParse("Cybermonklabs/legion"), ghrepo.MustParse("sjawhar/legion")}
	}

	// With no pair configured, the owner the repository moved from is an owner like any other:
	// its own read-only lease, minted for itself, as today.
	t.Run("no pair configured", func(t *testing.T) {
		var minted []string
		credential := GitHubCredential(countingTokens{minted: &minted}, both, ghrepo.Renames{})
		bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("Cybermonklabs/legion"), "")
		if err != nil {
			t.Fatalf("credential: %v", err)
		}
		want := map[string]string{"cybermonklabs": "implement-token-for-cybermonklabs", "sjawhar": "implement-read-for-sjawhar"}
		if got := bundleTokens(t, bundle); !maps.Equal(got, want) || bundle.Default != "cybermonklabs" {
			t.Errorf("Default %q and tokens %v, want cybermonklabs and %v", bundle.Default, got, want)
		}
		if !slices.Equal(minted, []string{"cybermonklabs", "sjawhar"}) {
			t.Errorf("minted for %v, want each owner its own lease", minted)
		}
	})

	// Where the project serves the repository the pair moved away from as well as the one it
	// moved to, the pair resolves for nobody: each name is a repository of its own, so each
	// owner's directory holds its own token and neither is an alias of the other.
	t.Run("the project serves the repository it moved from too", func(t *testing.T) {
		var minted []string
		credential := GitHubCredential(countingTokens{minted: &minted}, both, ghrepo.MustRenames(legionRename))
		bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("Cybermonklabs/legion"), "")
		if err != nil {
			t.Fatalf("credential: %v", err)
		}
		want := map[string]string{"cybermonklabs": "implement-token-for-cybermonklabs", "sjawhar": "implement-read-for-sjawhar"}
		if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
			t.Errorf("tokens = %v, want %v: each owner keeps its own token", got, want)
		}
		if !slices.Equal(minted, []string{"cybermonklabs", "sjawhar"}) {
			t.Errorf("minted for %v, want each owner its own lease", minted)
		}
		if _, err := credential(context.Background(), claim.RoleReviewer, ghrepo.MustParse("sjawhar/legion"), ""); err != nil {
			t.Fatalf("credential: %v", err)
		}
	})
}

// countingTokens records every owner it is asked to mint for.
type countingTokens struct{ minted *[]string }

func (t countingTokens) Token(ctx context.Context, role AppRole, owner string) (Lease, error) {
	*t.minted = append(*t.minted, owner)
	return ownerTokens{}.Token(ctx, role, owner)
}

func (t countingTokens) ReadToken(ctx context.Context, role AppRole, owner string) (Lease, error) {
	*t.minted = append(*t.minted, owner)
	return ownerTokens{}.ReadToken(ctx, role, owner)
}
