package api

import (
	"bytes"
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
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fakeGitHubApp is the GitHub App API surface the access check walks:
// installation lookup, token mint, and the repository read that proves the
// minted token works, plus the commit/tree/blob reads the architecture
// importer uses. installations maps "owner/repo" to its Contents permission;
// a missing repository answers 404. repos holds each repository's architecture
// branch under the same key, so two sources of a project read their own heads
// and files.
type fakeGitHubApp struct {
	installations map[string]string
	tokenMints    int
	repos         map[string]*fakeArchitectureRepo
	// headReads lists every branch head lookup, as "owner/repo@branch", in order: which
	// sources a sync attempted.
	headReads []string
}

// fakeArchitectureRepo is one repository's architecture branch. commit is the
// sha every branch resolves to; branchMissing answers 404 instead, as a
// repository absent from fakeGitHubApp.repos does. files is keyed by file name
// inside architecture.SourceDir.
type fakeArchitectureRepo struct {
	commit        string
	files         map[string]string
	branchMissing bool
}

func (f *fakeGitHubApp) repo(r *http.Request) *fakeArchitectureRepo {
	return f.repos[r.PathValue("owner")+"/"+r.PathValue("repo")]
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
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, r *http.Request) {
		f.headReads = append(f.headReads, r.PathValue("owner")+"/"+r.PathValue("repo")+"@"+r.PathValue("branch"))
		repo := f.repo(r)
		if repo == nil || repo.branchMissing {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"sha":%q}`, repo.commit)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if _, dir, ok := strings.Cut(r.PathValue("ref"), ":"); !ok || dir != architecture.SourceDir {
			t.Errorf("tree read %q is not the %s subtree", r.PathValue("ref"), architecture.SourceDir)
		}
		repo := f.repo(r)
		if repo == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The subtree sha is derived from the files, as git derives it: the
		// same files under another commit (or another repository) answer the
		// same sha, so the unchanged-tree short-circuit is reachable here.
		names := make([]string, 0, len(repo.files))
		for name := range repo.files {
			names = append(names, name)
		}
		sort.Strings(names)
		digest := sha256.New()
		entries := []map[string]any{}
		for _, name := range names {
			fmt.Fprintf(digest, "%s\x00%s\x00", name, repo.files[name])
			entries = append(entries, map[string]any{
				"path": name, "mode": "100644", "type": "blob", "sha": "blob-" + name, "size": len(repo.files[name]),
			})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"sha": "tree-" + hex.EncodeToString(digest.Sum(nil))[:12], "truncated": false, "tree": entries,
		}); err != nil {
			t.Errorf("encode tree: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		repo := f.repo(r)
		if repo == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		content, ok := repo.files[strings.TrimPrefix(r.PathValue("sha"), "blob-")]
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
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: github.URL,
	})
	return handler, database
}

// archFiles and infraFiles are the models of two sources with no component id in common:
// legion/arch's api depends on its store, and acme/infra holds deploy.
func archFiles() map[string]string {
	return map[string]string{
		"api.md":   "---\ntitle: HTTP API\ndepends_on: [store]\n---\nThe API.\n",
		"store.md": "---\ntitle: Store\n---\nThe store.\n",
	}
}

func infraFiles() map[string]string {
	return map[string]string{"deploy.md": "---\ntitle: Deploy\n---\nShipping.\n"}
}

// twoSourceFake is a GitHub where the App reads legion/arch and acme/infra, each at its own head.
func twoSourceFake() *fakeGitHubApp {
	return &fakeGitHubApp{
		installations: map[string]string{"legion/arch": "read", "acme/infra": "read"},
		repos: map[string]*fakeArchitectureRepo{
			"legion/arch": {commit: "sha-arch", files: archFiles()},
			"acme/infra":  {commit: "sha-infra", files: infraFiles()},
		},
	}
}

// architectureSourceResponse is every field of a source row's JSON: decodeSources refuses any
// other, so a server detail (the source's id, its installation id) can never leak into it.
type architectureSourceResponse struct {
	Project    string          `json:"project"`
	Repo       string          `json:"repo"`
	Branch     string          `json:"branch"`
	Enabled    bool            `json:"enabled"`
	CreatedBy  json.RawMessage `json:"created_by"`
	CreatedAt  string          `json:"created_at"`
	LastSyncAt *string         `json:"last_sync_at"`
	LastCommit *string         `json:"last_commit"`
	LastError  *string         `json:"last_error"`
	Components int             `json:"components"`
}

// String is the row without its timestamps and author, for comparing whole rows.
func (s architectureSourceResponse) String() string {
	optional := func(value *string) string {
		if value == nil {
			return "-"
		}
		return *value
	}
	return fmt.Sprintf("%s %s@%s enabled=%t commit=%s error=%s components=%d",
		s.Project, s.Repo, s.Branch, s.Enabled, optional(s.LastCommit), optional(s.LastError), s.Components)
}

func decodeStrict[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
	decoder.DisallowUnknownFields()
	var value T
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode response body %q: %v", response.Body.String(), err)
	}
	return value
}

func createTestProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key,
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
}

func sourceStrings(sources []architectureSourceResponse) []string {
	rows := make([]string, len(sources))
	for index, source := range sources {
		rows[index] = source.String()
	}
	return rows
}

// sourcePath addresses project's source from repo ("owner/name").
func sourcePath(project, repo string) string {
	return "/api/v1/projects/" + project + "/architecture-sources/" + repo
}

func putSource(t *testing.T, handler http.Handler, project, repo, branch string) architectureSourceResponse {
	t.Helper()
	saved := dispatchRequest(t, handler, http.MethodPut, sourcePath(project, repo), map[string]string{"branch": branch}, "alice")
	if saved.Code != http.StatusOK {
		t.Fatalf("put %s source %s: status=%d body=%s", project, repo, saved.Code, saved.Body.String())
	}
	return decodeStrict[architectureSourceResponse](t, saved)
}

func syncSource(t *testing.T, handler http.Handler, project, repo string) architectureSourceResponse {
	t.Helper()
	synced := dispatchRequest(t, handler, http.MethodPost, sourcePath(project, repo)+"/sync", nil, "alice")
	if synced.Code != http.StatusOK {
		t.Fatalf("sync %s source %s: status=%d body=%s", project, repo, synced.Code, synced.Body.String())
	}
	return decodeStrict[architectureSourceResponse](t, synced)
}

func syncSources(t *testing.T, handler http.Handler, project string) []architectureSourceResponse {
	t.Helper()
	synced := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/"+project+"/architecture-sources/sync", nil, "alice")
	if synced.Code != http.StatusOK {
		t.Fatalf("sync %s sources: status=%d body=%s", project, synced.Code, synced.Body.String())
	}
	return decodeStrict[[]architectureSourceResponse](t, synced)
}

func listSources(t *testing.T, handler http.Handler, project string) []architectureSourceResponse {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/"+project+"/architecture-sources", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list %s sources: status=%d body=%s", project, listed.Code, listed.Body.String())
	}
	return decodeStrict[[]architectureSourceResponse](t, listed)
}

func sourceID(t *testing.T, database *store.Store, project, repo string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(),
		`select id from architecture_sources where project_key = $1 and repo = $2`, project, repo).Scan(&id); err != nil {
		t.Fatalf("read the id of %s source %s: %v", project, repo, err)
	}
	return id
}

// sourceEvents lists every settings.architecture_source.updated event as
// "<project> <repo>@<branch> deleted=<bool> by <actor id>", in order.
func sourceEvents(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select coalesce(project_key, '') || ' ' || (payload->'source'->>'repo') || '@' || (payload->'source'->>'branch') ||
		       ' deleted=' || (payload->>'deleted') || ' by ' || (actor->>'id')
		from events where type = 'settings.architecture_source.updated' order by id
	`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	events := []string{}
	for rows.Next() {
		var event string
		if err := rows.Scan(&event); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read events: %v", err)
	}
	return events
}

// A project lists its sources in the order it added them; saving a repository it already has
// changes that source's branch in place.
func TestArchitectureSourcesListAddAndChangeBranch(t *testing.T) {
	fake := twoSourceFake()
	fake.installations["legion/arch"] = "write"
	handler, database := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")

	// No source is an ordinary answer, not a missing resource; a project that does not exist is.
	if empty := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/CORE/architecture-sources", nil, "alice"); empty.Code != http.StatusOK ||
		strings.TrimSpace(empty.Body.String()) != "[]" {
		t.Fatalf("list without a source: status=%d body=%s", empty.Code, empty.Body.String())
	}
	if absent := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/NOPE/architecture-sources", nil, "alice"); absent.Code != http.StatusNotFound {
		t.Fatalf("list for a project that does not exist: status=%d body=%s", absent.Code, absent.Body.String())
	}

	// The repository in the path is stored as repository mappings are: lowercase, .git trimmed.
	saved := putSource(t, handler, "CORE", "Legion/Arch.git", "main")
	if got, want := saved.String(), "CORE legion/arch@main enabled=true commit=- error=- components=0"; got != want {
		t.Fatalf("saved source = %s, want %s", got, want)
	}
	if fake.tokenMints != 1 {
		t.Fatalf("access check minted %d tokens, want 1", fake.tokenMints)
	}
	var installationID int64
	if err := database.Pool.QueryRow(context.Background(),
		"select installation_id from architecture_sources where project_key = 'CORE' and repo = 'legion/arch'").Scan(&installationID); err != nil || installationID != 77 {
		t.Fatalf("persisted installation id: got %d, err %v, want 77", installationID, err)
	}

	putSource(t, handler, "CORE", "acme/infra", "main")
	createTestProject(t, handler, "APP")
	putSource(t, handler, "APP", "acme/infra", "main")

	// Id order, not repository order: legion/arch was added first.
	if got, want := sourceStrings(listSources(t, handler, "CORE")), []string{
		"CORE legion/arch@main enabled=true commit=- error=- components=0",
		"CORE acme/infra@main enabled=true commit=- error=- components=0",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CORE sources = %q, want %q", got, want)
	}
	settings := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/architecture-sources", nil, "alice")
	if settings.Code != http.StatusOK {
		t.Fatalf("list every source: status=%d body=%s", settings.Code, settings.Body.String())
	}
	if got, want := sourceStrings(decodeStrict[[]architectureSourceResponse](t, settings)), []string{
		"APP acme/infra@main enabled=true commit=- error=- components=0",
		"CORE legion/arch@main enabled=true commit=- error=- components=0",
		"CORE acme/infra@main enabled=true commit=- error=- components=0",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("every source = %q, want %q", got, want)
	}

	if got, want := sourceStrings(syncSources(t, handler, "CORE")), []string{
		"CORE legion/arch@main enabled=true commit=sha-arch error=- components=2",
		"CORE acme/infra@main enabled=true commit=sha-infra error=- components=1",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("synced CORE sources = %q, want %q", got, want)
	}

	// A re-save of legion/arch moves that source to another branch: the same row, its sync
	// bookkeeping reset for the next import, its components kept until then.
	before := sourceID(t, database, "CORE", "legion/arch")
	if got, want := putSource(t, handler, "CORE", "legion/arch", "release").String(), "CORE legion/arch@release enabled=true commit=- error=- components=2"; got != want {
		t.Fatalf("re-saved source = %s, want %s", got, want)
	}
	if after := sourceID(t, database, "CORE", "legion/arch"); after != before {
		t.Fatalf("re-save moved legion/arch from id %d to %d", before, after)
	}
	if got, want := sourceStrings(listSources(t, handler, "CORE")), []string{
		"CORE legion/arch@release enabled=true commit=- error=- components=2",
		"CORE acme/infra@main enabled=true commit=sha-infra error=- components=1",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CORE sources after the re-save = %q, want %q", got, want)
	}

	if got, want := sourceEvents(t, database), []string{
		"CORE legion/arch@main deleted=false by alice",
		"CORE acme/infra@main deleted=false by alice",
		"APP acme/infra@main deleted=false by alice",
		"CORE legion/arch@release deleted=false by alice",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("source events = %q, want %q", got, want)
	}
}

