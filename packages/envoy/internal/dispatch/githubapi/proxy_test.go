package githubapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
)

// fakeGitHub answers the App's installation lookup and token mint, and records every other read.
type fakeGitHub struct {
	mu            sync.Mutex
	installations map[string]int64
	reads         []string
	response      string
}

func (f *fakeGitHub) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		id, ok := f.installations[r.PathValue("owner")+"/"+r.PathValue("repo")]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "app_slug": "envoy", "permissions": map[string]string{"pull_requests": "read", "checks": "read"}})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_installation_" + r.PathValue("id"), "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reads = append(f.reads, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-GitHub-Request-Id", "req-1")
		if f.response != "" {
			fmt.Fprint(w, f.response)
			return
		}
		fmt.Fprint(w, `{"state":"open","title":"Ship it"}`)
	})
	return mux
}

func (f *fakeGitHub) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reads...)
}

func newProxyRig(t *testing.T) (*githubapp.Client, *fakeGitHub) {
	t.Helper()
	fake := &fakeGitHub{installations: map[string]int64{"acme/web": 11}}
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	_, pemText := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.proxy", PEM: pemText}, server.URL)
	if err != nil {
		t.Fatalf("githubapp.New: %v", err)
	}
	return client, fake
}

func proxy(client *githubapp.Client, method, target string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	ProxyREST(response, httptest.NewRequest(method, target, nil), client)
	return response
}

func errorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", response.Body.String(), err)
	}
	return body.Code
}

// The web app's three reads (a pull request or issue's state, and a commit's checks) go to GitHub
// as the App installation that covers the repository, never as the person reading.
func TestProxyReadsTheRepositoryAsItsAppInstallation(t *testing.T) {
	client, fake := newProxyRig(t)
	for _, path := range []string{
		"repos/acme/web/pulls/7",
		"repos/acme/web/issues/8",
		"repos/acme/web/commits/0123abcd/check-runs?per_page=100",
	} {
		response := proxy(client, http.MethodGet, "/api/github/rest/"+path)
		if response.Code != http.StatusOK || response.Body.String() != `{"state":"open","title":"Ship it"}` {
			t.Fatalf("GET %s: status %d body %s, want GitHub's answer", path, response.Code, response.Body.String())
		}
		if contentType := response.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
			t.Errorf("GET %s: Content-Type %q, want GitHub's", path, contentType)
		}
	}
	want := []string{
		"GET /repos/acme/web/pulls/7 Bearer ghs_installation_11",
		"GET /repos/acme/web/issues/8 Bearer ghs_installation_11",
		"GET /repos/acme/web/commits/0123abcd/check-runs?per_page=100 Bearer ghs_installation_11",
	}
	if seen := fake.seen(); strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("GitHub saw:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
}

func TestProxyRejectsAResponseLargerThanTheGitHubLimit(t *testing.T) {
	client, fake := newProxyRig(t)
	fake.response = strings.Repeat("x", 1<<20+1)

	response := proxy(client, http.MethodGet, "/api/github/rest/repos/acme/web/pulls/7")
	if response.Code != http.StatusBadGateway || errorCode(t, response) != "GITHUB_UPSTREAM" ||
		!strings.Contains(response.Body.String(), "1048576-byte limit") {
		t.Fatalf("oversized GitHub response: status %d body %s, want 502 naming the response limit", response.Code, response.Body.String())
	}
}

func TestProxyRefusesEveryMethodButGET(t *testing.T) {
	client, fake := newProxyRig(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead} {
		response := proxy(client, method, "/api/github/rest/repos/acme/web/pulls/7")
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s: status %d Allow %q, want 405 Allow GET", method, response.Code, response.Header().Get("Allow"))
		}
	}
	if seen := fake.seen(); len(seen) != 0 {
		t.Fatalf("a refused method reached GitHub: %q", seen)
	}
}

// The App can read more of a repository than the web app shows; the proxy reads only what it shows.
func TestProxyRefusesAPathTheWebAppDoesNotRead(t *testing.T) {
	client, fake := newProxyRig(t)
	for _, path := range []string{
		"user",
		"repos/acme/web",
		"repos/acme/web/contents/.env",
		"repos/acme/web/pulls/7/files",
		"repos/acme/web/pulls/x",
		"repos/acme/web/pulls/7/../../contents/.env",
		"repos/acme/../other/pulls/7",
		"graphql",
	} {
		response := proxy(client, http.MethodGet, "/api/github/rest/"+path)
		if response.Code != http.StatusNotFound || errorCode(t, response) != "GITHUB_PATH_REFUSED" {
			t.Errorf("GET %s: status %d body %s, want 404 GITHUB_PATH_REFUSED", path, response.Code, response.Body.String())
		}
	}
	if seen := fake.seen(); len(seen) != 0 {
		t.Fatalf("a refused path reached GitHub: %q", seen)
	}
}

func TestProxyWithoutTheAppIsUnavailable(t *testing.T) {
	client, _ := newProxyRig(t)
	response := proxy(client, http.MethodGet, "/api/github/rest/repos/acme/elsewhere/pulls/1")
	if response.Code != http.StatusServiceUnavailable || errorCode(t, response) != "GITHUB_TOKEN_UNAVAILABLE" {
		t.Fatalf("uninstalled repository: status %d body %s, want 503 GITHUB_TOKEN_UNAVAILABLE", response.Code, response.Body.String())
	}
	response = proxy(nil, http.MethodGet, "/api/github/rest/repos/acme/web/pulls/1")
	if response.Code != http.StatusServiceUnavailable || errorCode(t, response) != "GITHUB_TOKEN_UNAVAILABLE" {
		t.Fatalf("no App key: status %d body %s, want 503 GITHUB_TOKEN_UNAVAILABLE", response.Code, response.Body.String())
	}
}
