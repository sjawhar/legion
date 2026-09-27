package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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

// The credential-request UI feature is off by default (no broker URL configured), and
// resolveBootConfig must not require a token when the URL is unset.
func TestResolveBootConfigLeavesAgentSecretsOffByDefault(t *testing.T) {
	boot, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":            "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":    "agent-token",
		"DISPATCH_IDENTITY":       "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS": "sjawhar",
	}))
	if err != nil || boot.AgentSecretsURL != "" || boot.AgentSecretsToken != "" {
		t.Fatalf("boot=%#v err=%v, want the feature off", boot, err)
	}
}

func TestResolveBootConfigAcceptsAgentSecretsURLAndToken(t *testing.T) {
	boot, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":                 "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":         "agent-token",
		"DISPATCH_IDENTITY":            "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS":      "sjawhar",
		"DISPATCH_AGENT_SECRETS_URL":   "https://broker.internal/",
		"DISPATCH_AGENT_SECRETS_TOKEN": "ui-bearer",
	}))
	if err != nil {
		t.Fatalf("resolve boot config: %v", err)
	}
	if boot.AgentSecretsURL != "https://broker.internal" {
		t.Fatalf("AgentSecretsURL = %q, want the trailing slash trimmed", boot.AgentSecretsURL)
	}
	if boot.AgentSecretsToken != "ui-bearer" {
		t.Fatalf("AgentSecretsToken = %q, want ui-bearer", boot.AgentSecretsToken)
	}
}

// DISPATCH_AGENT_SECRETS_URL must be an absolute http(s) URL with no path, matching the
// ENVOY_URL check but additionally refusing a path component: the broker's client builds every
// call by appending a fixed path to this base.
func TestResolveBootConfigRejectsAgentSecretsURLWithPathOrBadScheme(t *testing.T) {
	base := map[string]string{
		"DATABASE_URL":                 "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":         "agent-token",
		"DISPATCH_IDENTITY":            "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS":      "sjawhar",
		"DISPATCH_AGENT_SECRETS_TOKEN": "ui-bearer",
	}
	for _, badURL := range []string{"broker.internal:9090", "https://broker.internal/v1"} {
		env := map[string]string{}
		for key, value := range base {
			env[key] = value
		}
		env["DISPATCH_AGENT_SECRETS_URL"] = badURL
		if _, err := resolveBootConfig(envGetter(env)); err == nil || !strings.Contains(err.Error(), "DISPATCH_AGENT_SECRETS_URL") {
			t.Fatalf("URL=%q: err = %v, want a DISPATCH_AGENT_SECRETS_URL rejection", badURL, err)
		}
	}
}

// A broker URL with no token at all -- bare or file-backed -- must refuse to boot rather than
// construct a client that authenticates with an empty bearer.
func TestResolveBootConfigRequiresTokenWhenAgentSecretsURLSet(t *testing.T) {
	_, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":               "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":       "agent-token",
		"DISPATCH_IDENTITY":          "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS":    "sjawhar",
		"DISPATCH_AGENT_SECRETS_URL": "https://broker.internal",
	}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_AGENT_SECRETS_TOKEN") {
		t.Fatalf("err = %v, want a token-required rejection", err)
	}
}

