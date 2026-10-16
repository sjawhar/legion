package ghbranch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

const baseCommit = "c0ffee0123456789abcdef0123456789abcdef01"

// github is a repository's REST API as Create and DefaultBranch call it: the repository itself,
// naming defaultBranch, each branch of refs by its exact name, and a create of a ref answered with
// create's status and body. It records each read and each create; any other request fails the
// test, so a Create that reads the base's tree, or posts a tree or a commit of its own, is caught
// by name.
type github struct {
	refs          map[string]string
	defaultBranch string
	create        func() (int, string)
	mu            sync.Mutex
	read          []string
	creates       []map[string]string
	authorized    []string
}

func (g *github) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.authorized = append(g.authorized, r.Header.Get("Authorization"))
		g.mu.Unlock()
		branch, isRef := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/git/ref/heads/")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets":
			g.reading(r.URL.Path)
			_, _ = io.WriteString(w, `{"full_name":"acme/widgets","default_branch":"`+g.defaultBranch+`"}`)
		case r.Method == http.MethodGet && isRef:
			g.reading(r.URL.Path)
			commit, found := g.refs[branch]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Not Found","status":"404"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ref":"refs/heads/`+branch+`","object":{"sha":"`+commit+`","type":"commit"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/git/refs":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode the create: %v", err)
			}
			g.mu.Lock()
			g.creates = append(g.creates, body)
			g.mu.Unlock()
			status, answer := g.create()
			w.WriteHeader(status)
			_, _ = io.WriteString(w, answer)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func (g *github) reading(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.read = append(g.read, path)
}

// reads is every path the fake answered a GET for, in order.
func (g *github) reads() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.read)
}

// The branch starts at the base branch the caller names, whatever its name: a default branch with a
// slash reads as one ref path, and the create names its commit.
func TestCreateMakesTheBranchAtTheNamedBase(t *testing.T) {
	for _, base := range []string{"main", "develop", "release/v7.1"} {
		t.Run(base, func(t *testing.T) {
			g := &github{refs: map[string]string{base: baseCommit}, create: func() (int, string) { return http.StatusCreated, `{}` }}
			if err := Create(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", base); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got := g.reads(); !slices.Equal(got, []string{"/repos/acme/widgets/git/ref/heads/" + base}) {
				t.Fatalf("read %v, want the base ref alone", got)
			}
			if got := g.creates[0]["sha"]; got != baseCommit {
				t.Fatalf("created at %s, want %s", got, baseCommit)
			}
		})
	}
}

const decoyCommit = "dec0de0123456789abcdef0123456789abcdef02"

// A branch name git accepts (`git check-ref-format --branch`) may hold characters a URL reads as
// its own syntax: `#` starts a fragment and `%` an escape. The base is read as that one ref and no
// other, so a ref those characters would turn it into, at another commit (decoy), is never where
// the branch starts, and a base the URL cannot carry unescaped is read all the same.
func TestCreateReadsABaseWhoseNameHoldsURLSyntaxAsItself(t *testing.T) {
	for _, tc := range []struct{ base, decoy string }{
		{"release#stable", "release"},
		{"release%2Fstable", "release/stable"},
		{"release%stable", "release"},
		{"hotfix/#12", "hotfix/"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			g := &github{refs: map[string]string{tc.base: baseCommit, tc.decoy: decoyCommit}, create: func() (int, string) { return http.StatusCreated, `{}` }}
			if err := Create(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", tc.base); err != nil {
				t.Fatalf("Create at %q: %v", tc.base, err)
			}
			if got := g.reads(); !slices.Equal(got, []string{"/repos/acme/widgets/git/ref/heads/" + tc.base}) {
				t.Fatalf("read %q, want the ref %q alone", got, tc.base)
			}
			if got := g.creates[0]["sha"]; got != baseCommit {
				t.Fatalf("created at %s, want %q's %s, not %q's %s", got, tc.base, baseCommit, tc.decoy, decoyCommit)
			}
		})
	}
}

