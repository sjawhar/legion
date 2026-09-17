package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fakeGitHubApp is the GitHub App API surface the access check walks:
// installation lookup, token mint, and the repository read that proves the
// minted token works. installations maps "owner/repo" to its Contents
// permission; a missing repository answers 404.
type fakeGitHubApp struct {
	installations map[string]string
	tokenMints    int
}

func (f *fakeGitHubApp) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		contents, ok := f.installations[r.PathValue("owner")+"/"+r.PathValue("repo")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id":          77,
			"app_slug":    "dispatch-test",
			"permissions": map[string]string{"contents": contents},
		}); err != nil {
			t.Errorf("encode installation: %v", err)
		}
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.tokenMints++
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_fake_%d", f.tokenMints),
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		}); err != nil {
			t.Errorf("encode token: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_fake_") {
			t.Errorf("repository read used %q, want an installation token", r.Header.Get("Authorization"))
		}
		if _, ok := f.installations[r.PathValue("owner")+"/"+r.PathValue("repo")]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	return mux
}

func newArchitectureSourceServer(t *testing.T, fake *fakeGitHubApp) (http.Handler, *store.Store) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	github := httptest.NewServer(fake.handler(t))
	t.Cleanup(github.Close)
	return newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", ClientSecret: "secret", PEM: pemText},
		githubAPIBase: github.URL,
	})
}

type architectureSourceResponse struct {
	Project    string  `json:"project"`
	Repo       string  `json:"repo"`
	Branch     string  `json:"branch"`
	Enabled    bool    `json:"enabled"`
	LastSyncAt *string `json:"last_sync_at"`
	LastError  *string `json:"last_error"`
}

func createTestProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key,
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestArchitectureSourcePutGetListAndDelete(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{"legion/arch": "write"}}
	handler, database := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")

	saved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/architecture-source", map[string]string{
		"repo": "Legion/Arch.git", "branch": "main",
	}, "alice")
	if saved.Code != http.StatusOK {
		t.Fatalf("put architecture source: status=%d body=%s", saved.Code, saved.Body.String())
	}
	want := architectureSourceResponse{Project: "CORE", Repo: "legion/arch", Branch: "main", Enabled: true}
	if got := decodeBody[architectureSourceResponse](t, saved); got != want {
		t.Fatalf("saved source: got %#v, want %#v", got, want)
	}
	if fake.tokenMints != 1 {
		t.Fatalf("access check minted %d tokens, want 1", fake.tokenMints)
	}
	var installationID int64
	if err := database.Pool.QueryRow(context.Background(),
		"select installation_id from architecture_sources where project_key = 'CORE'").Scan(&installationID); err != nil || installationID != 77 {
		t.Fatalf("persisted installation id: got %d, err %v, want 77", installationID, err)
	}
	if body := saved.Body.String(); strings.Contains(body, "installation_id") {
		t.Fatalf("installation_id leaked into the JSON shape: %s", body)
	}

	fetched := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/CORE/architecture-source", nil, "alice")
	if fetched.Code != http.StatusOK || decodeBody[architectureSourceResponse](t, fetched) != want {
		t.Fatalf("get architecture source: status=%d body=%s", fetched.Code, fetched.Body.String())
	}
	// The per-project GET is deliberately authAny: agents read it too.
	agentFetched := agentRequest(t, handler, http.MethodGet, "/api/v1/projects/CORE/architecture-source", nil, "agent-token")
	if agentFetched.Code != http.StatusOK {
		t.Fatalf("agent get architecture source: status=%d body=%s", agentFetched.Code, agentFetched.Body.String())
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/architecture-sources", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list architecture sources: status=%d body=%s", listed.Code, listed.Body.String())
	}
	if got := decodeBody[[]architectureSourceResponse](t, listed); len(got) != 1 || got[0] != want {
		t.Fatalf("listed sources: got %#v", got)
	}

	deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/projects/CORE/architecture-source", nil, "alice")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete architecture source: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	missing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/CORE/architecture-source", nil, "alice")
	if missing.Code != http.StatusNotFound || decodeBody[map[string]string](t, missing)["code"] != "SOURCE_NOT_FOUND" {
		t.Fatalf("get after delete: status=%d body=%s", missing.Code, missing.Body.String())
	}
	if again := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/projects/CORE/architecture-source", nil, "alice"); again.Code != http.StatusNotFound {
		t.Fatalf("delete absent source: status=%d body=%s", again.Code, again.Body.String())
	}

	rows, err := database.Pool.Query(context.Background(), `
		select coalesce(project_key, ''), payload->>'deleted' from events
		where type = 'settings.architecture_source.updated' order by id
	`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var project, deleted string
		if err := rows.Scan(&project, &deleted); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		events = append(events, project+" deleted="+deleted)
	}
	if len(events) != 2 || events[0] != "CORE deleted=false" || events[1] != "CORE deleted=true" {
		t.Fatalf("architecture source events: got %#v", events)
	}
}