// DISPATCH_AGENT_SECRETS_TOKEN_FILE wins over the bare variable, and a set-but-unreadable file
// refuses to boot naming the path rather than silently falling back to the bare variable.
func TestResolveBootConfigAgentSecretsTokenFileWinsOverBareVariable(t *testing.T) {
	dir := t.TempDir()
	tokenPath := dir + "/agent-secrets-token"
	if err := os.WriteFile(tokenPath, []byte("  file-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	boot, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":                      "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":              "agent-token",
		"DISPATCH_IDENTITY":                 "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS":           "sjawhar",
		"DISPATCH_AGENT_SECRETS_URL":        "https://broker.internal",
		"DISPATCH_AGENT_SECRETS_TOKEN":      "bare-token",
		"DISPATCH_AGENT_SECRETS_TOKEN_FILE": tokenPath,
	}))
	if err != nil {
		t.Fatalf("resolve boot config: %v", err)
	}
	if boot.AgentSecretsToken != "file-token" {
		t.Fatalf("AgentSecretsToken = %q, want the file's trimmed contents to win", boot.AgentSecretsToken)
	}

	if _, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":                      "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":              "agent-token",
		"DISPATCH_IDENTITY":                 "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS":           "sjawhar",
		"DISPATCH_AGENT_SECRETS_URL":        "https://broker.internal",
		"DISPATCH_AGENT_SECRETS_TOKEN_FILE": dir + "/missing",
	})); err == nil || !strings.Contains(err.Error(), "DISPATCH_AGENT_SECRETS_TOKEN_FILE") {
		t.Fatalf("unreadable file: err = %v, want a DISPATCH_AGENT_SECRETS_TOKEN_FILE rejection", err)
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
	handler := dispatchHandler(http.NewServeMux(), nil, nil, "")
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
	handler := dispatchHandler(http.NewServeMux(), nil, &bus.Client{}, "")
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

// /healthz names what is deployed, for a deploy check to compare with what was meant to be:
// the commit the image build stamped, and the highest migration the database has applied. The
// schema version is the database's, read per probe, never the binary's own list: a row a later
// image applied shows up at once, which is the only reading under which comparing it with a
// commit's migrations tests anything.
func TestHealthzReportsTheBuildCommitAndTheAppliedSchemaVersion(t *testing.T) {
	database := storetest.Open(t)
	const commit = "0123456789abcdef0123456789abcdef01234567"
	handler := dispatchHandler(http.NewServeMux(), database, nil, commit)
	probe := func() map[string]any {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var health map[string]any
		if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
			t.Fatalf("decode health response: %v", err)
		}
		if response.Code != http.StatusOK || health["ok"] != true || health["db"] != true {
			t.Fatalf("healthz = %d %#v, want 200 with ok and db true", response.Code, health)
		}
		return health
	}

	highest := highestMigrationFile(t)
	health := probe()
	if health["commit"] != commit {
		t.Fatalf("healthz commit = %#v, want %q", health["commit"], commit)
	}
	if health["schema_version"] != float64(highest) {
		t.Fatalf("healthz schema_version = %#v, want %d, the highest migration file", health["schema_version"], highest)
	}

	if _, err := database.Pool.Exec(context.Background(), "insert into schema_migrations (version) values ($1)", highest+1); err != nil {
		t.Fatalf("record a later migration: %v", err)
	}
	if got := probe()["schema_version"]; got != float64(highest+1) {
		t.Fatalf("healthz schema_version after the database recorded %d = %#v, want the database's", highest+1, got)
	}
}

// A binary the image build did not stamp says so: null, never an empty string or a guess.
func TestHealthzReportsAnUnstampedCommitAsNull(t *testing.T) {
	response := httptest.NewRecorder()
	dispatchHandler(http.NewServeMux(), nil, nil, "").ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	for _, field := range []string{"commit", "schema_version"} {
		if value, found := health[field]; !found || value != nil {
			t.Fatalf("healthz %s = %#v (present %t), want null", field, value, found)
		}
	}
}

// highestMigrationFile is the number of the last up migration in the source tree, read the way
// store.Migrate names versions: the digits before the first underscore.
func highestMigrationFile(t *testing.T) int {
	t.Helper()
	names, err := filepath.Glob("../../internal/dispatch/store/migrations/*.up.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(names))
	}
	highest := 0
	for _, name := range names {
		prefix, _, _ := strings.Cut(filepath.Base(name), "_")
		version, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
		highest = max(highest, version)
	}
	return highest
}

// /healthz answers while every connection of the shared pool is held. A busy period
// legitimately empties that pool - writers queued on one issue's row lock hold theirs until
// they commit - and a probe that queues behind them is read as a dead process: the ALB fails
// it at five seconds, ECS replaces the task, and every in-flight request of every other client
// is cancelled. The probe therefore takes its connection from a pool of its own, and the short
// client timeout here is what tells a queued probe from an answered one.
func TestHealthzAnswersWhileEveryPooledConnectionIsHeld(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	for held := int32(0); held < database.Pool.Config().MaxConns; held++ {
		connection, err := database.Pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold pooled connection %d: %v", held, err)
		}
		defer connection.Release()
	}

	server := httptest.NewServer(dispatchHandler(http.NewServeMux(), database, nil, ""))
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	started := time.Now()
	response, err := client.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("health probe with every pooled connection held: %v (after %s)", err, time.Since(started))
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if response.StatusCode != http.StatusOK || health["ok"] != true || health["db"] != true {
		t.Fatalf("health probe = %d %#v, want 200 with ok and db true", response.StatusCode, health)
	}
}

