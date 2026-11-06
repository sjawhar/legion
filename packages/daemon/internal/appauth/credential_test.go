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

// ownerTokens names each lease for the App and what the lease reaches, so a test reads which mint
// answered for which directory: a write lease names its one repository, a read lease the owner
// and the repositories it was narrowed to.
type ownerTokens struct{ refuse string }

func (t ownerTokens) Token(_ context.Context, role AppRole, repo ghrepo.Repository) (Lease, error) {
	if repo.IsZero() {
		return Lease{}, errors.New("a write token names no repository")
	}
	if repo.Owner() == t.refuse {
		return Lease{}, errors.New("no installation on " + repo.Owner())
	}
	return Lease{Token: string(role) + "-token-for-" + strings.ToLower(repo.String()), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (t ownerTokens) ReadToken(_ context.Context, role AppRole, owner string, repos []string) (Lease, error) {
	if len(repos) == 0 {
		return Lease{}, errors.New("a read token names no repository")
	}
	if owner == t.refuse {
		return Lease{}, errors.New("no installation on " + owner)
	}
	return Lease{Token: string(role) + "-read-for-" + owner + "/" + strings.Join(repos, "+"), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// bundleTokens is each directory's token in bundle.
func bundleTokens(t *testing.T, bundle ghconfig.Bundle) map[string]string {
	t.Helper()
	tokens := map[string]string{}
	for dir, rendered := range bundle.Dirs {
		token, err := ghconfig.TokenFromHosts([]byte(rendered.Hosts))
		if err != nil {
			t.Fatalf("%s's hosts.yml: %v", dir, err)
		}
		tokens[dir] = token
	}
	return tokens
}

// projectRepos is the mapping's repositories as the daemon serves them.
func projectRepos(names ...string) func() []ghrepo.Repository {
	repos := make([]ghrepo.Repository, 0, len(names))
	for _, name := range names {
		repos = append(repos, ghrepo.MustParse(name))
	}
	return func() []ghrepo.Repository { return repos }
}

// A role's gh bundle holds every owner of the project: its App's write lease for the one
// repository its issue works, in that repository's owner's directory, which is the bundle's
// default, a directory of its own under that owner for every other repository the mapping serves
// there, each holding a lease narrowed to that repository alone, and one read-only lease per
// other owner over the repositories that owner serves. A repository outside the project's set
// still gets its write lease, since a role's repository is its issue's, from the daemon's own
// record. A call that names no repository writes to nothing: every directory is read-only, as
// the review App for a controller, and the default is the first owner sorted. A mint that fails
// fails the bundle, naming what it would have reached; a project with no owner at all and no
// repository of its own has no bundle.
func TestGitHubCredentialBundlesEveryOwnerOfTheProject(t *testing.T) {
	repos := projectRepos("sjawhar/legion", "Acme/widgets", "acme/gadgets")
	credential := GitHubCredential(ownerTokens{}, repos, ghrepo.Renames{})
	for _, tc := range []struct {
		name    string
		role    claim.Role
		repo    ghrepo.Repository
		def     string
		tokens  map[string]string
		appName string
	}{
		{"an implementer on one of acme's two repositories", claim.RoleImplementer, ghrepo.MustParse("Acme/widgets"), "acme",
			map[string]string{
				"acme":         "implement-token-for-acme/widgets",
				"acme/gadgets": "implement-read-for-acme/gadgets",
				"sjawhar":      "implement-read-for-sjawhar/legion",
			}, "implement"},
		{"a reviewer on sjawhar's only repository", claim.RoleReviewer, ghrepo.MustParse("sjawhar/legion"), "sjawhar",
			map[string]string{
				"acme":    "review-read-for-acme/gadgets+widgets",
				"sjawhar": "review-token-for-sjawhar/legion",
			}, "review"},
		{"a merger on a repository outside the set", claim.RoleMerger, ghrepo.MustParse("Other/thing"), "other",
			map[string]string{
				"acme":    "implement-read-for-acme/gadgets+widgets",
				"other":   "implement-token-for-other/thing",
				"sjawhar": "implement-read-for-sjawhar/legion",
			}, "implement"},
		{"the controller, which names no repository", claim.RoleController, ghrepo.Repository{}, "acme",
			map[string]string{
				"acme":    "review-read-for-acme/gadgets+widgets",
				"sjawhar": "review-read-for-sjawhar/legion",
			}, "review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle, err := credential(context.Background(), tc.role, tc.repo)
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			if bundle.Default != tc.def {
				t.Errorf("Default = %q, want %q", bundle.Default, tc.def)
			}
			if got := bundleTokens(t, bundle); !maps.Equal(got, tc.tokens) {
				t.Errorf("tokens = %v, want %v", got, tc.tokens)
			}
			for dir, rendered := range bundle.Dirs {
				if rendered.App != tc.appName || rendered.Config != ghconfig.Config {
					t.Errorf("%s's render = App %q, config %q; want %q and the fixed config", dir, rendered.App, rendered.Config, tc.appName)
				}
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
			if dirs := GitHubDirs(repos, ghrepo.Renames{}, tc.repo); !slices.Equal(dirs, bundle.DirNames()) {
				t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
			}
		})
	}

	refused := GitHubCredential(ownerTokens{refuse: "sjawhar"}, repos, ghrepo.Renames{})
	if _, err := refused(context.Background(), claim.RoleImplementer, ghrepo.MustParse("acme/widgets")); err == nil ||
		!strings.Contains(err.Error(), "sjawhar") || !strings.Contains(err.Error(), string(claim.RoleImplementer)) {
		t.Errorf("a refused read mint = %v, want an error naming the owner and the role", err)
	}
	none := GitHubCredential(ownerTokens{}, projectRepos(), ghrepo.Renames{})
	if _, err := none(context.Background(), claim.RoleController, ghrepo.Repository{}); !errors.Is(err, runtime.ErrNoGitHubOwner) {
		t.Errorf("a bundle with no owner = %v, want runtime.ErrNoGitHubOwner", err)
	}
	if dirs := GitHubDirs(projectRepos(), ghrepo.Renames{}, ghrepo.Repository{}); len(dirs) != 0 {
		t.Errorf("GitHubDirs of a project with no repository = %q, want none", dirs)
	}
}

// The issue's own owner serves the repository the role writes to and others beside it. Its
// directory holds the write token for the issue's repository alone; each sibling repository has a
// directory of its own whose token names that repository and no other; and another owner's one
// directory names that owner's repositories. No directory holds a token wider than its name.
func TestGitHubCredentialNarrowsEverySiblingRepositoryToItsOwnDirectory(t *testing.T) {
	var minted []string
	repos := projectRepos("cybermonklabs/legion", "theorem-labs/agent-infra", "theorem-labs/nats-server")
	credential := GitHubCredential(countingTokens{minted: &minted}, repos, ghrepo.Renames{})
	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("theorem-labs/agent-infra"))
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	want := map[string]string{
		"theorem-labs":             "implement-token-for-theorem-labs/agent-infra",
		"theorem-labs/nats-server": "implement-read-for-theorem-labs/nats-server",
		"cybermonklabs":            "implement-read-for-cybermonklabs/legion",
	}
	if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
		t.Errorf("tokens = %v, want %v", got, want)
	}
	if bundle.Default != "theorem-labs" {
		t.Errorf("Default = %q, want theorem-labs, the issue's own owner's directory", bundle.Default)
	}
	// The issue's own repository has no directory of its own: its token is the write one, in the
	// owner's directory, and a second directory for it would hold a read-only token the role
	// would read in place of the write one.
	if _, held := bundle.Dirs["theorem-labs/agent-infra"]; held {
		t.Error("the bundle holds a directory for the issue's own repository beside its owner's")
	}
	if want := []string{"cybermonklabs", "theorem-labs", "theorem-labs/nats-server"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("directories = %q, want %q", bundle.DirNames(), want)
	}
	if dirs := GitHubDirs(repos, ghrepo.Renames{}, ghrepo.MustParse("theorem-labs/agent-infra")); !slices.Equal(dirs, bundle.DirNames()) {
		t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
	}
	if want := []string{"cybermonklabs/legion", "theorem-labs/agent-infra", "theorem-labs/nats-server"}; !slices.Equal(minted, want) {
		t.Errorf("minted for %v, want one lease per scope %v", minted, want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
}

// legionRename is the pair a deployment configures over sjawhar/legion's transfer to
// cybermonklabs: one repository under two names, whose owners the bundle holds one entry for.
var legionRename = ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")

// A configured rename whose `to` the project owns is one pair of owners: the bundle's sjawhar
// directory and its cybermonklabs directory hold one entry, minted once for the repository as
// cybermonklabs holds it now, and the repository directories under them are the same pair twice
// over. That one entry is the write lease whenever the issue's repository is the moved one — a
// pod launched for an issue recorded on sjawhar before the transfer has to write the repository
// as GitHub holds it now — and the read-only one for a role on any other repository. Every owner
// outside the pair is minted for itself, as it always was.
func TestGitHubCredentialBundlesARenamedPairAsOneOwner(t *testing.T) {
	repos := projectRepos("Cybermonklabs/legion", "cybermonklabs/tools", "acme/widgets")
	for _, tc := range []struct {
		name    string
		role    claim.Role
		repo    ghrepo.Repository
		def     string
		tokens  map[string]string
		minted  []string
		appName string
	}{
		{
			// The record still names sjawhar, so sjawhar's directory is the default and holds the
			// write token minted for cybermonklabs, the owner the repository moved to.
			name: "an implementer on the repository as it was recorded", role: claim.RoleImplementer,
			repo: ghrepo.MustParse("sjawhar/legion"), def: "sjawhar", appName: "implement",
			tokens: map[string]string{
				"acme":                "implement-read-for-acme/widgets",
				"cybermonklabs":       "implement-token-for-cybermonklabs/legion",
				"cybermonklabs/tools": "implement-read-for-cybermonklabs/tools",
				"sjawhar":             "implement-token-for-cybermonklabs/legion",
				"sjawhar/tools":       "implement-read-for-cybermonklabs/tools",
			},
			minted: []string{"acme/widgets", "cybermonklabs/legion", "cybermonklabs/tools"},
		},
		{
			name: "an implementer on the repository as GitHub holds it now", role: claim.RoleImplementer,
			repo: ghrepo.MustParse("Cybermonklabs/legion"), def: "cybermonklabs", appName: "implement",
			tokens: map[string]string{
				"acme":                "implement-read-for-acme/widgets",
				"cybermonklabs":       "implement-token-for-cybermonklabs/legion",
				"cybermonklabs/tools": "implement-read-for-cybermonklabs/tools",
				"sjawhar":             "implement-token-for-cybermonklabs/legion",
				"sjawhar/tools":       "implement-read-for-cybermonklabs/tools",
			},
			minted: []string{"acme/widgets", "cybermonklabs/legion", "cybermonklabs/tools"},
		},
		{
			name: "an implementer on another owner of the project", role: claim.RoleImplementer,
			repo: ghrepo.MustParse("acme/widgets"), def: "acme", appName: "implement",
			tokens: map[string]string{
				"acme":          "implement-token-for-acme/widgets",
				"cybermonklabs": "implement-read-for-cybermonklabs/legion+tools",
				"sjawhar":       "implement-read-for-cybermonklabs/legion+tools",
			},
			minted: []string{"acme/widgets", "cybermonklabs/legion+tools"},
		},
		{
			name: "the controller, which writes to nothing", role: claim.RoleController,
			repo: ghrepo.Repository{}, def: "acme", appName: "review",
			tokens: map[string]string{
				"acme":          "review-read-for-acme/widgets",
				"cybermonklabs": "review-read-for-cybermonklabs/legion+tools",
				"sjawhar":       "review-read-for-cybermonklabs/legion+tools",
			},
			minted: []string{"acme/widgets", "cybermonklabs/legion+tools"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minted []string
			renames := ghrepo.MustRenames(legionRename)
			credential := GitHubCredential(countingTokens{minted: &minted}, repos, renames)
			bundle, err := credential(context.Background(), tc.role, tc.repo)
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
				t.Errorf("minted for %v, want %v: the pair's directories hold one lease each, minted once", minted, tc.minted)
			}
			// The pair's two owner directories hold the one entry, and so do the two repository
			// directories under them where the bundle has any.
			for _, pair := range [][2]string{{"sjawhar", "cybermonklabs"}, {"sjawhar/tools", "cybermonklabs/tools"}} {
				from, to := bundle.Dirs[pair[0]], bundle.Dirs[pair[1]]
				if _, held := bundle.Dirs[pair[0]]; !held {
					continue
				}
				if from != to {
					t.Errorf("%s's entry = %+v and %s's = %+v, want the one entry in both directories", pair[0], from, pair[1], to)
				}
			}
			for dir, rendered := range bundle.Dirs {
				if rendered.App != tc.appName {
					t.Errorf("%s's render = App %q, want %q", dir, rendered.App, tc.appName)
				}
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
			if dirs := GitHubDirs(repos, renames, tc.repo); !slices.Equal(dirs, bundle.DirNames()) {
				t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
			}
		})
	}

	// With no pair configured, the owner the repository moved from is an owner like any other:
	// its own read-only lease, minted for itself, as today.
	t.Run("no pair configured", func(t *testing.T) {
		var minted []string
		both := projectRepos("Cybermonklabs/legion", "sjawhar/legion")
		credential := GitHubCredential(countingTokens{minted: &minted}, both, ghrepo.Renames{})
		bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("cybermonklabs/legion"))
		if err != nil {
			t.Fatalf("credential: %v", err)
		}
		want := map[string]string{
			"cybermonklabs": "implement-token-for-cybermonklabs/legion",
			"sjawhar":       "implement-read-for-sjawhar/legion",
		}
		if got := bundleTokens(t, bundle); !maps.Equal(got, want) || bundle.Default != "cybermonklabs" {
			t.Errorf("Default %q and tokens %v, want cybermonklabs and %v", bundle.Default, got, want)
		}
		if !slices.Equal(minted, []string{"cybermonklabs/legion", "sjawhar/legion"}) {
			t.Errorf("minted for %v, want each owner its own lease", minted)
		}
	})

	// Where the owner the repository moved from is itself an owner of the project, its own entry
	// wins — its repositories are the project's and its token is its own. The conflict is warned
	// of where the owners that decide it are read, once per mapping load (the daemon's
	// logRenameOwnerConflicts), so nothing is logged here.
	t.Run("the owner it moved from is an owner of the project", func(t *testing.T) {
		var minted []string
		both := projectRepos("Cybermonklabs/legion", "sjawhar/legion")
		credential := GitHubCredential(countingTokens{minted: &minted}, both, ghrepo.MustRenames(legionRename))
		bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("cybermonklabs/legion"))
		if err != nil {
			t.Fatalf("credential: %v", err)
		}
		want := map[string]string{
			"cybermonklabs": "implement-token-for-cybermonklabs/legion",
			"sjawhar":       "implement-read-for-sjawhar/legion",
		}
		if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
			t.Errorf("tokens = %v, want %v: each owner keeps its own token", got, want)
		}
		if !slices.Equal(minted, []string{"cybermonklabs/legion", "sjawhar/legion"}) {
			t.Errorf("minted for %v, want each owner its own lease", minted)
		}
		if _, err := credential(context.Background(), claim.RoleReviewer, ghrepo.MustParse("sjawhar/legion")); err != nil {
			t.Fatalf("credential: %v", err)
		}
	})
}

// ControllerCredential is one read-only directory per owner, each narrowed to the repositories
// that owner serves and holding no repository directory of its own: the controller writes to
// none. Its default is the owner it is asked for — an alias owner included, which its launch may
// have recorded — or the first owner sorted when it is asked for none. An owner outside the
// project is refused before anything is minted, and a project with no owner at all has no bundle.
func TestControllerCredentialIsReadOnlyForEveryOwner(t *testing.T) {
	repos := projectRepos("Cybermonklabs/legion", "cybermonklabs/tools", "acme/widgets")
	renames := ghrepo.MustRenames(legionRename)
	want := map[string]string{
		"acme":          "review-read-for-acme/widgets",
		"cybermonklabs": "review-read-for-cybermonklabs/legion+tools",
		"sjawhar":       "review-read-for-cybermonklabs/legion+tools",
	}
	for asked, def := range map[string]string{"": "acme", "Cybermonklabs": "cybermonklabs", "Sjawhar": "sjawhar"} {
		bundle, err := ControllerCredential(ownerTokens{}, repos, renames)(context.Background(), asked)
		if err != nil {
			t.Fatalf("ControllerCredential(%q): %v", asked, err)
		}
		if bundle.Default != def {
			t.Errorf("ControllerCredential(%q).Default = %q, want %q", asked, bundle.Default, def)
		}
		if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
			t.Errorf("ControllerCredential(%q) tokens = %v, want %v", asked, got, want)
		}
		if err := bundle.Validate(); err != nil {
			t.Errorf("the bundle does not validate: %v", err)
		}
	}

	var minted []string
	outside := ControllerCredential(countingTokens{minted: &minted}, repos, renames)
	if _, err := outside(context.Background(), "Outside-Project"); !errors.Is(err, ErrOwnerOutsideProject) ||
		!strings.Contains(err.Error(), `"outside-project"`) || len(minted) != 0 {
		t.Errorf("the controller asking for an owner outside the project = %v, minted %q; want ErrOwnerOutsideProject naming it and no mint", err, minted)
	}
	none := ControllerCredential(ownerTokens{}, projectRepos(), ghrepo.Renames{})
	if _, err := none(context.Background(), ""); !errors.Is(err, runtime.ErrNoGitHubOwner) {
		t.Errorf("a controller bundle with no owner = %v, want runtime.ErrNoGitHubOwner", err)
	}
}

// countingTokens records what each mint was asked to reach, in the order the bundle asked.
type countingTokens struct{ minted *[]string }

func (t countingTokens) Token(ctx context.Context, role AppRole, repo ghrepo.Repository) (Lease, error) {
	*t.minted = append(*t.minted, strings.ToLower(repo.String()))
	return ownerTokens{}.Token(ctx, role, repo)
}

func (t countingTokens) ReadToken(ctx context.Context, role AppRole, owner string, repos []string) (Lease, error) {
	*t.minted = append(*t.minted, owner+"/"+strings.Join(repos, "+"))
	return ownerTokens{}.ReadToken(ctx, role, owner, repos)
}