func TestArchitectureSourceAccessFailuresAnswer409(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{"legion/noread": "none"}}
	handler, _ := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")

	notInstalled := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/architecture-source", map[string]string{
		"repo": "legion/missing", "branch": "main",
	}, "alice")
	body := decodeBody[map[string]string](t, notInstalled)
	if notInstalled.Code != http.StatusConflict || body["code"] != "SOURCE_ACCESS" || !strings.Contains(body["error"], "not installed on legion/missing") {
		t.Fatalf("uninstalled repository: status=%d body=%s", notInstalled.Code, notInstalled.Body.String())
	}

	noContents := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/architecture-source", map[string]string{
		"repo": "legion/noread", "branch": "main",
	}, "alice")
	body = decodeBody[map[string]string](t, noContents)
	if noContents.Code != http.StatusConflict || body["code"] != "SOURCE_ACCESS" || !strings.Contains(body["error"], "Contents: read") {
		t.Fatalf("contents none: status=%d body=%s", noContents.Code, noContents.Body.String())
	}
}

func TestArchitectureSourceWithoutAppKeyAnswers409(t *testing.T) {
	handler, _ := newTestServer(t, testServerOptions{})
	createTestProject(t, handler, "CORE")
	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/architecture-source", map[string]string{
		"repo": "legion/arch", "branch": "main",
	}, "alice")
	body := decodeBody[map[string]string](t, response)
	if response.Code != http.StatusConflict || body["code"] != "SOURCE_ACCESS" || !strings.Contains(body["error"], "private key") {
		t.Fatalf("no app key: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestArchitectureSourceInputValidation(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{"legion/arch": "read"}}
	handler, _ := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")

	for name, input := range map[string]map[string]string{
		"no slash":        {"repo": "legionarch", "branch": "main"},
		"extra segment":   {"repo": "legion/arch/deep", "branch": "main"},
		"empty owner":     {"repo": "/arch", "branch": "main"},
		"whitespace":      {"repo": "legion/ar ch", "branch": "main"},
		"missing branch":  {"repo": "legion/arch", "branch": "  "},
		"dot-dot owner":   {"repo": "../arch", "branch": "main"},
		"dot-dot name":    {"repo": "legion/..", "branch": "main"},
		"dot name":        {"repo": "legion/.", "branch": "main"},
		"owner charset":   {"repo": "leg!on/arch", "branch": "main"},
		"owner edge dash": {"repo": "-legion/arch", "branch": "main"},
	} {
		t.Run(name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/architecture-source", input, "alice")
			if response.Code != http.StatusBadRequest || decodeBody[map[string]string](t, response)["code"] != "SOURCE_INPUT" {
				t.Fatalf("%s: status=%d body=%s", name, response.Code, response.Body.String())
			}
		})
	}
	if fake.tokenMints != 0 {
		t.Fatalf("invalid input reached GitHub: %d mints", fake.tokenMints)
	}

	missingProject := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/NOPE/architecture-source", map[string]string{
		"repo": "legion/arch", "branch": "main",
	}, "alice")
	if missingProject.Code != http.StatusNotFound {
		t.Fatalf("unknown project: status=%d body=%s", missingProject.Code, missingProject.Body.String())
	}
}

func TestArchitectureSourceRePutRevalidatesBeforeWriting(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{"legion/arch": "write"}}
	handler, _ := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")

	if saved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/architecture-source", map[string]string{
		"repo": "legion/arch", "branch": "main",
	}, "alice"); saved.Code != http.StatusOK {
		t.Fatalf("initial put: status=%d body=%s", saved.Code, saved.Body.String())
	}

	// The App gets uninstalled; the re-PUT fails its revalidation and the
	// stored source survives untouched.
	fake.installations = map[string]string{}
	rePut := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/architecture-source", map[string]string{
		"repo": "legion/arch", "branch": "release",
	}, "alice")
	if rePut.Code != http.StatusConflict || decodeBody[map[string]string](t, rePut)["code"] != "SOURCE_ACCESS" {
		t.Fatalf("re-put after uninstall: status=%d body=%s", rePut.Code, rePut.Body.String())
	}
	fetched := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/CORE/architecture-source", nil, "alice")
	got := decodeBody[architectureSourceResponse](t, fetched)
	if fetched.Code != http.StatusOK || got.Branch != "main" || got.Repo != "legion/arch" {
		t.Fatalf("source after failed re-put: status=%d body=%s", fetched.Code, fetched.Body.String())
	}
}
