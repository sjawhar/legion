package routes

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

type memoryUserStore struct {
	users map[string]*auth.User
}

func (s *memoryUserStore) Read(_ context.Context, login string) (*auth.User, error) {
	user := s.users[login]
	if user == nil {
		return nil, nil
	}
	copy := *user
	return &copy, nil
}

func (s *memoryUserStore) Write(_ context.Context, user *auth.User) error {
	copy := *user
	s.users[user.Login] = &copy
	return nil
}

func (s *memoryUserStore) Remove(_ context.Context, login string) error {
	delete(s.users, login)
	return nil
}

type callbackHTTPClient struct {
	login string
}

func (c callbackHTTPClient) Do(req *http.Request) (*http.Response, error) {
	body := ""
	switch req.URL.Path {
	case "/login/oauth/access_token":
		body = `{"access_token":"access","refresh_token":"refresh","expires_in":28800,"refresh_token_expires_in":15552000}`
	case "/user":
		body = `{"login":"` + c.login + `"}`
	default:
		body = `{}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func newTestRouter(t *testing.T, users auth.UserStore, allowed map[string]struct{}) (http.Handler, *AppContext) {
	t.Helper()
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey: "signing-key",
		Users:      users,
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: allowed,
		},
		AllowedLogins: allowed,
		App:           &auth.AppConfig{ClientID: "client-id", ClientSecret: "client-secret"},
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	return New(ctx), ctx
}

func oauthState(t *testing.T, handler http.Handler) string {
	t.Helper()
	start := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/start", nil)
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, start)
	if startResponse.Code != http.StatusFound {
		t.Fatalf("start status: got %d, want %d", startResponse.Code, http.StatusFound)
	}
	location, err := url.Parse(startResponse.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse start location: %v", err)
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("start response missing state")
	}
	return state
}

func TestOAuthCallbackRejectsUnlistedLoginBeforePersistingOrIssuingCookie(t *testing.T) {
	users := &memoryUserStore{users: map[string]*auth.User{}}
	handler, ctx := newTestRouter(t, users, map[string]struct{}{"sjawhar": {}})

	state := oauthState(t, handler)
	callback := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/callback?code=code&state="+url.QueryEscape(state), nil)
	callbackResponse := httptest.NewRecorder()
	ctx.HTTPClient = callbackHTTPClient{login: "mallory"}
	handler.ServeHTTP(callbackResponse, callback)

	if callbackResponse.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want %d", callbackResponse.Code, http.StatusForbidden)
	}
	if callbackResponse.Header().Get("Set-Cookie") != "" {
		t.Errorf("unexpected cookie: %q", callbackResponse.Header().Get("Set-Cookie"))
	}
	if user, _ := users.Read(context.Background(), "mallory"); user != nil {
		t.Errorf("unlisted user persisted: %+v", user)
	}
}

func TestOAuthCallbackIssuesCookieForAllowedLogin(t *testing.T) {
	users := &memoryUserStore{users: map[string]*auth.User{}}
	handler, ctx := newTestRouter(t, users, map[string]struct{}{"sjawhar": {}})

	state := oauthState(t, handler)
	callback := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/callback?code=code&state="+url.QueryEscape(state), nil)
	callbackResponse := httptest.NewRecorder()
	ctx.HTTPClient = callbackHTTPClient{login: "sjawhar"}
	handler.ServeHTTP(callbackResponse, callback)
	if callbackResponse.Code != http.StatusFound {
		t.Errorf("status: got %d, want %d", callbackResponse.Code, http.StatusFound)
	}
	if callbackResponse.Header().Get("Set-Cookie") == "" {
		t.Error("allowed login did not receive a session cookie")
	}
}

func TestWhoamiUsesHeaderIdentity(t *testing.T) {
	handler, _ := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, map[string]struct{}{"sjawhar": {}})

	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
	request.Header.Set("X-Dispatch-User", "sjawhar")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"login":"sjawhar"`) ||
		!strings.Contains(response.Body.String(), `"kind":"user"`) {
		t.Errorf("allowed header response: status=%d body=%s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil))
	if response.Code != http.StatusUnauthorized {
		t.Errorf("missing header status: got %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestGitHubProxyUsesHeaderIdentity(t *testing.T) {
	users := &memoryUserStore{users: map[string]*auth.User{
		"sjawhar": {
			Login: "sjawhar",
			Tokens: auth.Tokens{
				AccessToken:     "access",
				AccessExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			},
		},
	}}
	handler, ctx := newTestRouter(t, users, map[string]struct{}{"sjawhar": {}})
	ctx.HTTPClient = callbackHTTPClient{}
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/api/github/rest/user", nil)
	request.Header.Set("X-Dispatch-User", "sjawhar")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Errorf("proxy status: got %d, want %d", response.Code, http.StatusOK)
	}
}

func TestCookieIdentityRechecksAllowedLogins(t *testing.T) {
	allowed := map[string]struct{}{"sjawhar": {}}
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:    "signing-key",
		Users:         &memoryUserStore{users: map[string]*auth.User{}},
		Identity:      identity.CookieIdentity{SigningKey: "signing-key", AllowedLogins: allowed},
		AllowedLogins: allowed,
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	handler := New(ctx)
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie("sjawhar", "signing-key"))
	if err != nil {
		t.Fatalf("parse session cookie: %v", err)
	}
	request.AddCookie(cookie)
	allowedResponse := httptest.NewRecorder()
	handler.ServeHTTP(allowedResponse, request)
	if allowedResponse.Code != http.StatusOK {
		t.Fatalf("allowed cookie status: got %d, want %d", allowedResponse.Code, http.StatusOK)
	}

	delete(allowed, "sjawhar")
	revokedResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokedResponse, request)
	if revokedResponse.Code != http.StatusForbidden || !strings.Contains(revokedResponse.Body.String(), `"code":"LOGIN_NOT_ALLOWED"`) {
		t.Fatalf("revoked cookie status: got %d body=%s, want LOGIN_NOT_ALLOWED", revokedResponse.Code, revokedResponse.Body.String())
	}
}

func TestAuthStartCapsPendingStates(t *testing.T) {
	handler, _ := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
	for requestNumber := range 1000 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/start", nil))
		if response.Code != http.StatusFound {
			t.Fatalf("start %d status: got %d, want %d", requestNumber, response.Code, http.StatusFound)
		}
	}
	overflow := httptest.NewRecorder()
	handler.ServeHTTP(overflow, httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/start", nil))
	if overflow.Code != http.StatusTooManyRequests {
		t.Fatalf("overflow status: got %d body=%s, want %d", overflow.Code, overflow.Body.String(), http.StatusTooManyRequests)
	}
}

func TestOAuthCallbackRejectsExpiredPendingState(t *testing.T) {
	_, ctx := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, map[string]struct{}{"sjawhar": {}})
	router := &router{
		ctx: ctx,
		pendingStates: map[string]pendingState{
			"expired": {next: "/", expiresAt: time.Now().Add(-time.Second)},
		},
	}
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/callback?code=code&state=expired", nil)
	response := httptest.NewRecorder()
	router.authCallback(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid or expired state") {
		t.Fatalf("expired callback status: got %d body=%s", response.Code, response.Body.String())
	}
}

