package ghbranch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

const mainCommit = "c0ffee0123456789abcdef0123456789abcdef01"
const strippedTreeSHA = "beefc0de0123456789abcdef0123456789abcdef"
const strippedCommitSHA = "decaf00d0123456789abcdef0123456789abcdef"

// github is a repository's REST API as Create calls it: main's ref, the top-level tree of a commit
// (treeEntries, keyed by commit sha; a sha with no entry answers an empty tree, the shape of a main
// with no .legion/), a create of a tree (answered with strippedTreeSHA) and of a commit (answered
// with strippedCommitSHA), and a create of a ref answered with create's status and body. It records
// each create and each strip (tree post, commit post) it is asked for.
type github struct {
	create      func() (int, string)
	treeEntries map[string][]map[string]string
	mu          sync.Mutex
	creates     []map[string]string
	treePosts   []map[string]any
	commitPosts []map[string]any
	authorized  []string
}

func (g *github) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.authorized = append(g.authorized, r.Header.Get("Authorization"))
		g.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/git/ref/heads/main":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"`+mainCommit+`","type":"commit"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/git/trees/"):
			sha := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/git/trees/")
			encoded, err := json.Marshal(map[string]any{"sha": sha, "tree": g.treeEntries[sha]})
			if err != nil {
				t.Fatalf("encode the tree of %s: %v", sha, err)
			}
			_, _ = w.Write(encoded)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/git/trees":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode the tree post: %v", err)
			}
			g.mu.Lock()
			g.treePosts = append(g.treePosts, body)
			g.mu.Unlock()
			_, _ = io.WriteString(w, `{"sha":"`+strippedTreeSHA+`"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/git/commits":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode the commit post: %v", err)
			}
			g.mu.Lock()
			g.commitPosts = append(g.commitPosts, body)
			g.mu.Unlock()
			_, _ = io.WriteString(w, `{"sha":"`+strippedCommitSHA+`"}`)
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

func TestCreateMakesTheBranchAtMainOrLeavesTheOneGitHubHas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "created", status: http.StatusCreated, body: `{"ref":"refs/heads/legion/WIDGETS-12","object":{"sha":"` + mainCommit + `"}}`},
		{name: "already exists", status: http.StatusUnprocessableEntity,
			body: `{"message":"Reference already exists","documentation_url":"https://docs.github.com/rest/git/refs#create-a-reference","status":"422"}`},
		{name: "refused", status: http.StatusForbidden,
			body: `{"message":"Resource not accessible by integration","status":"403"}`,
			want: `create refs/heads/legion/WIDGETS-12 on acme/widgets at ` + mainCommit +
				`: GitHub answered POST /git/refs with 403: {"message":"Resource not accessible by integration","status":"403"}`},
		{name: "another 422", status: http.StatusUnprocessableEntity,
			body: `{"message":"Reference update failed","status":"422"}`,
			want: `create refs/heads/legion/WIDGETS-12 on acme/widgets at ` + mainCommit +
				`: GitHub answered POST /git/refs with 422: {"message":"Reference update failed","status":"422"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{create: func() (int, string) { return tc.status, tc.body }}
			api := g.serve(t)

			err := Create(context.Background(), widgets(api), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12")

			if got := errorText(err); got != tc.want {
				t.Fatalf("Create error = %q, want %q", got, tc.want)
			}
			want := map[string]string{"ref": "refs/heads/legion/WIDGETS-12", "sha": mainCommit}
			if len(g.creates) != 1 || len(g.creates[0]) != 2 || g.creates[0]["ref"] != want["ref"] || g.creates[0]["sha"] != want["sha"] {
				t.Fatalf("creates = %v, want one of %v", g.creates, want)
			}
			if len(g.treePosts) != 0 || len(g.commitPosts) != 0 {
				t.Fatalf("a main with no .legion/ posted a strip: trees %v, commits %v", g.treePosts, g.commitPosts)
			}
			for _, authorization := range g.authorized {
				if authorization != "Bearer installation-token" {
					t.Fatalf("a request authorized as %q, want the installation token", authorization)
				}
			}
		})
	}
}

