// Command dispatch serves the Dispatch dashboard, GitHub OAuth flow, and
// per-user GitHub REST and GraphQL proxy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/config"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	// defaultListenPort is the port when DISPATCH_PORT is unset; listenAddress joins it to the host.
	defaultListenPort = "8766"
	shutdownTimout    = 5 * time.Second
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
)

// buildCommit is the legion commit this binary was built from. Only the image build stamps it
// (packages/envoy/docker/Dockerfile passes its LEGION_COMMIT build argument through -ldflags -X,
// and refuses anything but a full commit sha); every other build leaves it empty, and /healthz
// then reports the commit as null rather than naming one it cannot vouch for.
var buildCommit string

type bootConfig struct {
	DatabaseURL      string
	AgentToken       string
	RepoProjects     string
	DefaultProject   string
	EnvoyURL         string
	GitHubAPIBase    string
	IdentityHeader   string
	AllowedLogins    map[string]struct{}
	NATSDisabled     bool
	TestHooksEnabled bool
	// OIDCIssuer and OIDCAudience configure verification of projected
	// service-account tokens. Both set or neither; empty means no verifier.
	OIDCIssuer   string
	OIDCAudience string
	// AgentSecretsURL is DISPATCH_AGENT_SECRETS_URL, the secrets broker's UI-bearer API; empty
	// means the credential-request feature is off. AgentSecretsToken is the resolved UI bearer
	// (required when AgentSecretsURL is set).
	AgentSecretsURL   string
	AgentSecretsToken string
	// ListenAddr is the address the server binds, from DISPATCH_LISTEN_HOST and DISPATCH_PORT
	// (listenAddress). The dev sign-in fence checks this value, so what it checks is what binds.
	ListenAddr string
	// DevSignIn (DISPATCH_DEV_SIGNIN=1) mounts GET /auth/_dev/signin behind the dev sign-in
	// fence; the signing key is then per process.
	DevSignIn bool
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if len(os.Args) > 1 {
		os.Exit(runSubcommand(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
	}
	boot, err := resolveBootConfig(os.Getenv)
	if err != nil {
		slog.Error("dispatch: resolve boot config", "error", err)
		os.Exit(1)
	}

	envoyConfig, err := config.Load(config.LoadOptions{})
	if err != nil {
		slog.Error("dispatch: load envoy config", "error", err)
		os.Exit(1)
	}
	serverURL := ""
	if envoyConfig.Dispatch != nil {
		serverURL = envoyConfig.Dispatch.ServerURL
	}
	// The App credentials are read before anything connects, so the dev sign-in fence refuses a
	// key it will not sign with before NATS or Postgres is dialled.
	dataDir, err := defaultDataDir()
	if err != nil {
		slog.Error("dispatch: resolve data dir", "error", err)
		os.Exit(1)
	}
	appCfg, appSource, err := loadAppCredentials(dataDir)
	if err != nil {
		slog.Error("dispatch: load app credentials", "error", err)
		os.Exit(1)
	}
	if err := devSignInLoadedFence(boot, serverURL, appCfg, appSource); err != nil {
		slog.Error("dispatch: dev sign-in", "error", err)
		os.Exit(1)
	}
	if appCfg == nil {
		slog.Info("dispatch: no app credentials yet — dashboard will respond 503 until configured")
	} else {
		slog.Info("dispatch: loaded github app", "slug", appCfg.Slug, "client_id", appCfg.ClientID, "source", appSource.String())
	}
	if boot.DevSignIn {
		slog.Warn("dispatch: dev sign-in mounted: any allowlisted login signs in at /auth/_dev/signin without GitHub; cookies are valid on this process only", "origin", serverURL)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var natsClient *bus.Client
	if boot.NATSDisabled {
		slog.Info("dispatch: NATS publisher disabled")
	} else {
		natsClient, err = bus.ConnectOwningStream(envoyConfig.NatsURLs)
		if err != nil {
			slog.Error("dispatch: connect NATS", "error", err)
			os.Exit(1)
		}
		defer natsClient.Close()
	}

	// The live agent conversation relay takes a connection of its own: a viewer's core
	// subscription must not be lost to the publisher client replacing a shared one, and this
	// connection never touches JetStream. With test hooks and no NATS it is an in-process
	// source the e2e harness publishes into instead.
	var agentStream agentstream.Source
	if natsClient == nil {
		if boot.TestHooksEnabled {
			agentStream = agentstream.NewMemory()
			slog.Info("dispatch: agent conversation relay served from the test hook")
		} else {
			slog.Info("dispatch: agent conversation relay off: it needs NATS")
		}
	} else {
		streamConn, err := bus.Dial("dispatch-agent-stream", envoyConfig.NatsURLs)
		if err != nil {
			slog.Error("dispatch: connect the agent conversation relay", "error", err)
			os.Exit(1)
		}
		defer streamConn.Close()
		agentStream = agentstream.NewNATS(streamConn)
	}

	database, err := store.Open(ctx, boot.DatabaseURL)
	if err != nil {
		slog.Error("dispatch: open database", "error", err)
		os.Exit(1)
	}
	defer database.Pool.Close()
	if err := database.Migrate(ctx); err != nil {
		slog.Error("dispatch: migrate database", "error", err)
		os.Exit(1)
	}

	if err := seedRepoProjects(ctx, database, boot.RepoProjects); err != nil {
		slog.Error("dispatch: seed repository projects", "error", err)
		os.Exit(1)
	}
	if err := validateDefaultProject(ctx, database, boot.DefaultProject); err != nil {
		slog.Error("dispatch: validate default project", "error", err)
		os.Exit(1)
	}

	webDistDir, err := defaultWebDistDir()
	if err != nil {
		slog.Error("dispatch: resolve web dist dir", "error", err)
		os.Exit(1)
	}

	var signingKey string
	if boot.DevSignIn {
		signingKey, err = auth.NewSigningKey()
	} else {
		signingKey, err = auth.LoadSigningKey(filepath.Join(dataDir, "signing-key"))
	}
	if err != nil {
		slog.Error("dispatch: load signing key", "error", err)
		os.Exit(1)
	}

	users := store.NewPgUserStore(database.Pool)
	sessions := store.NewPgSessionStore(database.Pool)

	var requestIdentity identity.Identity
	if boot.IdentityHeader == "" {
		requestIdentity = identity.CookieIdentity{
			SigningKey:    signingKey,
			AllowedLogins: boot.AllowedLogins,
			Sessions:      sessions,
		}
	} else {
		slog.Warn("dispatch: trusting request identity header", "header", boot.IdentityHeader)
		requestIdentity = identity.HeaderIdentity{
			Header:        boot.IdentityHeader,
			AllowedLogins: boot.AllowedLogins,
		}
	}

	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store:      database,
		Events:     broker,
		Identity:   requestIdentity,
		AgentToken: boot.AgentToken,
		ServerURL:  serverURL,
	})

	serviceTokens, err := oidc.Discover(ctx, boot.OIDCIssuer, boot.OIDCAudience, oidc.DiscoveryTimeout)
	if err != nil {
		slog.Error("dispatch: discover OIDC issuer", "error", err)
		os.Exit(1)
	}
	if serviceTokens != nil {
		slog.Info("dispatch: verifying service-account tokens", "issuer", boot.OIDCIssuer, "audience", boot.OIDCAudience)
	}

	appCtx, err := routes.BuildAppContext(routes.AppContextOptions{
		SigningKey: signingKey,
		WebDistDir: webDistDir,
		Users:      users,
		Sessions:   sessions,
		Identity:   requestIdentity,

		AllowedLogins:  boot.AllowedLogins,
		Store:          database,
		AgentToken:     boot.AgentToken,
		RepoProjects:   boot.RepoProjects,
		DefaultProject: boot.DefaultProject,
		ServerURL:      serverURL,
		EnvoyURL:       boot.EnvoyURL,
		Docs:           documentService,
		Events:         broker,
		App:            appCfg,
		GitHubAPIBase:  boot.GitHubAPIBase,
		OIDC:           serviceTokens,
		AgentStream:    agentStream,
		Lifetime:       ctx,

		AgentSecretsURL:   boot.AgentSecretsURL,
		AgentSecretsToken: boot.AgentSecretsToken,

		TestHooksEnabled: boot.TestHooksEnabled,
		DevSignIn:        boot.DevSignIn,
	})

	if err != nil {
		slog.Error("dispatch: build app context", "error", err)
		os.Exit(1)
	}

	if natsClient != nil {
		go outbox.Run(ctx, outbox.Deps{
			Store:     database,
			Publisher: natsClient,
			Broker:    broker,
			Docs:      documentService,
		})
	}
	// A settlement a shutdown cut short, here or in the task this one replaces, runs without
	// anyone opening its document.
	go documentService.RunSettlementResumption(ctx)

	sweeper, err := webhookSweeper(natsClient, appCfg, boot.GitHubAPIBase)
	if err != nil {
		slog.Error("dispatch: open webhook redelivery", "error", err)
		os.Exit(1)
	}
	if sweeper != nil {
		slog.Info("dispatch: redelivering the GitHub App webhook's failed deliveries", "client_id", appCfg.ClientID, "interval", redeliver.Interval)
		go sweeper.Run(ctx, redeliver.Interval)
	} else {
		slog.Info("dispatch: webhook redelivery off: it needs NATS and the GitHub App private key")
	}

	go architecture.Run(ctx, appCtx.Architecture())

	handler := dispatchHandler(routes.New(appCtx), database, natsClient, buildCommit)
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// WriteTimeout remains zero because the event stream is long-lived.
	}
	// Bound before serving, so an address that cannot be taken ends the process with 1: a
	// supervisor must not read a port clash as a clean stop.
	listener, err := net.Listen("tcp", boot.ListenAddr)
	if err != nil {
		slog.Error("dispatch: listen", "addr", boot.ListenAddr, "error", err)
		os.Exit(1)
	}
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("dispatch: listening", "addr", boot.ListenAddr)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			cancel()
		}
	}()

	<-ctx.Done()
	slog.Info("dispatch: shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimout)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("dispatch: shutdown", "error", err)
	}
	if err := documentService.Shutdown(shutdownCtx); err != nil {
		slog.Warn("dispatch: shutdown document service", "error", err)
	}
	select {
	case err := <-serveErr:
		slog.Error("dispatch: serve", "error", err)
		os.Exit(1)
	default:
	}
}

func defaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "dispatch"), nil
}

// defaultWebDistDir resolves the SPA build directory relative to the running
// binary. The binary lives at packages/envoy/dispatch (when built locally) or
// is installed elsewhere; we walk up to find packages/dispatch/web/dist.
func defaultWebDistDir() (string, error) {
	// First try $DISPATCH_WEB_DIST.
	if env := os.Getenv("DISPATCH_WEB_DIST"); env != "" {
		return env, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Resolve symlinks so we get the real on-disk binary path.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	// Walk up looking for packages/dispatch/web/dist.
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "packages", "dispatch", "web", "dist")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Fall back to a sibling layout: ../../dispatch/web/dist relative to binary.
	candidate := filepath.Join(filepath.Dir(exe), "..", "..", "dispatch", "web", "dist")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		abs, _ := filepath.Abs(candidate)
		return abs, nil
	}
	// Last resort: cwd-relative.
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, "packages", "dispatch", "web", "dist"), nil
}

// appCredentialSource is where loadAppCredentials found the App: the environment, or the data
// dir's app.json at Path. The dev sign-in fence decides on it; String is the form the boot log
// prints.
type appCredentialSource struct {
	FromFile bool
	Path     string
}

func (s appCredentialSource) String() string {
	if s.FromFile {
		return "file:" + s.Path
	}
	return "env"
}

