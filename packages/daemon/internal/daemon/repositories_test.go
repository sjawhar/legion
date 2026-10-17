package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// installations answers each App's installation on each repository, by "<role> <owner/name>";
// a repository it does not name is installed for both Apps, and fail answers an error.
type installations struct {
	mu      sync.Mutex
	missing map[string]bool
	fail    error
	asked   []string
}

func (i *installations) InstalledOn(_ context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	key := string(role) + " " + repo.String()
	i.asked = append(i.asked, key)
	if i.fail != nil {
		return false, i.fail
	}
	return !i.missing[key], nil
}

// mappingDispatch is a Dispatch whose repository mapping is mappings.
type mappingDispatch struct {
	outboxDispatch
	mu       sync.Mutex
	mappings []dispatch.RepoProject
	err      error
}

func (d *mappingDispatch) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dispatch.RepoProject{}, d.mappings...), d.err
}

func (d *mappingDispatch) set(mappings ...dispatch.RepoProject) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mappings = mappings
}

// The project's repositories are Dispatch's mapping of the project, each served only once both
// Apps are installed on it: a repository missing an App is left out with the App named, the
// owners are the served repositories', lowercased and sorted, and a served repository is held as
// GitHub names it, at its default branch.
func TestTheMappingServesOnlyRepositoriesBothAppsAreInstalledOn(t *testing.T) {
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@release/v7.1", "zeta/tools": "zeta/tools@main"})
	repos := newRepositories("LEGION")
	installs := &installations{missing: map[string]bool{"review acme/gadgets": true}}
	client := &mappingDispatch{mappings: []dispatch.RepoProject{
		{Repo: "acme/widgets", Project: "LEGION", Default: true},
		{Repo: "acme/gadgets", Project: "LEGION"},
		{Repo: "zeta/tools", Project: "LEGION"},
		{Repo: "other/thing", Project: "OTHER", Default: true},
	}}
	branches := newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now)

	accepted, refused, err := readMapping(context.Background(), client, "LEGION", installs)
	if err != nil {
		t.Fatalf("readMapping: %v", err)
	}
	if accepted, err = nameAccepted(context.Background(), accepted, branches); err != nil {
		t.Fatalf("nameAccepted: %v", err)
	}
	repos.set(accepted, refused)
	if got, want := repos.Owners(), []string{"acme", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Owners = %v, want %v", got, want)
	}
	if got := repos.Accepted(); len(got) != 2 || got[0] != ghrepo.MustParse("Acme/Widgets") || got[1] != ghrepo.MustParse("zeta/tools") {
		t.Fatalf("Accepted = %v, want Acme/Widgets and zeta/tools", got)
	}
	if got, want := repos.Refused(), []refusedRepository{{Repo: "acme/gadgets", Reason: "the review App is not installed on acme/gadgets"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Refused = %#v, want %#v", got, want)
	}
	if mapped := repos.Mapped(); len(mapped) != 2 || !mapped[0].Default || mapped[1].Default {
		t.Fatalf("Mapped = %#v, want Acme/Widgets the default and zeta/tools not", mapped)
	}

	for _, tc := range []struct {
		resolved string
		want     workflow.Resolution
	}{
		{"acme/widgets", workflow.Resolution{Repo: "Acme/Widgets", DefaultBranch: "release/v7.1", GitHubRepo: "Acme/Widgets"}},
		{"Acme/Widgets", workflow.Resolution{Repo: "Acme/Widgets", DefaultBranch: "release/v7.1", GitHubRepo: "Acme/Widgets"}},
		{"acme/gadgets", workflow.Resolution{Refusal: "the review App is not installed on acme/gadgets", LeftOut: true}},
		{"other/thing", workflow.Resolution{Refusal: "other/thing is not mapped to project LEGION in Dispatch"}},
	} {
		got, err := (&workflowRuntime{repositories: repos, defaultBranches: branches}).ResolveRepository(tc.resolved)
		if err != nil || got != tc.want {
			t.Errorf("ResolveRepository(%s) = %#v, %v; want %#v", tc.resolved, got, err, tc.want)
		}
	}
}

