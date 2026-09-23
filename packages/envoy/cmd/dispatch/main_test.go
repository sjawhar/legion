package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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

// The issuer and the audience are one setting in two variables: half of it is a
// misconfiguration a deployment must not boot with, and neither means the server
// verifies no service-account token at all.
func TestResolveBootConfigRequiresBothOIDCVariables(t *testing.T) {
	base := map[string]string{
		"DATABASE_URL":            "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":    "agent-token",
		"DISPATCH_IDENTITY":       "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS": "sjawhar",
	}
	env := func(overrides map[string]string) func(string) string {
		values := map[string]string{}
		for key, value := range base {
			values[key] = value
		}
		for key, value := range overrides {
			values[key] = value
		}
		return envGetter(values)
	}

	boot, err := resolveBootConfig(env(nil))
	if err != nil || boot.OIDCIssuer != "" || boot.OIDCAudience != "" {
		t.Fatalf("unset: boot=%#v err=%v", boot, err)
	}

	_, err = resolveBootConfig(env(map[string]string{"DISPATCH_OIDC_ISSUER": "https://oidc.example"}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_OIDC_AUDIENCE") {
		t.Fatalf("issuer alone: err = %v, want one naming DISPATCH_OIDC_AUDIENCE", err)
	}

	_, err = resolveBootConfig(env(map[string]string{"DISPATCH_OIDC_AUDIENCE": "dispatch"}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_OIDC_ISSUER") {
		t.Fatalf("audience alone: err = %v, want one naming DISPATCH_OIDC_ISSUER", err)
	}

	boot, err = resolveBootConfig(env(map[string]string{
		"DISPATCH_OIDC_ISSUER":   "https://oidc.example",
		"DISPATCH_OIDC_AUDIENCE": "dispatch",
	}))
	if err != nil || boot.OIDCIssuer != "https://oidc.example" || boot.OIDCAudience != "dispatch" {
		t.Fatalf("both: boot=%#v err=%v", boot, err)
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

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

func TestSeedRepoProjectsAddsMissingRowsWithoutOverwritingSettings(t *testing.T) {
	database := storetest.Open(t)
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
	database := storetest.Open(t)
	err := seedRepoProjects(context.Background(), database, "missing/repo=MISSING")
	if err == nil || !strings.Contains(err.Error(), `seed repository project "missing/repo"`) {
		t.Fatalf("seed missing project: got %v", err)
	}
}

func TestWriteBlockIDBackfillReportLabelsSkippedDocuments(t *testing.T) {
	var output bytes.Buffer
	writeBlockIDBackfillReport(&output, docs.BlockIDBackfill{
		ArtifactID: "artifact-1",
		Skipped:    "service stopping",
	})
	const want = "artifact-1 skipped (service stopping)\n"
	if got := output.String(); got != want {
		t.Fatalf("backfill skipped output = %q, want %q", got, want)
	}
}

func TestWriteBlockIDBackfillReportLabelsDocumentErrors(t *testing.T) {
	var output bytes.Buffer
	if writeBlockIDBackfillReport(&output, docs.BlockIDBackfill{
		ArtifactID: "artifact-1",
		Err:        errors.New("persist update"),
	}) {
		t.Fatal("error report succeeded")
	}
	const want = "artifact-1 error (persist update)\n"
	if got := output.String(); got != want {
		t.Fatalf("backfill error output = %q, want %q", got, want)
	}
}

func TestWriteAnchorBlockBackfillReport(t *testing.T) {
	var output bytes.Buffer
	writeAnchorBlockBackfillReport(&output, docs.AnchorBlockBackfill{Asks: 2, Comments: 3, Skipped: 1})
	const want = "backfill-anchor-blocks: asks=2 comments=3 skipped=1\n"
	if got := output.String(); got != want {
		t.Fatalf("anchor block backfill output = %q, want %q", got, want)
	}
}

// An empty dashboard origin would make every URL-form mention unrecognisable and the rebuild
// would delete them all, so the subcommand refuses before it opens the database.
func TestRebuildRefsRefusesWithoutServerURL(t *testing.T) {
	var output bytes.Buffer
	if code := rebuildRefs(context.Background(), "postgres://unused", "  ", &output); code != 1 {
		t.Fatalf("rebuild-refs without server URL exited %d, want 1", code)
	}
	if !strings.Contains(output.String(), "dispatch.server_url is required") {
		t.Fatalf("rebuild-refs refusal = %q", output.String())
	}
	output.Reset()
	if code := rebuildRefs(context.Background(), "", "https://dispatch.example", &output); code != 1 || !strings.Contains(output.String(), "DATABASE_URL is required") {
		t.Fatalf("rebuild-refs without DATABASE_URL: code=%d output=%q", code, output.String())
	}
}

func TestWriteRebuildRefsReport(t *testing.T) {
	var output bytes.Buffer
	writeRebuildRefsReport(&output, refs.Rebuild{Documents: 4, Asks: 3, Comments: 2, Messages: 1, Orphans: 5, Edges: 9})
	const want = "rebuild-refs: documents=4 asks=3 comments=2 messages=1 orphans=5 edges=9\n"
	if got := output.String(); got != want {
		t.Fatalf("rebuild-refs output = %q, want %q", got, want)
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