// loadAppCredentials returns the App and where it came from. The environment wins over the file;
// either may be absent, which returns a nil App.
func loadAppCredentials(dataDir string) (*auth.AppConfig, appCredentialSource, error) {
	if cfg, err := auth.LoadAppFromEnv(); err != nil {
		return nil, appCredentialSource{}, fmt.Errorf("load app from env: %w", err)
	} else if cfg != nil {
		return cfg, appCredentialSource{}, nil
	}
	path := filepath.Join(dataDir, "app.json")
	cfg, err := auth.ReadApp(path)
	if err != nil {
		return nil, appCredentialSource{}, fmt.Errorf("read %s: %w", path, err)
	}
	if cfg == nil {
		return nil, appCredentialSource{}, nil
	}
	return cfg, appCredentialSource{FromFile: true, Path: path}, nil
}

func resolveBootConfig(getenv func(string) string) (bootConfig, error) {
	boot := bootConfig{
		DatabaseURL:      strings.TrimSpace(getenv("DATABASE_URL")),
		AgentToken:       strings.TrimSpace(getenv("DISPATCH_AGENT_TOKEN")),
		RepoProjects:     strings.TrimSpace(getenv("DISPATCH_REPO_PROJECTS")),
		DefaultProject:   strings.TrimSpace(getenv("DISPATCH_DEFAULT_PROJECT")),
		GitHubAPIBase:    strings.TrimSpace(getenv("DISPATCH_GITHUB_API_BASE")),
		AllowedLogins:    parseAllowedLogins(getenv("DISPATCH_ALLOWED_LOGINS")),
		NATSDisabled:     getenv("DISPATCH_NATS_DISABLED") == "1",
		TestHooksEnabled: getenv("DISPATCH_TEST_HOOKS") == "1",
	}
	if boot.DatabaseURL == "" {
		return bootConfig{}, errors.New("DATABASE_URL required")
	}
	if boot.AgentToken == "" {
		return bootConfig{}, errors.New("DISPATCH_AGENT_TOKEN required")
	}
	listenAddr, err := listenAddress(getenv)
	if err != nil {
		return bootConfig{}, err
	}
	boot.ListenAddr = listenAddr

	switch mode := strings.TrimSpace(getenv("DISPATCH_IDENTITY")); {
	case mode == "" || mode == "cookie":
		if len(boot.AllowedLogins) == 0 {
			return bootConfig{}, errors.New("DISPATCH_ALLOWED_LOGINS required in cookie identity mode")
		}
	case strings.HasPrefix(mode, "header:"):
		boot.IdentityHeader = strings.TrimSpace(strings.TrimPrefix(mode, "header:"))
		if boot.IdentityHeader == "" {
			return bootConfig{}, errors.New("DISPATCH_IDENTITY header name required")
		}
		if getenv("DISPATCH_APP_CLIENT_ID") != "" && getenv("DISPATCH_IDENTITY_HEADER_TRUSTED") != "1" {
			return bootConfig{}, errors.New("DISPATCH_IDENTITY_HEADER_TRUSTED=1 required with OAuth and header identity")
		}
	default:
		return bootConfig{}, fmt.Errorf("DISPATCH_IDENTITY=%q (expected cookie or header:<Header-Name>)", mode)
	}
	envoyURL := strings.TrimSpace(getenv("ENVOY_URL"))
	if envoyURL == "" {
		envoyURL = "http://127.0.0.1:9020"
	}
	parsed, err := url.Parse(envoyURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return bootConfig{}, fmt.Errorf("ENVOY_URL=%q (expected an absolute http(s) URL)", envoyURL)
	}
	boot.EnvoyURL = strings.TrimSuffix(envoyURL, "/")

	// The pair's both-or-neither rule lives in internal/oidc so the listener
	// applies the same one; oidc.New stays out of this function, which reads
	// the environment and returns errors and nothing else.
	boot.OIDCIssuer, boot.OIDCAudience, err = oidc.ConfigFromEnv(getenv,
		"DISPATCH_OIDC_ISSUER", "DISPATCH_OIDC_AUDIENCE")
	if err != nil {
		return bootConfig{}, err
	}

	agentSecretsURL := strings.TrimSuffix(strings.TrimSpace(getenv("DISPATCH_AGENT_SECRETS_URL")), "/")
	if agentSecretsURL != "" {
		parsed, err := url.Parse(agentSecretsURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" {
			return bootConfig{}, fmt.Errorf("DISPATCH_AGENT_SECRETS_URL=%q (expected an absolute http(s) URL with no path)", agentSecretsURL)
		}
		boot.AgentSecretsURL = agentSecretsURL
		boot.AgentSecretsToken, err = agentSecretsToken(getenv)
		if err != nil {
			return bootConfig{}, err
		}
		if boot.AgentSecretsToken == "" {
			return bootConfig{}, errors.New("DISPATCH_AGENT_SECRETS_TOKEN_FILE or DISPATCH_AGENT_SECRETS_TOKEN is required when DISPATCH_AGENT_SECRETS_URL is set")
		}
	}

	switch flag := getenv("DISPATCH_DEV_SIGNIN"); flag {
	case "":
	case "1":
		if err := devSignInFence(boot, getenv); err != nil {
			return bootConfig{}, err
		}
		boot.DevSignIn = true
	default:
		return bootConfig{}, fmt.Errorf("DISPATCH_DEV_SIGNIN=%q (expected 1 or unset)", flag)
	}
	return boot, nil
}

