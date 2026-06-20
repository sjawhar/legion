// Package routes assembles the http.ServeMux for the Dispatch HTTP server.
//
// The dashboard is multi-user (one record per GitHub login on disk under
// ~/.local/share/dispatch/users/<login>.json) and multi-repo (each user keeps
// their own list of watched <owner>/<repo> pairs; the sidebar aggregates).
//
// Auth is GitHub Apps web flow:
//
//	GET  /auth/start          → redirect to github.com/login/oauth/authorize
//	GET  /auth/callback       → exchange ?code= for tokens, persist, set cookie
//	POST /auth/logout         → drop user record + clear cookie
//	GET  /auth/whoami         → return current login (or 401)
//
// Per-user view:
//
//	GET   /api/view                  → { watchedRepos: ["…", …] }
//	PATCH /api/view                  → replace watchedRepos
//	GET   /api/installations         → user's Envoy App installations
//	GET   /api/installations/{id}/repositories → repos in one installation
//
// The Envoy App's credentials (client_id, client_secret, …) live in
// ~/.local/share/dispatch/app.json. If that file is missing, all auth/proxy
// routes return 503; the dashboard's login button surfaces the message so
// the operator knows to drop the file in place.
package routes

import (
	"crypto/rand"
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
	"regexp"
	"strings"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
	"github.com/sjawhar/envoy/internal/dispatch/mcp"
	"github.com/sjawhar/envoy/internal/dispatch/sse"
)

// threadKeyShape validates the "<owner>/<repo>#<number>" form used as the
// composite key for per-user thread state (currently just the addressed
// map). Loose validation — GitHub's own repo-name rules are more complex,
// but anything matching the obvious shape will round-trip safely.
var threadKeyShape = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+#[1-9][0-9]*$`)

// AppContext holds shared dependencies for the HTTP handlers. All fields are
// read-only after BuildAppContext returns; per-request state lives on
// *router.
type AppContext struct {
	SigningKey string
	WebDistDir string
	Users      auth.UserStore
	HTTPClient auth.HTTPClient
	Hub        *sse.Hub
	MCPServer  *mcp.Server
	app        *auth.AppConfig // nil ⇒ not configured
	appSource  string          // "env" | "file:<path>" | "" — for diagnostic logs
	appMu      sync.RWMutex
}

// AppContextOptions is the explicit-injection bundle main.go assembles
// after deciding which storage / config sources to use. The router takes
// what it's given; selection logic stays in cmd/dispatch/main.go.
type AppContextOptions struct {
	SigningKey  string
	WebDistDir  string
	Users       auth.UserStore
	App         *auth.AppConfig
	AppSource   string
	DefaultRepo string
}

// BuildAppContext bundles the shared HTTP-handler state.
func BuildAppContext(opts AppContextOptions) (*AppContext, error) {
	if opts.SigningKey == "" {
		return nil, fmt.Errorf("BuildAppContext: SigningKey required")
	}
	if opts.Users == nil {
		return nil, fmt.Errorf("BuildAppContext: Users store required")
	}
	return &AppContext{
		SigningKey: opts.SigningKey,
		WebDistDir: opts.WebDistDir,
		Users:      opts.Users,
		Hub:        sse.New(),
		MCPServer:  mcp.New(opts.DefaultRepo),
		app:        opts.App,
		appSource:  opts.AppSource,
	}, nil
}

// App returns the loaded Envoy App credentials, or nil if app.json is missing
// or malformed. Callers must handle nil explicitly (503).
func (ctx *AppContext) App() *auth.AppConfig {
	ctx.appMu.RLock()
	defer ctx.appMu.RUnlock()
	return ctx.app
}

type router struct {
	ctx *AppContext
	// pendingStates holds opaque state tokens emitted by /auth/start. The
	// callback validates and consumes them. Bounded because tokens are
	// removed on consume or implicitly when the process restarts.
	pendingStates sync.Map // map[string]pendingState
}

type pendingState struct {
	next string // optional `?next=` redirect target, sanitized
}

