package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestResolveBootConfigRejectsUntrustedHeaderIdentityWithOAuth(t *testing.T) {
	_, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":            "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":    "agent-token",
		"DISPATCH_IDENTITY":       "header:X-Dispatch-User",
		"DISPATCH_APP_CLIENT_ID":  "client-id",
		"DISPATCH_ALLOWED_LOGINS": "sjawhar",
		"DISPATCH_REPO_PROJECTS":  "owner/repo=TEST",
	}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_IDENTITY_HEADER_TRUSTED") {
		t.Fatalf("error: got %v, want trusted header rejection", err)
	}
}

func TestResolveBootConfigAllowsRepositorySettingsWithoutDefaultProject(t *testing.T) {
	boot, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":            "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":    "agent-token",
		"DISPATCH_IDENTITY":       "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS": "sjawhar",
	}))
	if err != nil || boot.DefaultProject != "" {
		t.Fatalf("resolve boot config: boot=%#v err=%v", boot, err)
	}
}

func TestResolveBootConfigAcceptsDefaultProjectWithoutRepoMapping(t *testing.T) {
	_, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":             "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":     "agent-token",
		"DISPATCH_IDENTITY":        "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS":  "sjawhar",
		"DISPATCH_DEFAULT_PROJECT": "TEST",
	}))
	if err != nil {
		t.Fatalf("resolve boot config: %v", err)
	}
}

func TestResolveBootConfigDisablesNATS(t *testing.T) {
	boot, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":            "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":    "agent-token",
		"DISPATCH_IDENTITY":       "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS": "sjawhar",
		"DISPATCH_NATS_DISABLED":  "1",
		"DISPATCH_REPO_PROJECTS":  "owner/repo=TEST",
	}))
	if err != nil {
		t.Fatalf("resolve boot config: %v", err)
	}
	if !boot.NATSDisabled {
		t.Fatal("DISPATCH_NATS_DISABLED=1 did not disable NATS")
	}
}

func TestResolveBootConfigDefaultsAndValidatesEnvoyURL(t *testing.T) {
	base := map[string]string{
		"DATABASE_URL":            "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":    "t",
		"DISPATCH_IDENTITY":       "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS": "sjawhar",
	}

	boot, err := resolveBootConfig(envGetter(base))
	if err != nil || boot.EnvoyURL != "http://127.0.0.1:9020" {
		t.Fatalf("default: boot=%#v err=%v", boot, err)
	}

	withURL := map[string]string{}
	for key, value := range base {
		withURL[key] = value
	}
	withURL["ENVOY_URL"] = "http://envoy.internal:9020/"
	if boot, err := resolveBootConfig(envGetter(withURL)); err != nil || boot.EnvoyURL != "http://envoy.internal:9020" {
		t.Fatalf("explicit: boot=%#v err=%v", boot, err)
	}

	withURL["ENVOY_URL"] = "envoy.internal:9020"
	if _, err := resolveBootConfig(envGetter(withURL)); err == nil || !strings.Contains(err.Error(), "ENVOY_URL") {
		t.Fatalf("invalid: err = %v", err)
	}
}

func TestDispatchHandlerReportsDisabledNATS(t *testing.T) {
	handler := dispatchHandler(http.NewServeMux(), nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if nats, found := health["nats"]; !found || nats != nil {
		t.Fatalf("healthz nats = %#v, want null", nats)
	}
}

func TestDispatchHandlerReportsDisconnectedNATS(t *testing.T) {
	handler := dispatchHandler(http.NewServeMux(), nil, &bus.Client{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if nats, ok := health["nats"].(bool); !ok || nats {
		t.Fatalf("healthz nats = %#v, want false", nats)
	}
}

func envGetter(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func openSeedTestStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL, err := url.Parse(os.Getenv("DISPATCH_TEST_DATABASE_URL"))
	if err != nil || baseURL.String() == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run Postgres command tests")
	}
	adminURL := *baseURL
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("open test database admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	databaseName := "dispatch_command_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(context.Background(), "create database "+databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+databaseName+" with (force)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
	})

	testURL := *baseURL
	testURL.Path = "/" + databaseName
	database, err := store.Open(context.Background(), testURL.String())
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate isolated database: %v", err)
	}
	return database
}

func TestSeedRepoProjectsAddsMissingRowsWithoutOverwritingSettings(t *testing.T) {
	database := openSeedTestStore(t)
	ctx := context.Background()
	for _, project := range []string{"APP", "CORE"} {
		if _, err := database.Pool.Exec(ctx, "insert into projects (key, name) values ($1, $1)", project); err != nil {
			t.Fatalf("create project %q: %v", project, err)
		}
	}
	createdBy, err := json.Marshal(map[string]string{"kind": "user", "id": "alice"})
	if err != nil {
		t.Fatalf("encode actor: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by) values ('dashboard/repo', 'CORE', $1)
	`, createdBy); err != nil {
		t.Fatalf("create dashboard mapping: %v", err)
	}

	if err := seedRepoProjects(ctx, database, "Seed/Repo.git=APP,dashboard/repo=APP"); err != nil {
		t.Fatalf("seed repository projects: %v", err)
	}

	rows, err := database.Pool.Query(ctx, "select repo, project from repo_projects order by repo")
	if err != nil {
		t.Fatalf("list mappings: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var repo, project string
		if err := rows.Scan(&repo, &project); err != nil {
			t.Fatalf("scan mapping: %v", err)
		}
		got[repo] = project
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate mappings: %v", err)
	}
	if got["seed/repo"] != "APP" || got["dashboard/repo"] != "CORE" || len(got) != 2 {
		t.Fatalf("seeded mappings: got %#v", got)
	}
}

func TestSeedRepoProjectsRejectsMissingProject(t *testing.T) {
	database := openSeedTestStore(t)
	err := seedRepoProjects(context.Background(), database, "missing/repo=MISSING")
	if err == nil || !strings.Contains(err.Error(), `seed repository project "missing/repo"`) {
		t.Fatalf("seed missing project: got %v", err)
	}
}

func TestCheckDocumentsReportsLegacyParseFailures(t *testing.T) {
	database := openSeedTestStore(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ('TEST', 'Test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by)
		values ('TEST-1', 'TEST', 1, 'Legacy document', '{"kind":"user","id":"alice"}')
	`); err != nil {
		t.Fatal(err)
	}
	var artifactID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('TEST-1', 'TEST', 'legacy', 'legacy.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	doc := crdt.New()
	content := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		content.Insert(txn, 0, "<details>\nblock HTML\n</details>\n", nil)
	})
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := docs.NewPgVersioned(database).AppendUpdateTx(ctx, tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if exitCode := checkDocuments(ctx, database.Pool.Config().ConnString(), &output); exitCode != 1 {
		t.Fatalf("check-documents exit = %d, want 1; output=%s", exitCode, output.String())
	}
	if got := output.String(); !strings.Contains(got, artifactID) || !strings.Contains(got, "parse=error:") || !strings.Contains(got, "block HTML") {
		t.Fatalf("check-documents output = %q, want artifact parse failure", got)
	}
}

func TestParseAllowedLoginsLowerCasesAndTrimsEntries(t *testing.T) {
	logins := parseAllowedLogins(" sjawhar, Xodarap ,,")
	if len(logins) != 2 {
		t.Fatalf("logins: got %v, want two entries", logins)
	}
	for _, want := range []string{"sjawhar", "xodarap"} {
		if _, ok := logins[want]; !ok {
			t.Errorf("logins: missing %q in %v", want, logins)
		}
	}
}
