package appauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
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
// and the repositories it was narrowed to. refuse is an owner every mint for it fails, with an
// error that is no narrowing refusal, and unreachable names the repositories, as
// `<owner>/<repo>`, that the App's installation does not reach: a lease naming any of them is
// ErrNarrowingRefused, as GitHub answers 422 to a request naming one.
type ownerTokens struct {
	refuse      string
	unreachable map[string]bool
}

// refusesNarrowing reports that the installation on owner reaches none of repos, as a 422 says
// it: GitHub refuses the request whole over any one repository it does not reach.
func (t ownerTokens) refusesNarrowing(owner string, repos ...string) error {
	for _, repo := range repos {
		if t.unreachable[strings.ToLower(owner+"/"+repo)] {
			return fmt.Errorf("%w (repositories %s)", ErrNarrowingRefused, strings.Join(repos, ", "))
		}
	}
	return nil
}

func (t ownerTokens) Token(_ context.Context, role AppRole, repo ghrepo.Repository) (Lease, error) {
	if repo.IsZero() {
		return Lease{}, errors.New("a write token names no repository")
	}
	if repo.Owner() == t.refuse {
		return Lease{}, errors.New("no installation on " + repo.Owner())
	}
	if err := t.refusesNarrowing(repo.Owner(), repo.Name()); err != nil {
		return Lease{}, err
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
	if err := t.refusesNarrowing(owner, repos...); err != nil {
		return Lease{}, err
	}
	return Lease{Token: string(role) + "-read-for-" + owner + "/" + strings.Join(repos, "+"), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// bundleLog is a logger a test reads what a bundle said on, and the lines it holds.
func bundleLog() (*slog.Logger, *bytes.Buffer) {
	said := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(said, &slog.HandlerOptions{Level: slog.LevelWarn})), said
}

// quietBundleLog is the logger of a test that reads no line of its own.
func quietBundleLog() *slog.Logger {
	log, _ := bundleLog()
	return log
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
	credential := GitHubCredential(ownerTokens{}, repos, ghrepo.Renames{}, nil, quietBundleLog())
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
			if dirs := GitHubDirs(repos, ghrepo.Renames{}, nil, Roles, tc.repo); !slices.Equal(dirs, bundle.DirNames()) {
				t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
			}
		})
	}

	refused := GitHubCredential(ownerTokens{refuse: "sjawhar"}, repos, ghrepo.Renames{}, nil, quietBundleLog())
	if _, err := refused(context.Background(), claim.RoleImplementer, ghrepo.MustParse("acme/widgets")); err == nil ||
		!strings.Contains(err.Error(), "sjawhar") || !strings.Contains(err.Error(), string(claim.RoleImplementer)) {
		t.Errorf("a refused read mint = %v, want an error naming the owner and the role", err)
	}
	none := GitHubCredential(ownerTokens{}, projectRepos(), ghrepo.Renames{}, nil, quietBundleLog())
	if _, err := none(context.Background(), claim.RoleController, ghrepo.Repository{}); !errors.Is(err, runtime.ErrNoGitHubOwner) {
		t.Errorf("a bundle with no owner = %v, want runtime.ErrNoGitHubOwner", err)
	}
	if dirs := GitHubDirs(projectRepos(), ghrepo.Renames{}, nil, Roles, ghrepo.Repository{}); len(dirs) != 0 {
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
	credential := GitHubCredential(countingTokens{minted: &minted}, repos, ghrepo.Renames{}, nil, quietBundleLog())
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
	if dirs := GitHubDirs(repos, ghrepo.Renames{}, nil, Roles, ghrepo.MustParse("theorem-labs/agent-infra")); !slices.Equal(dirs, bundle.DirNames()) {
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

// A configured rename whose `to` the project owns puts the owner the repository moved from in
// the bundle, with no repository of its own: its directory is minted for the owner the
// repository moved to, and the repository directories under the two owners are the same pair
// twice over. Which lease that is turns on the issue's own repository. Where it is the moved
// one, the recorded owner's directory and the serving owner's hold one write lease, minted once
// for the repository as cybermonklabs holds it now, since a pod launched for an issue recorded
// on sjawhar before the transfer has to write the repository as GitHub holds it; where it is
// any other repository, or none, every directory of the pair is read-only, the alias owner's
// over the repositories the owner that serves the moved repository serves. Every owner outside
// the pair is minted for itself, as it always was.
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
		// oneLease are the directory pairs holding the one entry, minted once: the pair's two
		// owner directories wherever one lease is theirs to share, and the repository
		// directories under them, which always are.
		oneLease [][2]string
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
			minted:   []string{"acme/widgets", "cybermonklabs/legion", "cybermonklabs/tools"},
			oneLease: [][2]string{{"sjawhar", "cybermonklabs"}, {"sjawhar/tools", "cybermonklabs/tools"}},
		},
		{
			// The record names the repository as GitHub holds it, so the write lease stands in
			// the cybermonklabs directory alone: sjawhar is neither the recorded owner nor the
			// owner serving the issue's repository, and a directory only the rename puts in the
			// bundle holds the read lease of the owner that serves the moved repository.
			name: "an implementer on the repository as GitHub holds it now", role: claim.RoleImplementer,
			repo: ghrepo.MustParse("Cybermonklabs/legion"), def: "cybermonklabs", appName: "implement",
			tokens: map[string]string{
				"acme":                "implement-read-for-acme/widgets",
				"cybermonklabs":       "implement-token-for-cybermonklabs/legion",
				"cybermonklabs/tools": "implement-read-for-cybermonklabs/tools",
				"sjawhar":             "implement-read-for-cybermonklabs/legion+tools",
				"sjawhar/tools":       "implement-read-for-cybermonklabs/tools",
			},
			minted:   []string{"acme/widgets", "cybermonklabs/legion", "cybermonklabs/tools", "cybermonklabs/legion+tools"},
			oneLease: [][2]string{{"sjawhar/tools", "cybermonklabs/tools"}},
		},
		{
			name: "an implementer on another owner of the project", role: claim.RoleImplementer,
			repo: ghrepo.MustParse("acme/widgets"), def: "acme", appName: "implement",
			tokens: map[string]string{
				"acme":          "implement-token-for-acme/widgets",
				"cybermonklabs": "implement-read-for-cybermonklabs/legion+tools",
				"sjawhar":       "implement-read-for-cybermonklabs/legion+tools",
			},
			minted:   []string{"acme/widgets", "cybermonklabs/legion+tools"},
			oneLease: [][2]string{{"sjawhar", "cybermonklabs"}},
		},
		{
			name: "the controller, which writes to nothing", role: claim.RoleController,
			repo: ghrepo.Repository{}, def: "acme", appName: "review",
			tokens: map[string]string{
				"acme":          "review-read-for-acme/widgets",
				"cybermonklabs": "review-read-for-cybermonklabs/legion+tools",
				"sjawhar":       "review-read-for-cybermonklabs/legion+tools",
			},
			minted:   []string{"acme/widgets", "cybermonklabs/legion+tools"},
			oneLease: [][2]string{{"sjawhar", "cybermonklabs"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minted []string
			renames := ghrepo.MustRenames(legionRename)
			credential := GitHubCredential(countingTokens{minted: &minted}, repos, renames, nil, quietBundleLog())
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
			// Every pair of directories that shares a lease holds the one entry, minted once:
			// the pair's two owner directories where one lease is theirs to share, and the two
			// repository directories under them where the bundle has any.
			for _, pair := range tc.oneLease {
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
			if dirs := GitHubDirs(repos, renames, nil, Roles, tc.repo); !slices.Equal(dirs, bundle.DirNames()) {
				t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
			}
		})
	}

	// With no pair configured, the owner the repository moved from is an owner like any other:
	// its own read-only lease, minted for itself, as today.
	t.Run("no pair configured", func(t *testing.T) {
		var minted []string
		both := projectRepos("Cybermonklabs/legion", "sjawhar/legion")
		credential := GitHubCredential(countingTokens{minted: &minted}, both, ghrepo.Renames{}, nil, quietBundleLog())
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

	// Where the project serves the pair's `from` as well as its `to`, the pair resolves for
	// nobody (ghrepo.Renames.Serving: a name the project serves under its own name is a
	// repository of its own, somebody having made one at the old location, which deletes
	// GitHub's redirect). So the owner the repository moved from is no alias, its own entry
	// wins — its repositories are the project's and its token is its own — and nothing is
	// warned of anywhere: the rule decides it per repository rather than leaving two owners to
	// argue over one directory.
	t.Run("the owner it moved from is an owner of the project under the pair's own from", func(t *testing.T) {
		var minted []string
		both := projectRepos("Cybermonklabs/legion", "sjawhar/legion")
		credential := GitHubCredential(countingTokens{minted: &minted}, both, ghrepo.MustRenames(legionRename), nil, quietBundleLog())
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

// A resolving pair moves one repository, never an owner: the write lease stands in the directory
// named for the issue's recorded owner and the one named for the owner whose installation serves
// that repository, and in no other, so every directory named for an owner the project serves
// repositories of under its own name keeps that owner's own read lease over that owner's own
// repositories.
//
// Both cases are the state production was in on 2026-11-06: the pair sjawhar/legion ->
// cybermonklabs/legion resolving while the project also serves sjawhar/oh-my-pi. Redirecting
// every directory of the pair to the write lease, as the bundle did while an alias owner's
// directory took its mint owner's lease whatever the issue's repository was, put a write lease
// for sjawhar/oh-my-pi in the directory named cybermonklabs, and left sjawhar/oh-my-pi reachable
// from no directory at all for an issue on cybermonklabs/legion. Neither case may change a
// directory name, which is why each asserts the whole set against GitHubDirs as well: a pod's gh
// projection is fixed when the pod is created, so a name this rule added or dropped would be
// drift the sweep answers with an update round over every worker in the pod.
func TestEveryServedOwnerKeepsItsOwnReadLeaseWhileAPairResolves(t *testing.T) {
	repos := projectRepos("Cybermonklabs/legion", "acme/widgets", "sjawhar/oh-my-pi")
	renames := ghrepo.MustRenames(legionRename)
	for _, tc := range []struct {
		name   string
		repo   ghrepo.Repository
		dirs   []string
		tokens map[string]string
		minted []string
	}{
		{
			// sjawhar's own repository is the issue's, so the sjawhar directory holds its write
			// lease — and the cybermonklabs directory holds cybermonklabs's own read lease, not
			// a write lease for a repository of another owner.
			name: "an issue on the old owner's own other repository", repo: ghrepo.MustParse("sjawhar/oh-my-pi"),
			dirs: []string{"acme", "cybermonklabs", "cybermonklabs/legion", "sjawhar", "sjawhar/legion"},
			tokens: map[string]string{
				"acme":                 "implement-read-for-acme/widgets",
				"cybermonklabs":        "implement-read-for-cybermonklabs/legion",
				"cybermonklabs/legion": "implement-read-for-cybermonklabs/legion",
				"sjawhar":              "implement-token-for-sjawhar/oh-my-pi",
				"sjawhar/legion":       "implement-read-for-cybermonklabs/legion",
			},
			minted: []string{"acme/widgets", "cybermonklabs/legion", "sjawhar/oh-my-pi"},
		},
		{
			// The issue is the moved repository's, recorded under the name GitHub has, so the
			// write lease stands in the cybermonklabs directory alone and the sjawhar directory
			// reads sjawhar's own repositories: the project's own sjawhar/oh-my-pi is reachable.
			name: "an issue on the repository the pair moved", repo: ghrepo.MustParse("cybermonklabs/legion"),
			dirs: []string{"acme", "cybermonklabs", "sjawhar"},
			tokens: map[string]string{
				"acme":          "implement-read-for-acme/widgets",
				"cybermonklabs": "implement-token-for-cybermonklabs/legion",
				"sjawhar":       "implement-read-for-sjawhar/oh-my-pi",
			},
			minted: []string{"acme/widgets", "cybermonklabs/legion", "sjawhar/oh-my-pi"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minted []string
			credential := GitHubCredential(countingTokens{minted: &minted}, repos, renames, nil, quietBundleLog())
			bundle, err := credential(context.Background(), claim.RoleImplementer, tc.repo)
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			if got := bundleTokens(t, bundle); !maps.Equal(got, tc.tokens) {
				t.Errorf("tokens = %v, want %v", got, tc.tokens)
			}
			if !slices.Equal(minted, tc.minted) {
				t.Errorf("minted for %v, want %v: one lease per scope, each owner's own", minted, tc.minted)
			}
			if got := bundle.DirNames(); !slices.Equal(got, tc.dirs) {
				t.Errorf("the bundle's directories are %q, want %q: this rule renames none", got, tc.dirs)
			}
			if dirs := GitHubDirs(repos, renames, nil, Roles, tc.repo); !slices.Equal(dirs, tc.dirs) {
				t.Errorf("GitHubDirs = %q, want the same set %q, which is what a running pod is compared against", dirs, tc.dirs)
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
		})
	}
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
		bundle, err := ControllerCredential(ownerTokens{}, repos, renames, nil, quietBundleLog())(context.Background(), asked)
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
	outside := ControllerCredential(countingTokens{minted: &minted}, repos, renames, nil, quietBundleLog())
	if _, err := outside(context.Background(), "Outside-Project"); !errors.Is(err, ErrOwnerOutsideProject) ||
		!strings.Contains(err.Error(), `"outside-project"`) || len(minted) != 0 {
		t.Errorf("the controller asking for an owner outside the project = %v, minted %q; want ErrOwnerOutsideProject naming it and no mint", err, minted)
	}
	none := ControllerCredential(ownerTokens{}, projectRepos(), ghrepo.Renames{}, nil, quietBundleLog())
	if _, err := none(context.Background(), ""); !errors.Is(err, runtime.ErrNoGitHubOwner) {
		t.Errorf("a controller bundle with no owner = %v, want runtime.ErrNoGitHubOwner", err)
	}
}

// countingTokens records what each mint was asked to reach, in the order the bundle asked, and
// answers as the ownerTokens it holds: a test that names unreachable repositories reads both
// what was asked and what was refused.
type countingTokens struct {
	ownerTokens
	minted *[]string
}

func (t countingTokens) Token(ctx context.Context, role AppRole, repo ghrepo.Repository) (Lease, error) {
	*t.minted = append(*t.minted, strings.ToLower(repo.String()))
	return t.ownerTokens.Token(ctx, role, repo)
}

func (t countingTokens) ReadToken(ctx context.Context, role AppRole, owner string, repos []string) (Lease, error) {
	*t.minted = append(*t.minted, owner+"/"+strings.Join(repos, "+"))
	return t.ownerTokens.ReadToken(ctx, role, owner, repos)
}

// GitHub answers 422 to a token request naming a repository its installation does not reach — one
// dropped from the App's selected repositories, deleted, or transferred without a configured
// rename — and mints nothing, so a read lease over several repositories is refused whole over any
// one of them. The refusal costs that repository alone: the lease is asked for again one
// repository at a time, the owner's directory holds a lease over the ones that answered, and the
// bundle is served rather than the role's launch failing for every owner.
func TestGitHubCredentialKeepsTheReadLeaseTheReachableRepositoriesGive(t *testing.T) {
	var minted []string
	log, said := bundleLog()
	repos := projectRepos("sjawhar/legion", "acme/widgets", "acme/gadgets")
	tokens := countingTokens{ownerTokens: ownerTokens{unreachable: map[string]bool{"acme/gadgets": true}}, minted: &minted}
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, nil, log)

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	want := map[string]string{
		"acme":    "implement-read-for-acme/widgets",
		"sjawhar": "implement-token-for-sjawhar/legion",
	}
	if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
		t.Errorf("tokens = %v, want %v: acme's directory reaches the one repository that answered", got, want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
	// The refused lease, then one ask per repository, and the write lease: the lease over the one
	// repository that answered is the ask already made for it, which the Manager serves from its
	// cache, so no fourth read ask is made for acme.
	if asked := []string{"acme/gadgets+widgets", "acme/gadgets", "acme/widgets", "sjawhar/legion"}; !slices.Equal(minted, asked) {
		t.Errorf("minted for %v, want %v", minted, asked)
	}
	if count := strings.Count(said.String(), `repository=gadgets`); count != 1 {
		t.Errorf("the log holds %d lines naming the refused repository, want one:\n%s", count, said.String())
	}
	for _, named := range []string{"app=implement", "owner=acme"} {
		if !strings.Contains(said.String(), named) {
			t.Errorf("the log line does not name %q:\n%s", named, said.String())
		}
	}
}

// A sibling repository of the role's own owner holds a directory of its own, over that repository
// alone, so a refusal of its lease is a refusal of everything that directory could hold: it is
// left out of the bundle, and every other directory — the write lease among them — is minted and
// served.
func TestGitHubCredentialLeavesOutASiblingRepositoryGitHubRefuses(t *testing.T) {
	log, said := bundleLog()
	repos := projectRepos("theorem-labs/agent-infra", "theorem-labs/nats-server", "cybermonklabs/legion")
	tokens := ownerTokens{unreachable: map[string]bool{"theorem-labs/nats-server": true}}
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, nil, log)

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("theorem-labs/agent-infra"))
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	want := map[string]string{
		"cybermonklabs": "implement-read-for-cybermonklabs/legion",
		"theorem-labs":  "implement-token-for-theorem-labs/agent-infra",
	}
	if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
		t.Errorf("tokens = %v, want %v: the refused repository's directory is left out and the rest whole", got, want)
	}
	if _, held := bundle.Dirs["theorem-labs/nats-server"]; held {
		t.Error("the bundle holds a directory for a repository the installation does not reach")
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
	if count := strings.Count(said.String(), "repository=nats-server"); count != 1 {
		t.Errorf("the log holds %d lines naming the refused repository, want one:\n%s", count, said.String())
	}
}

// The write lease is not softened: a role that cannot act on its own issue's repository has
// nothing to do, so a refusal of it fails the whole bundle, naming the role, the App and the
// repository the lease would have reached.
func TestGitHubCredentialStillFailsOnARefusedWriteLease(t *testing.T) {
	log, said := bundleLog()
	repos := projectRepos("acme/widgets", "sjawhar/legion")
	tokens := ownerTokens{unreachable: map[string]bool{"acme/widgets": true}}
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, nil, log)

	bundle, err := credential(context.Background(), claim.RoleReviewer, ghrepo.MustParse("acme/widgets"))
	if !errors.Is(err, ErrNarrowingRefused) || len(bundle.Dirs) != 0 {
		t.Fatalf("a refused write lease = %+v, %v; want no bundle and the refusal", bundle, err)
	}
	for _, named := range []string{"review", string(claim.RoleReviewer), "acme/widgets"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal does not name %q: %v", named, err)
		}
	}
	if said.Len() != 0 {
		t.Errorf("the log holds %q, want nothing: a refused write lease is the bundle's failure, not a repository left out", said.String())
	}
}

// A repository GitHub refuses is refused at every launch and every refresh of every role, so the
// line is said once for the life of the process rather than at each of them, as a Manager says a
// grant shortfall once.
func TestGitHubCredentialSaysARefusedRepositoryOnceOverTwoBundles(t *testing.T) {
	log, said := bundleLog()
	repos := projectRepos("oncelabs/served", "oncelabs/twice")
	tokens := ownerTokens{unreachable: map[string]bool{"oncelabs/twice": true}}
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, nil, log)

	for i := range 2 {
		if _, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("other/thing")); err != nil {
			t.Fatalf("bundle %d: %v", i+1, err)
		}
	}
	if count := strings.Count(said.String(), "repository=twice"); count != 1 {
		t.Errorf("the log holds %d lines over two bundles, want one:\n%s", count, said.String())
	}
}

// gh keeps an owner's own hosts.yml and config.yml in that owner's directory, so a repository of
// either name has no directory of its own to hold its lease: writing one would make a directory
// where a file of the bundle's is (ghconfig.IsDirName). That repository is left out, said once on
// the log, and every other directory of the bundle is written.
func TestGitHubCredentialLeavesOutARepositoryNamedLikeGhsOwnFiles(t *testing.T) {
	for owner, named := range map[string]string{"hostlabs": ghconfig.HostsFile, "conflabs": ghconfig.ConfigFile} {
		t.Run(named, func(t *testing.T) {
			log, said := bundleLog()
			repos := projectRepos(owner+"/legion", owner+"/"+named)
			credential := GitHubCredential(ownerTokens{}, repos, ghrepo.Renames{}, nil, log)

			bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse(owner+"/legion"))
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			want := map[string]string{owner: "implement-token-for-" + owner + "/legion"}
			if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
				t.Errorf("tokens = %v, want %v: the owner's own directory alone", got, want)
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
			if dirs := GitHubDirs(repos, ghrepo.Renames{}, nil, Roles, ghrepo.MustParse(owner+"/legion")); !slices.Equal(dirs, bundle.DirNames()) {
				t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
			}
			if count := strings.Count(said.String(), "repository="+named); count != 1 {
				t.Errorf("the log holds %d lines naming the repository left out, want one:\n%s", count, said.String())
			}
		})
	}
}

