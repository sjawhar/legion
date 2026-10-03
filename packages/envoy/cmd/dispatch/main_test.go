package main

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"github.com/sjawhar/envoy/internal/dispatch/auth"
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

// TestMain runs the package's tests, or, run by a memory test with memoryTestServeEnv set, is that
// test's Dispatch server.
func TestMain(m *testing.M) {
	if os.Getenv(memoryTestServeEnv) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(storetest.Main(m))
}

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

// An argument envoy-dispatch does not know is refused, never served: the server migrates the
// database at boot, so `envoy-dispatch census` on an image that predates the subcommand would
// otherwise apply the very migrations the census was to inspect.
func TestRunSubcommandRefusesAnUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSubcommand(context.Background(), []string{"cenus"}, func(string) string { return "" }, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	for _, want := range []string{`unknown subcommand "cenus"`, "census", "redeliver-webhooks"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q lacks %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), "DATABASE_URL") {
		t.Errorf("stderr %q reads like a boot, not a refusal", stderr.String())
	}
}

// The census exits 2 when it could not be taken and 0 when nothing refuses, and prints the report
// to stdout.
func TestCensusSubcommandExitCodes(t *testing.T) {
	ctx := context.Background()
	var out, errOut bytes.Buffer
	if code := census(ctx, "", &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "DATABASE_URL is required") {
		t.Errorf("no URL: exit %d, stderr %q", code, errOut.String())
	}
	errOut.Reset()
	if code := census(ctx, "postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "census: connect") {
		t.Errorf("unreachable: exit %d, stderr %q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("a census that could not be taken printed a report: %q", out.String())
	}
	database := storetest.Open(t)
	out.Reset()
	errOut.Reset()
	if code := census(ctx, database.Pool.Config().ConnString(), &out, &errOut); code != 0 {
		t.Fatalf("migrated database: exit %d, stderr %q, stdout %q", code, errOut.String(), out.String())
	}
	if !strings.Contains(out.String(), "no pending migration") || !strings.HasSuffix(strings.TrimSpace(out.String()), "census: ok") {
		t.Errorf("stdout:\n%s", out.String())
	}
}

// pgmigrate.Census already prefixes its errors with "census:", so the command prints that
// prefix once when a census cannot be taken.
func TestCensusSubcommandPrintsACensusErrorWithOnePrefix(t *testing.T) {
	database := storetest.Open(t)
	var out, errOut bytes.Buffer
	if code := census(context.Background(), database.Pool.Config().ConnString()+"&search_path=nosuch", &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2; stderr %q", code, errOut.String())
	}
	const want = "census: the connection's search_path names no schema that exists, so current_schema() is null: the census would find neither schema_migrations nor any table, and the runner can create neither; name an existing schema in the search_path\n"
	if got := errOut.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// A database at 52 holding a row 0053's check refuses is refused by the embedded 0053 census,
// exit 1, and the report names the migration, the count and the census file.
func TestCensusSubcommandExitsOneWhenAPendingMigrationsCensusCountsRows(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		alter table asks drop constraint asks_approval_kind_check;
		delete from schema_migrations where version >= 53;
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Spec', '{"kind":"session","id":"s"}', 'U');
		insert into asks (issue_key, author, question, options, kind, approval)
			values ('CORE-1', '{"kind":"session","id":"s"}', 'A question?', '[]', 'question', '{}');
	`); err != nil {
		t.Fatalf("seed a database at 52 with a row 0053 refuses: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := census(ctx, database.Pool.Config().ConnString(), &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %q, stdout:\n%s", code, errOut.String(), out.String())
	}
	for _, want := range []string{
		"census: schema version 52 (schema_migrations); pending: 0053_asks_approval_kind_check.up.sql, 0054_message_delivery_acceptance.up.sql, 0055_broadcast_idempotency_keys.up.sql",
		"census: REFUSED 0053_asks_approval_kind_check.up.sql: its census counts 1 (0053_asks_approval_kind_check.census.sql)",
		"census: 0055_broadcast_idempotency_keys.up.sql touches broadcasts: 0 rows, ",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
	if last := strings.TrimSpace(out.String()); !strings.HasSuffix(last, "census: REFUSED (1 reason)") {
		t.Errorf("stdout does not end with the verdict:\n%s", out.String())
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

// devSignInEnvironment is a boot environment the dev sign-in fence accepts, with overrides
// applied; an override of "" leaves that variable empty, which resolveBootConfig reads as unset.
func devSignInEnvironment(overrides map[string]string) func(string) string {
	values := map[string]string{
		"DATABASE_URL":            "postgres://postgres:x@127.0.0.1:5432/dispatch_e2e?sslmode=disable",
		"DISPATCH_AGENT_TOKEN":    "x",
		"DISPATCH_ALLOWED_LOGINS": "alice",
		"DISPATCH_LISTEN_HOST":    "127.0.0.1",
		"DISPATCH_DEV_SIGNIN":     "1",
	}
	for key, value := range overrides {
		values[key] = value
	}
	return envGetter(values)
}

func TestResolveBootConfigDevSignInFlagValue(t *testing.T) {
	// Off (getenv answers "" for an unset variable as for an empty one), the fence asks nothing of
	// the rest of the environment.
	boot, err := resolveBootConfig(devSignInEnvironment(map[string]string{
		"DISPATCH_DEV_SIGNIN":  "",
		"DISPATCH_LISTEN_HOST": "0.0.0.0",
		"DATABASE_URL":         "postgres://u@db.example.com/d",
		"DISPATCH_SIGNING_KEY": "abc",
	}))
	if err != nil || boot.DevSignIn {
		t.Fatalf("DISPATCH_DEV_SIGNIN unset: DevSignIn=%t err=%v, want off", boot.DevSignIn, err)
	}

	boot, err = resolveBootConfig(devSignInEnvironment(nil))
	if err != nil || !boot.DevSignIn {
		t.Fatalf("DISPATCH_DEV_SIGNIN=1: DevSignIn=%t err=%v, want on", boot.DevSignIn, err)
	}

	for _, flag := range []string{"true", "yes", "0", " 1"} {
		_, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_DEV_SIGNIN": flag}))
		if err == nil || !strings.Contains(err.Error(), "DISPATCH_DEV_SIGNIN") || !strings.Contains(err.Error(), strconv.Quote(flag)) {
			t.Errorf("DISPATCH_DEV_SIGNIN=%q: err = %v, want a refusal naming the variable and the value", flag, err)
		}
	}
}

func TestResolveBootConfigDevSignInRequiresCookieIdentity(t *testing.T) {
	_, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_IDENTITY": "header:X-Dispatch-User"}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_IDENTITY") {
		t.Fatalf("header identity: err = %v, want a refusal naming DISPATCH_IDENTITY", err)
	}
}

// The fence checks the address the server binds, not a second reading of the variable: every
// spelling it accepts yields boot.ListenAddr, listenAddress's one value, which main binds.
func TestResolveBootConfigDevSignInChecksTheAddressItBinds(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "::", "10.0.0.5", "localhost", "::ffff:127.0.0.1", "[::1%lo]", "]]::1[[", "[[::1]]", "127.0.0.1:9000", "[::1]:9000"} {
		boot, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_LISTEN_HOST": host}))
		if err == nil || !strings.Contains(err.Error(), "DISPATCH_LISTEN_HOST") {
			t.Errorf("DISPATCH_LISTEN_HOST=%q: listen address %q, err = %v; want a refusal naming DISPATCH_LISTEN_HOST", host, boot.ListenAddr, err)
		}
	}
	for host, want := range map[string]string{
		"127.0.0.1":       "127.0.0.1:8799",
		" 127.0.0.1 ":     "127.0.0.1:8799",
		"[127.0.0.1]":     "127.0.0.1:8799",
		"127.255.255.254": "127.255.255.254:8799",
		"::1":             "[::1]:8799",
		"[::1]":           "[::1]:8799",
		"0:0:0:0:0:0:0:1": "[0:0:0:0:0:0:0:1]:8799",
	} {
		environment := devSignInEnvironment(map[string]string{"DISPATCH_LISTEN_HOST": host, "DISPATCH_PORT": "8799"})
		bound, err := listenAddress(environment)
		if err != nil || bound != want {
			t.Fatalf("DISPATCH_LISTEN_HOST=%q: listenAddress = %q, %v; want %q", host, bound, err, want)
		}
		boot, err := resolveBootConfig(environment)
		if err != nil || boot.ListenAddr != bound {
			t.Errorf("DISPATCH_LISTEN_HOST=%q: ListenAddr = %q, %v; want the bound %q accepted", host, boot.ListenAddr, err, bound)
		}
	}

	// Without the flag the same field carries the address, so main binds one value either way, and
	// a bad port is refused before anything connects.
	boot, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_DEV_SIGNIN": "", "DISPATCH_LISTEN_HOST": "", "DISPATCH_PORT": "8799"}))
	if err != nil || boot.ListenAddr != ":8799" {
		t.Errorf("without the flag: ListenAddr = %q, %v; want \":8799\"", boot.ListenAddr, err)
	}
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_DEV_SIGNIN": "", "DISPATCH_PORT": "70000"})); err == nil || !strings.Contains(err.Error(), "DISPATCH_PORT") {
		t.Errorf("DISPATCH_PORT=70000: err = %v, want a refusal naming DISPATCH_PORT", err)
	}
}

func TestResolveBootConfigDevSignInRequiresALoopbackDatabase(t *testing.T) {
	for _, tc := range []struct{ databaseURL, host string }{
		{databaseURL: "postgres://postgres:x@db.example.com:5432/d", host: "db.example.com"},
		{databaseURL: "postgres://u@10.0.0.5/d", host: "10.0.0.5"},
		{databaseURL: "postgres://u@127.0.0.1:5432,db.example.com:5432/d", host: "db.example.com"},
		{databaseURL: "host=db.example.com dbname=d", host: "db.example.com"},
		// pgx dials the host parameter, not the URL's authority.
		{databaseURL: "postgres://u@127.0.0.1:5432/d?host=db.example.com", host: "db.example.com"},
	} {
		_, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DATABASE_URL": tc.databaseURL}))
		if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") || !strings.Contains(err.Error(), tc.host) {
			t.Errorf("DATABASE_URL=%q: err = %v, want a refusal naming DATABASE_URL and %s", tc.databaseURL, err, tc.host)
		}
	}
	for _, databaseURL := range []string{
		"postgres://u@127.0.0.1:5432/d",
		"postgres://u@localhost:5432/d",
		"postgres://u@[::1]:5432/d",
		"postgres:///d?host=/var/run/postgresql",
		"host=/tmp dbname=d",
	} {
		if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DATABASE_URL": databaseURL})); err != nil {
			t.Errorf("DATABASE_URL=%q: %v, want a loopback database accepted", databaseURL, err)
		}
	}

	// A URL naming no host is dialled at libpq's PGHOST default, which the check reads too.
	t.Setenv("PGHOST", "db.example.com")
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DATABASE_URL": "postgres:///d"})); err == nil || !strings.Contains(err.Error(), "db.example.com") {
		t.Errorf("DATABASE_URL=postgres:///d with PGHOST=db.example.com: err = %v, want a refusal naming db.example.com", err)
	}
}

func TestResolveBootConfigDevSignInRefusesAConfiguredSigningKey(t *testing.T) {
	_, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_SIGNING_KEY": "abc"}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_SIGNING_KEY") {
		t.Fatalf("configured signing key: err = %v, want a refusal naming DISPATCH_SIGNING_KEY", err)
	}
}

// With the flag a loopback client acts as any allowlisted human, so the process may not publish
// into another machine's NATS.
func TestResolveBootConfigDevSignInRefusesRemoteNATS(t *testing.T) {
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_ALLOW_REMOTE_NATS": "1"})); err == nil || !strings.Contains(err.Error(), "ENVOY_ALLOW_REMOTE_NATS") {
		t.Fatalf("ENVOY_ALLOW_REMOTE_NATS=1 with NATS on: err = %v, want a refusal naming ENVOY_ALLOW_REMOTE_NATS", err)
	}
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_ALLOW_REMOTE_NATS": "1", "DISPATCH_NATS_DISABLED": "1"})); err != nil {
		t.Fatalf("ENVOY_ALLOW_REMOTE_NATS=1 with NATS off: %v, want accepted", err)
	}
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_ALLOW_REMOTE_NATS": "1", "DISPATCH_DEV_SIGNIN": ""})); err != nil {
		t.Fatalf("ENVOY_ALLOW_REMOTE_NATS=1 without the flag: %v, want accepted", err)
	}
}

// With the flag a loopback client acts as any allowlisted human, so the process may not decide
// another machine's secrets broker requests as one.
func TestResolveBootConfigDevSignInRefusesARemoteAgentSecretsBroker(t *testing.T) {
	broker := func(brokerURL string) map[string]string {
		return map[string]string{"DISPATCH_AGENT_SECRETS_URL": brokerURL, "DISPATCH_AGENT_SECRETS_TOKEN": "ui-token"}
	}
	for _, brokerURL := range []string{"https://broker.example.com", "http://10.0.0.5:13380"} {
		if _, err := resolveBootConfig(devSignInEnvironment(broker(brokerURL))); err == nil || !strings.Contains(err.Error(), "DISPATCH_AGENT_SECRETS_URL") {
			t.Errorf("DISPATCH_AGENT_SECRETS_URL=%s: err = %v, want a refusal naming DISPATCH_AGENT_SECRETS_URL", brokerURL, err)
		}
	}
	for _, brokerURL := range []string{"http://127.0.0.1:13380", "http://localhost:13380", "http://[::1]:13380"} {
		if _, err := resolveBootConfig(devSignInEnvironment(broker(brokerURL))); err != nil {
			t.Errorf("DISPATCH_AGENT_SECRETS_URL=%s: %v, want a loopback broker accepted", brokerURL, err)
		}
	}
	withoutFlag := broker("https://broker.example.com")
	withoutFlag["DISPATCH_DEV_SIGNIN"] = ""
	if _, err := resolveBootConfig(devSignInEnvironment(withoutFlag)); err != nil {
		t.Errorf("a remote broker without the flag: %v, want accepted", err)
	}
}

// With the flag a loopback client acts as any allowlisted human, so the process may not deliver
// that human's mentions through another machine's Envoy listener.
func TestResolveBootConfigDevSignInRefusesARemoteEnvoyListener(t *testing.T) {
	for _, listenerURL := range []string{"https://listener.example.com", "http://10.0.0.5:9020"} {
		if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_URL": listenerURL})); err == nil || !strings.Contains(err.Error(), "ENVOY_URL") {
			t.Errorf("ENVOY_URL=%s: err = %v, want a refusal naming ENVOY_URL", listenerURL, err)
		}
	}
	// Unset, ENVOY_URL defaults to this machine's listener.
	for _, listenerURL := range []string{"", "http://127.0.0.1:9020", "http://localhost:9020", "http://[::1]:9020"} {
		if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_URL": listenerURL})); err != nil {
			t.Errorf("ENVOY_URL=%q: %v, want a loopback listener accepted", listenerURL, err)
		}
	}
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_URL": "https://listener.example.com", "DISPATCH_DEV_SIGNIN": ""})); err != nil {
		t.Errorf("a remote listener without the flag: %v, want accepted", err)
	}
}

// devSignInOrigin is a dashboard origin the dev sign-in fence accepts.
const devSignInOrigin = "http://127.0.0.1:8799"

// With the flag a loopback client acts as any allowlisted human, and a human can have the App probe
// and import any repository it is installed on, so an App private key from the environment may
// sign calls only to a loopback host.
func TestDevSignInRefusesAnAppKeyThatReachesGitHub(t *testing.T) {
	withKey := &auth.AppConfig{ClientID: "Iv1.app", ClientSecret: "secret", PEM: "app private key"}
	fromEnv := appCredentialSource{}
	// Unset, the App calls https://api.github.com.
	for _, base := range []string{"", "https://api.github.com", "http://10.0.0.5:9022", "http://127.0.0.1@api.github.com"} {
		err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_GITHUB_API_BASE": base}), devSignInOrigin, withKey, fromEnv)
		if err == nil || !strings.Contains(err.Error(), "DISPATCH_GITHUB_API_BASE="+strconv.Quote(base)) || !strings.Contains(err.Error(), "DISPATCH_APP_PEM_B64") {
			t.Errorf("DISPATCH_GITHUB_API_BASE=%q: err = %v, want a refusal naming DISPATCH_GITHUB_API_BASE and DISPATCH_APP_PEM_B64", base, err)
		}
	}
	// The e2e harness points the App at its fake GitHub on 127.0.0.1 (packages/dispatch/e2e/run-server.sh).
	for _, base := range []string{"http://127.0.0.1:9022", "http://localhost:9022", "http://[::1]:9022"} {
		if err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_GITHUB_API_BASE": base}), devSignInOrigin, withKey, fromEnv); err != nil {
			t.Errorf("DISPATCH_GITHUB_API_BASE=%s: %v, want a loopback host accepted", base, err)
		}
	}
	// No App, or an App without its private key, signs no App call.
	for _, app := range []*auth.AppConfig{nil, {ClientID: "Iv1.app", ClientSecret: "secret"}} {
		if err := devSignInLoadedFence(devSignInBoot(t, nil), devSignInOrigin, app, fromEnv); err != nil {
			t.Errorf("App %+v: %v, want accepted", app, err)
		}
	}
	if err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_DEV_SIGNIN": ""}), devSignInOrigin, withKey, fromEnv); err != nil {
		t.Errorf("an App key reaching GitHub without the flag: %v, want accepted", err)
	}
}

// The dashboard origin comes from envoy.json, which main reads after the environment, so the fence
// over loaded values checks it, before anything connects.
func TestDevSignInLoadedFenceRefusesAPublicOrigin(t *testing.T) {
	const public = "https://dispatch.example.com"
	if err := devSignInLoadedFence(devSignInBoot(t, nil), public, nil, appCredentialSource{}); err == nil || !strings.Contains(err.Error(), "DISPATCH_SERVER_URL") {
		t.Errorf("origin %s with the flag: err = %v, want a refusal naming DISPATCH_SERVER_URL", public, err)
	}
	if err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_DEV_SIGNIN": ""}), public, nil, appCredentialSource{}); err != nil {
		t.Errorf("origin %s without the flag: %v, want accepted", public, err)
	}
}

// app.json is where a developer keeps the real App's key, and a loopback base proves only where the
// port is, not that a fake owns it. So with the flag a key from app.json is refused whatever the
// base, naming the file and the environment key that replaces it, while a throwaway key in the
// environment with a loopback fake boots, as packages/dispatch/e2e/run-server.sh's does.
func TestDevSignInRefusesAnAppJSONKeyWhateverTheBase(t *testing.T) {
	const fake = "http://127.0.0.1:9022"
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "app.json")
	if err := os.WriteFile(path, []byte(`{"clientId":"Iv1.file","clientSecret":"secret","pem":"app private key"}`), 0o600); err != nil {
		t.Fatalf("write app.json: %v", err)
	}
	t.Setenv("DISPATCH_APP_CLIENT_ID", "")
	app, source, err := loadAppCredentials(dataDir)
	if err != nil || app == nil {
		t.Fatalf("loadAppCredentials from app.json: %+v, %v", app, err)
	}
	for _, base := range []string{fake, "http://localhost:9022", "http://[::1]:9022", "", "https://api.github.com"} {
		err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_GITHUB_API_BASE": base}), devSignInOrigin, app, source)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "DISPATCH_APP_PEM_B64") {
			t.Errorf("a key in app.json, DISPATCH_GITHUB_API_BASE=%q: err = %v, want a refusal naming %s and DISPATCH_APP_PEM_B64", base, err, path)
		}
	}
	if err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_DEV_SIGNIN": ""}), devSignInOrigin, app, source); err != nil {
		t.Errorf("a key in app.json without the flag: %v, want accepted", err)
	}

	t.Setenv("DISPATCH_APP_CLIENT_ID", "Iv1.env")
	t.Setenv("DISPATCH_APP_CLIENT_SECRET", "secret")
	t.Setenv("DISPATCH_APP_PEM_B64", base64.StdEncoding.EncodeToString([]byte("throwaway key")))
	app, source, err = loadAppCredentials(dataDir)
	if err != nil || app == nil {
		t.Fatalf("loadAppCredentials from the environment: %+v, %v", app, err)
	}
	if err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_GITHUB_API_BASE": fake}), devSignInOrigin, app, source); err != nil {
		t.Errorf("a key in DISPATCH_APP_PEM_B64 with a loopback fake: %v, want accepted", err)
	}
	if err := devSignInLoadedFence(devSignInBoot(t, nil), devSignInOrigin, app, source); err == nil || !strings.Contains(err.Error(), "DISPATCH_APP_PEM_B64") || strings.Contains(err.Error(), path) {
		t.Errorf("a key in DISPATCH_APP_PEM_B64, DISPATCH_GITHUB_API_BASE unset: err = %v, want a refusal naming DISPATCH_APP_PEM_B64 and not %s", err, path)
	}
}

// devSignInBoot resolves devSignInEnvironment(overrides), which the boot fence accepts.
func devSignInBoot(t *testing.T, overrides map[string]string) bootConfig {
	t.Helper()
	boot, err := resolveBootConfig(devSignInEnvironment(overrides))
	if err != nil {
		t.Fatalf("resolveBootConfig(%v): %v", overrides, err)
	}
	return boot
}

func TestListenAddressJoinsAnIPv6LoopbackHost(t *testing.T) {
	for _, tc := range []struct{ host, port, want string }{
		{host: "::1", port: "", want: "[::1]:8766"},
		{host: "::1", port: "8799", want: "[::1]:8799"},
		{host: "[::1]", port: "", want: "[::1]:8766"},
		{host: "[::1]", port: "8799", want: "[::1]:8799"},
		{host: "127.0.0.1", port: "8799", want: "127.0.0.1:8799"},
		{host: "127.0.0.1", port: "", want: "127.0.0.1:8766"},
		{host: "", port: "", want: ":8766"},
		{host: "", port: "8799", want: ":8799"},
	} {
		got, err := listenAddress(envGetter(map[string]string{"DISPATCH_LISTEN_HOST": tc.host, "DISPATCH_PORT": tc.port}))
		if err != nil || got != tc.want {
			t.Errorf("host %q port %q: listenAddress = %q, %v; want %q", tc.host, tc.port, got, err, tc.want)
		}
	}
	for _, port := range []string{"0", "70000"} {
		if got, err := listenAddress(envGetter(map[string]string{"DISPATCH_LISTEN_HOST": "127.0.0.1", "DISPATCH_PORT": port})); err == nil || !strings.Contains(err.Error(), "DISPATCH_PORT") {
			t.Errorf("port %q: listenAddress = %q, %v; want a refusal naming DISPATCH_PORT", port, got, err)
		}
	}
}
