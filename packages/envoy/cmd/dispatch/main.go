// Command dispatch is the Go port of the Dispatch HTTP server.
//
// Routes:
//
//	/auth/*           — web-flow OAuth (start/callback/logout/whoami)
//	/api/events       — SSE stream of NATS-forwarded GitHub events
//	/api/github/*     — reverse proxy to GitHub REST + GraphQL (per-user token)
//	/api/installations— enumerate the user's Envoy App installations + repos
//	/api/view         — GET/PATCH per-user watched-repos list
//	/mcp              — MCP Streamable HTTP endpoint (per-request bearer auth)
//	/healthz          — liveness check
//	everything else   — SPA from packages/dispatch/web/dist (SPA fallback)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	natsclient "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/config"
	"github.com/sjawhar/envoy/internal/dispatch/nats"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
)

const (
	listenAddr     = ":8766"
	shutdownTimout = 5 * time.Second
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := config.Load(config.LoadOptions{})
	if err != nil {
		slog.Error("dispatch: load config", "error", err)
		os.Exit(1)
	}

	defaultRepo := ""
	if cfg.Dispatch != nil {
		defaultRepo = cfg.Dispatch.DefaultRepo
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	nc, err := nats.Connect(cfg.NatsURLs)
	if err != nil {
		slog.Error("dispatch: connect nats", "error", err)
		os.Exit(1)
	}
	defer nc.Close()

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

	users, err := openUserStore(nc, dataDir)
	if err != nil {
		slog.Error("dispatch: open user store", "error", err)
		os.Exit(1)
	}

	appCtx, err := routes.BuildAppContext(routes.AppContextOptions{
		SigningKey:  signingKey,
		WebDistDir:  webDistDir,
		Users:       users,
		App:         appCfg,
		AppSource:   appSource,
		DefaultRepo: defaultRepo,
	})
	if err != nil {
		slog.Error("dispatch: build app context", "error", err)
		os.Exit(1)
	}

	if _, err := nats.SubscribeGithub(ctx, nc, appCtx.Hub); err != nil {
		slog.Error("dispatch: subscribe github", "error", err)
		os.Exit(1)
	}

	handler := routes.New(appCtx)
	server := &http.Server{
		Addr:    listenAddr,
		Handler: handler,
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

// openUserStore picks the file-backed or NATS-KV-backed implementation
// based on DISPATCH_USER_STORE:
//
//	unset / "file"   → FileUserStore in ~/.local/share/dispatch/users
//	"kv"             → KVUserStore on the dispatch_users JetStream KV bucket
//	                   (DISPATCH_USER_STORE_REPLICAS controls the replica
//	                   count on first creation; default 1)
//
// Production deployments (Fargate, k8s) set DISPATCH_USER_STORE=kv so user
// records survive container restarts and replicas share state.
func openUserStore(nc *natsclient.Conn, dataDir string) (auth.UserStore, error) {
	kind := strings.ToLower(strings.TrimSpace(os.Getenv("DISPATCH_USER_STORE")))
	if kind == "" || kind == "file" {
		return &auth.FileUserStore{Dir: filepath.Join(dataDir, "users")}, nil
	}
	if kind != "kv" {
		return nil, fmt.Errorf("DISPATCH_USER_STORE=%q (expected file or kv)", kind)
	}
	replicas := 1
	if raw := os.Getenv("DISPATCH_USER_STORE_REPLICAS"); raw != "" {
		n, err := parsePositiveInt(raw)
		if err != nil {
			return nil, fmt.Errorf("DISPATCH_USER_STORE_REPLICAS: %w", err)
		}
		replicas = n
	}
	return auth.OpenKVUserStore(nc, replicas)
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