// A bundle's default is the directory its GH_CONFIG_DIR names, so it is chosen from the owners
// the mint answered for and not from the mapping: a caller that names no repository of its own —
// a tmux tree pane whose claim records none, every controller launch — would otherwise take the
// first owner sorted, and where GitHub refuses a lease over every repository that owner serves
// the bundle would come back without the one directory it is served from, which
// ghconfig.Bundle.Validate refuses long after the ask that could say why.
func TestABundleTakesItsDefaultFromTheOwnersTheMintAnsweredFor(t *testing.T) {
	repos := projectRepos("acme/widgets", "sjawhar/legion")
	tokens := ownerTokens{unreachable: map[string]bool{"acme/widgets": true}}
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, NewLeftOut(), quietBundleLog())

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.Repository{})
	if err != nil {
		t.Fatalf("a bundle whose first owner is refused: %v", err)
	}
	if bundle.Default != "sjawhar" {
		t.Errorf("the default is %q, want sjawhar: acme answered for no repository", bundle.Default)
	}
	if want := []string{"sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("directories = %q, want %q", bundle.DirNames(), want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
}

// A caller that names the default owner itself — the controller route carrying the owner its
// GH_CONFIG_DIR already names, and a role whose issue's repository is that owner's — cannot be
// served another owner's directory instead, so an owner GitHub refuses every lease of is
// ErrNarrowingRefused, naming the owner and the repositories the lease would have reached.
func TestABundleRefusesADefaultOwnerGitHubRefusesEveryLeaseOf(t *testing.T) {
	repos := projectRepos("acme/widgets", "acme/gadgets", "sjawhar/legion")
	tokens := ownerTokens{unreachable: map[string]bool{"acme/widgets": true, "acme/gadgets": true}}
	left := NewLeftOut()

	controller := ControllerCredential(tokens, repos, ghrepo.Renames{}, left, quietBundleLog())
	bundle, err := controller(context.Background(), "Acme")
	if !errors.Is(err, ErrNarrowingRefused) || len(bundle.Dirs) != 0 {
		t.Fatalf("the controller's bundle on a refused default owner = %+v, %v; want no bundle and ErrNarrowingRefused", bundle, err)
	}
	for _, named := range []string{"acme", "gadgets", "widgets"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal does not name %q: %v", named, err)
		}
	}
	if _, err := controller(context.Background(), "sjawhar"); err != nil {
		t.Errorf("the controller asking for an owner that answers = %v, want its bundle", err)
	}
}