// Before this branch exists, nothing has run any role of its issue, so a .legion/ directory
// already on main holds only another issue's merged handoffs. Create strips it with a single
// git-tree-API deletion against base_tree main, commits that tree on main, and branches at the new
// commit, never at main's own. On today's main (before this change), Create never reads the tree at
// all and branches directly at mainCommit, so this is the regression this fix closes.
func TestCreateStripsLegionFromMainBeforeBranching(t *testing.T) {
	g := &github{
		create: func() (int, string) { return http.StatusCreated, `{}` },
		treeEntries: map[string][]map[string]string{
			mainCommit: {
				{"path": ".legion", "mode": "040000", "type": "tree", "sha": "oldlegiontree0123456789abcdef0123456789a"},
				{"path": "README.md", "mode": "100644", "type": "blob", "sha": "readmeblob0123456789abcdef0123456789abcd"},
			},
		},
	}
	api := g.serve(t)

	if err := Create(context.Background(), widgets(api), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12"); err != nil {
		t.Fatalf("Create error = %v, want none", err)
	}

	if len(g.treePosts) != 1 {
		t.Fatalf("tree posts = %v, want exactly one", g.treePosts)
	}
	tree := g.treePosts[0]
	if tree["base_tree"] != mainCommit {
		t.Fatalf("tree post base_tree = %v, want %s", tree["base_tree"], mainCommit)
	}
	entries, _ := tree["tree"].([]any)
	if len(entries) != 1 {
		t.Fatalf("tree post entries = %v, want exactly one deletion", entries)
	}
	deletion, _ := entries[0].(map[string]any)
	if deletion["path"] != ".legion" || deletion["sha"] != nil {
		t.Fatalf("tree post entry = %v, want {path: .legion, sha: null}", deletion)
	}

	if len(g.commitPosts) != 1 {
		t.Fatalf("commit posts = %v, want exactly one", g.commitPosts)
	}
	commit := g.commitPosts[0]
	if commit["tree"] != strippedTreeSHA {
		t.Fatalf("commit post tree = %v, want %s", commit["tree"], strippedTreeSHA)
	}
	if parents, _ := commit["parents"].([]any); len(parents) != 1 || parents[0] != mainCommit {
		t.Fatalf("commit post parents = %v, want [%s]", commit["parents"], mainCommit)
	}

	if len(g.creates) != 1 || g.creates[0]["sha"] != strippedCommitSHA {
		t.Fatalf("branch create = %v, want it at the stripped commit %s, not main's own %s", g.creates, strippedCommitSHA, mainCommit)
	}
}

// main with no .legion/ entry already covers the no-op case
// (TestCreateMakesTheBranchAtMainOrLeavesTheOneGitHubHas asserts zero tree/commit posts); this
// covers the tree read failing.
func TestCreateRefusesARepositoryWhoseTopLevelTreeItCannotRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/git/ref/heads/main":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"`+mainCommit+`","type":"commit"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/git/trees/"+mainCommit:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found","status":"404"}`+"\n")
		default:
			t.Errorf("unexpected %s %s after the tree could not be read", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	err := Create(context.Background(), widgets(server.URL), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12")

	if want := `read the top-level tree of acme/widgets at ` + mainCommit +
		`: GitHub answered GET /git/trees/` + mainCommit + ` with 404: {"message":"Not Found","status":"404"}`; errorText(err) != want {
		t.Fatalf("Create error = %q, want %q", errorText(err), want)
	}
}

// A main GitHub does not show is refused before anything is created, naming its answer.
func TestCreateRefusesARepositoryWhoseMainItCannotRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s after main could not be read", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found","status":"404"}`+"\n")
	}))
	t.Cleanup(server.Close)

	err := Create(context.Background(), widgets(server.URL), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12")

	if want := `read refs/heads/main of acme/widgets: GitHub answered GET /git/ref/heads/main with 404: {"message":"Not Found","status":"404"}`; errorText(err) != want {
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
