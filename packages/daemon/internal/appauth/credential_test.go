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
	owners := func() []string { return []string{"sjawhar", "Acme"} }
	credential := GitHubCredential(ownerTokens{}, owners, ghrepo.Renames{})
	for _, tc := range []struct {
		name    string
		role    claim.Role
		owner   string
		def     string
		tokens  map[string]string
		appName string
	}{
		{"an implementer on acme", claim.RoleImplementer, "Acme", "acme",
			map[string]string{"acme": "implement-token-for-acme", "sjawhar": "implement-read-for-sjawhar"}, "implement"},
		{"a reviewer on sjawhar", claim.RoleReviewer, "sjawhar", "sjawhar",
			map[string]string{"acme": "review-read-for-acme", "sjawhar": "review-token-for-sjawhar"}, "review"},
		{"a merger on an owner outside the set", claim.RoleMerger, "Other", "other",
			map[string]string{"acme": "implement-read-for-acme", "other": "implement-token-for-other", "sjawhar": "implement-read-for-sjawhar"}, "implement"},
		{"the controller", claim.RoleController, "", "acme",
			map[string]string{"acme": "review-read-for-acme", "sjawhar": "review-read-for-sjawhar"}, "review"},
		{"the controller asked for the default its launch recorded", claim.RoleController, "Sjawhar", "sjawhar",
			map[string]string{"acme": "review-read-for-acme", "sjawhar": "review-read-for-sjawhar"}, "review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle, err := credential(context.Background(), tc.role, tc.owner)
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

	refused := GitHubCredential(ownerTokens{refuse: "sjawhar"}, owners, ghrepo.Renames{})
	if _, err := refused(context.Background(), claim.RoleImplementer, "acme"); err == nil ||
		!strings.Contains(err.Error(), "sjawhar") || !strings.Contains(err.Error(), string(claim.RoleImplementer)) {
		t.Errorf("a refused read mint = %v, want an error naming the owner and the role", err)
	}
	none := GitHubCredential(ownerTokens{}, func() []string { return nil }, ghrepo.Renames{})
	if _, err := none(context.Background(), claim.RoleController, ""); !errors.Is(err, runtime.ErrNoGitHubOwner) {
		t.Errorf("a bundle with no owner = %v, want runtime.ErrNoGitHubOwner", err)
	}
	var minted []string
	counting := GitHubCredential(countingTokens{minted: &minted}, owners, ghrepo.Renames{})
	if _, err := counting(context.Background(), claim.RoleController, "Outside-Project"); !errors.Is(err, ErrOwnerOutsideProject) ||
		!strings.Contains(err.Error(), `"outside-project"`) || len(minted) != 0 {
		t.Errorf("the controller asking for an owner outside the project = %v, minted %q; want ErrOwnerOutsideProject naming it and no mint", err, minted)
	}
}

// legionRename is the pair a deployment configures over sjawhar/legion's transfer to
// cybermonklabs: one repository under two names, whose owners the bundle holds one entry for.
var legionRename = ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")

// A configured rename whose `to` the project owns is one pair of owners: the bundle's sjawhar
// directory and its cybermonklabs directory hold one entry, minted once for cybermonklabs, the
// owner the repository moved to and whose installation holds it now. That one entry is the write
// lease whenever either of them is the bundle's Default — a pod launched for an issue recorded on
// sjawhar before the transfer has to write the repository as GitHub holds it now — and the
// read-only one for a role on any other owner. Every owner outside the pair is minted for itself,
// as it always was. The alias owner is a default the controller may ask for.
func TestGitHubCredentialBundlesARenamedPairAsOneOwner(t *testing.T) {
	owners := func() []string { return []string{"Cybermonklabs", "acme"} }
	for _, tc := range []struct {
		name    string
		role    claim.Role
		owner   string
		def     string
		tokens  map[string]string
		minted  []string
		shared  bool
		appName string
	}{
		{
			name: "an implementer on the owner the repository moved from", role: claim.RoleImplementer, owner: "sjawhar", def: "sjawhar", shared: true, appName: "implement",
			tokens: map[string]string{"acme": "implement-read-for-acme", "cybermonklabs": "implement-token-for-cybermonklabs", "sjawhar": "implement-token-for-cybermonklabs"},
			minted: []string{"acme", "cybermonklabs"},
		},
		{
			name: "an implementer on the owner it moved to", role: claim.RoleImplementer, owner: "Cybermonklabs", def: "cybermonklabs", shared: true, appName: "implement",
			tokens: map[string]string{"acme": "implement-read-for-acme", "cybermonklabs": "implement-token-for-cybermonklabs", "sjawhar": "implement-token-for-cybermonklabs"},
			minted: []string{"acme", "cybermonklabs"},
		},
		{
			name: "an implementer on another owner of the project", role: claim.RoleImplementer, owner: "acme", def: "acme", shared: true, appName: "implement",
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
			credential := GitHubCredential(countingTokens{minted: &minted}, owners, ghrepo.MustRenames(legionRename))
			bundle, err := credential(context.Background(), tc.role, tc.owner)
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
				t.Errorf("minted for %v, want %v: the pair's two directories hold one lease, minted once", minted, tc.minted)
			}
			if tc.shared && bundle.Owners["sjawhar"] != bundle.Owners["cybermonklabs"] {
				t.Errorf("sjawhar's entry = %+v and cybermonklabs' = %+v, want the one entry in both directories", bundle.Owners["sjawhar"], bundle.Owners["cybermonklabs"])
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

	// With no pair configured, the owner the repository moved from is an owner like any other:
	// its own read-only lease, minted for itself, as today.
	t.Run("no pair configured", func(t *testing.T) {
		var minted []string
		both := func() []string { return []string{"Cybermonklabs", "sjawhar"} }
		credential := GitHubCredential(countingTokens{minted: &minted}, both, ghrepo.Renames{})
		bundle, err := credential(context.Background(), claim.RoleImplementer, "cybermonklabs")
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

	// Where the owner the repository moved from is itself an owner of the project, its own entry
	// wins — its repositories are the project's and its token is its own. The conflict is warned
	// of where the owners that decide it are read, once per mapping load (the daemon's
	// logRenameOwnerConflicts), so nothing is logged here.
	t.Run("the owner it moved from is an owner of the project", func(t *testing.T) {
		var minted []string
		both := func() []string { return []string{"Cybermonklabs", "sjawhar"} }
		credential := GitHubCredential(countingTokens{minted: &minted}, both, ghrepo.MustRenames(legionRename))
		bundle, err := credential(context.Background(), claim.RoleImplementer, "cybermonklabs")
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
		if _, err := credential(context.Background(), claim.RoleReviewer, "sjawhar"); err != nil {
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
