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
// (3). It records every request it receives and every token request's body, and answers a token
// request with answer, asGitHubAnswers unless a test sets another before its first request.
type narrowingGitHub struct {
	server *httptest.Server

	mu       sync.Mutex
	requests int
	tokens   []tokenRequest
	answer   func(installation string, body []byte) (int, any)
}

func newNarrowingGitHub(t *testing.T) *narrowingGitHub {
	t.Helper()
	g := &narrowingGitHub{answer: asGitHubAnswers}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.requests++
		answer := g.answer
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
			status, reply := answer(id, body)
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(reply)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(g.server.Close)
	return g
}

// setAnswer makes the stub answer every later token request with answer.
func (g *narrowingGitHub) setAnswer(answer func(installation string, body []byte) (int, any)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answer = answer
}

// asGitHubAnswers is the 201 GitHub gives a token request with body: the permissions the body
// names, or every permission the App holds when it names none, and, when it names repositories,
// those alone with the selection "selected"; with none, the installation's own selection, "all".
func asGitHubAnswers(installation string, body []byte) (int, any) {
	var asked struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	_ = json.Unmarshal(body, &asked)
	answer := map[string]any{
		"token": "token-" + installation, "expires_at": "2026-10-23T13:00:00Z", "repository_selection": "all",
		"permissions": map[string]string{"administration": "write", "contents": "write", "metadata": "read"},
	}
	if asked.Permissions != nil {
		answer["permissions"] = asked.Permissions
	}
	if len(asked.Repositories) > 0 {
		var repositories []map[string]string
		for _, name := range asked.Repositories {
			repositories = append(repositories, map[string]string{"name": name, "full_name": "owner/" + name})
		}
		answer["repository_selection"] = "selected"
		answer["repositories"] = repositories
	}
	return http.StatusCreated, answer
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
// lists or it is refused as the caller's narrowing (class narrowing, not the App's refused),
// without a request to GitHub, even where the App is installed on that account; and a listed
// account the App is not installed on mints nothing.
func TestGitHubAppMintRefusesANarrowingToAnUncoveredOwner(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)

	_, err = app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: &secrets.GitHubNarrowing{Owner: "outsider", ReadOnly: true}})
	if secrets.MintErrorClass(err) != "narrowing" || !strings.Contains(err.Error(), "outsider") || !strings.Contains(err.Error(), "github-owners") {
		t.Fatalf("Mint narrowed to an unlisted owner = %v (class %s), want class narrowing naming the owner and github-owners", err, secrets.MintErrorClass(err))
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

// TestGitHubAppMintNamesTheNarrowingWhenGitHubRefusesIt: GitHub's 422 to a narrowed token request
// is the caller's narrowing (class narrowing), and the error names what it likely got wrong: the
// repositories it names, or the read access it asks for. A 422 to a request that sent no body (no
// narrowing, or an owner alone) narrowed nothing and stays the App's (class refused). No error
// quotes GitHub's own text.
func TestGitHubAppMintNamesTheNarrowingWhenGitHubRefusesIt(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	github.setAnswer(func(string, []byte) (int, any) {
		return http.StatusUnprocessableEntity, map[string]string{"message": "There is at least one repository that does not exist or is not accessible to the parent installation."}
	})
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)

	for _, tc := range []struct {
		name      string
		narrowing *secrets.GitHubNarrowing
		class     string
		mentions  []string
	}{
		{"repositories", &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"proofs", "widgets"}}, "narrowing", []string{"422", "ada", "proofs, widgets"}},
		{"read-only", &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true}, "narrowing", []string{"422", "ada", "contents: read"}},
		{"the owner alone", &secrets.GitHubNarrowing{Owner: "ada"}, "refused", []string{"422"}},
		{"no narrowing", nil, "refused", []string{"422"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: tc.narrowing})
			if secrets.MintErrorClass(err) != tc.class {
				t.Fatalf("Mint = %v (class %s), want class %s", err, secrets.MintErrorClass(err), tc.class)
			}
			for _, want := range tc.mentions {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Mint = %v, want it to name %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "parent installation") {
				t.Fatalf("Mint = %v quotes GitHub's own text", err)
			}
		})
	}
}

// TestGitHubAppMintRefusesATokenWiderThanTheNarrowing: Mint releases a narrowed token only when
// GitHub's own answer describes no more than the narrowing asked: for read-only, no permission
// past contents: read and metadata: read and not none named; for repositories, a selection of
// those alone, named. Anything else (a body something between dropped or widened) is refused as
// GitHub's fault (class upstream), the token in no error. A token not narrowed is not checked, and
// a repository GitHub names in another case than asked is the same repository.
func TestGitHubAppMintRefusesATokenWiderThanTheNarrowing(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)
	readOnly := &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true}
	proofs := &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"proofs"}}
	answering := func(change func(map[string]any)) func(string, []byte) (int, any) {
		return func(installation string, body []byte) (int, any) {
			status, answer := asGitHubAnswers(installation, body)
			change(answer.(map[string]any))
			return status, answer
		}
	}

	for _, tc := range []struct {
		name      string
		narrowing *secrets.GitHubNarrowing
		change    func(map[string]any)
		mentions  string
	}{
		{"write contents", readOnly, func(a map[string]any) { a["permissions"] = map[string]string{"contents": "write", "metadata": "read"} }, "contents: write"},
		{"another permission", readOnly, func(a map[string]any) {
			a["permissions"] = map[string]string{"administration": "write", "contents": "read", "metadata": "read"}
		}, "administration: write"},
		{"no permissions named", readOnly, func(a map[string]any) { delete(a, "permissions") }, "permissions"},
		{"every repository", proofs, func(a map[string]any) { a["repository_selection"] = "all"; delete(a, "repositories") }, "all"},
		{"another repository", proofs, func(a map[string]any) {
			a["repositories"] = []map[string]string{{"name": "proofs"}, {"name": "secrets"}}
		}, "secrets"},
		{"no repositories named", proofs, func(a map[string]any) { delete(a, "repositories") }, "repositories"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github.setAnswer(answering(tc.change))
			release, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: tc.narrowing})
			if release != "" || secrets.MintErrorClass(err) != "upstream" || !strings.Contains(err.Error(), tc.mentions) || strings.Contains(err.Error(), "token-") {
				t.Fatalf("Mint = %q, %v (class %s), want nothing released, class upstream naming %q and no token", release, err, secrets.MintErrorClass(err), tc.mentions)
			}
		})
	}

	github.setAnswer(answering(func(a map[string]any) { a["repositories"] = []map[string]string{{"name": "Proofs"}} }))
	if release, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubNarrowing: proofs}); err != nil || !strings.Contains(release, "token-2") {
		t.Fatalf("Mint with GitHub naming the repository in its own case = %q, %v; want ada's token", release, err)
	}
	github.setAnswer(asGitHubAnswers)
	if release, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}}); err != nil || !strings.Contains(release, "token-1") || !strings.Contains(release, "token-2") {
		t.Fatalf("Mint with no narrowing and GitHub's whole answer = %q, %v; want every listed owner's token", release, err)
	}
}