// A read lease over several repositories GitHub refuses is asked for again one repository at a
// time, and then over the ones that answered. Where every one of them answered, that last ask
// names what the first ask named, which the Manager remembers as refused for five minutes
// (narrowingRefusalWindow) — and that memory is stale by construction: every repository it
// covers has just answered alone. Were it served, the directory would be refused with no call
// to GitHub, and the whole bundle with it — the write lease, the launch and the controller's
// own pod alike — over repositories GitHub reaches, until the window passed (INFRA-513).
func TestTheLastAskOverTheRepositoriesThatAnsweredIsMadePastTheRememberedRefusal(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	// Whether the installation reaches gadgets: the account adds it part-way through.
	reaches := false
	github := &keyModeGitHub{owners: []string{"acme"}, slug: "legion-reviewer"}
	github.answer = func(_ int, body accessTokensBody) (int, string) {
		if !reaches && slices.Contains(body.Repositories, "gadgets") {
			return http.StatusUnprocessableEntity, `{"message":"There is at least one repository that does not exist or is not accessible to the parent installation."}`
		}
		return http.StatusOK, mintedFor("ghs_"+strings.Join(body.Repositories, "_"), now.Add(time.Hour), body.Permissions, body.Repositories...)
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	controller := ControllerCredential(tokens, projectRepos("acme/gadgets", "acme/widgets"), ghrepo.Renames{}, NewLeftOut(), quietBundleLog())

	// The probe of an earlier bundle asked for the one repository the installation does not
	// reach, and the Manager remembers that scope's refusal.
	if _, err := tokens.ReadToken(context.Background(), Review, "acme", []string{"gadgets"}); !errors.Is(err, ErrNarrowingRefused) {
		t.Fatalf("the read lease over the repository the installation does not reach = %v, want ErrNarrowingRefused", err)
	}

	// Four minutes on, the bundle's own ask over both repositories is refused for that same
	// repository and remembered under a window of its own; the directory holds the one
	// repository that answered.
	now = now.Add(4 * time.Minute)
	bundle, err := controller(context.Background(), "acme")
	if err != nil {
		t.Fatalf("the controller's bundle while the installation reaches gadgets not at all: %v", err)
	}
	if got := bundleTokens(t, bundle)["acme"]; got != "ghs_widgets" {
		t.Fatalf("acme's directory holds %q, want the lease over widgets alone", got)
	}

	// The account adds gadgets to the installation. Two minutes later the refusal of gadgets
	// alone has passed its window, so the probe asks GitHub and is answered, while the refusal
	// of the two together, met at four minutes, is remembered still — and is the refusal of an
	// ask every repository of which has just answered.
	reaches = true
	now = now.Add(2 * time.Minute)
	bundle, err = controller(context.Background(), "acme")
	if err != nil {
		t.Fatalf("the controller's bundle once GitHub reaches both repositories = %v, want a bundle: the refusal remembered for their scope was met before either of them answered", err)
	}
	if got := bundleTokens(t, bundle)["acme"]; got != "ghs_gadgets_widgets" {
		t.Fatalf("acme's directory holds %q, want the lease over both repositories", got)
	}
}

// refusedNarrowingAnswer is GitHub's 422 to a token request naming a repository the installation
// does not reach: it mints nothing, whatever else the request named.
const refusedNarrowingAnswer = `{"message":"There is at least one repository that does not exist or is not accessible to the parent installation."}`

// askedOfGitHub is the repositories each token exchange named, joined by a plus, in the order
// GitHub got them: what a test reads to tell an ask that reached GitHub from one the Manager
// answered out of its cache.
func askedOfGitHub(github *keyModeGitHub) []string {
	bodies := github.exchanges()
	asked := make([]string, 0, len(bodies))
	for _, body := range bodies {
		asked = append(asked, strings.Join(body.Repositories, "+"))
	}
	return asked
}

// A probe of one repository is served from the Manager's lease cache wherever a lease naming that
// repository alone was minted within the hour — and one nearly always was: every issue pod's
// `<owner>/<repo>` directory mints one, for both Apps, as does the provisioning adapter. Such an
// answer says a token was minted, not that GitHub reaches the repository now, so an installation
// that drops a repository leaves every probe of it answering as it did, and the ask over the
// repositories the probes called reached is refused as the first ask was.
//
// That refusal costs the repositories it names and nothing else: the probes are made again, past
// the lease the Manager holds for each of them as well as past the refusal it remembers, so each
// is GitHub's answer now, and the directory holds a lease over what answers then. The bundle is
// whole — the write lease, the launch and the controller's own pod included — where it would
// otherwise be refused for the rest of a cached lease's life over repositories GitHub reaches
// (INFRA-513).
func TestADirectoryDropsTheRepositoryItsCachedProbeStillAnswersFor(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	// Whether the installation reaches gadgets: the account drops it part-way through.
	reaches := true
	github := &keyModeGitHub{owners: []string{"cachedlabs", "sjawhar"}, slug: "legion-implementer"}
	github.answer = func(_ int, body accessTokensBody) (int, string) {
		if !reaches && slices.Contains(body.Repositories, "gadgets") {
			return http.StatusUnprocessableEntity, refusedNarrowingAnswer
		}
		return http.StatusOK, mintedFor("ghs_"+strings.Join(body.Repositories, "_"), now.Add(time.Hour), body.Permissions, body.Repositories...)
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	log, said := bundleLog()
	repos := projectRepos("cachedlabs/gadgets", "cachedlabs/widgets", "sjawhar/legion")
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, NewLeftOut(), log)

	// An issue pod's own `cachedlabs/gadgets` directory minted this lease, and the Manager holds
	// it for the hour it lasts.
	if _, err := tokens.ReadToken(context.Background(), Implement, "cachedlabs", []string{"gadgets"}); err != nil {
		t.Fatalf("the read lease an earlier bundle minted over gadgets alone: %v", err)
	}

	// The account drops gadgets from the installation. A minute on, the lease over it is still
	// cached and still served, so the probe of gadgets answers while GitHub refuses it.
	reaches = false
	now = now.Add(time.Minute)
	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle once the installation dropped gadgets = %v, want a bundle over the repositories it reaches", err)
	}
	want := map[string]string{"cachedlabs": "ghs_widgets", "sjawhar": "ghs_legion"}
	if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
		t.Errorf("tokens = %v, want %v: the owner's directory over widgets alone, and the write lease whole", got, want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
	// The earlier lease, the bundle's own ask, the probe of widgets (gadgets' is served from the
	// cache), the ask over the two the probes called reached, the two probes made again past that
	// cache, and the write lease.
	asked := []string{"gadgets", "gadgets+widgets", "widgets", "gadgets+widgets", "gadgets", "widgets", "legion"}
	if got := askedOfGitHub(github); !slices.Equal(got, asked) {
		t.Errorf("GitHub was asked for %v, want %v", got, asked)
	}
	if count := strings.Count(said.String(), "repository=gadgets"); count != 1 {
		t.Errorf("the log holds %d lines naming the repository left out, want one:\n%s", count, said.String())
	}
	if !strings.Contains(said.String(), "owner=cachedlabs") {
		t.Errorf("the log line does not name the owner:\n%s", said.String())
	}
}

// Where the second round of probes is refused for every repository the directory holds — an
// installation that dropped them all, each of them answering from a lease the Manager cached
// before it did — the directory is left out of the bundle and the rest of the bundle stands, the
// write lease and every other directory in it. The drift check is told the same, so no pod is
// replaced for a directory no mint produces (LeftOut, GitHubDirs).
func TestADirectoryWhoseEveryCachedProbeIsRefusedAgainIsLeftOut(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	reaches := true
	github := &keyModeGitHub{owners: []string{"gonelabs", "sjawhar"}, slug: "legion-implementer"}
	github.answer = func(_ int, body accessTokensBody) (int, string) {
		dropped := slices.Contains(body.Repositories, "gadgets") || slices.Contains(body.Repositories, "widgets")
		if !reaches && dropped {
			return http.StatusUnprocessableEntity, refusedNarrowingAnswer
		}
		return http.StatusOK, mintedFor("ghs_"+strings.Join(body.Repositories, "_"), now.Add(time.Hour), body.Permissions, body.Repositories...)
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	log, said := bundleLog()
	repos := projectRepos("gonelabs/gadgets", "gonelabs/widgets", "sjawhar/legion")
	left := NewLeftOut()
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, left, log)

	for _, repo := range []string{"gadgets", "widgets"} {
		if _, err := tokens.ReadToken(context.Background(), Implement, "gonelabs", []string{repo}); err != nil {
			t.Fatalf("the read lease an earlier bundle minted over %s alone: %v", repo, err)
		}
	}

	// The installation is removed from both repositories, and both probes answer from the cache
	// all the same.
	reaches = false
	now = now.Add(time.Minute)
	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle once the installation dropped every repository of an owner = %v, want the rest of the bundle", err)
	}
	want := map[string]string{"sjawhar": "ghs_legion"}
	if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
		t.Errorf("tokens = %v, want %v: the owner's directory left out and the write lease whole", got, want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
	if dirs := GitHubDirs(repos, ghrepo.Renames{}, left, Roles, ghrepo.MustParse("sjawhar/legion")); !slices.Equal(dirs, bundle.DirNames()) {
		t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
	}
	// The two earlier leases, the bundle's own ask, the ask over the two the cached probes called
	// reached, the two probes made again past that cache — refused, both of them — and the write
	// lease.
	asked := []string{"gadgets", "widgets", "gadgets+widgets", "gadgets+widgets", "gadgets", "widgets", "legion"}
	if got := askedOfGitHub(github); !slices.Equal(got, asked) {
		t.Errorf("GitHub was asked for %v, want %v", got, asked)
	}
	for _, named := range []string{"repository=gadgets", "repository=widgets"} {
		if count := strings.Count(said.String(), named); count != 1 {
			t.Errorf("the log holds %d lines naming %q, want one:\n%s", count, named, said.String())
		}
	}
}

// One extra round, never a loop: where GitHub answers each repository alone and refuses them
// together — answers that disagree, which no account action makes and a daemon must still answer
// — the second round of probes answers as the first did and the ask over them is refused as
// before. The directory is left out, its repositories said on the log, and the bundle stands; the
// ask over the pair is made three times and no more.
func TestARepeatedRefusalOfTheRepositoriesThatAnsweredLeavesTheDirectoryOut(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"pairlabs", "sjawhar"}, slug: "legion-implementer"}
	github.answer = func(_ int, body accessTokensBody) (int, string) {
		if len(body.Repositories) > 1 {
			return http.StatusUnprocessableEntity, refusedNarrowingAnswer
		}
		return http.StatusOK, mintedFor("ghs_"+strings.Join(body.Repositories, "_"), now.Add(time.Hour), body.Permissions, body.Repositories...)
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	log, said := bundleLog()
	repos := projectRepos("pairlabs/gadgets", "pairlabs/widgets", "sjawhar/legion")
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, NewLeftOut(), log)

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle against an installation whose answers disagree = %v, want the rest of the bundle", err)
	}
	want := map[string]string{"sjawhar": "ghs_legion"}
	if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
		t.Errorf("tokens = %v, want %v: the owner's directory left out and the write lease whole", got, want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
	asked := []string{"gadgets+widgets", "gadgets", "widgets", "gadgets+widgets", "gadgets", "widgets", "gadgets+widgets", "legion"}
	if got := askedOfGitHub(github); !slices.Equal(got, asked) {
		t.Errorf("GitHub was asked for %v, want %v: one extra round and no more", got, asked)
	}
	// No repository of the set is refused on its own, so none is said left out on its own; the
	// directory is, and that line is said at every bundle rather than once for the process,
	// since nothing about it is keyed by a repository (sayDirectoryLeftOut).
	if count := strings.Count(said.String(), "repository="); count != 0 {
		t.Errorf("the log holds %d lines naming a repository left out on its own, want none:\n%s", count, said.String())
	}
	directory := `an owner's directory is left out of the role's gh bundle`
	if count := strings.Count(said.String(), directory); count != 1 {
		t.Errorf("the log holds %d lines naming the directory left out, want one:\n%s", count, said.String())
	}
	if !strings.Contains(said.String(), `repositories="gadgets, widgets"`) {
		t.Errorf("the line names no repositories of the directory:\n%s", said.String())
	}
	if _, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion")); err != nil {
		t.Fatalf("the second bundle: %v", err)
	}
	if count := strings.Count(said.String(), directory); count != 2 {
		t.Errorf("the log holds %d lines naming the directory left out over two bundles, want one each:\n%s", count, said.String())
	}
}

