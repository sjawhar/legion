// Package routes assembles the Dispatch HTTP server routes.
//
// Authenticated humans identify through either a signed cookie or a trusted
// proxy header. GitHub OAuth stores each allowed user's refreshable token pair
// so the GitHub REST and GraphQL proxy can act on their behalf.
package routes

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// AppContext holds shared dependencies for the HTTP handlers. All fields are
// read-only after BuildAppContext returns; per-request state lives on
// *router.
type AppContext struct {
	SigningKey     string
	WebDistDir     string
	Users          auth.UserStore
	Sessions       auth.SessionStore
	Identity       identity.Identity
	AllowedLogins  map[string]struct{}
	Store          *store.Store
	AgentToken     string
	RepoProjects   string
	DefaultProject string
	ServerURL      string
	HTTPClient     auth.HTTPClient
	apiDeps        api.Deps
	app            *auth.AppConfig // nil ⇒ not configured
	appSource      string          // "env" | "file:<path>" | "" — for diagnostic logs
	appMu          sync.RWMutex
}

// AppContextOptions is the explicit-injection bundle main.go assembles
// after deciding which storage / config sources to use. The router takes
// what it's given; selection logic stays in cmd/dispatch/main.go.
type AppContextOptions struct {
	SigningKey       string
	WebDistDir       string
	Users            auth.UserStore
	Sessions         auth.SessionStore
	Identity         identity.Identity
	AllowedLogins    map[string]struct{}
	Store            *store.Store
	AgentToken       string
	RepoProjects     string
	DefaultProject   string
	ServerURL        string
	EnvoyURL         string
	Docs             docs.API
	Events           *events.Broker
	App              *auth.AppConfig
	AppSource        string
	TestHooksEnabled bool
}

// BuildAppContext bundles the shared HTTP-handler state.
func BuildAppContext(opts AppContextOptions) (*AppContext, error) {
	if opts.SigningKey == "" {
		return nil, fmt.Errorf("BuildAppContext: SigningKey required")
	}
	if opts.Users == nil {
		return nil, fmt.Errorf("BuildAppContext: Users store required")
	}
	if opts.Identity == nil {
		return nil, fmt.Errorf("BuildAppContext: Identity required")
	}
	apiDeps, err := api.NewDeps(api.DepsInput{
		Store:            opts.Store,
		Identity:         opts.Identity,
		AgentToken:       opts.AgentToken,
		RepoProjectsRaw:  opts.RepoProjects,
		DefaultProject:   opts.DefaultProject,
		ServerURL:        opts.ServerURL,
		EnvoyURL:         opts.EnvoyURL,
		Docs:             opts.Docs,
		Events:           opts.Events,
		TestHooksEnabled: opts.TestHooksEnabled,
	})
	if err != nil {
		return nil, fmt.Errorf("BuildAppContext: %w", err)
	}
	return &AppContext{
		SigningKey:     opts.SigningKey,
		WebDistDir:     opts.WebDistDir,
		Users:          opts.Users,
		Sessions:       opts.Sessions,
		Identity:       opts.Identity,
		AllowedLogins:  opts.AllowedLogins,
		Store:          opts.Store,
		AgentToken:     opts.AgentToken,
		RepoProjects:   opts.RepoProjects,
		DefaultProject: opts.DefaultProject,
		ServerURL:      strings.TrimSuffix(opts.ServerURL, "/"),
		apiDeps:        apiDeps,
		app:            opts.App,
		appSource:      opts.AppSource,
	}, nil
}

// App returns the loaded Envoy App credentials, or nil if app.json is missing
// or malformed. Callers must handle nil explicitly (503).
func (ctx *AppContext) App() *auth.AppConfig {
	ctx.appMu.RLock()
	defer ctx.appMu.RUnlock()
	return ctx.app
}

const (
	pendingStateTTL  = 10 * time.Minute
	maxPendingStates = 1000
	oauthStateCookie = "dispatch_oauth_state"
	oauthStateMaxAge = 10 * 60
)

type router struct {
	ctx           *AppContext
	pendingMu     sync.Mutex
	pendingStates map[string]pendingState
}

type pendingState struct {
	next      string
	nonce     string
	expiresAt time.Time
}