// Removing one source retires only the components it projected: another source's components and
// edges stay, and an issue attached to one of each keeps the removed one as unknown.
func TestArchitectureSourceDeleteRemovesOnlyItsComponents(t *testing.T) {
	handler, database := newArchitectureSourceServer(t, twoSourceFake())
	createTestProject(t, handler, "CORE")
	putSource(t, handler, "CORE", "legion/arch", "main")
	putSource(t, handler, "CORE", "acme/infra", "main")
	syncSources(t, handler, "CORE")
	issue := createComponentIssue(t, handler, "Both sources", nil, map[string]any{"mode": "explicit", "ids": []string{"api", "deploy"}})

	projection := func() string {
		t.Helper()
		var components string
		var depends, snapshots int
		if err := database.Pool.QueryRow(context.Background(), `
			select coalesce((select string_agg(s.repo || ' ' || c.id, ', ' order by s.id, c.id)
			                 from components c join architecture_sources s on s.project_key = c.project_key and s.id = c.source_id
			                 where c.project_key = 'CORE'), ''),
			       (select count(*) from component_depends where project_key = 'CORE'),
			       (select count(*) from architecture_snapshots where project_key = 'CORE')
		`).Scan(&components, &depends, &snapshots); err != nil {
			t.Fatalf("read the projection: %v", err)
		}
		return fmt.Sprintf("%s; depends=%d snapshots=%d", components, depends, snapshots)
	}
	if got, want := projection(), "legion/arch api, legion/arch store, acme/infra deploy; depends=1 snapshots=2"; got != want {
		t.Fatalf("projection before the delete = %q, want %q", got, want)
	}

	if deleted := dispatchRequest(t, handler, http.MethodDelete, sourcePath("CORE", "Legion/Arch"), nil, "alice"); deleted.Code != http.StatusNoContent {
		t.Fatalf("delete legion/arch: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if got, want := projection(), "acme/infra deploy; depends=0 snapshots=2"; got != want {
		t.Fatalf("projection after the delete = %q, want %q", got, want)
	}
	read := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice"))
	if !reflect.DeepEqual(read.Components.IDs, []string{"deploy"}) || !reflect.DeepEqual(read.Components.Unknown, []string{"api"}) {
		t.Fatalf("issue components after the delete = %+v, want deploy live and api retired", read.Components)
	}
	if got, want := sourceStrings(listSources(t, handler, "CORE")), []string{
		"CORE acme/infra@main enabled=true commit=sha-infra error=- components=1",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CORE sources after the delete = %q, want %q", got, want)
	}

	for _, repo := range []string{"legion/arch", "other/repo"} {
		again := dispatchRequest(t, handler, http.MethodDelete, sourcePath("CORE", repo), nil, "alice")
		if again.Code != http.StatusNotFound || decodeBody[map[string]string](t, again)["code"] != "SOURCE_NOT_FOUND" {
			t.Fatalf("delete %s, not a source: status=%d body=%s", repo, again.Code, again.Body.String())
		}
	}
	if got, want := sourceEvents(t, database), []string{
		"CORE legion/arch@main deleted=false by alice",
		"CORE acme/infra@main deleted=false by alice",
		"CORE legion/arch@main deleted=true by alice",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("source events = %q, want %q", got, want)
	}
}

// Every source route is authAny: an agent session lists, saves, refreshes and removes sources,
// and a bearer save or removal is attributed to the session its body names.
func TestAgentUsesEveryArchitectureSourceRoute(t *testing.T) {
	handler, database := newArchitectureSourceServer(t, twoSourceFake())
	createTestProject(t, handler, "CORE")
	session := func(id string) map[string]string { return map[string]string{"kind": "session", "id": id} }

	// A bearer write names its acting session, as every bearer write does.
	unnamed := agentRequest(t, handler, http.MethodPut, sourcePath("CORE", "legion/arch"), map[string]any{"branch": "main"}, "agent-token")
	if unnamed.Code != http.StatusBadRequest || decodeBody[map[string]string](t, unnamed)["code"] != "ACTOR_KIND" {
		t.Fatalf("agent put without an actor: status=%d body=%s", unnamed.Code, unnamed.Body.String())
	}
	for _, repo := range []string{"legion/arch", "acme/infra"} {
		saved := agentRequest(t, handler, http.MethodPut, sourcePath("CORE", repo), map[string]any{
			"branch": "main", "actor": session("agent-architecture-put"),
		}, "agent-token")
		if saved.Code != http.StatusOK {
			t.Fatalf("agent put %s: status=%d body=%s", repo, saved.Code, saved.Body.String())
		}
	}
	var authors string
	if err := database.Pool.QueryRow(t.Context(), `
		select string_agg(repo || ' by ' || (created_by->>'id'), ', ' order by id) from architecture_sources where project_key = 'CORE'
	`).Scan(&authors); err != nil || authors != "legion/arch by agent-architecture-put, acme/infra by agent-architecture-put" {
		t.Fatalf("source authors = %q (err %v)", authors, err)
	}

	for _, target := range []string{"/api/v1/settings/architecture-sources", "/api/v1/projects/CORE/architecture-sources"} {
		listed := agentRequest(t, handler, http.MethodGet, target, nil, "agent-token")
		if listed.Code != http.StatusOK || len(decodeStrict[[]architectureSourceResponse](t, listed)) != 2 {
			t.Fatalf("agent GET %s: status=%d body=%s", target, listed.Code, listed.Body.String())
		}
	}
	one := agentRequest(t, handler, http.MethodPost, sourcePath("CORE", "legion/arch")+"/sync", nil, "agent-token")
	if one.Code != http.StatusOK {
		t.Fatalf("agent sync legion/arch: status=%d body=%s", one.Code, one.Body.String())
	}
	if got, want := decodeStrict[architectureSourceResponse](t, one).String(), "CORE legion/arch@main enabled=true commit=sha-arch error=- components=2"; got != want {
		t.Fatalf("agent synced legion/arch = %s, want %s", got, want)
	}
	every := agentRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/architecture-sources/sync", nil, "agent-token")
	if every.Code != http.StatusOK {
		t.Fatalf("agent sync every source: status=%d body=%s", every.Code, every.Body.String())
	}
	if got, want := sourceStrings(decodeStrict[[]architectureSourceResponse](t, every)), []string{
		"CORE legion/arch@main enabled=true commit=sha-arch error=- components=2",
		"CORE acme/infra@main enabled=true commit=sha-infra error=- components=1",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("agent synced sources = %q, want %q", got, want)
	}
	if tree := agentRequest(t, handler, http.MethodGet, "/api/v1/projects/CORE/architecture", nil, "agent-token"); tree.Code != http.StatusOK {
		t.Fatalf("agent tree: status=%d body=%s", tree.Code, tree.Body.String())
	}
	removed := agentRequest(t, handler, http.MethodDelete, sourcePath("CORE", "acme/infra"), map[string]any{
		"actor": session("agent-architecture-delete"),
	}, "agent-token")
	if removed.Code != http.StatusNoContent {
		t.Fatalf("agent delete acme/infra: status=%d body=%s", removed.Code, removed.Body.String())
	}
	if got, want := sourceEvents(t, database), []string{
		"CORE legion/arch@main deleted=false by agent-architecture-put",
		"CORE acme/infra@main deleted=false by agent-architecture-put",
		"CORE acme/infra@main deleted=true by agent-architecture-delete",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("source events = %q, want %q", got, want)
	}
}

func TestArchitectureSourceAccessFailuresAnswer409(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{"legion/noread": "none"}}
	handler, database := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")

	notInstalled := dispatchRequest(t, handler, http.MethodPut, sourcePath("CORE", "legion/missing"), map[string]string{"branch": "main"}, "alice")
	body := decodeBody[map[string]string](t, notInstalled)
	if notInstalled.Code != http.StatusConflict || body["code"] != "SOURCE_ACCESS" || !strings.Contains(body["error"], "not installed on legion/missing") {
		t.Fatalf("uninstalled repository: status=%d body=%s", notInstalled.Code, notInstalled.Body.String())
	}

	noContents := dispatchRequest(t, handler, http.MethodPut, sourcePath("CORE", "legion/noread"), map[string]string{"branch": "main"}, "alice")
	body = decodeBody[map[string]string](t, noContents)
	if noContents.Code != http.StatusConflict || body["code"] != "SOURCE_ACCESS" || !strings.Contains(body["error"], "Contents: read") {
		t.Fatalf("contents none: status=%d body=%s", noContents.Code, noContents.Body.String())
	}

	// Neither refusal wrote anything: no source row, no event.
	if sources := listSources(t, handler, "CORE"); len(sources) != 0 {
		t.Fatalf("refused saves wrote sources: %q", sourceStrings(sources))
	}
	if events := sourceEvents(t, database); len(events) != 0 {
		t.Fatalf("refused saves wrote events: %q", events)
	}
}

