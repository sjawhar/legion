// Command dispatch serves the Dispatch dashboard and API, signs people in with Google Workspace
// through the sign-in pool, and reads GitHub for the web app as the GitHub App.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	// defaultListenPort is the port when DISPATCH_PORT is unset; listenAddress joins it to the host.
	defaultListenPort = "8766"
	// defaultEnvoyURL is the Envoy listener when ENVOY_URL is unset: this machine's.
	defaultEnvoyURL   = "http://127.0.0.1:9020"
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
	DatabaseURL    string
	AgentToken     string
	RepoProjects   string
	DefaultProject string
	EnvoyURL       string
	GitHubAPIBase  string
	IdentityHeader string
	// SignInIssuer, SignInClientID, SignInClientSecret and SignInGroup configure Google sign-in
	// through the sign-in pool (DISPATCH_SIGNIN_*): all four or none; empty means no sign-in.
	SignInIssuer       string
	SignInClientID     string
	SignInClientSecret string
	SignInGroup        string
	NATSDisabled       bool
	TestHooksEnabled   bool
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
	// WebDist is DISPATCH_WEB_DIST: the dashboard directory to serve, or empty to find it from
	// the binary (defaultWebDistDir).
	WebDist string
	// SigningKey is DISPATCH_SIGNING_KEY: the session cookie key, or empty to keep one in the
	// data dir (sessionSigningKey).
	SigningKey string
	// InsecureCookie (DISPATCH_INSECURE_COOKIE set to anything) drops Secure from every cookie.
	InsecureCookie bool
	// EnvoyToken is ENVOY_TOKEN, the bearer every Envoy listener call sends.
	EnvoyToken string
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	env := processSettings()
	if len(os.Args) > 1 {
		os.Exit(runSubcommand(context.Background(), os.Args[1:], env, os.Stdout, os.Stderr))
	}
	boot, err := resolveBootConfig(env)
	if err != nil {
		slog.Error("dispatch: resolve boot config", "error", err)
		os.Exit(1)
	}

	envoyConfig, err := loadEnvoyConfig(env, config.LoadOptions{})
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
	appCfg, appSource, err := loadAppCredentials(env, dataDir)
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
		slog.Warn("dispatch: dev sign-in mounted: any email signs in at /auth/_dev/signin without the sign-in pool; cookies are valid on this process only", "origin", serverURL)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var natsClient *bus.Client
	if boot.NATSDisabled {
		slog.Info("dispatch: NATS publisher disabled")
	} else {
		natsClient, err = bus.ConnectOwningStream(envoyConfig.NatsURLs, bus.WithEnvironment(env.lookup))
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
		streamConn, err := bus.Dial("dispatch-agent-stream", envoyConfig.NatsURLs, env.lookup)
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

	webDistDir, err := defaultWebDistDir(boot.WebDist)
	if err != nil {
		slog.Error("dispatch: resolve web dist dir", "error", err)
		os.Exit(1)
	}

	signIn, err := discoverSignIn(ctx, boot)
	if err != nil {
		slog.Error("dispatch: discover the sign-in issuer", "error", err)
		os.Exit(1)
	}
	if signIn != nil {
		slog.Info("dispatch: signing people in through the sign-in pool", "issuer", boot.SignInIssuer, "client_id", boot.SignInClientID, "group", boot.SignInGroup)
	}

	people, signingKey, err := openPeople(boot, dataDir, database.Pool, signIn)
	if err != nil {
		slog.Error("dispatch: load signing key", "error", err)
		os.Exit(1)
	}
	retirePlainRefreshTokens(ctx, people, plainRefreshTokenRetireTimeout)
	sessions := store.NewPgSessionStore(database.Pool)

	if boot.IdentityHeader != "" {
		slog.Warn("dispatch: using test/local header identity", "header", boot.IdentityHeader)
	}
	requestIdentity := requestIdentityFor(boot, signingKey, people, sessions, signIn)

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

	appCtx, err := routes.BuildAppContext(appContextOptions(boot, routes.AppContextOptions{
		SigningKey:  signingKey,
		WebDistDir:  webDistDir,
		People:      people,
		Sessions:    sessions,
		Identity:    requestIdentity,
		SignIn:      signIn,
		Store:       database,
		ServerURL:   serverURL,
		Docs:        documentService,
		Events:      broker,
		App:         appCfg,
		OIDC:        serviceTokens,
		AgentStream: agentStream,
		Lifetime:    ctx,
	}))

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

// defaultWebDistDir resolves the SPA build directory: configured (DISPATCH_WEB_DIST) when it is
// not empty, otherwise relative to the running binary. The binary lives at packages/envoy/dispatch
// (when built locally) or is installed elsewhere; we walk up to find packages/dispatch/web/dist.
func defaultWebDistDir(configured string) (string, error) {
	// The configured directory is made absolute and clean like every path below, so a value
	// spelled through `..` names the same directory the static handler joins requests under.
	if configured != "" {
		return filepath.Abs(configured)
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
	Path string
}

func (s appCredentialSource) String() string {
	if s.Path != "" {
		return "file:" + s.Path
	}
	return "env"
}

// loadAppCredentials returns the App and where it came from. The environment (the DISPATCH_APP_*
// rows of the settings table) wins over the file; either may be absent, which returns a nil App.
func loadAppCredentials(env settingValues, dataDir string) (*auth.AppConfig, appCredentialSource, error) {
	if cfg, err := auth.LoadAppFromEnv(env.get); err != nil {
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
	return cfg, appCredentialSource{Path: path}, nil
}

// resolveBootConfig is the server's configuration from what the settings table read (env),
// checked before anything connects. It holds the settings main hands on as values; three groups
// reach their readers from env instead, through the table's lookup: the GitHub App credentials
// (loadAppCredentials), the envoy.json overrides (loadEnvoyConfig) and NATS's reach and nkey (the
// bus connects). A new setting goes wherever its reader takes it, and always into the table.
// A variable a release removed (removedSettings) refuses startup before anything else is read.
func resolveBootConfig(env settingValues) (bootConfig, error) {
	if err := refuseRemovedSettings(env); err != nil {
		return bootConfig{}, err
	}
	boot := bootConfig{
		DatabaseURL:        strings.TrimSpace(env.get("DATABASE_URL")),
		AgentToken:         strings.TrimSpace(env.get("DISPATCH_AGENT_TOKEN")),
		RepoProjects:       strings.TrimSpace(env.get("DISPATCH_REPO_PROJECTS")),
		DefaultProject:     strings.TrimSpace(env.get("DISPATCH_DEFAULT_PROJECT")),
		GitHubAPIBase:      strings.TrimSpace(env.get("DISPATCH_GITHUB_API_BASE")),
		SignInIssuer:       strings.TrimSpace(env.get("DISPATCH_SIGNIN_ISSUER")),
		SignInClientID:     strings.TrimSpace(env.get("DISPATCH_SIGNIN_CLIENT_ID")),
		SignInClientSecret: strings.TrimSpace(env.get("DISPATCH_SIGNIN_CLIENT_SECRET")),
		SignInGroup:        strings.TrimSpace(env.get("DISPATCH_SIGNIN_GROUP")),
		NATSDisabled:       env.get("DISPATCH_NATS_DISABLED") == "1",
		TestHooksEnabled:   env.get("DISPATCH_TEST_HOOKS") == "1",
		WebDist:            env.get("DISPATCH_WEB_DIST"),
		SigningKey:         env.get("DISPATCH_SIGNING_KEY"),
		InsecureCookie:     env.get("DISPATCH_INSECURE_COOKIE") != "",
		EnvoyToken:         env.get("ENVOY_TOKEN"),
	}
	if boot.DatabaseURL == "" {
		return bootConfig{}, errors.New("DATABASE_URL required")
	}
	if boot.AgentToken == "" {
		return bootConfig{}, errors.New("DISPATCH_AGENT_TOKEN required")
	}
	listenAddr, err := listenAddress(env)
	if err != nil {
		return bootConfig{}, err
	}
	boot.ListenAddr = listenAddr
	identityMode := strings.TrimSpace(env.get("DISPATCH_IDENTITY"))
	if err := checkSignInSettings(boot, identityMode); err != nil {
		return bootConfig{}, err
	}
	var devSignIn bool
	switch flag := env.get("DISPATCH_DEV_SIGNIN"); flag {
	case "":
	case "1":
		devSignIn = true
	default:
		return bootConfig{}, fmt.Errorf("DISPATCH_DEV_SIGNIN=%q (expected 1 or unset)", flag)
	}

	switch mode := identityMode; {
	case mode == "" || mode == "cookie":
		if err := requireCookieSignIn(boot, devSignIn); err != nil {
			return bootConfig{}, err
		}
	case strings.HasPrefix(mode, "header:"):
		boot.IdentityHeader = strings.TrimSpace(strings.TrimPrefix(mode, "header:"))
		if boot.IdentityHeader == "" {
			return bootConfig{}, errors.New("DISPATCH_IDENTITY header name required")
		}
		if env.get("DISPATCH_IDENTITY_HEADER_TRUSTED") != "1" {
			return bootConfig{}, errors.New("DISPATCH_IDENTITY_HEADER_TRUSTED=1 required: header identity is only for tests and local harnesses")
		}
	default:
		return bootConfig{}, fmt.Errorf("DISPATCH_IDENTITY=%q (expected cookie or header:<Header-Name>)", mode)
	}
	envoyURL := strings.TrimSpace(env.get("ENVOY_URL"))
	if envoyURL == "" {
		envoyURL = defaultEnvoyURL
	}
	parsed, err := url.Parse(envoyURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return bootConfig{}, fmt.Errorf("ENVOY_URL=%q (expected an absolute http(s) URL)", envoyURL)
	}
	boot.EnvoyURL = strings.TrimSuffix(envoyURL, "/")

	// The pair's both-or-neither rule lives in internal/oidc so the listener
	// applies the same one; oidc.New stays out of this function, which reads
	// the environment and returns errors and nothing else.
	boot.OIDCIssuer, boot.OIDCAudience, err = oidc.ConfigFromEnv(env.get,
		"DISPATCH_OIDC_ISSUER", "DISPATCH_OIDC_AUDIENCE")
	if err != nil {
		return bootConfig{}, err
	}

	agentSecretsURL := strings.TrimSuffix(strings.TrimSpace(env.get("DISPATCH_AGENT_SECRETS_URL")), "/")
	if agentSecretsURL != "" {
		parsed, err := url.Parse(agentSecretsURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" {
			return bootConfig{}, fmt.Errorf("DISPATCH_AGENT_SECRETS_URL=%q (expected an absolute http(s) URL with no path)", agentSecretsURL)
		}
		boot.AgentSecretsURL = agentSecretsURL
		boot.AgentSecretsToken, err = agentSecretsToken(env)
		if err != nil {
			return bootConfig{}, err
		}
		if boot.AgentSecretsToken == "" {
			return bootConfig{}, errors.New("DISPATCH_AGENT_SECRETS_TOKEN_FILE or DISPATCH_AGENT_SECRETS_TOKEN is required when DISPATCH_AGENT_SECRETS_URL is set")
		}
	}

	if devSignIn {
		if err := devSignInFence(boot, env); err != nil {
			return bootConfig{}, err
		}
		boot.DevSignIn = true
	}
	return boot, nil
}

// devSignInFence is the dev sign-in fence over the environment; devSignInLoadedFence covers what
// main loads after it. With the flag any loopback client signs in as any person it names, so the
// process must listen, keep its data and reach the services that act on a human's word (NATS, the
// secrets broker, the Envoy listener that delivers mentions and messages, GitHub as the App) on
// this machine alone, and sign its cookies with a key no other process holds.
func devSignInFence(boot bootConfig, env settingValues) error {
	if boot.IdentityHeader != "" {
		return errors.New("DISPATCH_DEV_SIGNIN=1 mints session cookies, so DISPATCH_IDENTITY must be cookie")
	}
	if boot.SignInIssuer != "" {
		return errors.New("DISPATCH_DEV_SIGNIN=1 signs people in without the sign-in pool, so DISPATCH_SIGNIN_* must be unset: a loopback server holds no sign-in client secret")
	}
	if !routes.LoopbackHostPort(boot.ListenAddr) {
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 is for a loopback server only: DISPATCH_LISTEN_HOST=%q (listen address %q) must be 127.0.0.1 or [::1]", env.get("DISPATCH_LISTEN_HOST"), boot.ListenAddr)
	}
	if err := loopbackDatabase(boot.DatabaseURL); err != nil {
		return err
	}
	if boot.SigningKey != "" {
		return errors.New("DISPATCH_DEV_SIGNIN=1 signs cookies with a key generated for this process; unset DISPATCH_SIGNING_KEY")
	}
	if !boot.NATSDisabled && env.get(bus.AllowRemoteEnvVar) == "1" {
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
// app.json. With the flag a loopback client acts as any person it names, and a person can make the
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
	if source.Path != "" {
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 refuses the App private key in %s, whatever DISPATCH_GITHUB_API_BASE names: that file is where the real App's key is kept, and with the flag any loopback client can have the App sign calls. Pass a throwaway App in the environment instead (DISPATCH_APP_CLIENT_ID and a generated DISPATCH_APP_PEM_B64, which take precedence over app.json), with DISPATCH_GITHUB_API_BASE naming a GitHub fake on a loopback host, as packages/dispatch/e2e/run-server.sh does", source.Path)
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

// sessionSigningKey is the key session cookies are signed with: one generated for this process
// under dev sign-in, otherwise DISPATCH_SIGNING_KEY when it is set (a deployment's, from its
// secrets manager), otherwise the data dir's signing-key file, created on first start (a local
// run's). Outside dev sign-in the key must stay the same across deploys, or every dsession cookie
// is invalidated whenever a container rolls.
func sessionSigningKey(boot bootConfig, dataDir string) (string, error) {
	if boot.DevSignIn {
		return auth.NewSigningKey()
	}
	if boot.SigningKey != "" {
		return boot.SigningKey, nil
	}
	return auth.LoadOrCreateSigningKey(filepath.Join(dataDir, "signing-key"))
}

// appContextOptions is what main hands routes.BuildAppContext: built, what main made from the
// configuration, with every setting the router takes from boot filled in. The settings tests build
// the router through it, so dropping any hand-off here fails a case.
func appContextOptions(boot bootConfig, built routes.AppContextOptions) routes.AppContextOptions {
	built.SignInGroup = boot.SignInGroup
	built.AgentToken = boot.AgentToken
	built.DefaultProject = boot.DefaultProject
	built.InsecureCookie = boot.InsecureCookie
	built.EnvoyURL = boot.EnvoyURL
	built.EnvoyToken = boot.EnvoyToken
	built.GitHubAPIBase = boot.GitHubAPIBase
	built.AgentSecretsURL = boot.AgentSecretsURL
	built.AgentSecretsToken = boot.AgentSecretsToken
	built.TestHooksEnabled = boot.TestHooksEnabled
	built.DevSignIn = boot.DevSignIn
	return built
}

// loadEnvoyConfig is envoy.json as Dispatch reads it: the files config.Load finds, under the
// DISPATCH_SERVER_URL and NATS_URLS rows of the settings table.
func loadEnvoyConfig(env settingValues, options config.LoadOptions) (*config.EnvoyConfig, error) {
	return config.Load(env.lookup, options)
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

// listenAddress builds the listen address from DISPATCH_LISTEN_HOST and DISPATCH_PORT
// (defaultListenPort when unset). An empty host binds every interface (the containerized
// production default); local compose deployments set 127.0.0.1. An IPv6 host may be written with
// or without brackets: one pair comes off here and JoinHostPort puts it back.
func listenAddress(env settingValues) (string, error) {
	host := strings.TrimSpace(env.get("DISPATCH_LISTEN_HOST"))
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	port := strings.TrimSpace(env.get("DISPATCH_PORT"))
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