// New returns an http.Handler that serves all dispatch routes.
func New(ctx *AppContext) http.Handler {
	r := &router{ctx: ctx, pendingStates: make(map[string]pendingState)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/start", r.authStart)
	mux.HandleFunc("GET /auth/callback", r.authCallback)
	mux.HandleFunc("POST /auth/logout", r.authLogout)
	mux.HandleFunc("GET /auth/whoami", r.authWhoami)
	mux.HandleFunc("/api/github/rest/", r.apiGithubRest)
	mux.HandleFunc("/api/github/graphql", r.apiGithubGraphql)
	mux.HandleFunc("GET /healthz", r.healthz)
	api.Register(mux, r.ctx.apiDeps)
	mux.HandleFunc("/", r.staticHandler)
	return r.enforceCookieOrigin(mux)
}

// ───── auth ─────────────────────────────────────────────────────────────────

func (r *router) authStart(w http.ResponseWriter, req *http.Request) {
	app := r.ctx.App()
	if app == nil {
		writeError(w, http.StatusServiceUnavailable, "Envoy App not configured — set DISPATCH_APP_CLIENT_ID/_SECRET/_PEM_B64 (env) or write ~/.local/share/dispatch/app.json")
		return
	}
	state, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "state")
		return
	}
	nonce, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "state")
		return
	}
	next := sanitizeNext(req.URL.Query().Get("next"))
	if !r.putPendingState(state, nonce, next) {
		writeError(w, http.StatusTooManyRequests, "too many pending sign-in attempts")
		return
	}
	http.SetCookie(w, oauthStateCookieFor(nonce, oauthStateMaxAge))
	redirectURI := callbackURL(r.ctx.ServerURL, req)
	target := auth.BuildAuthorizeURL(app.ClientID, redirectURI, state)
	http.Redirect(w, req, target, http.StatusFound)
}