func TestArchitectureSourceWithoutAppKeyAnswers409(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createTestProject(t, handler, "CORE")
	response := dispatchRequest(t, handler, http.MethodPut, sourcePath("CORE", "legion/arch"), map[string]string{"branch": "main"}, "alice")
	body := decodeBody[map[string]string](t, response)
	if response.Code != http.StatusConflict || body["code"] != "SOURCE_ACCESS" || !strings.Contains(body["error"], "private key") {
		t.Fatalf("no app key: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestArchitectureSourceInputValidation(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{"legion/arch": "read"}}
	handler, _ := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")

	// The repository is the path's {owner}/{repo}, each segment percent-decoded.
	for name, input := range map[string]struct{ repo, branch string }{
		"extra segment":           {"legion/arch%2Fdeep", "main"},
		"whitespace":              {"legion/ar%20ch", "main"},
		"missing branch":          {"legion/arch", "  "},
		"owner charset":           {"leg!on/arch", "main"},
		"owner edge dash":         {"-legion/arch", "main"},
		"dot-dot after .git trim": {"legion/...git", "main"},
		"dot after .git trim":     {"legion/..git", "main"},
		"empty after .git trim":   {"legion/.git", "main"},
	} {
		t.Run(name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPut, sourcePath("CORE", input.repo), map[string]string{"branch": input.branch}, "alice")
			if response.Code != http.StatusBadRequest || decodeBody[map[string]string](t, response)["code"] != "SOURCE_INPUT" {
				t.Fatalf("%s: status=%d body=%s", name, response.Code, response.Body.String())
			}
		})
	}
	for _, request := range []struct{ method, target string }{
		{http.MethodDelete, sourcePath("CORE", "leg!on/arch")},
		{http.MethodPost, sourcePath("CORE", "leg!on/arch") + "/sync"},
	} {
		response := dispatchRequest(t, handler, request.method, request.target, nil, "alice")
		if response.Code != http.StatusBadRequest || decodeBody[map[string]string](t, response)["code"] != "SOURCE_INPUT" {
			t.Fatalf("%s %s: status=%d body=%s", request.method, request.target, response.Code, response.Body.String())
		}
	}
	if fake.tokenMints != 0 {
		t.Fatalf("invalid input reached GitHub: %d mints", fake.tokenMints)
	}

	missingProject := dispatchRequest(t, handler, http.MethodPut, sourcePath("NOPE", "legion/arch"), map[string]string{"branch": "main"}, "alice")
	if missingProject.Code != http.StatusNotFound {
		t.Fatalf("unknown project: status=%d body=%s", missingProject.Code, missingProject.Body.String())
	}
}