// devSignInFence is the dev sign-in fence over the environment; devSignInLoadedFence covers what
// main loads after it. With the flag any loopback client signs in as any allowlisted login, so the
// process must listen, keep its data and reach the services that act on a human's word (NATS, the
// secrets broker, the Envoy listener that delivers mentions and messages, GitHub as the App) on
// this machine alone, and sign its cookies with a key no other process holds.
func devSignInFence(boot bootConfig, getenv func(string) string) error {
	if boot.IdentityHeader != "" {
		return errors.New("DISPATCH_DEV_SIGNIN=1 mints session cookies, so DISPATCH_IDENTITY must be cookie")
	}
	if !routes.LoopbackHostPort(boot.ListenAddr) {
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 is for a loopback server only: DISPATCH_LISTEN_HOST=%q (listen address %q) must be 127.0.0.1 or [::1]", getenv("DISPATCH_LISTEN_HOST"), boot.ListenAddr)
	}
	if err := loopbackDatabase(boot.DatabaseURL); err != nil {
		return err
	}
	if getenv("DISPATCH_SIGNING_KEY") != "" {
		return errors.New("DISPATCH_DEV_SIGNIN=1 signs cookies with a key generated for this process; unset DISPATCH_SIGNING_KEY")
	}
	if !boot.NATSDisabled && getenv(bus.AllowRemoteEnvVar) == "1" {
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 publishes to this machine's NATS only: unset %s, or set DISPATCH_NATS_DISABLED=1", bus.AllowRemoteEnvVar)
	}
	if boot.AgentSecretsURL != "" {
		if !routes.LoopbackURL(boot.AgentSecretsURL) {
			return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 decides credential requests on this machine's secrets broker only: DISPATCH_AGENT_SECRETS_URL=%q must name 127.0.0.1, [::1] or localhost", boot.AgentSecretsURL)
		}
	}
	if !routes.LoopbackURL(boot.EnvoyURL) {
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 delivers through this machine's Envoy listener only: ENVOY_URL=%q must name 127.0.0.1, [::1] or localhost", boot.EnvoyURL)
	}
	return nil
}

