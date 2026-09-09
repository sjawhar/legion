package routes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

type memoryUserStore struct {
	users map[string]*auth.User
}

func (s *memoryUserStore) Read(login string) (*auth.User, error) {
	user := s.users[login]
	if user == nil {
		return nil, nil
	}
	copy := *user
	return &copy, nil
}

func (s *memoryUserStore) Write(user *auth.User) error {
	copy := *user
	s.users[user.Login] = &copy
	return nil
}

func (s *memoryUserStore) Remove(login string) error {
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
		App: &auth.AppConfig{ClientID: "client-id", ClientSecret: "client-secret"},
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
	if user, _ := users.Read("mallory"); user != nil {
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
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"login":"sjawhar"`) {
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