// A repository both Apps lack names both; an installation read that fails is no answer, and fails
// the whole read rather than leaving the repository out on a guess.
func TestTheMappingNamesEveryMissingAppAndFailsOnAFailedRead(t *testing.T) {
	client := &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/gadgets", Project: "LEGION"}}}
	installs := &installations{missing: map[string]bool{"implement acme/gadgets": true, "review acme/gadgets": true}}
	accepted, refused, err := readMapping(context.Background(), client, "LEGION", installs)
	if err != nil || len(accepted) != 0 || len(refused) != 1 || refused[0].Reason != "the implement and review Apps are not installed on acme/gadgets" {
		t.Fatalf("readMapping = %#v, %#v, %v; want acme/gadgets left out naming both Apps", accepted, refused, err)
	}

	installs = &installations{fail: &appauth.TransientError{Err: errors.New("GitHub answered 502")}}
	if _, _, err := readMapping(context.Background(), client, "LEGION", installs); err == nil || !strings.Contains(err.Error(), "acme/gadgets") {
		t.Fatalf("readMapping with a failed installation read = %v, want an error naming acme/gadgets", err)
	}
}

// The boot refuses only a project with no repository mapped. A mapping whose every repository
// lacks an App boots serving none, each left out and reported; a mapping with one served
// repository and one left out boots serving the one.
func TestTheBootRefusesOnlyAnEmptyMapping(t *testing.T) {
	for _, tc := range []struct {
		name            string
		mappings        []dispatch.RepoProject
		missing         map[string]bool
		want            string
		served, leftOut int
	}{
		{name: "no repository mapped", mappings: []dispatch.RepoProject{{Repo: "other/thing", Project: "OTHER"}},
			want: "project LEGION has no repository mapped in Dispatch (Settings → Repositories → Projects)"},
		{name: "no repository with both Apps", mappings: []dispatch.RepoProject{{Repo: "acme/gadgets", Project: "LEGION"}},
			missing: map[string]bool{"review acme/gadgets": true}, leftOut: 1},
		{name: "one served, one left out", mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION"}, {Repo: "acme/gadgets", Project: "LEGION"}},
			missing: map[string]bool{"review acme/gadgets": true}, served: 1, leftOut: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &workflowRuntime{dispatch: &mappingDispatch{mappings: tc.mappings}, dispatchProject: "LEGION", installs: &installations{missing: tc.missing},
				repositories: newRepositories("LEGION"), log: quietLogger()}
			err := w.loadMapping(context.Background())
			if tc.want != "" {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("loadMapping = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil || len(w.repositories.Accepted()) != tc.served || len(w.repositories.Refused()) != tc.leftOut {
				t.Fatalf("loadMapping = %v, served %v, left out %v; want it to boot serving %d with %d left out",
					err, w.repositories.Accepted(), w.repositories.Refused(), tc.served, tc.leftOut)
			}
		})
	}
}

// namedGitHub answers GET /repos/{owner}/{repo} for each repository it names, as
// "<GitHub's spelling>@<default branch>", or as "301 <owner>/<name>" for one GitHub renamed, which
// it answers as GitHub does: a 301 to the new name's path.
type namedGitHub struct{ url string }

func newNamedGitHub(t *testing.T, repos map[string]string) *namedGitHub {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/")
		named, ok := repos[strings.ToLower(path)]
		if r.Method != http.MethodGet || !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		if renamed, moved := strings.CutPrefix(named, "301 "); moved {
			http.Redirect(w, r, "/repos/"+renamed, http.StatusMovedPermanently)
			return
		}
		name, branch, _ := strings.Cut(named, "@")
		_, _ = w.Write([]byte(`{"full_name":"` + name + `","default_branch":"` + branch + `"}`))
	}))
	t.Cleanup(server.Close)
	return &namedGitHub{url: server.URL}
}
