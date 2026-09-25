package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fakeGitHubApp is the GitHub App API surface the access check walks:
// installation lookup, token mint, and the repository read that proves the
// minted token works, plus the commit/tree/blob reads the architecture
// importer uses. installations maps "owner/repo" to its Contents permission;
// a missing repository answers 404.
type fakeGitHubApp struct {
	installations map[string]string
	tokenMints    int
	// commit is the sha every branch resolves to; branchMissing answers 404
	// instead. files is keyed by file name inside architecture.SourceDir.
	commit        string
	files         map[string]string
	branchMissing bool
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
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, _ *http.Request) {
		if f.branchMissing {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"sha":%q}`, f.commit)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{ref}", func(w http.ResponseWriter, r *http.Request) {
		commit, dir, ok := strings.Cut(r.PathValue("ref"), ":")
		if !ok || dir != architecture.SourceDir {
			t.Errorf("tree read %q is not the %s subtree", r.PathValue("ref"), architecture.SourceDir)
		}
		// The subtree sha is derived from the files, as git derives it: the
		// same files under another commit (or another repository) answer the
		// same sha, so the unchanged-tree short-circuit is reachable here.
		_ = commit
		names := make([]string, 0, len(f.files))
		for name := range f.files {
			names = append(names, name)
		}
		sort.Strings(names)
		digest := sha256.New()
		entries := []map[string]any{}
		for _, name := range names {
			fmt.Fprintf(digest, "%s\x00%s\x00", name, f.files[name])
			entries = append(entries, map[string]any{
				"path": name, "mode": "100644", "type": "blob", "sha": "blob-" + name, "size": len(f.files[name]),
			})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"sha": "tree-" + hex.EncodeToString(digest.Sum(nil))[:12], "truncated": false, "tree": entries,
		}); err != nil {
			t.Errorf("encode tree: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		content, ok := f.files[strings.TrimPrefix(r.PathValue("sha"), "blob-")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64",
		}); err != nil {
			t.Errorf("encode blob: %v", err)
		}
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
	handler, database, _ := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", ClientSecret: "secret", PEM: pemText},
		githubAPIBase: github.URL,
	})
	return handler, database
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

	// Project a model first: deleting the source retires it (components and
	// their edges), while the snapshot stays as history.
	fake.commit = "sha-one"
	fake.files = map[string]string{
		"api.md":   "---\ntitle: HTTP API\ndepends_on: [store]\n---\nThe API.\n",
		"store.md": "---\ntitle: Store\n---\nThe store.\n",
	}
	if synced := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/architecture-source/sync", nil, "alice"); synced.Code != http.StatusOK {
		t.Fatalf("sync before delete: status=%d body=%s", synced.Code, synced.Body.String())
	}
	var projected int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from components where project_key = 'CORE'`).Scan(&projected); err != nil || projected != 2 {
		t.Fatalf("components before delete = %d err %v, want 2", projected, err)
	}

	deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/projects/CORE/architecture-source", nil, "alice")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete architecture source: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	var components, depends, edges, snapshots int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from components where project_key = 'CORE'),
		       (select count(*) from component_depends where project_key = 'CORE'),
		       (select count(*) from graph_edges where from_kind = 'component'),
		       (select count(*) from architecture_snapshots where project_key = 'CORE')
	`).Scan(&components, &depends, &edges, &snapshots); err != nil {
		t.Fatalf("count projection after delete: %v", err)
	}
	if components != 0 || depends != 0 || edges != 0 || snapshots != 1 {
		t.Fatalf("after delete: components=%d depends=%d component edges=%d snapshots=%d; want 0/0/0/1", components, depends, edges, snapshots)
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
	handler, _, _ := newTestServer(t, testServerOptions{})
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
		"no slash":                {"repo": "legionarch", "branch": "main"},
		"extra segment":           {"repo": "legion/arch/deep", "branch": "main"},
		"empty owner":             {"repo": "/arch", "branch": "main"},
		"whitespace":              {"repo": "legion/ar ch", "branch": "main"},
		"missing branch":          {"repo": "legion/arch", "branch": "  "},
		"dot-dot owner":           {"repo": "../arch", "branch": "main"},
		"dot-dot name":            {"repo": "legion/..", "branch": "main"},
		"dot name":                {"repo": "legion/.", "branch": "main"},
		"owner charset":           {"repo": "leg!on/arch", "branch": "main"},
		"owner edge dash":         {"repo": "-legion/arch", "branch": "main"},
		"dot-dot after .git trim": {"repo": "legion/...git", "branch": "main"},
		"dot after .git trim":     {"repo": "legion/..git", "branch": "main"},
		"empty after .git trim":   {"repo": "legion/.git", "branch": "main"},
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

// The Refresh route (and the dispatch_architecture_sync tool that rides it):
// a happy import answers the updated row; a rejected model still answers 200
// with the reason on the row, because the HTTP call itself succeeded and the
// previous projection stays up.
func TestArchitectureSourceSyncRoute(t *testing.T) {
	fake := &fakeGitHubApp{
		installations: map[string]string{"legion/arch": "read"},
		commit:        "sha-one",
		files: map[string]string{
			"api.md":   "---\ntitle: HTTP API\ndepends_on: [store]\n---\nThe API.\n",
			"store.md": "---\ntitle: Store\n---\nThe store.\n",
		},
	}
	handler, database := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")

	if missing := agentRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/architecture-source/sync", nil, "agent-token"); missing.Code != http.StatusNotFound ||
		decodeBody[map[string]string](t, missing)["code"] != "SOURCE_NOT_FOUND" {
		t.Fatalf("sync without a source: status=%d body=%s", missing.Code, missing.Body.String())
	}

	if saved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/architecture-source", map[string]string{
		"repo": "legion/arch", "branch": "main",
	}, "alice"); saved.Code != http.StatusOK {
		t.Fatalf("put source: status=%d body=%s", saved.Code, saved.Body.String())
	}

	// authAny: an agent bearer may refresh, which is how the tool reaches it.
	synced := agentRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/architecture-source/sync", nil, "agent-token")
	if synced.Code != http.StatusOK {
		t.Fatalf("sync: status=%d body=%s", synced.Code, synced.Body.String())
	}
	var body struct {
		LastCommit *string `json:"last_commit"`
		LastError  *string `json:"last_error"`
		LastSyncAt *string `json:"last_sync_at"`
	}
	if err := json.NewDecoder(synced.Body).Decode(&body); err != nil {
		t.Fatalf("decode sync response: %v", err)
	}
	if body.LastCommit == nil || *body.LastCommit != "sha-one" || body.LastError != nil || body.LastSyncAt == nil {
		t.Fatalf("sync response: %+v", body)
	}
	var components string
	if err := database.Pool.QueryRow(context.Background(), `
		select string_agg(id, ',' order by id) from components where project_key = 'CORE'
	`).Scan(&components); err != nil || components != "api,store" {
		t.Fatalf("components = %q err %v", components, err)
	}

	// A rejected model: 200, the reason on the row, the projection intact.
	fake.commit = "sha-broken"
	fake.files = map[string]string{"api.md": "x\n", "API.md": "duplicate\n"}
	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/architecture-source/sync", nil, "alice")
	if rejected.Code != http.StatusOK {
		t.Fatalf("rejected model: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if err := json.NewDecoder(rejected.Body).Decode(&body); err != nil {
		t.Fatalf("decode rejected response: %v", err)
	}
	if body.LastError == nil || !strings.Contains(*body.LastError, "duplicate component id") {
		t.Fatalf("rejected response last_error: %+v", body)
	}
	if body.LastCommit == nil || *body.LastCommit != "sha-one" {
		t.Fatalf("rejected import moved last_commit: %+v", body)
	}
	if err := database.Pool.QueryRow(context.Background(), `
		select string_agg(id, ',' order by id) from components where project_key = 'CORE'
	`).Scan(&components); err != nil || components != "api,store" {
		t.Fatalf("components after a rejected import = %q err %v", components, err)
	}

	// A credential- or branch-shaped failure is the one 409: something a human
	// must fix in GitHub or Settings.
	fake.branchMissing = true
	blocked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/architecture-source/sync", nil, "alice")
	if blocked.Code != http.StatusConflict || decodeBody[map[string]string](t, blocked)["code"] != "SOURCE_ACCESS" {
		t.Fatalf("missing branch: status=%d body=%s", blocked.Code, blocked.Body.String())
	}
}

// Re-pointing a source at another repository whose architecture files are
// byte-identical must still re-project: the PUT resets the subtree memory so
// the next sync records a snapshot for the new source's commit rather than
// taking the unchanged-tree short-circuit against the old repository's import.
func TestArchitectureSourceRePutReprojectsAnIdenticalTree(t *testing.T) {
	files := map[string]string{
		"api.md":   "---\ntitle: HTTP API\ndepends_on: [store]\n---\nThe API.\n",
		"store.md": "---\ntitle: Store\n---\nThe store.\n",
	}
	fake := &fakeGitHubApp{
		installations: map[string]string{"legion/arch": "read", "legion/fork": "read"},
		commit:        "sha-one",
		files:         files,
	}
	handler, database := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")
	if saved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/architecture-source", map[string]string{
		"repo": "legion/arch", "branch": "main",
	}, "alice"); saved.Code != http.StatusOK {
		t.Fatalf("put source: status=%d body=%s", saved.Code, saved.Body.String())
	}
	if synced := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/architecture-source/sync", nil, "alice"); synced.Code != http.StatusOK {
		t.Fatalf("first sync: status=%d body=%s", synced.Code, synced.Body.String())
	}

	fake.commit = "sha-fork"
	if moved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/architecture-source", map[string]string{
		"repo": "legion/fork", "branch": "main",
	}, "alice"); moved.Code != http.StatusOK {
		t.Fatalf("re-put source: status=%d body=%s", moved.Code, moved.Body.String())
	}
	if synced := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/architecture-source/sync", nil, "alice"); synced.Code != http.StatusOK {
		t.Fatalf("second sync: status=%d body=%s", synced.Code, synced.Body.String())
	}

	var snapshots int
	var snapshotCommit string
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), (select commit from architecture_snapshots where project_key = 'CORE' order by id desc limit 1)
		from architecture_snapshots where project_key = 'CORE'
	`).Scan(&snapshots, &snapshotCommit); err != nil {
		t.Fatalf("snapshots: %v", err)
	}
	if snapshots != 2 || snapshotCommit != "sha-fork" {
		t.Fatalf("snapshots = %d newest %q, want 2 with sha-fork (the re-pointed source must re-project)", snapshots, snapshotCommit)
	}
	var owned string
	if err := database.Pool.QueryRow(context.Background(), `
		select string_agg(distinct s.commit, ',') from components c
		join architecture_snapshots s on s.id = c.snapshot_id where c.project_key = 'CORE'
	`).Scan(&owned); err != nil || owned != "sha-fork" {
		t.Fatalf("components owned by snapshot %q err %v, want sha-fork", owned, err)
	}
}