// troubledTokens answers every read with GitHub's trouble that also reads as a narrowing
// refusal, which a rate-limited 422 is: the lease a caller is refused that way says nothing
// about which repositories the installation reaches.
type troubledTokens struct{ ownerTokens }

func (troubledTokens) ReadToken(context.Context, AppRole, string, []string) (Lease, error) {
	return Lease{}, &TransientError{Err: fmt.Errorf("%w: GitHub App token exchange failed (422)", ErrNarrowingRefused)}
}

// A read refused with GitHub's own trouble leaves no directory out, whatever else the refusal
// reads as: the bundle fails, so the caller tries again, where leaving a directory out would
// record an owner as unreachable on the strength of a rate limit (dirEntry, NarrowingRefused).
func TestGitHubsTroubleThatReadsAsARefusalFailsTheBundleAndLeavesNothingOut(t *testing.T) {
	log, said := bundleLog()
	left := NewLeftOut()
	repos := projectRepos("acme/gadgets", "acme/widgets", "sjawhar/legion")
	credential := GitHubCredential(troubledTokens{}, repos, ghrepo.Renames{}, left, log)

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	var trouble *TransientError
	if !errors.As(err, &trouble) {
		t.Fatalf("the implementer's bundle under GitHub's trouble = %v, %v; want the trouble, so the caller asks again", bundle, err)
	}
	if said.Len() != 0 {
		t.Errorf("the log holds a left-out line for a refusal that is GitHub's trouble:\n%s", said.String())
	}
	dirs := GitHubDirs(repos, ghrepo.Renames{}, left, Roles, ghrepo.MustParse("sjawhar/legion"))
	for _, want := range []string{"acme", "sjawhar"} {
		if !slices.Contains(dirs, want) {
			t.Errorf("the directories a bundle would hold are %v, want %s among them: nothing is recorded left out", dirs, want)
		}
	}
}