func (r *router) authCallback(w http.ResponseWriter, req *http.Request) {
	app := r.ctx.App()
	if app == nil {
		writeError(w, http.StatusServiceUnavailable, "Envoy App not configured — set DISPATCH_APP_CLIENT_ID/_SECRET/_PEM_B64 (env) or write ~/.local/share/dispatch/app.json")
		return
	}
	code, state, err := auth.ParseCallback(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	http.SetCookie(w, oauthStateCookieFor("", -1))
	pending, ok := r.takePendingState(state)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid or expired state — start over at /auth/start")
		return
	}
	cookie, err := req.Cookie(oauthStateCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(pending.nonce)) != 1 {
		writeError(w, http.StatusBadRequest, "invalid or expired state — start over at /auth/start")
		return
	}
	tokens, err := auth.ExchangeCode(req.Context(), app.ClientID, app.ClientSecret, code, callbackURL(r.ctx.ServerURL, req), r.ctx.HTTPClient)
	if err != nil {
		slog.Warn("dispatch: oauth code exchange failed", "error", err)
		writeError(w, http.StatusBadGateway, "code exchange failed: "+err.Error())
		return
	}
	if _, allowed := r.ctx.AllowedLogins[strings.ToLower(tokens.GithubLogin)]; !allowed {
		identity.WriteError(w, identity.ErrLoginNotAllowed)
		return
	}
	user := &auth.User{Login: tokens.GithubLogin, Tokens: *tokens}
	if err := r.ctx.Users.Write(req.Context(), user); err != nil {
		slog.Error("dispatch: persist user failed", "login", user.Login, "error", err)
		writeError(w, http.StatusInternalServerError, "persist user")
		return
	}
	generation := int64(0)
	if r.ctx.Sessions != nil {
		generation, err = r.ctx.Sessions.EnsureSession(req.Context(), user.Login)
		if err != nil {
			slog.Error("dispatch: ensure session generation failed", "login", user.Login, "error", err)
			writeError(w, http.StatusInternalServerError, "session generation")
			return
		}
	}
	w.Header().Add("Set-Cookie", auth.IssueSessionCookie(user.Login, generation, r.ctx.SigningKey))
	http.Redirect(w, req, pending.next, http.StatusFound)
}
func (r *router) authLogout(w http.ResponseWriter, req *http.Request) {
	login, ok := r.login(w, req)
	if !ok {
		return
	}
	if r.ctx.Sessions != nil {
		if err := r.ctx.Sessions.RevokeSessions(req.Context(), login); err != nil {
			slog.Warn("dispatch: revoke sessions failed", "login", login, "error", err)
			writeError(w, http.StatusInternalServerError, "revoke session")
			return
		}
	}
	if err := r.ctx.Users.Remove(req.Context(), login); err != nil {
		slog.Warn("dispatch: remove user failed", "login", login, "error", err)
	}
	w.Header().Set("Set-Cookie", auth.ClearSessionCookie())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (r *router) authWhoami(w http.ResponseWriter, req *http.Request) {
	login, ok := r.login(w, req)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": "user", "login": login})
}

func (r *router) apiGithubRest(w http.ResponseWriter, req *http.Request) {
	cfg, ok := r.buildProxyConfig(w, req)
	if !ok {
		return
	}
	githubapi.ProxyREST(w, req, cfg)
}

func (r *router) apiGithubGraphql(w http.ResponseWriter, req *http.Request) {
	cfg, ok := r.buildProxyConfig(w, req)
	if !ok {
		return
	}
	githubapi.ProxyGraphQL(w, req, cfg)
}

// requireUser resolves the request identity to its stored GitHub token pair.
func (r *router) requireUser(w http.ResponseWriter, req *http.Request) *auth.User {
	login, ok := r.login(w, req)
	if !ok {
		return nil
	}
	user, err := r.ctx.Users.Read(req.Context(), login)
	if err != nil {
		slog.Warn("dispatch: read user failed", "login", login, "error", err)
		writeError(w, http.StatusInternalServerError, "read user")
		return nil
	}
	if user == nil {
		writeCodeError(w, http.StatusServiceUnavailable, "github token unavailable", "GITHUB_TOKEN_UNAVAILABLE")
		return nil
	}
	return user
}

func (r *router) login(w http.ResponseWriter, req *http.Request) (string, bool) {
	login, err := r.ctx.Identity.Login(req)
	if err != nil {
		identity.WriteError(w, err)
		return "", false
	}
	return login, true
}

func (r *router) buildProxyConfig(w http.ResponseWriter, req *http.Request) (*githubapi.ProxyConfig, bool) {
	user := r.requireUser(w, req)
	if user == nil {
		return nil, false
	}
	return r.proxyConfigForUser(w, user)
}

// proxyConfigForUser builds a GitHub proxy config from an already-resolved
// user. Returns false (writing 503) when the Envoy App credentials are absent,
// since no per-user GitHub call can be authorized without them.
func (r *router) proxyConfigForUser(w http.ResponseWriter, user *auth.User) (*githubapi.ProxyConfig, bool) {
	app := r.ctx.App()
	if app == nil {
		writeError(w, http.StatusServiceUnavailable, "Envoy App not configured — set DISPATCH_APP_CLIENT_ID/_SECRET/_PEM_B64 (env) or write ~/.local/share/dispatch/app.json")
		return nil, false
	}
	return &githubapi.ProxyConfig{
		Tokens:       &user.Tokens,
		Users:        r.ctx.Users,
		Login:        user.Login,
		ClientID:     app.ClientID,
		ClientSecret: app.ClientSecret,
		HTTPClient:   r.ctx.HTTPClient,
	}, true
}

func (r *router) healthz(w http.ResponseWriter, req *http.Request) {
	databaseOK := r.ctx.Store != nil && r.ctx.Store.Pool != nil
	if databaseOK {
		databaseOK = r.ctx.Store.Pool.Ping(req.Context()) == nil
	}
	status := http.StatusOK
	if !databaseOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ok": databaseOK, "db": databaseOK, "nats": nil})
}

// ───── static ───────────────────────────────────────────────────────────────

func (r *router) staticHandler(w http.ResponseWriter, req *http.Request) {
	if r.ctx.WebDistDir == "" {
		writeError(w, http.StatusNotFound, "dashboard build not found")
		return
	}
	requestedPath := req.URL.Path
	normalized := filepath.Clean("/" + requestedPath)
	if normalized == "/" {
		normalized = "/index.html"
	}
	if normalized == "/favicon.ico" {
		faviconPath := filepath.Join(r.ctx.WebDistDir, "favicon.svg")
		if info, err := os.Stat(faviconPath); err == nil && !info.IsDir() {
			serveFile(w, req, faviconPath)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	candidate := filepath.Join(r.ctx.WebDistDir, normalized)
	if !strings.HasPrefix(candidate, r.ctx.WebDistDir) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	info, err := os.Stat(candidate)
	if err == nil && !info.IsDir() {
		serveFile(w, req, candidate)
		return
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "stat failed")
		return
	}
	if !isBrowserRoute(normalized) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	indexPath := filepath.Join(r.ctx.WebDistDir, "index.html")
	if _, err := os.Stat(indexPath); err != nil {
		writeError(w, http.StatusNotFound, "dashboard build not found")
		return
	}
	serveFile(w, req, indexPath)
}

// isBrowserRoute reports whether an unmatched, non-static path should fall
// back to the SPA shell so the client router can render its own view
// (including its own not-found page). Reserved server prefixes and anything
// that looks like a missing static asset must stay a real 404 instead, so
// API/asset clients never get an HTML body where they expected JSON or a
// file.
func isBrowserRoute(normalized string) bool {
	if isReservedPath(normalized, []string{"/api", "/auth", "/ws", "/healthz", "/assets"}) {
		return false
	}
	// Issue routes carry user-controlled segments (artifact slugs, ask/comment
	// ids) that may legitimately contain a dot, so the file-extension
	// heuristic below must never apply to them.
	if normalized == "/issues" || strings.HasPrefix(normalized, "/issues/") {
		return true
	}
	return !strings.Contains(filepath.Base(normalized), ".")
}

// isReservedPath reports whether normalized is exactly one of roots or a
// subpath of one — never a mere string-prefix match, so a root like "/api"
// does not also claim an unrelated path like "/apix".
func isReservedPath(normalized string, roots []string) bool {
	for _, root := range roots {
		if normalized == root || strings.HasPrefix(normalized, root+"/") {
			return true
		}
	}
	return false
}

func serveFile(w http.ResponseWriter, req *http.Request, path string) {
	w.Header().Set("Content-Type", contentType(path))
	http.ServeFile(w, req, path)
}

func contentType(path string) string {
	switch filepath.Ext(path) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// ───── helpers ──────────────────────────────────────────────────────────────

// callbackURL uses Dispatch's configured canonical public origin whenever it
// is available. A backend request's Host and TLS state are used only for
// unconfigured local deployments; forwarded headers are deliberately ignored.
func callbackURL(serverURL string, req *http.Request) string {
	return publicOrigin(serverURL, req) + "/auth/callback"
}

func publicOrigin(serverURL string, req *http.Request) string {
	if parsed, err := url.Parse(serverURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
	}
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, req.Host)
}

func (r *router) enforceCookieOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !unsafeMethod(req.Method) || strings.TrimSpace(req.Header.Get("Authorization")) != "" || !auth.HasSessionCookie(req) {
			next.ServeHTTP(w, req)
			return
		}
		if origin := req.Header.Get("Origin"); origin != "" && origin == publicOrigin(r.ctx.ServerURL, req) {
			next.ServeHTTP(w, req)
			return
		}
		if req.Header.Get("Origin") == "" && req.Header.Get("Sec-Fetch-Site") == "same-origin" {
			next.ServeHTTP(w, req)
			return
		}
		writeError(w, http.StatusForbidden, "invalid request origin")
	})
}

func unsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// sanitizeNext restricts post-login redirect targets to local paths. An
// open-redirect bug here would let a phishing site bounce victims back to
// their own page after passing through our domain.
func sanitizeNext(raw string) string {
	if raw == "" || strings.Contains(raw, "\\") || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "/"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") {
		return "/"
	}
	path, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil || strings.Contains(path, "\\") || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "/"
	}
	return raw
}

func (r *router) putPendingState(token, nonce, next string) bool {
	now := time.Now()
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	for token, pending := range r.pendingStates {
		if !pending.expiresAt.After(now) {
			delete(r.pendingStates, token)
		}
	}
	if len(r.pendingStates) >= maxPendingStates {
		return false
	}
	r.pendingStates[token] = pendingState{next: next, nonce: nonce, expiresAt: now.Add(pendingStateTTL)}
	return true
}

func (r *router) takePendingState(token string) (pendingState, bool) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	pending, ok := r.pendingStates[token]
	if !ok {
		return pendingState{}, false
	}
	delete(r.pendingStates, token)
	return pending, pending.expiresAt.After(time.Now())
}

func oauthStateCookieFor(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     oauthStateCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   os.Getenv("DISPATCH_INSECURE_COOKIE") == "",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Warn("dispatch: write json failed", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeCodeError(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

func randomToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
