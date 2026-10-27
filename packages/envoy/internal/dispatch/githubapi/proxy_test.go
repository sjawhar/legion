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
		if strings.HasSuffix(r.URL.Path, "/actions/runs") {
			fmt.Fprint(w, `{"total_count":1,"workflow_runs":[{"id":5,"check_suite_id":6,"event":"push","name":"CI"}]}`)
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

// headSHA is a full commit sha, the only commit a workflow-runs read may name.
const headSHA = "0123456789abcdef0123456789abcdef01234567"

// The web app's reads (a pull request or issue's state, a commit's checks and the workflow runs of
// that commit) go to GitHub as the App installation that covers the repository, never as the
// person reading.
func TestProxyReadsTheRepositoryAsItsAppInstallation(t *testing.T) {
	client, fake := newProxyRig(t)
	for _, path := range []string{
		"repos/acme/web/pulls/7",
		"repos/acme/web/issues/8",
		"repos/acme/web/commits/0123abcd/check-runs?per_page=100",
		"repos/acme/web/actions/runs?head_sha=" + headSHA,
		"repos/acme/web/actions/runs?head_sha=" + headSHA + "&per_page=100&page=2",
		"repos/acme/web/actions/runs?page=3&head_sha=" + headSHA,
	} {
		response := proxy(client, http.MethodGet, "/api/github/rest/"+path)
		wantBody, wantContentType := `{"state":"open","title":"Ship it"}`, "application/json; charset=utf-8"
		if strings.Contains(path, "/actions/runs?") {
			wantBody, wantContentType = `{"total_count":1,"workflow_runs":[{"check_suite_id":6,"event":"push","name":"CI"}]}`+"\n", "application/json"
		}
		if response.Code != http.StatusOK || response.Body.String() != wantBody {
			t.Fatalf("GET %s: status %d body %s, want GitHub's answer", path, response.Code, response.Body.String())
		}
		if contentType := response.Header().Get("Content-Type"); contentType != wantContentType {
			t.Errorf("GET %s: Content-Type %q, want %q", path, contentType, wantContentType)
		}
	}
	want := []string{
		"GET /repos/acme/web/pulls/7 Bearer ghs_installation_11",
		"GET /repos/acme/web/issues/8 Bearer ghs_installation_11",
		"GET /repos/acme/web/commits/0123abcd/check-runs?per_page=100 Bearer ghs_installation_11",
		"GET /repos/acme/web/actions/runs?head_sha=" + headSHA + " Bearer ghs_installation_11",
		"GET /repos/acme/web/actions/runs?head_sha=" + headSHA + "&per_page=100&page=2 Bearer ghs_installation_11",
		"GET /repos/acme/web/actions/runs?page=3&head_sha=" + headSHA + " Bearer ghs_installation_11",
	}
	if seen := fake.seen(); strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("GitHub saw:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
}

// A workflow run carries its head commit's message and author, the actor and the repository; the
// web app reads only each run's check suite, event and workflow name, and the proxy passes on
// nothing else.
func TestProxyPassesOnOnlyTheWorkflowRunFieldsTheWebAppReads(t *testing.T) {
	client, fake := newProxyRig(t)
	fake.response = `{"total_count":2,"workflow_runs":[` +
		`{"id":901,"check_suite_id":7001,"event":"pull_request","name":"CI","status":"completed","conclusion":"success",` +
		`"head_sha":"` + headSHA + `","head_branch":"feature","html_url":"https://github.com/acme/web/actions/runs/901",` +
		`"head_commit":{"id":"` + headSHA + `","message":"Rotate the staging credentials","author":{"name":"Pat Doe","email":"pat.doe@example.com"}},` +
		`"repository":{"id":42,"full_name":"acme/web","private":true},"actor":{"login":"patdoe","id":77}},` +
		`{"id":902,"check_suite_id":7002,"event":"push","name":"Deploy","run_attempt":2,"workflow_id":31,` +
		`"head_commit":{"message":"Rotate the staging credentials","author":{"email":"pat.doe@example.com"}},` +
		`"repository":{"full_name":"acme/web"},"actor":{"login":"patdoe"}}]}`

	response := proxy(client, http.MethodGet, "/api/github/rest/repos/acme/web/actions/runs?head_sha="+headSHA)
	want := `{"total_count":2,"workflow_runs":[{"check_suite_id":7001,"event":"pull_request","name":"CI"},{"check_suite_id":7002,"event":"push","name":"Deploy"}]}` + "\n"
	if response.Code != http.StatusOK || response.Body.String() != want {
		t.Fatalf("workflow runs: status %d body %s, want only the fields the web app reads:\n%s", response.Code, response.Body.String(), want)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("workflow runs: Content-Type %q, want application/json", contentType)
	}
	for _, extra := range []string{"pat.doe@example.com", "Rotate the staging credentials", "head_commit", "repository", "actor", "patdoe", "html_url", "901"} {
		if strings.Contains(response.Body.String(), extra) {
			t.Errorf("workflow runs: body %s carries %q", response.Body.String(), extra)
		}
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

// A full page of 100 workflow runs is more than the 1 MiB a pull request's read may be, since
// GitHub writes each run's repository and head commit; the proxy reads it whole and trims it.
func TestProxyTrimsAPageOfWorkflowRunsLargerThanTheGeneralLimit(t *testing.T) {
	client, fake := newProxyRig(t)
	padding := strings.Repeat("p", 12800)
	runs := make([]string, 100)
	for i := range runs {
		runs[i] = fmt.Sprintf(`{"check_suite_id":%d,"event":"pull_request","name":"CI","head_commit":{"message":%q}}`, 7000+i, padding)
	}
	fake.response = `{"total_count":100,"workflow_runs":[` + strings.Join(runs, ",") + `]}`
	if len(fake.response) <= 1<<20 {
		t.Fatalf("fixture is %d bytes, want more than 1 MiB", len(fake.response))
	}

	response := proxy(client, http.MethodGet, "/api/github/rest/repos/acme/web/actions/runs?head_sha="+headSHA+"&per_page=100&page=1")
	var got struct {
		TotalCount   int              `json:"total_count"`
		WorkflowRuns []map[string]any `json:"workflow_runs"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil {
		t.Fatalf("a workflow-runs page over 1 MiB: status %d body %.200s, want 200 and the trimmed runs", response.Code, response.Body.String())
	}
	if got.TotalCount != 100 || len(got.WorkflowRuns) != 100 || response.Body.Len() > 10_000 {
		t.Fatalf("trimmed page: total %d, %d runs, %d bytes; want 100, 100 and a few KB", got.TotalCount, len(got.WorkflowRuns), response.Body.Len())
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
		// The workflow runs are read for one commit, by its full sha, and nothing deeper.
		"repos/acme/web/actions/runs",
		"repos/acme/web/actions/runs?per_page=100",
		"repos/acme/web/actions/runs?branch=main",
		"repos/acme/web/actions/runs?head_sha=0123abcd",
		"repos/acme/web/actions/runs?head_sha=" + headSHA + "0",
		"repos/acme/web/actions/runs?head_sha=" + headSHA + "&status=failure",
		"repos/acme/web/actions/runs?head_sha=" + headSHA + "&event=pull_request",
		"repos/acme/web/actions/runs?head_sha=" + headSHA + "&head_sha=" + headSHA,
		"repos/acme/web/actions/runs?head_sha=" + headSHA + "&page=1&page=2",
		"repos/acme/web/actions/runs?head_sha=" + headSHA + "&per_page=all",
		"repos/acme/web/actions/runs?head_sha=" + headSHA + "&&page=1",
		"repos/acme/web/actions/runs?head_sha=" + headSHA + "&",
		"repos/acme/web/actions/runs?head_sha=" + headSHA + "%26status=failure",
		"repos/acme/web/actions/runs/123",
		"repos/acme/web/actions/runs/123?head_sha=" + headSHA,
		"repos/acme/web/actions/runs/123/jobs",
		"repos/acme/web/actions/runs/123/logs",
		"repos/acme/web/actions/workflows",
		"repos/acme/web/actions/artifacts",
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
