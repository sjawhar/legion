package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// devSignInOrigin is the loopback dashboard origin the e2e harness serves on.
const devSignInOrigin = "http://127.0.0.1:8777"

// newDevSignInRouter builds a cookie-identity router with the dev sign-in route mounted, alice
// on the allowlist and the shared e2e bearer configured.
func newDevSignInRouter(t *testing.T, serverURL string, sessions *memorySessionStore) http.Handler {
	t.Helper()
	allowed := map[string]struct{}{"alice": {}}
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey: "signing-key",
		Users:      &memoryUserStore{users: map[string]*auth.User{}},
		Sessions:   sessions,
		Identity: identity.CookieIdentity{
			SigningKey: "signing-key", AllowedLogins: allowed, Sessions: sessions,
		},
		AllowedLogins: allowed,
		AgentTokens:   sharedAgentTokens(t, "e2e-token"),
		ServerURL:     serverURL,
		DevSignIn:     true,
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	return New(ctx)
}

// devSignInRequest is a request as a browser on this machine sends it to the dashboard origin.
// httptest.NewRequest's own defaults (a 192.0.2.1 peer, Host example.com) are what the fence
// refuses, so the negative tests change one of these two fields.
func devSignInRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = "127.0.0.1:40000"
	request.Host = "127.0.0.1:8777"
	return request
}

func sessionCookieOf(response *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "dsession" {
			return cookie
		}
	}
	return nil
}

func errorCodeOf(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", response.Body.String(), err)
	}
	return body.Code
}

