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
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/config"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	defaultListenAddr = ":8766"
	shutdownTimout    = 5 * time.Second
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
)

type bootConfig struct {
	DatabaseURL      string
	AgentToken       string
	RepoProjects     string
	DefaultProject   string
	EnvoyURL         string
	IdentityHeader   string
	AllowedLogins    map[string]struct{}
	NATSDisabled     bool
	TestHooksEnabled bool
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if len(os.Args) > 1 && os.Args[1] == "check-documents" {
		os.Exit(checkDocuments(context.Background(), os.Getenv("DATABASE_URL"), os.Stdout))
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var natsClient *bus.Client
	if boot.NATSDisabled {
		slog.Info("dispatch: NATS publisher disabled")
	} else {
		natsClient, err = bus.Connect(envoyConfig.NatsURLs)
		if err != nil {
			slog.Error("dispatch: connect NATS", "error", err)
			os.Exit(1)
		}
		defer natsClient.Close()
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
	if err := docs.MigrateLegacyDocuments(ctx, database); err != nil {
		slog.Error("dispatch: migrate legacy documents", "error", err)
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

	dataDir, err := defaultDataDir()
	if err != nil {
		slog.Error("dispatch: resolve data dir", "error", err)
		os.Exit(1)
	}
	webDistDir, err := defaultWebDistDir()
	if err != nil {
		slog.Error("dispatch: resolve web dist dir", "error", err)
		os.Exit(1)
	}

	signingKey, err := auth.LoadSigningKey(filepath.Join(dataDir, "signing-key"))
	if err != nil {
		slog.Error("dispatch: load signing key", "error", err)
		os.Exit(1)
	}

	appCfg, appSource, err := loadAppCredentials(dataDir)
	if err != nil {
		slog.Error("dispatch: load app credentials", "error", err)
		os.Exit(1)
	}
	if appCfg == nil {
		slog.Info("dispatch: no app credentials yet — dashboard will respond 503 until configured")
	} else {
		slog.Info("dispatch: loaded github app", "slug", appCfg.Slug, "client_id", appCfg.ClientID, "source", appSource)
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
		AppSource:      appSource,

		TestHooksEnabled: boot.TestHooksEnabled,
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

	handler := dispatchHandler(routes.New(appCtx), database, natsClient)
	listenAddr, err := listenAddress()
	if err != nil {
		slog.Error("dispatch: resolve listen address", "error", err)
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// WriteTimeout remains zero because the event stream is long-lived.
	}

	go func() {
		slog.Info("dispatch: listening", "addr", listenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("dispatch: listen", "error", err)
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

// loadAppCredentials returns (cfg, source, err). source is "env" or
// "file:<path>" for diagnostic logging. Env wins over file; either may be
// absent (returns nil, "", nil).
func loadAppCredentials(dataDir string) (*auth.AppConfig, string, error) {
	if cfg, err := auth.LoadAppFromEnv(); err != nil {
		return nil, "", fmt.Errorf("load app from env: %w", err)
	} else if cfg != nil {
		return cfg, "env", nil
	}
	path := filepath.Join(dataDir, "app.json")
	cfg, err := auth.ReadApp(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	if cfg == nil {
		return nil, "", nil
	}
	return cfg, "file:" + path, nil
}

func resolveBootConfig(getenv func(string) string) (bootConfig, error) {
	boot := bootConfig{
		DatabaseURL:      strings.TrimSpace(getenv("DATABASE_URL")),
		AgentToken:       strings.TrimSpace(getenv("DISPATCH_AGENT_TOKEN")),
		RepoProjects:     strings.TrimSpace(getenv("DISPATCH_REPO_PROJECTS")),
		DefaultProject:   strings.TrimSpace(getenv("DISPATCH_DEFAULT_PROJECT")),
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

	return boot, nil
}

// validateDefaultProject confirms DISPATCH_DEFAULT_PROJECT names a project
// that already exists, so a misconfigured deployment fails loudly at boot
// instead of silently rejecting every unmapped external issue at request
// time. An empty project (no default configured) is not validated here.
func validateDefaultProject(ctx context.Context, database *store.Store, project string) error {
	if project == "" {
		return nil
	}
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

func dispatchHandler(handler http.Handler, database *store.Store, natsClient *bus.Client) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthzHandler(database, natsClient))
	mux.Handle("/", handler)
	return mux
}

func healthzHandler(database *store.Store, natsClient *bus.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		databaseOK := database != nil && database.Pool != nil
		if databaseOK {
			databaseOK = database.Pool.Ping(req.Context()) == nil
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
			OK   bool  `json:"ok"`
			DB   bool  `json:"db"`
			NATS *bool `json:"nats"`
		}{OK: ok, DB: databaseOK, NATS: natsOK})
	}
}

// listenAddress builds the listen address from DISPATCH_LISTEN_HOST and
// DISPATCH_PORT. An empty host binds every interface (the containerized
// production default); local compose deployments set 127.0.0.1.
func listenAddress() (string, error) {
	host := strings.TrimSpace(os.Getenv("DISPATCH_LISTEN_HOST"))
	port := strings.TrimSpace(os.Getenv("DISPATCH_PORT"))
	if port == "" {
		return host + defaultListenAddr, nil
	}
	parsed, err := parsePositiveInt(port)
	if err != nil {
		return "", fmt.Errorf("invalid DISPATCH_PORT: %w", err)
	}
	if parsed > 65535 {
		return "", fmt.Errorf("invalid DISPATCH_PORT: %q", port)
	}
	return host + ":" + port, nil
}

func checkDocuments(ctx context.Context, databaseURL string, out io.Writer) int {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(out, "check-documents: DATABASE_URL is required")
		return 1
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(out, "check-documents: open database: %v\n", err)
		return 1
	}
	defer database.Pool.Close()

	reports, err := docs.InspectLegacyDocuments(ctx, database)
	if err != nil {
		fmt.Fprintf(out, "check-documents: inspect documents: %v\n", err)
		return 1
	}
	exitCode := 0
	for _, report := range reports {
		parse := "ok"
		if report.ParseError != nil {
			parse = "error: " + report.ParseError.Error()
			exitCode = 1
		}
		fmt.Fprintf(out, "%s %s/%s %s parse=%s anchors=%d resolvable=%d\n",
			report.ArtifactID, report.IssueKey, report.Name, report.State, parse, report.Anchors, report.Resolvable)
	}
	return exitCode
}