func TestArchitectureSourceRePutRevalidatesBeforeWriting(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{"legion/arch": "write"}}
	handler, _ := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")
	putSource(t, handler, "CORE", "legion/arch", "main")

	// The App gets uninstalled; the re-PUT fails its revalidation and the
	// stored source survives untouched.
	fake.installations = map[string]string{}
	rePut := dispatchRequest(t, handler, http.MethodPut, sourcePath("CORE", "legion/arch"), map[string]string{"branch": "release"}, "alice")
	if rePut.Code != http.StatusConflict || decodeBody[map[string]string](t, rePut)["code"] != "SOURCE_ACCESS" {
		t.Fatalf("re-put after uninstall: status=%d body=%s", rePut.Code, rePut.Body.String())
	}
	if got, want := sourceStrings(listSources(t, handler, "CORE")), []string{
		"CORE legion/arch@main enabled=true commit=- error=- components=0",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sources after the failed re-put = %q, want %q", got, want)
	}
}

// The one-source Refresh route (and the `dispatch architecture-sync --repo` command that rides
// it): a happy import answers the updated row; a rejected model still answers 200 with the reason
// on the row, because the HTTP call itself succeeded and the previous projection stays up; a
// credential or branch problem is 409. It imports its own source only.
func TestArchitectureSourceSyncRoute(t *testing.T) {
	fake := twoSourceFake()
	handler, database := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")
	arch := fake.repos["legion/arch"]
	arch.commit = "sha-one"

	if missing := agentRequest(t, handler, http.MethodPost, sourcePath("CORE", "legion/arch")+"/sync", nil, "agent-token"); missing.Code != http.StatusNotFound ||
		decodeBody[map[string]string](t, missing)["code"] != "SOURCE_NOT_FOUND" {
		t.Fatalf("sync without a source: status=%d body=%s", missing.Code, missing.Body.String())
	}
	putSource(t, handler, "CORE", "legion/arch", "main")
	putSource(t, handler, "CORE", "acme/infra", "main")
	if other := dispatchRequest(t, handler, http.MethodPost, sourcePath("CORE", "other/repo")+"/sync", nil, "alice"); other.Code != http.StatusNotFound ||
		decodeBody[map[string]string](t, other)["code"] != "SOURCE_NOT_FOUND" {
		t.Fatalf("sync of a repository that is not a source: status=%d body=%s", other.Code, other.Body.String())
	}

	// authAny: an agent bearer may refresh, which is how the command reaches it.
	synced := agentRequest(t, handler, http.MethodPost, sourcePath("CORE", "legion/arch")+"/sync", nil, "agent-token")
	if synced.Code != http.StatusOK {
		t.Fatalf("sync: status=%d body=%s", synced.Code, synced.Body.String())
	}
	if got := decodeStrict[architectureSourceResponse](t, synced); got.String() != "CORE legion/arch@main enabled=true commit=sha-one error=- components=2" || got.LastSyncAt == nil {
		t.Fatalf("sync response: %s (last_sync_at %v)", got, got.LastSyncAt)
	}
	components := func() string {
		t.Helper()
		var ids string
		if err := database.Pool.QueryRow(context.Background(), `
			select coalesce(string_agg(id, ',' order by id), '') from components where project_key = 'CORE'
		`).Scan(&ids); err != nil {
			t.Fatalf("read components: %v", err)
		}
		return ids
	}
	if got := components(); got != "api,store" {
		t.Fatalf("components = %q, want api,store", got)
	}

	// A rejected model: 200, the reason on the row, the projection intact.
	arch.commit = "sha-broken"
	arch.files = map[string]string{"api.md": "x\n", "API.md": "duplicate\n"}
	rejected := syncSource(t, handler, "CORE", "legion/arch")
	if rejected.LastError == nil || !strings.Contains(*rejected.LastError, "duplicate component id") {
		t.Fatalf("rejected response last_error: %s", rejected)
	}
	if rejected.LastCommit == nil || *rejected.LastCommit != "sha-one" || rejected.Components != 2 {
		t.Fatalf("rejected import moved the source: %s", rejected)
	}
	if got := components(); got != "api,store" {
		t.Fatalf("components after a rejected import = %q, want api,store", got)
	}

	// Front matter that spells U+0000 as a YAML escape is a rejected model too, not a 500 that
	// records nothing (LEGION-527).
	arch.commit = "sha-nul"
	arch.files = map[string]string{"api.md": "---\ntitle: \"HTTP\\0API\"\n---\nThe API.\n", "store.md": "---\ntitle: Store\n---\nThe store.\n"}
	escaped := syncSource(t, handler, "CORE", "legion/arch")
	if escaped.LastError == nil || !strings.Contains(*escaped.LastError, "api.md: front matter title") || escaped.LastCommit == nil || *escaped.LastCommit != "sha-one" {
		t.Fatalf("front matter NUL response: %s", escaped)
	}

	// An unknown front-matter key that YAML decodes to U+0000 reaches KnownFields before Parse can
	// name a decoded value. The recorded failure is made storable before it reaches the row/event,
	// so this route is still the same 200 with last_error (LEGION-527).
	arch.commit = "sha-nul-key"
	arch.files = map[string]string{"api.md": "---\n\"\\0\": value\n---\nThe API.\n", "store.md": "---\ntitle: Store\n---\nThe store.\n"}
	key := syncSource(t, handler, "CORE", "legion/arch")
	if key.LastError == nil || !strings.Contains(*key.LastError, "api.md: front matter") || strings.IndexByte(*key.LastError, 0) >= 0 || key.LastCommit == nil || *key.LastCommit != "sha-one" {
		t.Fatalf("front matter NUL key response: %s", key)
	}

	// A credential- or branch-shaped failure is the one 409: something a human
	// must fix in GitHub or Settings.
	arch.branchMissing = true
	blocked := dispatchRequest(t, handler, http.MethodPost, sourcePath("CORE", "legion/arch")+"/sync", nil, "alice")
	if blocked.Code != http.StatusConflict || decodeBody[map[string]string](t, blocked)["code"] != "SOURCE_ACCESS" {
		t.Fatalf("missing branch: status=%d body=%s", blocked.Code, blocked.Body.String())
	}

	// None of it touched the other source.
	for _, read := range fake.headReads {
		if read != "legion/arch@main" {
			t.Fatalf("the one-source sync read %s; head reads %q", read, fake.headReads)
		}
	}
	if got, want := listSources(t, handler, "CORE")[1].String(), "CORE acme/infra@main enabled=true commit=- error=- components=0"; got != want {
		t.Fatalf("acme/infra after legion/arch's syncs = %s, want %s", got, want)
	}
}