// A base GitHub does not have is refused naming it, before anything is created.
func TestCreateRefusesABaseGitHubDoesNotHave(t *testing.T) {
	g := &github{refs: map[string]string{"main": baseCommit}}
	err := Create(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", "develop")
	if err == nil || !strings.Contains(err.Error(), "read refs/heads/develop of acme/widgets") || len(g.creates) != 0 {
		t.Fatalf("err=%v creates=%d, want a refusal naming refs/heads/develop and no create", err, len(g.creates))
	}
}

// DefaultBranch is GitHub's default_branch for the repository, and an answer naming none is refused.
func TestDefaultBranchReadsTheRepository(t *testing.T) {
	g := &github{defaultBranch: "release/v7.1"}
	got, err := DefaultBranch(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"))
	if err != nil || got != "release/v7.1" {
		t.Fatalf("DefaultBranch = %q, %v", got, err)
	}
	g.defaultBranch = ""
	if _, err := DefaultBranch(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets")); err == nil || !strings.Contains(err.Error(), "named no default branch") {
		t.Fatalf("empty default_branch: %v, want a refusal", err)
	}
}

// A repository GitHub does not show is refused naming it and GitHub's answer.
func TestDefaultBranchRefusesARepositoryItCannotRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found","status":"404"}`+"\n")
	}))
	t.Cleanup(server.Close)
	_, err := DefaultBranch(context.Background(), widgets(server.URL), ghrepo.MustParse("acme/widgets"))
	if want := `read the default branch of acme/widgets: GitHub answered GET /repos/acme/widgets with 404: {"message":"Not Found","status":"404"}`; errorText(err) != want {
		t.Fatalf("DefaultBranch error = %q, want %q", errorText(err), want)
	}
}

// The branch starts at the base's own commit, whatever the base's tree holds: no role of the issue
// has run yet, and nothing a role reads comes from another issue's directory (each tree reads and
// writes only .legion/<its issue>/, and retro removes its own before READY, so no merge carries one
// onto the default branch; dispatch://LEGION-605). Create reads the base's ref and creates the
// branch there, and reads, posts or commits no tree: the fake answers nothing else.
func TestCreateMakesTheBranchAtTheBaseOrLeavesTheOneGitHubHas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "created", status: http.StatusCreated, body: `{"ref":"refs/heads/legion/WIDGETS-12","object":{"sha":"` + baseCommit + `"}}`},
		{name: "already exists", status: http.StatusUnprocessableEntity,
			body: `{"message":"Reference already exists","documentation_url":"https://docs.github.com/rest/git/refs#create-a-reference","status":"422"}`},
		{name: "refused", status: http.StatusForbidden,
			body: `{"message":"Resource not accessible by integration","status":"403"}`,
			want: `create refs/heads/legion/WIDGETS-12 on acme/widgets at ` + baseCommit +
				`: GitHub answered POST /git/refs with 403: {"message":"Resource not accessible by integration","status":"403"}`},
		{name: "another 422", status: http.StatusUnprocessableEntity,
			body: `{"message":"Reference update failed","status":"422"}`,
			want: `create refs/heads/legion/WIDGETS-12 on acme/widgets at ` + baseCommit +
				`: GitHub answered POST /git/refs with 422: {"message":"Reference update failed","status":"422"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{refs: map[string]string{"main": baseCommit}, create: func() (int, string) { return tc.status, tc.body }}
			api := g.serve(t)

			err := Create(context.Background(), widgets(api), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", "main")

			if got := errorText(err); got != tc.want {
				t.Fatalf("Create error = %q, want %q", got, tc.want)
			}
			want := map[string]string{"ref": "refs/heads/legion/WIDGETS-12", "sha": baseCommit}
			if len(g.creates) != 1 || len(g.creates[0]) != 2 || g.creates[0]["ref"] != want["ref"] || g.creates[0]["sha"] != want["sha"] {
				t.Fatalf("creates = %v, want one of %v", g.creates, want)
			}
			for _, authorization := range g.authorized {
				if authorization != "Bearer installation-token" {
					t.Fatalf("a request authorized as %q, want the installation token", authorization)
				}
			}
		})
	}
}

// A base GitHub does not show is refused before anything is created, naming its answer.
func TestCreateRefusesARepositoryWhoseBaseItCannotRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s after the base could not be read", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found","status":"404"}`+"\n")
	}))
	t.Cleanup(server.Close)

	err := Create(context.Background(), widgets(server.URL), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", "release/v7.1")

	if want := `read refs/heads/release/v7.1 of acme/widgets: GitHub answered GET /git/ref/heads/release/v7.1 with 404: {"message":"Not Found","status":"404"}`; errorText(err) != want {
		t.Fatalf("Create error = %q, want %q", errorText(err), want)
	}
}

// widgets is acme/widgets's REST API under api, called with the installation token.
func widgets(api string) githubrest.Client {
	return githubrest.Client{Token: "installation-token", API: githubrest.RepositoryAPI(api, ghrepo.MustParse("acme/widgets"))}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