// New returns an http.Handler that serves all dispatch routes.
func New(ctx *AppContext) http.Handler {
	r := &router{ctx: ctx}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/start", r.authStart)
	mux.HandleFunc("GET /auth/callback", r.authCallback)
	mux.HandleFunc("POST /auth/logout", r.authLogout)
	mux.HandleFunc("GET /auth/whoami", r.authWhoami)
	mux.HandleFunc("GET /api/events", r.apiEvents)
	mux.HandleFunc("/api/github/rest/", r.apiGithubRest)
	mux.HandleFunc("/api/github/graphql", r.apiGithubGraphql)
	mux.HandleFunc("GET /api/installations", r.apiInstallations)
	mux.HandleFunc("GET /api/installations/{id}/repositories", r.apiInstallationRepos)
	mux.HandleFunc("GET /api/view", r.apiViewGet)
	mux.HandleFunc("PATCH /api/view", r.apiViewPatch)
	mux.Handle("/mcp", ctx.MCPServer.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/", r.staticHandler)
	return mux
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
	next := sanitizeNext(req.URL.Query().Get("next"))
	r.pendingStates.Store(state, pendingState{next: next})
	redirectURI := callbackURL(req)
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
	rawPending, ok := r.pendingStates.LoadAndDelete(state)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid or expired state — start over at /auth/start")
		return
	}
	pending := rawPending.(pendingState)
	tokens, err := auth.ExchangeCode(req.Context(), app.ClientID, app.ClientSecret, code, callbackURL(req), r.ctx.HTTPClient)
	if err != nil {
		slog.Warn("dispatch: oauth code exchange failed", "error", err)
		writeError(w, http.StatusBadGateway, "code exchange failed: "+err.Error())
		return
	}
	// Preserve any existing watched-repos list on re-auth; first-time users
	// get an empty list and pick from the topbar.
	existing, _ := r.ctx.Users.Read(tokens.GithubLogin)
	user := &auth.User{Login: tokens.GithubLogin, Tokens: *tokens}
	if existing != nil {
		user.WatchedRepos = existing.WatchedRepos
	}
	if err := r.ctx.Users.Write(user); err != nil {
		slog.Error("dispatch: persist user failed", "login", user.Login, "error", err)
		writeError(w, http.StatusInternalServerError, "persist user")
		return
	}
	w.Header().Set("Set-Cookie", auth.IssueSessionCookie(user.Login, r.ctx.SigningKey))
	http.Redirect(w, req, pending.next, http.StatusFound)
}

func (r *router) authLogout(w http.ResponseWriter, req *http.Request) {
	login := auth.SessionLogin(req, r.ctx.SigningKey)
	if login != "" {
		if err := r.ctx.Users.Remove(login); err != nil {
			slog.Warn("dispatch: remove user failed", "login", login, "error", err)
		}
	}
	w.Header().Set("Set-Cookie", auth.ClearSessionCookie())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (r *router) authWhoami(w http.ResponseWriter, req *http.Request) {
	login := auth.RequireSession(w, req, r.ctx.SigningKey)
	if login == "" {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"login": login})
}

// ───── view (watched repos) ─────────────────────────────────────────────────

func (r *router) apiViewGet(w http.ResponseWriter, req *http.Request) {
	user := r.requireUser(w, req)
	if user == nil {
		return
	}
	writeJSON(w, http.StatusOK, viewPayload(user))
}