func TestAuthStartEvictsExpiredPendingStates(t *testing.T) {
	_, ctx := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
	router := &router{ctx: ctx, pendingStates: make(map[string]pendingState, maxPendingStates)}
	for token := range maxPendingStates {
		router.pendingStates[fmt.Sprintf("expired-%d", token)] = pendingState{expiresAt: time.Now().Add(-time.Second)}
	}
	response := httptest.NewRecorder()
	router.authStart(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/start", nil))
	if response.Code != http.StatusFound || len(router.pendingStates) != 1 {
		t.Fatalf("expired-state eviction: status=%d states=%d, want redirect with one fresh state", response.Code, len(router.pendingStates))
	}
}

func TestBuildAppContextRejectsMalformedRepoProjectMapping(t *testing.T) {
	_, err := BuildAppContext(AppContextOptions{
		SigningKey:   "signing-key",
		Users:        &memoryUserStore{users: map[string]*auth.User{}},
		Identity:     identity.HeaderIdentity{Header: "X-Dispatch-User"},
		RepoProjects: "not-a-repo-project-mapping",
	})
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_REPO_PROJECTS") {
		t.Fatalf("error: got %v, want malformed DISPATCH_REPO_PROJECTS rejection", err)
	}
}

func TestStaticHandlerServesSpaShellForUnknownRoute(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
	context.WebDistDir = webDist

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/unroutable", nil))

	// An unknown path isn't a static asset, so the server hands back the SPA
	// shell (like any client-routed app) and lets the client router render its
	// own not-found view, instead of a bare JSON 404 the client never sees.
	if response.Code != http.StatusOK {
		t.Fatalf("unknown route status: got %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if response.Body.String() != "<!doctype html>" {
		t.Fatalf("unknown route body: got %q, want dashboard shell", response.Body.String())
	}
}

func TestStaticHandlerServesIndexAtRoot(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
	context.WebDistDir = webDist

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("root status: got %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if response.Body.String() != "<!doctype html>" {
		t.Fatalf("root body: got %q, want dashboard shell", response.Body.String())
	}
}

func TestStaticHandlerServesSpaShellForBrowserDeepLink(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
	context.WebDistDir = webDist

	for _, path := range []string{
		"/issues/CORE-1/not-a-tab",
		"/issues",
		// An artifact slug is user-controlled and may legitimately contain a
		// dot; it must never be misclassified as a missing static asset.
		"/issues/CORE-1/artifacts/notes.md",
	} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))

			if response.Code != http.StatusOK {
				t.Fatalf("deep link status for %s: got %d, want %d; body=%s", path, response.Code, http.StatusOK, response.Body.String())
			}
			if response.Body.String() != "<!doctype html>" {
				t.Fatalf("deep link body for %s: got %q, want dashboard shell", path, response.Body.String())
			}
		})
	}
}

