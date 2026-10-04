// Package routes assembles the Dispatch HTTP server routes.
//
// Authenticated people use a signed cookie issued when they sign in with Google Workspace through
// the sign-in pool. Test and local harnesses can inject header identity. The web app's GitHub reads
// go to GitHub as the GitHub App.
package routes

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
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

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

// AppContext holds shared dependencies for the HTTP handlers. All fields are
// read-only after BuildAppContext returns; per-request state lives on
// *router.
type AppContext struct {
	SigningKey     string
	WebDistDir     string
	Sessions       auth.SessionStore
	People         auth.PeopleStore
	Identity       identity.Identity
	Store          *store.Store
	DefaultProject string
	ServerURL      string
	// SignIn is the sign-in pool's authorization code flow and SignInGroup the group a person
	// must be in to sign in; a nil SignIn is a test/local header-identity or dev sign-in server,
	// whose /auth/start and /auth/callback answer 503.
	SignIn      *oidc.CodeFlow
	SignInGroup string
	// InsecureCookie drops the Secure attribute from every cookie the router sets, for browsers
	// reaching Dispatch over plain http (cmd/dispatch: DISPATCH_INSECURE_COOKIE).
	InsecureCookie bool
	apiDeps        api.Deps
	// devSignInHost is the dashboard origin's host:port when the dev sign-in route is mounted,
	// and empty otherwise. Only BuildAppContext sets it, from DevSignInOrigin, so no caller can
	// turn the route on without the origin check.
	devSignInHost string
}

// AppContextOptions is the explicit-injection bundle main.go assembles
// after deciding which storage / config sources to use. The router takes
// what it's given; selection logic stays in cmd/dispatch/main.go.
type AppContextOptions struct {
	SigningKey     string
	WebDistDir     string
	Sessions       auth.SessionStore
	People         auth.PeopleStore
	Identity       identity.Identity
	SignIn         *oidc.CodeFlow
	SignInGroup    string
	Store          *store.Store
	AgentTokens    *auth.SharedAgentTokens
	DefaultProject string
	ServerURL      string
	InsecureCookie bool
	EnvoyURL       string
	// EnvoyToken is the bearer every Envoy listener call sends (cmd/dispatch: ENVOY_TOKEN); empty
	// sends none.
	EnvoyToken    string
	Docs          docs.API
	Events        *events.Broker
	App           *auth.AppConfig
	GitHubAPIBase string
	OIDC          *oidc.Verifier
	AgentStream   agentstream.Source
	Lifetime      context.Context
	// AgentSecretsURL/AgentSecretsToken configure the credential-request UI's secrets broker
	// client; empty URL means the feature is off. See api.DepsInput.
	AgentSecretsURL   string
	AgentSecretsToken string
	TestHooksEnabled  bool
	// DevSignIn mounts GET /auth/_dev/signin, which issues the session cookie for any person
	// named with no sign-in pool exchange, and makes the whole router refuse a request whose Host
	// is not the dashboard origin. cmd/dispatch sets it from DISPATCH_DEV_SIGNIN behind its boot
	// fence; BuildAppContext refuses it for a non-loopback ServerURL.
	DevSignIn bool
}