// whoamiAs answers GET /auth/whoami for a browser carrying cookie.
func whoamiAs(t *testing.T, handler http.Handler, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := devSignInRequest(t, devSignInOrigin+"/auth/whoami")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestDevSignInIssuesTheSessionCookieForAnAllowedLogin(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	// A generation other than the first proves the cookie carries the store's, not a constant.
	sessions := &memorySessionStore{generations: map[string]int64{"alice": 3}}
	handler := newDevSignInRouter(t, devSignInOrigin, sessions)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=alice&next=/issues/CORE-1"))

	if response.Code != http.StatusFound || response.Header().Get("Location") != "/issues/CORE-1" {
		t.Fatalf("sign-in: status %d Location %q body=%s, want 302 to /issues/CORE-1", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	cookie := sessionCookieOf(response)
	if cookie == nil {
		t.Fatalf("sign-in set no dsession cookie: %v", response.Header().Values("Set-Cookie"))
	}
	session, ok := auth.VerifySession(cookie.Value, "signing-key")
	if !ok || session.Login != "alice" || session.Generation != 3 {
		t.Fatalf("cookie session = %#v valid=%t, want alice at generation 3", session, ok)
	}
	whoami := whoamiAs(t, handler, cookie)
	if whoami.Code != http.StatusOK || strings.TrimSpace(whoami.Body.String()) != `{"kind":"user","login":"alice"}` {
		t.Fatalf("whoami with the minted cookie: %d %s", whoami.Code, whoami.Body.String())
	}
	logged := logs.String()
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "login=alice") || !strings.Contains(logged, "remote_addr=127.0.0.1:40000") {
		t.Fatalf("the mint was not logged at WARN with its login and peer: %q", logged)
	}
}

// The callback's own rule: the allowlist is checked lowercase and GitHub's spelling is minted,
// which the e2e casing specs depend on.
func TestDevSignInMintsTheSpellingRequested(t *testing.T) {
	handler := newDevSignInRouter(t, devSignInOrigin, &memorySessionStore{generations: map[string]int64{}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=Alice"))
	if response.Code != http.StatusFound {
		t.Fatalf("sign-in as Alice: %d %s", response.Code, response.Body.String())
	}
	cookie := sessionCookieOf(response)
	if cookie == nil {
		t.Fatal("sign-in as Alice set no dsession cookie")
	}
	whoami := whoamiAs(t, handler, cookie)
	if whoami.Code != http.StatusOK || strings.TrimSpace(whoami.Body.String()) != `{"kind":"user","login":"Alice"}` {
		t.Fatalf("whoami: %d %s, want the spelling requested", whoami.Code, whoami.Body.String())
	}
}

func TestDevSignInRefusesAnUnlistedLogin(t *testing.T) {
	sessions := &memorySessionStore{generations: map[string]int64{}}
	handler := newDevSignInRouter(t, devSignInOrigin, sessions)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=mallory"))
	if response.Code != http.StatusForbidden || errorCodeOf(t, response) != "LOGIN_NOT_ALLOWED" {
		t.Fatalf("unlisted login: %d %s, want 403 LOGIN_NOT_ALLOWED", response.Code, response.Body.String())
	}
	if setCookie := response.Header().Values("Set-Cookie"); len(setCookie) != 0 {
		t.Fatalf("unlisted login got Set-Cookie %v", setCookie)
	}
	if _, found := sessions.generations["mallory"]; found {
		t.Fatal("unlisted login got a session row")
	}
}

func TestDevSignInRequiresALogin(t *testing.T) {
	handler := newDevSignInRouter(t, devSignInOrigin, &memorySessionStore{generations: map[string]int64{}})
	for _, target := range []string{"/auth/_dev/signin", "/auth/_dev/signin?login=%20%20"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, devSignInRequest(t, devSignInOrigin+target))
		if response.Code != http.StatusBadRequest || errorCodeOf(t, response) != "DEV_SIGNIN_INPUT" {
			t.Fatalf("%s: %d %s, want 400 DEV_SIGNIN_INPUT", target, response.Code, response.Body.String())
		}
		if sessionCookieOf(response) != nil {
			t.Fatalf("%s: set a session cookie", target)
		}
	}
}

func TestDevSignInSanitizesNext(t *testing.T) {
	handler := newDevSignInRouter(t, devSignInOrigin, &memorySessionStore{generations: map[string]int64{}})
	for _, tc := range []struct{ next, want string }{
		{next: "https://evil.example/", want: "/"},
		{next: "//evil.example", want: "/"},
		{next: "/%2F%2Fevil.example", want: "/"},
		{next: "/issues/CORE-1?tab=spec", want: "/issues/CORE-1?tab=spec"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=alice&next="+url.QueryEscape(tc.next)))
		if response.Code != http.StatusFound || response.Header().Get("Location") != tc.want {
			t.Errorf("next=%q: status %d Location %q, want 302 to %q", tc.next, response.Code, response.Header().Get("Location"), tc.want)
		}
	}
}

func TestDevSignInRefusesANonLoopbackPeer(t *testing.T) {
	handler := newDevSignInRouter(t, devSignInOrigin, &memorySessionStore{generations: map[string]int64{}})
	for _, peer := range []string{"10.0.0.5:4000", "192.0.2.1:1234", "[::ffff:127.0.0.1]:4000", "not-an-address"} {
		request := devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=alice")
		request.RemoteAddr = peer
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || errorCodeOf(t, response) != "DEV_SIGNIN_FORBIDDEN" {
			t.Fatalf("peer %s: %d %s, want 403 DEV_SIGNIN_FORBIDDEN", peer, response.Code, response.Body.String())
		}
		if sessionCookieOf(response) != nil {
			t.Fatalf("peer %s: set a session cookie", peer)
		}
	}

	// An IPv6 loopback peer is this machine: a server listening on [::1] serves it.
	request := devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=alice")
	request.RemoteAddr = "[::1]:40000"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusFound || sessionCookieOf(response) == nil {
		t.Fatalf("peer [::1]:40000: %d %s, want 302 with a cookie", response.Code, response.Body.String())
	}
}

// Every fence item that takes a URL decides on LoopbackURL, so the spellings that dress another host
// up as this machine are refused in one place.
func TestLoopbackURL(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:9022",
		"https://127.0.0.2/api",
		"http://localhost:9022",
		"http://LocalHost",
		"http://[::1]:9022",
		"http://127.0.0.1:9022/path?next=x#frag",
		// No scheme but a host: net/http refuses such a request before dialling anything.
		"//127.0.0.1:9022",
	} {
		if !LoopbackURL(raw) {
			t.Errorf("LoopbackURL(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{
		"",
		"https://api.github.com",
		"http://10.0.0.5:9022",
		"http://127.0.0.1@api.github.com",
		"http://127.0.0.1:9022@api.github.com",
		"http://api.github.com#@127.0.0.1",
		"http://api.github.com?@127.0.0.1",
		"http://localhost./",
		"http://127.0.0.1./",
		"http://a.localhost/",
		"http://0.0.0.0:9022",
		"http://127.1:9022",
		"http://[::ffff:127.0.0.1]:9022",
		"http://[::1%25lo]:9022",
		"localhost:9022",
		"127.0.0.1:9022",
		"http://127.0.0.1 :9022",
	} {
		if LoopbackURL(raw) {
			t.Errorf("LoopbackURL(%q) = true, want false", raw)
		}
	}
}

func TestDevSignInRefusesForwardedRequests(t *testing.T) {
	handler := newDevSignInRouter(t, devSignInOrigin, &memorySessionStore{generations: map[string]int64{}})
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP", "Via"} {
		for _, value := range []string{"10.0.0.5", ""} {
			request := devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=alice")
			request.Header[http.CanonicalHeaderKey(header)] = []string{value}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || errorCodeOf(t, response) != "DEV_SIGNIN_FORBIDDEN" || !strings.Contains(response.Body.String(), header) {
				t.Fatalf("%s: %q: %d %s, want 403 DEV_SIGNIN_FORBIDDEN naming the header", header, value, response.Code, response.Body.String())
			}
			if sessionCookieOf(response) != nil {
				t.Fatalf("%s: %q: set a session cookie", header, value)
			}
		}
	}
}

func TestDevSignInRouterRefusesAnotherHost(t *testing.T) {
	handler := newDevSignInRouter(t, devSignInOrigin, &memorySessionStore{generations: map[string]int64{}})
	signIn := func(host string) *httptest.ResponseRecorder {
		request := devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=alice")
		request.Host = host
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// The fixed e2e bearer skips the cookie-origin check, so only the Host check stands between
	// it and a DNS-rebinding page.
	bearerWhoami := func(host string) *httptest.ResponseRecorder {
		request := devSignInRequest(t, devSignInOrigin+"/api/v1/whoami")
		request.Host = host
		request.Header.Set("Authorization", "Bearer e2e-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	for name, response := range map[string]*httptest.ResponseRecorder{
		"sign-in": signIn("dispatch.example.com"),
		"bearer":  bearerWhoami("dispatch.example.com"),
	} {
		if response.Code != http.StatusMisdirectedRequest || errorCodeOf(t, response) != "HOST_MISMATCH" {
			t.Fatalf("%s with Host dispatch.example.com: %d %s, want 421 HOST_MISMATCH", name, response.Code, response.Body.String())
		}
	}
	if response := signIn("127.0.0.1:8777"); response.Code != http.StatusFound {
		t.Fatalf("sign-in with the dashboard Host: %d %s", response.Code, response.Body.String())
	}
	if response := bearerWhoami("127.0.0.1:8777"); response.Code != http.StatusOK {
		t.Fatalf("bearer whoami with the dashboard Host: %d %s", response.Code, response.Body.String())
	}

	// A host name is compared without regard to case.
	named := newDevSignInRouter(t, "http://localhost:8777", &memorySessionStore{generations: map[string]int64{}})
	request := devSignInRequest(t, "http://localhost:8777/auth/_dev/signin?login=alice")
	request.Host = "LocalHost:8777"
	response := httptest.NewRecorder()
	named.ServeHTTP(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("sign-in with Host LocalHost:8777 against http://localhost:8777: %d %s", response.Code, response.Body.String())
	}
}

func TestDevSignInIsNotMountedByDefault(t *testing.T) {
	allowed := map[string]struct{}{"alice": {}}
	sessions := &memorySessionStore{generations: map[string]int64{}}
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey: "signing-key",
		Users:      &memoryUserStore{users: map[string]*auth.User{}},
		Sessions:   sessions,
		Identity: identity.CookieIdentity{
			SigningKey: "signing-key", AllowedLogins: allowed, Sessions: sessions,
		},
		AllowedLogins: allowed,
		ServerURL:     devSignInOrigin,
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	handler := New(ctx)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=alice"))
	if response.Code != http.StatusNotFound || errorCodeOf(t, response) != "NOT_FOUND" {
		t.Fatalf("unmounted route: %d %s, want 404 NOT_FOUND", response.Code, response.Body.String())
	}
	if sessionCookieOf(response) != nil || len(sessions.generations) != 0 {
		t.Fatal("unmounted route minted a session")
	}

	request := devSignInRequest(t, devSignInOrigin+"/auth/whoami")
	request.Host = "dispatch.example.com"
	whoami := httptest.NewRecorder()
	handler.ServeHTTP(whoami, request)
	if whoami.Code != http.StatusUnauthorized || errorCodeOf(t, whoami) != "NO_IDENTITY" {
		t.Fatalf("whoami with another Host and no cookie: %d %s, want the identity's 401", whoami.Code, whoami.Body.String())
	}
}

func TestBuildAppContextDevSignInNeedsSessions(t *testing.T) {
	_, err := BuildAppContext(AppContextOptions{
		SigningKey: "signing-key",
		Users:      &memoryUserStore{users: map[string]*auth.User{}},
		Identity:   identity.HeaderIdentity{Header: "X-Dispatch-User"},
		ServerURL:  devSignInOrigin,
		DevSignIn:  true,
	})
	if err == nil || !strings.Contains(err.Error(), "Sessions") {
		t.Fatalf("error: got %v, want a missing Sessions store refusal", err)
	}
}

func TestBuildAppContextDevSignInRefusesAPublicOrigin(t *testing.T) {
	build := func(serverURL string) error {
		_, err := BuildAppContext(AppContextOptions{
			SigningKey: "signing-key",
			Users:      &memoryUserStore{users: map[string]*auth.User{}},
			Sessions:   &memorySessionStore{generations: map[string]int64{}},
			Identity:   identity.HeaderIdentity{Header: "X-Dispatch-User"},
			ServerURL:  serverURL,
			DevSignIn:  true,
		})
		return err
	}
	for _, serverURL := range []string{
		"https://dispatch.example.com",
		"http://10.0.0.5:8777",
		"http://localhost.example.com",
		"http://127.0.0.1.nip.io",
		"http://[::ffff:127.0.0.1]:8777",
		"http://[::1%25lo]:8777",
		"",
	} {
		if err := build(serverURL); err == nil || !strings.Contains(err.Error(), "DISPATCH_SERVER_URL") {
			t.Errorf("ServerURL %q: error %v, want a refusal naming DISPATCH_SERVER_URL", serverURL, err)
		}
	}
	// The host DevSignInOrigin hands back is the one the router then requires as every Host.
	for serverURL, host := range map[string]string{
		"http://127.0.0.1:8777": "127.0.0.1:8777",
		"http://LOCALHOST:8777": "LOCALHOST:8777",
		"http://[::1]:8777":     "[::1]:8777",
	} {
		if err := build(serverURL); err != nil {
			t.Errorf("ServerURL %q: %v, want a loopback origin accepted", serverURL, err)
		}
		if got, err := DevSignInOrigin(serverURL); err != nil || got != host {
			t.Errorf("DevSignInOrigin(%q) = %q, %v; want %q", serverURL, got, err, host)
		}
	}
}

// readCountingUserStore counts reads of the stored GitHub token pairs.
type readCountingUserStore struct {
	*memoryUserStore
	reads int
}

func (s *readCountingUserStore) Read(ctx context.Context, login string) (*auth.User, error) {
	s.reads++
	return s.memoryUserStore.Read(ctx, login)
}

// Any loopback client can mint any allowlisted login on a dev sign-in server, so a token pair a
// GitHub sign-in stored, or one in a shared database, must never reach the GitHub proxy there.
func TestDevSignInNeverUsesAStoredGitHubToken(t *testing.T) {
	build := func(t *testing.T, devSignIn bool) (http.Handler, *readCountingUserStore) {
		t.Helper()
		users := &readCountingUserStore{memoryUserStore: &memoryUserStore{users: map[string]*auth.User{
			"alice": {Login: "alice", Tokens: auth.Tokens{AccessToken: "access", AccessExpiresAt: time.Now().Add(time.Hour).UnixMilli()}},
		}}}
		allowed := map[string]struct{}{"alice": {}}
		sessions := &memorySessionStore{generations: map[string]int64{"alice": 0}}
		ctx, err := BuildAppContext(AppContextOptions{
			SigningKey: "signing-key",
			Users:      users,
			Sessions:   sessions,
			Identity: identity.CookieIdentity{
				SigningKey: "signing-key", AllowedLogins: allowed, Sessions: sessions,
			},
			AllowedLogins: allowed,
			ServerURL:     devSignInOrigin,
			App:           &auth.AppConfig{ClientID: "client-id", ClientSecret: "client-secret"},
			DevSignIn:     devSignIn,
		})
		if err != nil {
			t.Fatalf("build context: %v", err)
		}
		ctx.HTTPClient = callbackHTTPClient{}
		return New(ctx), users
	}
	proxy := func(handler http.Handler, cookie *http.Cookie) *httptest.ResponseRecorder {
		request := devSignInRequest(t, devSignInOrigin+"/api/github/rest/user")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	// Without the flag, alice's stored pair is what the proxy uses.
	handler, users := build(t, false)
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie("alice", 0, "signing-key", true))
	if err != nil {
		t.Fatalf("parse session cookie: %v", err)
	}
	if response := proxy(handler, cookie); response.Code != http.StatusOK || users.reads != 1 {
		t.Fatalf("proxy without the flag: %d %s after %d reads, want 200 through the stored token", response.Code, response.Body.String(), users.reads)
	}

	// With it, the same row is never read.
	handler, users = build(t, true)
	signIn := httptest.NewRecorder()
	handler.ServeHTTP(signIn, devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=alice"))
	if cookie = sessionCookieOf(signIn); cookie == nil {
		t.Fatalf("sign-in set no cookie: %d %s", signIn.Code, signIn.Body.String())
	}
	response := proxy(handler, cookie)
	if response.Code != http.StatusServiceUnavailable || errorCodeOf(t, response) != "GITHUB_TOKEN_UNAVAILABLE" || users.reads != 0 {
		t.Fatalf("proxy with the flag: %d %s after %d reads, want 503 GITHUB_TOKEN_UNAVAILABLE and no read", response.Code, response.Body.String(), users.reads)
	}
}