// devSignInLoadedFence is the dev sign-in fence over what main loads after the environment: the
// dashboard origin from envoy.json, and the GitHub App from the environment or the data dir's
// app.json. With the flag a loopback client acts as an allowlisted human, and a human can make the
// App act: saving an architecture source has the App probe and import the repository the caller
// names. The private key is the credential that acts (githubapp.New builds no client without it,
// the App JWT names the client ID, and nothing sends the numeric App ID), so:
//   - a key from app.json, where a developer keeps the real App's key, is refused whatever the
//     base;
//   - a key from the environment may sign calls only to a loopback host. That checks the host, not
//     what listens there: every App call hands a signed App JWT to whatever owns the port, so the
//     key must be a throwaway, as the one packages/dispatch/e2e/run-server.sh generates.
func devSignInLoadedFence(boot bootConfig, serverURL string, app *auth.AppConfig, source appCredentialSource) error {
	if !boot.DevSignIn {
		return nil
	}
	if _, err := routes.DevSignInOrigin(serverURL); err != nil {
		return err
	}
	if app == nil || app.PEM == "" {
		return nil
	}
	if source.FromFile {
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 refuses the App private key in %s, whatever DISPATCH_GITHUB_API_BASE names: that file is where the real App's key is kept, and with the flag any loopback client can have the App sign calls. Pass a throwaway App in the environment instead (DISPATCH_APP_CLIENT_ID, DISPATCH_APP_CLIENT_SECRET and a generated DISPATCH_APP_PEM_B64, which take precedence over app.json), with DISPATCH_GITHUB_API_BASE naming a GitHub fake on a loopback host, as packages/dispatch/e2e/run-server.sh does", source.Path)
	}
	if !routes.LoopbackURL(boot.GitHubAPIBase) {
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 lets the App private key (DISPATCH_APP_PEM_B64) sign calls only to a loopback host: DISPATCH_GITHUB_API_BASE=%q (empty is https://api.github.com) must name 127.0.0.1, [::1] or localhost. Use a throwaway key: every App call hands a signed App JWT to whatever listens there", boot.GitHubAPIBase)
	}
	return nil
}

// loopbackDatabase refuses a DATABASE_URL with a host that is not this machine: the data a
// dev-sign-in server serves to any local process must be a scratch database here. pgx parses
// both the URL and the key=value forms, multi-host lists included, exactly as store.Open will.
func loopbackDatabase(databaseURL string) error {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("DATABASE_URL: %w", err)
	}
	hosts := []string{config.Host}
	for _, fallback := range config.Fallbacks {
		hosts = append(hosts, fallback.Host)
	}
	for _, host := range hosts {
		if !strings.HasPrefix(host, "/") && !routes.LoopbackName(host) {
			return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 serves a loopback database only: DATABASE_URL names host %q", host)
		}
	}
	return nil
}