// /healthz answers inside the load balancer's timeout when Postgres stops answering at all:
// packets dropped, nothing refused and nothing reset, which is what a security-group change, an
// availability-zone partition, or an endpoint that accepts and then stalls looks like to the
// client. A stopped container cannot produce it - its port refuses instantly - so the probe's
// own wait only shows here. Without store.healthProbeTimeout that wait is two minutes on a
// cold dial, which pgxpool floors it at, and unbounded once the connection is up, because an
// http.Server request context carries no deadline: either way the probe does not answer, and
// three polls that never answer replace the task and cancel every in-flight request of every
// other client - the outcome the health pool exists to prevent. 503 is the right answer here;
// silence is not.
//
// The health pool opens on the first probe, so black-holing the link before that probe leaves
// nothing to race: the connection this test hangs on is dialled after the outage begins. That
// makes this the cold case; the warm one, which production actually runs, is the unbounded
// one and is measured against the real binary rather than here.
func TestHealthzAnswersWhilePostgresStopsAnswering(t *testing.T) {
	migrated := storetest.Open(t)
	dsn, err := url.Parse(migrated.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	var blackholed atomic.Bool
	dsn.Host = blackholePostgres(t, dsn.Host, &blackholed)

	database, err := store.Open(context.Background(), dsn.String())
	if err != nil {
		t.Fatalf("open the store through the proxy: %v", err)
	}
	t.Cleanup(func() {
		blackholed.Store(false)
		database.Pool.Close()
	})

	server := httptest.NewServer(dispatchHandler(http.NewServeMux(), database, nil, ""))
	defer server.Close()
	// The shared pool's own round trip is the control: the link works right up to the outage.
	if _, err := database.Pool.Exec(context.Background(), "select 1"); err != nil {
		t.Fatalf("query through the proxy before the outage: %v", err)
	}

	// The load balancer's own timeout: a poll it has not heard back from in five seconds is a
	// failure, and three consecutive failures replace the task.
	client := &http.Client{Timeout: 5 * time.Second}

	blackholed.Store(true)
	started := time.Now()
	response, err := client.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("health probe with Postgres unreachable: %v (after %s)", err, time.Since(started))
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable || health["db"] != false || health["schema_version"] != nil {
		t.Fatalf("health probe with Postgres unreachable = %d %#v, want 503 with db false and no schema version",
			response.StatusCode, health)
	}
}

// blackholePostgres puts a proxy in front of upstream and returns its address. While blackholed
// is set, bytes stop moving in both directions and nothing is refused, reset or closed, so a
// client waiting on a reply waits for as long as its own deadline allows.
func blackholePostgres(t *testing.T, upstream string, blackholed *atomic.Bool) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the Postgres proxy: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go proxyPostgresConnection(client, upstream, blackholed)
		}
	}()
	return listener.Addr().String()
}

// proxyPostgresConnection completes the client's handshake even while the link is black-holed
// and only then dials upstream, which is what a dropped SYN-ACK path leaves a client holding: a
// connected socket nobody is answering on.
func proxyPostgresConnection(client net.Conn, upstream string, blackholed *atomic.Bool) {
	defer client.Close()
	for blackholed.Load() {
		time.Sleep(20 * time.Millisecond)
	}
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		return
	}
	defer server.Close()
	done := make(chan struct{}, 2)
	go pumpUntilBlackholed(server, client, blackholed, done)
	go pumpUntilBlackholed(client, server, blackholed, done)
	<-done
}

// pumpUntilBlackholed copies src to dst, freezing while the link is black-holed rather than
// tearing the connection down: a dropped packet does not close a socket.
func pumpUntilBlackholed(dst, src net.Conn, blackholed *atomic.Bool, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	buffer := make([]byte, 32*1024)
	for {
		if blackholed.Load() {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if err := src.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
			return
		}
		read, err := src.Read(buffer)
		if read > 0 {
			if _, err := dst.Write(buffer[:read]); err != nil {
				return
			}
		}
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				continue
			}
			return
		}
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