// The every-source Refresh imports each source whatever another's import did, and answers every
// row with its own outcome: one source's model error or access failure is on that row, and the
// other source still imports.
func TestArchitectureSourcesSyncEverySource(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "CORE")
	for _, target := range []string{"/api/v1/projects/CORE/architecture-sources/sync", "/api/v1/projects/NOPE/architecture-sources/sync"} {
		missing := dispatchRequest(t, handler, http.MethodPost, target, nil, "alice")
		if missing.Code != http.StatusNotFound || decodeBody[map[string]string](t, missing)["code"] != "SOURCE_NOT_FOUND" {
			t.Fatalf("POST %s without a source: status=%d body=%s", target, missing.Code, missing.Body.String())
		}
	}

	healthy := map[string]string{
		"legion/arch": "CORE legion/arch@main enabled=true commit=sha-arch error=- components=2",
		"acme/infra":  "CORE acme/infra@main enabled=true commit=sha-infra error=- components=1",
	}
	for _, tc := range []struct {
		name    string
		failing string
		access  bool
		want    string
	}{
		{"model error on the second source", "acme/infra", false,
			`CORE acme/infra@main enabled=true commit=sha-infra error=broken.md: unknown parent "nowhere" components=1`},
		{"model error on the first source", "legion/arch", false,
			`CORE legion/arch@main enabled=true commit=sha-arch error=broken.md: unknown parent "nowhere" components=2`},
		{"access failure on the second source", "acme/infra", true,
			"CORE acme/infra@main enabled=true commit=sha-infra error=the branch does not exist: main on acme/infra components=1"},
		{"access failure on the first source", "legion/arch", true,
			"CORE legion/arch@main enabled=true commit=sha-arch error=the branch does not exist: main on legion/arch components=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := twoSourceFake()
			handler, _ := newArchitectureSourceServer(t, fake)
			createTestProject(t, handler, "CORE")
			putSource(t, handler, "CORE", "legion/arch", "main")
			putSource(t, handler, "CORE", "acme/infra", "main")
			if got, want := sourceStrings(syncSources(t, handler, "CORE")), []string{healthy["legion/arch"], healthy["acme/infra"]}; !reflect.DeepEqual(got, want) {
				t.Fatalf("first sync = %q, want %q", got, want)
			}

			failing := fake.repos[tc.failing]
			if tc.access {
				failing.branchMissing = true
			} else {
				failing.commit = "sha-broken"
				failing.files = map[string]string{"broken.md": "---\ntitle: Broken\nparent: nowhere\n---\nBroken.\n"}
			}
			fake.headReads = nil
			synced := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/architecture-sources/sync", nil, "alice")
			if synced.Code != http.StatusOK {
				t.Fatalf("sync every source: status=%d body=%s", synced.Code, synced.Body.String())
			}
			if want := []string{"legion/arch@main", "acme/infra@main"}; !reflect.DeepEqual(fake.headReads, want) {
				t.Fatalf("head reads = %q, want both sources attempted: %q", fake.headReads, want)
			}
			want := []string{healthy["legion/arch"], healthy["acme/infra"]}
			if tc.failing == "legion/arch" {
				want[0] = tc.want
			} else {
				want[1] = tc.want
			}
			rows := decodeStrict[[]architectureSourceResponse](t, synced)
			if got := sourceStrings(rows); !reflect.DeepEqual(got, want) {
				t.Fatalf("sync every source = %q, want %q", got, want)
			}
			for _, row := range rows {
				if row.LastSyncAt == nil || row.CreatedAt == "" || !strings.Contains(string(row.CreatedBy), `"alice"`) {
					t.Fatalf("row %s: last_sync_at %v created_at %q created_by %s", row, row.LastSyncAt, row.CreatedAt, row.CreatedBy)
				}
			}
			// The answer is what the rows now hold.
			if got := sourceStrings(listSources(t, handler, "CORE")); !reflect.DeepEqual(got, want) {
				t.Fatalf("listed after the sync = %q, want %q", got, want)
			}
		})
	}
}