// agentSecretsToken resolves the secrets broker's UI bearer, reading
// DISPATCH_AGENT_SECRETS_TOKEN_FILE (trimmed contents) ahead of
// DISPATCH_AGENT_SECRETS_TOKEN; a set-but-unreadable or blank file is an error naming both,
// never a silent fallback to the bare variable.
func agentSecretsToken(getenv func(string) string) (string, error) {
	if path := strings.TrimSpace(getenv("DISPATCH_AGENT_SECRETS_TOKEN_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("DISPATCH_AGENT_SECRETS_TOKEN_FILE names %s, which could not be read: %w", path, err)
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", fmt.Errorf("DISPATCH_AGENT_SECRETS_TOKEN_FILE names %s, which is empty", path)
		}
		return value, nil
	}
	return strings.TrimSpace(getenv("DISPATCH_AGENT_SECRETS_TOKEN")), nil
}

// validateDefaultProject confirms DISPATCH_DEFAULT_PROJECT names a project
// that already exists, so a misconfigured deployment fails loudly at boot
// instead of silently rejecting every unmapped external issue at request
// time. An empty project (no default configured) is not validated here.
func validateDefaultProject(ctx context.Context, database *store.Store, project string) error {
	if project == "" {
		return nil
	}
	ctx = store.WithTransactionTracking(ctx)
	var exists bool
	if err := database.Pool.QueryRow(ctx, `select exists(select 1 from projects where key = $1)`, project).Scan(&exists); err != nil {
		return fmt.Errorf("query DISPATCH_DEFAULT_PROJECT %q: %w", project, err)
	}
	if !exists {
		return fmt.Errorf("DISPATCH_DEFAULT_PROJECT %q does not exist", project)
	}
	return nil
}

func seedRepoProjects(ctx context.Context, database *store.Store, raw string) error {
	ctx = store.WithTransactionTracking(ctx)
	mappings, err := api.ParseRepoProjects(raw)
	if err != nil {
		return err
	}
	createdBy := []byte(`{"kind":"session","id":"environment"}`)
	for repo, project := range mappings {
		if _, err := database.Pool.Exec(ctx, `
			insert into repo_projects (repo, project, created_by)
			values ($1, $2, $3)
			on conflict (repo) do nothing
		`, repo, project, createdBy); err != nil {
			return fmt.Errorf("seed repository project %q: %w", repo, err)
		}
	}
	return nil
}

// parseAllowedLogins lower-cases every entry: GitHub logins are case-insensitive, and the
// login GitHub returns at sign-in carries the user's display casing.
func parseAllowedLogins(raw string) map[string]struct{} {
	logins := map[string]struct{}{}
	for _, login := range strings.Split(raw, ",") {
		if login = strings.ToLower(strings.TrimSpace(login)); login != "" {
			logins[login] = struct{}{}
		}
	}
	return logins
}

func parsePositiveInt(raw string) (int, error) {
	n := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a positive integer: %q", raw)
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return 0, fmt.Errorf("not a positive integer: %q", raw)
	}
	return n, nil
}

// dispatchHandler mounts the one /healthz the process serves above every dashboard and API
// route, so the probe is answered whatever the router is doing. Go's ServeMux prefers the
// longer pattern, so "GET /healthz" wins over the router's "/".
func dispatchHandler(handler http.Handler, database *store.Store, natsClient *bus.Client, commit string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthzHandler(database, natsClient, commit))
	mux.Handle("/", handler)
	return mux
}

