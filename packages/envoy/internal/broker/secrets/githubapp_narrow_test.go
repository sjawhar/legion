package secrets_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// tokenRequest is one POST .../access_tokens the stub GitHub received: the installation it named,
// the body it carried ("" for none) and its Content-Type.
type tokenRequest struct {
	installation string
	body         string
	contentType  string
}

// narrowingGitHub is a stub GitHub with the App installed on Acme-Org (1), ada (2) and outsider
// (3). It records every request it receives and every token request's body.
type narrowingGitHub struct {
	server *httptest.Server

	mu       sync.Mutex
	requests int
	tokens   []tokenRequest
}

func newNarrowingGitHub(t *testing.T) *narrowingGitHub {
	t.Helper()
	g := &narrowingGitHub{}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.requests++
		g.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "account": map[string]string{"login": "Acme-Org"}},
				{"id": 2, "account": map[string]string{"login": "ada"}},
				{"id": 3, "account": map[string]string{"login": "outsider"}},
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read token request body: %v", err)
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			g.mu.Lock()
			g.tokens = append(g.tokens, tokenRequest{installation: id, body: string(body), contentType: r.Header.Get("Content-Type")})
			g.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "token-" + id, "expires_at": "2026-10-23T13:00:00Z"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(g.server.Close)
	return g
}

// take returns how many requests the stub received and the token requests among them since the
// last take, and forgets them.
func (g *narrowingGitHub) take() (int, []tokenRequest) {
	g.mu.Lock()
	defer g.mu.Unlock()
	requests, tokens := g.requests, g.tokens
	g.requests, g.tokens = 0, nil
	return requests, tokens
}

// TestGitHubAppMintSendsTheNarrowingAsTheAccessTokensBody: a narrowed release asks GitHub for one
// token, for the installation of the narrowing's owner alone, with GitHub's access_tokens body
// naming the repositories and, for read-only, contents: read and metadata: read; every other listed
// owner gets no token request. A release with no narrowing is today's: every listed owner's token,
// each asked for with no body.
func TestGitHubAppMintSendsTheNarrowingAsTheAccessTokensBody(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)

	for _, tc := range []struct {
		name      string
		narrowing *secrets.GitHubNarrowing
		owners    []string       // the installations Mint returns
		minted    []string       // the installations asked for a token
		body      map[string]any // each token request's decoded body, nil for none
	}{
		{
			name:      "repositories and read-only",
			narrowing: &secrets.GitHubNarrowing{Owner: "acme-org", Repositories: []string{"proofs", "widgets"}, ReadOnly: true},
			owners:    []string{"Acme-Org"},
			minted:    []string{"1"},
			body: map[string]any{
				"repositories": []any{"proofs", "widgets"},
				"permissions":  map[string]any{"contents": "read", "metadata": "read"},
			},
		},
		{
			name:      "repositories alone",
			narrowing: &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"lean"}},
			owners:    []string{"ada"},
			minted:    []string{"2"},
			body:      map[string]any{"repositories": []any{"lean"}},
		},
		{
			name:      "read-only alone",
			narrowing: &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true},
			owners:    []string{"ada"},
			minted:    []string{"2"},
			body:      map[string]any{"permissions": map[string]any{"contents": "read", "metadata": "read"}},
		},
		{
			name:      "the owner alone",
			narrowing: &secrets.GitHubNarrowing{Owner: "ada"},
			owners:    []string{"ada"},
			minted:    []string{"2"},
		},
		{
			name:   "no narrowing",
			owners: []string{"Acme-Org", "ada"},
			minted: []string{"1", "2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: tc.narrowing})
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			_, tokens := github.take()
			var got mintedGitHubApp
			if err := json.Unmarshal([]byte(release), &got); err != nil {
				t.Fatalf("Mint output: %v", err)
			}
			if len(got.Installations) != len(tc.owners) {
				t.Fatalf("Mint installations = %v, want %v alone", got.Installations, tc.owners)
			}
			for _, owner := range tc.owners {
				if _, ok := got.Installations[owner]; !ok {
					t.Fatalf("Mint installations = %v, want %v alone", got.Installations, tc.owners)
				}
			}
			var minted []string
			for _, token := range tokens {
				minted = append(minted, token.installation)
				if tc.body == nil {
					if token.body != "" {
						t.Fatalf("token request for installation %s carried %q, want no body", token.installation, token.body)
					}
					continue
				}
				var body map[string]any
				if err := json.Unmarshal([]byte(token.body), &body); err != nil {
					t.Fatalf("token request body %q: %v", token.body, err)
				}
				if !reflect.DeepEqual(body, tc.body) {
					t.Fatalf("token request body = %v, want %v", body, tc.body)
				}
				if token.contentType != "application/json" {
					t.Fatalf("token request Content-Type = %q, want application/json", token.contentType)
				}
			}
			if !reflect.DeepEqual(minted, tc.minted) {
				t.Fatalf("token requests for installations %v, want %v", minted, tc.minted)
			}
		})
	}
}

// TestGitHubAppMintRefusesANarrowingToAnUncoveredOwner: a narrowing names an account github-owners
// lists or it is refused, without a request to GitHub, even where the App is installed on that
// account; and a listed account the App is not installed on mints nothing.
func TestGitHubAppMintRefusesANarrowingToAnUncoveredOwner(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)

	_, err = app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: &secrets.GitHubNarrowing{Owner: "outsider", ReadOnly: true}})
	if secrets.MintErrorClass(err) != "refused" || !strings.Contains(err.Error(), "outsider") || !strings.Contains(err.Error(), "github-owners") {
		t.Fatalf("Mint narrowed to an unlisted owner = %v, want refused naming the owner and github-owners", err)
	}
	if requests, _ := github.take(); requests != 0 {
		t.Fatalf("GitHub received %d requests for an unlisted owner, want none", requests)
	}

	_, err = app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "grace"}, GitHubNarrowing: &secrets.GitHubNarrowing{Owner: "grace"}})
	if secrets.MintErrorClass(err) != "refused" || !strings.Contains(err.Error(), "grace") {
		t.Fatalf("Mint narrowed to a listed owner with no installation = %v, want refused naming the owner", err)
	}
	if _, tokens := github.take(); len(tokens) != 0 {
		t.Fatalf("token requests %v for an owner with no installation, want none", tokens)
	}
}
