package ghbranch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

const mainCommit = "c0ffee0123456789abcdef0123456789abcdef01"

// github is a repository's REST API as Create calls it: main's ref, and a create answered with
// create's status and body. It records each create it is asked for.
type github struct {
	create     func() (int, string)
	mu         sync.Mutex
	creates    []map[string]string
	authorized []string
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
				`: GitHub answered 403: {"message":"Resource not accessible by integration","status":"403"}`},
		{name: "another 422", status: http.StatusUnprocessableEntity,
			body: `{"message":"Reference update failed","status":"422"}`,
			want: `create refs/heads/legion/WIDGETS-12 on acme/widgets at ` + mainCommit +
				`: GitHub answered 422: {"message":"Reference update failed","status":"422"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{create: func() (int, string) { return tc.status, tc.body }}
			api := g.serve(t)

			err := Create(context.Background(), http.DefaultClient, api, "installation-token", ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", "main")

			if got := errorText(err); got != tc.want {
				t.Fatalf("Create error = %q, want %q", got, tc.want)
			}
			want := map[string]string{"ref": "refs/heads/legion/WIDGETS-12", "sha": mainCommit}
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

	err := Create(context.Background(), http.DefaultClient, server.URL, "installation-token", ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", "main")

	if want := `read refs/heads/main of acme/widgets: GitHub answered 404: {"message":"Not Found","status":"404"}`; errorText(err) != want {
		t.Fatalf("Create error = %q, want %q", errorText(err), want)
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