// healthzHandler answers the probe: the process is serving, Postgres is reachable on the
// health pool's own connection, and NATS is connected where it is configured. Nothing here
// waits on the shared pool, and Healthy bounds its own wait at store.healthProbeTimeout, which
// records why a probe that answers late is as bad as one that never answers.
//
// Beside those it reports what is deployed: `commit`, the legion commit the binary was built
// from (null when the build did not stamp one), and `schema_version`, the highest migration
// the database has applied, read by the same probe (null when the database did not answer).
// A deploy check compares the two with the commit its image pin names and that commit's
// migrations, so neither is ever filled with a guess.
func healthzHandler(database *store.Store, natsClient *bus.Client, commit string) http.HandlerFunc {
	var reportedCommit *string
	if commit != "" {
		reportedCommit = &commit
	}
	return func(w http.ResponseWriter, req *http.Request) {
		databaseOK := database != nil && database.Pool != nil
		var schemaVersion *int
		if databaseOK {
			// A 503 that records no reason works against the point of the probe: a closed
			// pool, a deadline on a stalled link, an authentication failure and a refused
			// dial are four incidents with four next steps, and the body distinguishes
			// none of them. One line per failed poll, for as long as the outage lasts.
			version, err := database.Pool.Healthy(req.Context())
			if err != nil {
				slog.Warn("dispatch: health probe failed", "error", err)
				databaseOK = false
			} else {
				schemaVersion = &version
			}
		}
		var natsOK *bool
		if natsClient != nil {
			connected := natsClient.Connected()
			natsOK = &connected
		}
		ok := databaseOK && (natsOK == nil || *natsOK)
		status := http.StatusOK
		if !ok {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(struct {
			OK            bool    `json:"ok"`
			DB            bool    `json:"db"`
			NATS          *bool   `json:"nats"`
			Commit        *string `json:"commit"`
			SchemaVersion *int    `json:"schema_version"`
		}{OK: ok, DB: databaseOK, NATS: natsOK, Commit: reportedCommit, SchemaVersion: schemaVersion})
	}
}

// listenAddress builds the listen address from DISPATCH_LISTEN_HOST and DISPATCH_PORT (8766
// when unset). An empty host binds every interface (the containerized production default);
// local compose deployments set 127.0.0.1. An IPv6 host may be written with or without brackets:
// one pair comes off here and JoinHostPort puts it back.
func listenAddress(getenv func(string) string) (string, error) {
	host := strings.TrimSpace(getenv("DISPATCH_LISTEN_HOST"))
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	port := strings.TrimSpace(getenv("DISPATCH_PORT"))
	if port == "" {
		port = defaultListenPort
	} else {
		parsed, err := parsePositiveInt(port)
		if err != nil {
			return "", fmt.Errorf("invalid DISPATCH_PORT: %w", err)
		}
		if parsed > 65535 {
			return "", fmt.Errorf("invalid DISPATCH_PORT: %q", port)
		}
	}
	return net.JoinHostPort(host, port), nil
}

// subcommand is an argument envoy-dispatch takes in place of serving. run gets the arguments
// after the name.
type subcommand struct {
	name string
	run  func(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int
}

// subcommands are every argument envoy-dispatch takes in place of serving: runSubcommand both
// dispatches on this table and names it, in this order, when it refuses an argument.
var subcommands = []subcommand{
	{"backfill-block-ids", func(ctx context.Context, _ []string, getenv func(string) string, stdout, _ io.Writer) int {
		return backfillBlockIDs(ctx, getenv("DATABASE_URL"), stdout)
	}},
	{"backfill-anchor-blocks", func(ctx context.Context, _ []string, getenv func(string) string, stdout, _ io.Writer) int {
		return backfillAnchorBlocks(ctx, getenv("DATABASE_URL"), stdout)
	}},
	{"rebuild-refs", func(ctx context.Context, _ []string, getenv func(string) string, stdout, _ io.Writer) int {
		return rebuildRefs(ctx, getenv("DATABASE_URL"), loadServerURL(), stdout)
	}},
	{"redeliver-webhooks", func(ctx context.Context, args []string, _ func(string) string, stdout, _ io.Writer) int {
		return redeliverWebhooks(ctx, args, stdout)
	}},
	{"census", func(ctx context.Context, _ []string, getenv func(string) string, stdout, stderr io.Writer) int {
		return census(ctx, getenv("DATABASE_URL"), stdout, stderr)
	}},
}

// runSubcommand runs the subcommand args name and returns its exit code. A name it does not know
// is refused with exit 2 rather than falling through to the server: the server migrates the
// database at boot, so a deployment running `envoy-dispatch census` on an image that predates
// the subcommand must get a refusal, never a boot that applies the migrations the census was to
// inspect.
func runSubcommand(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	names := make([]string, len(subcommands))
	for i, sub := range subcommands {
		if sub.name == args[0] {
			return sub.run(ctx, args[1:], getenv, stdout, stderr)
		}
		names[i] = sub.name
	}
	fmt.Fprintf(stderr, "envoy-dispatch: unknown subcommand %q; the subcommands are %s, and envoy-dispatch with no argument serves\n", args[0], strings.Join(names, ", "))
	return 2
}

// openMigrated opens the database a DB subcommand works on and brings it to the current
// schema, reporting each failure on out under the subcommand's label. The caller closes the
// returned store's pool.
func openMigrated(ctx context.Context, label, databaseURL string, out io.Writer) (*store.Store, bool) {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(out, label+": DATABASE_URL is required")
		return nil, false
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(out, "%s: open database: %v\n", label, err)
		return nil, false
	}
	if err := database.Migrate(ctx); err != nil {
		fmt.Fprintf(out, "%s: migrate database: %v\n", label, err)
		database.Pool.Close()
		return nil, false
	}
	return database, true
}

func backfillBlockIDs(ctx context.Context, databaseURL string, out io.Writer) int {
	database, ok := openMigrated(ctx, "backfill-block-ids", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	service := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	defer service.Shutdown(context.Background())
	reports, err := service.BackfillBlockIDs(ctx)
	if err != nil {
		fmt.Fprintf(out, "backfill-block-ids: %v\n", err)
		return 1
	}
	exitCode := 0
	for _, report := range reports {
		if !writeBlockIDBackfillReport(out, report) {
			exitCode = 1
		}
	}
	return exitCode
}

func backfillAnchorBlocks(ctx context.Context, databaseURL string, out io.Writer) int {
	database, ok := openMigrated(ctx, "backfill-anchor-blocks", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	service := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	defer service.Shutdown(context.Background())
	reports, err := service.BackfillBlockIDs(ctx)
	if err != nil {
		fmt.Fprintf(out, "backfill-anchor-blocks: stamp document blocks: %v\n", err)
		return 1
	}
	for _, report := range reports {
		if report.Err != nil {
			fmt.Fprintf(out, "backfill-anchor-blocks: stamp document %s: %v\n", report.ArtifactID, report.Err)
			return 1
		}
	}
	result, err := service.BackfillAnchorBlocks(ctx)
	if err != nil {
		fmt.Fprintf(out, "backfill-anchor-blocks: %v\n", err)
		return 1
	}
	writeAnchorBlockBackfillReport(out, result)
	return 0
}

// loadServerURL resolves the dashboard origin exactly as the server does, for a subcommand
// that parses reference text.
func loadServerURL() string {
	envoyConfig, err := config.Load(config.LoadOptions{})
	if err != nil {
		slog.Error("dispatch: load envoy config", "error", err)
		os.Exit(1)
	}
	if envoyConfig.Dispatch == nil {
		return ""
	}
	return envoyConfig.Dispatch.ServerURL
}

// rebuildRefs reparses every reference source and reconciles the refs index with it. It
// refuses an empty server URL: text.Extract recognises same-origin dashboard URLs only against
// it, so an empty value would delete every URL-form mention.
func rebuildRefs(ctx context.Context, databaseURL, serverURL string, out io.Writer) int {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(out, "rebuild-refs: DATABASE_URL is required")
		return 1
	}
	if strings.TrimSpace(serverURL) == "" {
		fmt.Fprintln(out, "rebuild-refs: dispatch.server_url is required to recognise dashboard URLs; refusing to drop URL-form mentions")
		return 1
	}
	database, ok := openMigrated(ctx, "rebuild-refs", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	// rebuild-refs opens a transaction per source, so it marks its context like the other
	// commands: a read taken inside one is refused rather than left to deadlock the pool.
	report, err := refs.RebuildAll(store.WithTransactionTracking(ctx), database.Pool, serverURL)
	if err != nil {
		fmt.Fprintf(out, "rebuild-refs: %v\n", err)
		return 1
	}
	writeRebuildRefsReport(out, report)
	return 0
}

func writeRebuildRefsReport(out io.Writer, report refs.Rebuild) {
	fmt.Fprintf(out, "rebuild-refs: documents=%d asks=%d comments=%d messages=%d orphans=%d edges=%d\n",
		report.Documents, report.Asks, report.Comments, report.Messages, report.Orphans, report.Edges)
}

func writeAnchorBlockBackfillReport(out io.Writer, result docs.AnchorBlockBackfill) {
	fmt.Fprintf(out, "backfill-anchor-blocks: asks=%d comments=%d skipped=%d\n", result.Asks, result.Comments, result.Skipped)
}

func writeBlockIDBackfillReport(out io.Writer, report docs.BlockIDBackfill) bool {
	switch {
	case report.Err != nil:
		fmt.Fprintf(out, "%s error (%v)\n", report.ArtifactID, report.Err)
		return false
	case report.Skipped != "":
		fmt.Fprintf(out, "%s skipped (%s)\n", report.ArtifactID, report.Skipped)
		return true
	default:
		fmt.Fprintf(out, "%s stamped=%d\n", report.ArtifactID, report.Stamped)
		return true
	}
}