// Re-pointing a source at another branch whose architecture files are
// byte-identical must still re-project: the PUT resets the subtree memory so
// the next sync records a snapshot for the new branch's commit rather than
// taking the unchanged-tree short-circuit against the old branch's import.
func TestArchitectureSourceRePutReprojectsAnIdenticalTree(t *testing.T) {
	fake := &fakeGitHubApp{
		installations: map[string]string{"legion/arch": "read"},
		repos:         map[string]*fakeArchitectureRepo{"legion/arch": {commit: "sha-one", files: archFiles()}},
	}
	handler, database := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")
	putSource(t, handler, "CORE", "legion/arch", "main")
	syncSource(t, handler, "CORE", "legion/arch")

	fake.repos["legion/arch"].commit = "sha-release"
	putSource(t, handler, "CORE", "legion/arch", "release")
	syncSource(t, handler, "CORE", "legion/arch")

	var snapshots int
	var snapshotCommit string
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), (select commit from architecture_snapshots where project_key = 'CORE' order by id desc limit 1)
		from architecture_snapshots where project_key = 'CORE'
	`).Scan(&snapshots, &snapshotCommit); err != nil {
		t.Fatalf("snapshots: %v", err)
	}
	if snapshots != 2 || snapshotCommit != "sha-release" {
		t.Fatalf("snapshots = %d newest %q, want 2 with sha-release (the re-pointed source must re-project)", snapshots, snapshotCommit)
	}
	var owned string
	if err := database.Pool.QueryRow(context.Background(), `
		select string_agg(distinct s.commit, ',') from components c
		join architecture_snapshots s on s.id = c.snapshot_id where c.project_key = 'CORE'
	`).Scan(&owned); err != nil || owned != "sha-release" {
		t.Fatalf("components owned by snapshot %q err %v, want sha-release", owned, err)
	}
}