func (r *router) apiViewPatch(w http.ResponseWriter, req *http.Request) {
	user := r.requireUser(w, req)
	if user == nil {
		return
	}
	// Both fields are optional. Send only the one you're updating; the other
	// stays untouched. Sending an empty object is a no-op (returns current).
	var body struct {
		WatchedRepos *[]string          `json:"watchedRepos,omitempty"`
		Addressed    *map[string]string `json:"addressed,omitempty"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.WatchedRepos != nil {
		for _, s := range *body.WatchedRepos {
			if err := auth.ValidateRepoSlug(s); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// Authorize any newly-added repo against the user's own GitHub token so
		// a user cannot subscribe to event streams for repositories they cannot
		// see. Repos already watched (and removals) skip the check.
		if added := addedRepos(user.WatchedRepos, *body.WatchedRepos); len(added) > 0 {
			cfg, ok := r.proxyConfigForUser(w, user)
			if !ok {
				return
			}
			for _, slug := range added {
				owner, name, valid := githubapi.SplitRepo(slug)
				if !valid {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid repo slug %q", slug))
					return
				}
				if err := githubapi.CheckRepoAccess(req.Context(), cfg, owner, name); err != nil {
					if errors.Is(err, githubapi.ErrRepoForbidden) {
						writeError(w, http.StatusForbidden, fmt.Sprintf("you do not have access to %s", slug))
						return
					}
					slog.Warn("dispatch: repo access check failed", "repo", slug, "error", err)
					writeError(w, http.StatusBadGateway, "could not verify repo access")
					return
				}
			}
		}
		user.WatchedRepos = *body.WatchedRepos
	}
	if body.Addressed != nil {
		// Validate each entry's key shape so we don't end up with garbage in
		// the on-disk record. ISO timestamps are accepted verbatim; mismatches
		// just look like "never reached this updatedAt" to the sidebar filter.
		next := map[string]string{}
		for key, ts := range *body.Addressed {
			if !threadKeyShape.MatchString(key) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid thread key %q (expected <owner>/<repo>#<n>)", key))
				return
			}
			next[key] = ts
		}
		user.Addressed = next
	}
	if err := r.ctx.Users.Write(user); err != nil {
		slog.Error("dispatch: persist user view failed", "login", user.Login, "error", err)
		writeError(w, http.StatusInternalServerError, "persist view")
		return
	}
	writeJSON(w, http.StatusOK, viewPayload(user))
}

func viewPayload(user *auth.User) map[string]any {
	return map[string]any{
		"login":        user.Login,
		"watchedRepos": user.WatchedRepos,
		"addressed":    user.Addressed,
	}
}

// addedRepos returns the entries in next that are not already present in
// current (case-insensitive). Used to authorize only newly-watched repos.
func addedRepos(current, next []string) []string {
	have := make(map[string]struct{}, len(current))
	for _, s := range current {
		have[strings.ToLower(strings.TrimSpace(s))] = struct{}{}
	}
	var added []string
	for _, s := range next {
		key := strings.ToLower(strings.TrimSpace(s))
		if key == "" {
			continue
		}
		if _, ok := have[key]; !ok {
			added = append(added, s)
		}
	}
	return added
}

// ───── installations ────────────────────────────────────────────────────────

func (r *router) apiInstallations(w http.ResponseWriter, req *http.Request) {
	r.proxyGithubPath(w, req, "/user/installations")
}

func (r *router) apiInstallationRepos(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing installation id")
		return
	}
	r.proxyGithubPath(w, req, "/user/installations/"+url.PathEscape(id)+"/repositories")
}

func (r *router) proxyGithubPath(w http.ResponseWriter, req *http.Request, path string) {
	cfg, ok := r.buildProxyConfig(w, req)
	if !ok {
		return
	}
	target := "https://api.github.com" + path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	githubapi.ForwardRequest(w, req, cfg, target)
}

// ───── events + github proxy ────────────────────────────────────────────────

func (r *router) apiEvents(w http.ResponseWriter, req *http.Request) {
	user := r.requireUser(w, req)
	if user == nil {
		return
	}
	sse.HandlerFor(r.ctx.Hub, user.Login, user.WatchedRepos)(w, req)
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

// requireUser resolves the session cookie to a User record, writing 401 to
// the response if the session is invalid or the user file is missing.
func (r *router) requireUser(w http.ResponseWriter, req *http.Request) *auth.User {
	login := auth.RequireSession(w, req, r.ctx.SigningKey)
	if login == "" {
		return nil
	}
	user, err := r.ctx.Users.Read(login)
	if err != nil {
		slog.Warn("dispatch: read user failed", "login", login, "error", err)
		writeJSON(w, http.StatusUnauthorized, map[string]bool{"needs_reauth": true})
		return nil
	}
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]bool{"needs_reauth": true})
		return nil
	}
	return user
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
		WatchedRepos: user.WatchedRepos,
		ClientID:     app.ClientID,
		ClientSecret: app.ClientSecret,
		HTTPClient:   r.ctx.HTTPClient,
	}, true
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
	indexPath := filepath.Join(r.ctx.WebDistDir, "index.html")
	if _, err := os.Stat(indexPath); err != nil {
		writeError(w, http.StatusNotFound, "dashboard build not found")
		return
	}
	serveFile(w, req, indexPath)
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

// callbackURL reconstructs the public origin from the incoming request and
// appends /auth/callback. Must match the App's configured callback URL on
// github.com — that's why we always recompute from req rather than store it
// in app.json (operator can move the deployment without re-bootstrapping).
func callbackURL(req *http.Request) string {
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/auth/callback", scheme, req.Host)
}

// sanitizeNext restricts post-login redirect targets to local paths. An
// open-redirect bug here would let a phishing site bounce victims back to
// their own page after passing through our domain.
func sanitizeNext(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	return raw
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

func randomToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