func TestStaticHandlerRoutingRules(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
	context.WebDistDir = webDist

	for _, tc := range []struct {
		path string
		want string // "404" | "shell" | "health"
	}{
		// Exact reserved roots: no static asset or SPA route lives here.
		{path: "/api", want: "404"},
		{path: "/auth", want: "404"},
		{path: "/ws", want: "404"},
		{path: "/assets", want: "404"},
		// Reserved subpaths.
		{path: "/api/v1/does-not-exist", want: "404"},
		{path: "/auth/does-not-exist", want: "404"},
		{path: "/ws/does-not-exist", want: "404"},
		{path: "/assets/missing.js", want: "404"},
		{path: "/healthz/extra", want: "404"},
		// A path that merely starts with the same characters as a reserved
		// root, but isn't the root or a subpath of it, is a real browser
		// route and must still get the SPA shell.
		{path: "/apix", want: "shell"},
		// A missing static asset outside any reserved root.
		{path: "/favicon.png", want: "404"},
		// The exact "GET /healthz" mux route has its own dedicated handler
		// and must stay unaffected by the static-fallback rules above:
		// neither a 404 nor the SPA shell, but the real health body.
		{path: "/healthz", want: "health"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))

			switch tc.want {
			case "404":
				if response.Code != http.StatusNotFound {
					t.Fatalf("status for %s: got %d, want %d; body=%s", tc.path, response.Code, http.StatusNotFound, response.Body.String())
				}
				if response.Body.String() == "<!doctype html>" {
					t.Fatalf("%s served the SPA shell instead of a 404", tc.path)
				}
			case "shell":
				if response.Code != http.StatusOK {
					t.Fatalf("status for %s: got %d, want %d; body=%s", tc.path, response.Code, http.StatusOK, response.Body.String())
				}
				if response.Body.String() != "<!doctype html>" {
					t.Fatalf("%s body: got %q, want dashboard shell", tc.path, response.Body.String())
				}
			case "health":
				if response.Body.String() == "<!doctype html>" {
					t.Fatalf("%s served the SPA shell instead of the health handler", tc.path)
				}
				if !strings.Contains(response.Body.String(), `"ok"`) {
					t.Fatalf("%s body: got %q, want the health handler's JSON", tc.path, response.Body.String())
				}
			}
		})
	}
}

func TestStaticHandlerServesFaviconIcoAsSvg(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webDist, "favicon.svg"), []byte("<svg></svg>"), 0o600); err != nil {
		t.Fatalf("write favicon: %v", err)
	}
	handler, context := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
	context.WebDistDir = webDist

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("favicon status: got %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "image/svg+xml" {
		t.Fatalf("favicon content-type: got %q, want image/svg+xml", contentType)
	}
	if response.Body.String() != "<svg></svg>" {
		t.Fatalf("favicon body: got %q, want svg source", response.Body.String())
	}
}

func TestStaticHandlerFaviconIcoFallsBackToNoContent(t *testing.T) {
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	handler, context := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
	context.WebDistDir = webDist

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))

	if response.Code != http.StatusNoContent {
		t.Fatalf("favicon status: got %d, want %d; body=%s", response.Code, http.StatusNoContent, response.Body.String())
	}
}