// BuildAppContext bundles the shared HTTP-handler state.
func BuildAppContext(opts AppContextOptions) (*AppContext, error) {
	if opts.SigningKey == "" {
		return nil, fmt.Errorf("BuildAppContext: SigningKey required")
	}
	if opts.People == nil {
		return nil, fmt.Errorf("BuildAppContext: People store required")
	}
	if opts.SignIn != nil && opts.SignInGroup == "" {
		return nil, fmt.Errorf("BuildAppContext: SignIn requires a SignInGroup")
	}
	if opts.Identity == nil {
		return nil, fmt.Errorf("BuildAppContext: Identity required")
	}
	var devSignInHost string
	if opts.DevSignIn {
		if opts.Sessions == nil {
			return nil, fmt.Errorf("BuildAppContext: DevSignIn requires a Sessions store")
		}
		host, err := DevSignInOrigin(opts.ServerURL)
		if err != nil {
			return nil, fmt.Errorf("BuildAppContext: %w", err)
		}
		devSignInHost = host
	}
	apiDeps, err := api.NewDeps(api.DepsInput{
		Store:             opts.Store,
		Identity:          opts.Identity,
		AgentTokens:       opts.AgentTokens,
		DefaultProject:    opts.DefaultProject,
		ServerURL:         opts.ServerURL,
		EnvoyURL:          opts.EnvoyURL,
		EnvoyToken:        opts.EnvoyToken,
		Docs:              opts.Docs,
		Events:            opts.Events,
		App:               opts.App,
		GitHubAPIBase:     opts.GitHubAPIBase,
		OIDC:              opts.OIDC,
		AgentStream:       opts.AgentStream,
		Lifetime:          opts.Lifetime,
		AgentSecretsURL:   opts.AgentSecretsURL,
		AgentSecretsToken: opts.AgentSecretsToken,
		TestHooksEnabled:  opts.TestHooksEnabled,
	})
	if err != nil {
		return nil, fmt.Errorf("BuildAppContext: %w", err)
	}
	return &AppContext{
		SigningKey:     opts.SigningKey,
		WebDistDir:     opts.WebDistDir,
		Sessions:       opts.Sessions,
		People:         opts.People,
		Identity:       opts.Identity,
		Store:          opts.Store,
		DefaultProject: opts.DefaultProject,
		ServerURL:      strings.TrimSuffix(opts.ServerURL, "/"),
		SignIn:         opts.SignIn,
		SignInGroup:    opts.SignInGroup,
		InsecureCookie: opts.InsecureCookie,
		devSignInHost:  devSignInHost,
		apiDeps:        apiDeps,
	}, nil
}

// Architecture is the shared architecture importer the HTTP routes use, so the
// server's five-minute sync ticker drives the same serialized Sync the Refresh
// route and the sync tool do.
func (ctx *AppContext) Architecture() *architecture.Importer {
	return ctx.apiDeps.Architecture
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
	mux.HandleFunc(githubapi.Prefix, r.apiGithubRest)
	api.Register(mux, r.ctx.apiDeps)
	mux.HandleFunc("/", r.staticHandler)
	if ctx.devSignInHost == "" {
		return r.enforceCookieOrigin(mux)
	}
	mux.HandleFunc("GET /auth/_dev/signin", r.authDevSignIn)
	return requireHost(ctx.devSignInHost, r.enforceCookieOrigin(mux))
}

// ───── auth ─────────────────────────────────────────────────────────────────

func (r *router) authStart(w http.ResponseWriter, req *http.Request) {
	if r.ctx.SignIn == nil {
		signInUnconfigured(w)
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
	http.SetCookie(w, oauthStateCookieFor(nonce, oauthStateMaxAge, !r.ctx.InsecureCookie))
	http.Redirect(w, req, r.ctx.SignIn.AuthURL(callbackURL(r.ctx.ServerURL, req), state, nonce), http.StatusFound)
}

// authCallback completes a sign-in the sign-in pool redirected back: it exchanges the code for
// the person's ID token, names them by the email their Google Workspace username carries, and
// signs in only a member of SignInGroup, keeping the pool's refresh token to confirm that
// membership hourly.
func (r *router) authCallback(w http.ResponseWriter, req *http.Request) {
	if r.ctx.SignIn == nil {
		signInUnconfigured(w)
		return
	}
	code, state, err := auth.ParseCallback(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	http.SetCookie(w, oauthStateCookieFor("", -1, !r.ctx.InsecureCookie))
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
	session, err := r.ctx.SignIn.Exchange(req.Context(), callbackURL(r.ctx.ServerURL, req), code, pending.nonce)
	if err != nil {
		slog.Warn("dispatch: sign-in code exchange failed", "error", err)
		writeError(w, http.StatusBadGateway, "sign-in failed: "+err.Error())
		return
	}
	email, err := identity.Person(session.Claims, r.ctx.SignInGroup)
	if err != nil {
		slog.Warn("dispatch: sign-in refused", "username", session.Claims.Username, "reason", err)
		writeSignInRefusedPage(w, email, err)
		return
	}
	if err := r.ctx.People.SignIn(req.Context(), email, session.RefreshToken, time.Now()); err != nil {
		slog.Error("dispatch: record sign-in failed", "email", email, "error", err)
		writeError(w, http.StatusInternalServerError, "record sign-in")
		return
	}
	if !r.issueSession(w, req, email) {
		return
	}
	http.Redirect(w, req, pending.next, http.StatusFound)
}

// issueSession ensures the person's session generation and sets the session cookie a sign-in
// issues, answering 500 when the generation cannot be recorded. authCallback and authDevSignIn
// both mint through it, so a local dev sign-in carries exactly the cookie a pool sign-in does.
func (r *router) issueSession(w http.ResponseWriter, req *http.Request, email string) bool {
	generation := int64(0)
	if r.ctx.Sessions != nil {
		var err error
		generation, err = r.ctx.Sessions.EnsureSession(req.Context(), email)
		if err != nil {
			slog.Error("dispatch: ensure session generation failed", "email", email, "error", err)
			writeError(w, http.StatusInternalServerError, "session generation")
			return false
		}
	}
	w.Header().Add("Set-Cookie", auth.IssueSessionCookie(email, generation, r.ctx.SigningKey, !r.ctx.InsecureCookie))
	return true
}

func (r *router) authLogout(w http.ResponseWriter, req *http.Request) {
	email, ok := r.login(w, req)
	if !ok {
		return
	}
	if r.ctx.Sessions != nil {
		if err := r.ctx.Sessions.RevokeSessions(req.Context(), email); err != nil {
			slog.Warn("dispatch: revoke sessions failed", "email", email, "error", err)
			writeError(w, http.StatusInternalServerError, "revoke session")
			return
		}
	}
	if err := r.ctx.People.End(req.Context(), email); err != nil {
		slog.Warn("dispatch: forget the sign-in's refresh token failed", "email", email, "error", err)
	}
	w.Header().Set("Set-Cookie", auth.ClearSessionCookie(!r.ctx.InsecureCookie))
	api.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (r *router) authWhoami(w http.ResponseWriter, req *http.Request) {
	email, ok := r.login(w, req)
	if !ok {
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"kind": "user", "login": email})
}

// apiGithubRest serves the web app's GitHub reads to a person, as the GitHub App.
func (r *router) apiGithubRest(w http.ResponseWriter, req *http.Request) {
	if _, ok := r.login(w, req); !ok {
		return
	}
	githubapi.ProxyREST(w, req, r.ctx.apiDeps.GitHub)
}

func (r *router) login(w http.ResponseWriter, req *http.Request) (string, bool) {
	login, err := r.ctx.Identity.Login(req)
	if err != nil {
		identity.WriteError(w, err)
		return "", false
	}
	return login, true
}

func signInUnconfigured(w http.ResponseWriter) {
	writeCodeError(w, http.StatusServiceUnavailable, "Google sign-in is not configured on this server (DISPATCH_SIGNIN_ISSUER, DISPATCH_SIGNIN_CLIENT_ID, DISPATCH_SIGNIN_CLIENT_SECRET, DISPATCH_SIGNIN_GROUP)", "SIGNIN_UNCONFIGURED")
}

// ───── static ───────────────────────────────────────────────────────────────

// serverRoots are the path roots the server itself answers: an unmatched path under one of them
// is a real JSON 404, decided before any dashboard lookup, never the SPA shell. "/v1" is
// reserved because it is the API path typed without its "/api" prefix — an API client's mistake
// that must be answered as one.
var serverRoots = []string{"/api", "/v1", "/auth", "/ws", "/healthz"}

// assetRoots hold Vite's content-hashed build output: a file there never changes under its name,
// because a changed file is written under a new one.
var assetRoots = []string{"/assets"}

const (
	// pageCacheControl makes a browser revalidate a page before it runs it. A page names the
	// hashed assets of the build that wrote it, and one kept by heuristic freshness from a
	// Last-Modified would, after a deploy, ask for assets the server no longer has.
	pageCacheControl = "no-cache"
	// assetCacheControl lets a browser keep a hashed asset for a year without asking.
	assetCacheControl = "public, max-age=31536000, immutable"
)

func (r *router) staticHandler(w http.ResponseWriter, req *http.Request) {
	requestedPath := req.URL.Path
	// A rooted clean holds no `..`, so every path joined under the dist directory below stays in it.
	normalized := filepath.Clean("/" + requestedPath)
	if isReservedPath(normalized, serverRoots) {
		api.WriteJSON(w, http.StatusNotFound, map[string]string{
			"code":  "NOT_FOUND",
			"error": "no route for " + req.Method + " " + requestedPath,
			"hint":  "GET /api/v1 lists every route",
		})
		return
	}
	if r.ctx.WebDistDir == "" {
		writeError(w, http.StatusNotFound, "dashboard build not found")
		return
	}
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
	info, err := os.Stat(candidate)
	if err == nil && !info.IsDir() {
		if filepath.Ext(candidate) == ".html" {
			servePage(w, req, candidate)
			return
		}
		if isReservedPath(normalized, assetRoots) {
			// A 304 keeps it, and net/http drops it from an error it answers instead (a file
			// removed after its stat), so a failure is never kept for an asset's year.
			w.Header().Set("Cache-Control", assetCacheControl)
		}
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
	servePage(w, req, filepath.Join(r.ctx.WebDistDir, "index.html"))
}

// isBrowserRoute reports whether an unmatched, non-static path should fall
// back to the SPA shell so the client router can render its own view
// (including its own not-found page). The built dashboard's asset root and
// anything that looks like a missing static asset must stay a real 404
// instead, so asset clients never get an HTML body where they expected a file.
func isBrowserRoute(normalized string) bool {
	if isReservedPath(normalized, assetRoots) {
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

// servePage serves an HTML page that the browser must revalidate, validated by its content and
// never by its modification time. The servers behind one load balancer can hold different builds
// whose pages' times say nothing about which build wrote them (a rollback serves the older file),
// so a revalidation answered by If-Modified-Since could keep one build's page in front of another
// build's assets. The page carries an ETag of its bytes and no Last-Modified, so a browser holding
// another build's page, or one cached before this, gets the page this server has.
func servePage(w http.ResponseWriter, req *http.Request, path string) {
	page, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "dashboard build not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read failed")
		return
	}
	sum := sha256.Sum256(page)
	w.Header().Set("Content-Type", contentType(path))
	w.Header().Set("Cache-Control", pageCacheControl)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
	http.ServeContent(w, req, path, time.Time{}, bytes.NewReader(page))
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

func oauthStateCookieFor(value string, maxAge int, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     oauthStateCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	api.WriteJSON(w, status, map[string]string{"error": message})
}

func writeCodeError(w http.ResponseWriter, status int, message, code string) {
	api.WriteJSON(w, status, map[string]string{"error": message, "code": code})
}

// writeSignInRefusedPage answers the sign-in callback, a top-level browser navigation back from
// the sign-in pool where JSON would be unreadable, for a person the pool vouched for but Dispatch
// does not admit: one outside the group, named by email, or a sign-in that names nobody by email.
func writeSignInRefusedPage(w http.ResponseWriter, email string, reason error) {
	message := "This sign-in does not name a person by a Google Workspace email. Sign in with your Google Workspace account."
	if errors.Is(reason, identity.ErrNotMember) {
		message = html.EscapeString(email) + " is not in the group that may use Dispatch. Ask an administrator to add you, then sign in again."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Dispatch: sign-in refused</title></head>
<body style="font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem">
<h1>Sign-in refused</h1>
<p>%s</p>
<p><a href="/auth/start">Sign in with a different account</a></p>
</body></html>
`, message)
}

func randomToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
