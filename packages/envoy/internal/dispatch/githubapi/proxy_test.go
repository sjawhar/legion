package githubapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

type fakeHTTPClient struct {
	status int
	body   string
	calls  int
}

func (f *fakeHTTPClient) Do(_ *http.Request) (*http.Response, error) {
	f.calls++
	body := f.body
	if body == "" {
		body = "{}"
	}
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func accessTestConfig(client auth.HTTPClient) *ProxyConfig {
	return &ProxyConfig{
		Tokens: &auth.Tokens{
			AccessToken:     "tok",
			AccessExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
		HTTPClient: client,
	}
}

func TestInstallationOwnersParsesAccountLogins(t *testing.T) {
	fc := &fakeHTTPClient{
		status: http.StatusOK,
		body:   `{"installations":[{"account":{"login":"sjawhar"}},{"account":{"login":"acme-org"}},{"account":null}]}`,
	}
	owners, err := InstallationOwners(context.Background(), accessTestConfig(fc))
	if err != nil {
		t.Fatalf("InstallationOwners: %v", err)
	}
	if len(owners) != 2 || owners[0] != "sjawhar" || owners[1] != "acme-org" {
		t.Errorf("owners: got %v", owners)
	}
	if fc.calls != 1 {
		t.Errorf("expected exactly one GitHub call, got %d", fc.calls)
	}
}

func TestInstallationOwnersErrorsOnNon200(t *testing.T) {
	fc := &fakeHTTPClient{status: http.StatusForbidden}
	if _, err := InstallationOwners(context.Background(), accessTestConfig(fc)); err == nil {
		t.Fatal("expected an error when GitHub does not return 200")
	}
}

type memUserStore struct{ users map[string]*auth.User }

func (m *memUserStore) Read(login string) (*auth.User, error) {
	u, ok := m.users[login]
	if !ok {
		return nil, nil
	}
	copy := *u
	return &copy, nil
}
func (m *memUserStore) Write(u *auth.User) error  { m.users[u.Login] = u; return nil }
func (m *memUserStore) Remove(login string) error { delete(m.users, login); return nil }

func TestRefreshAndStoreKeepsAddressed(t *testing.T) {
	store := &memUserStore{users: map[string]*auth.User{
		"sjawhar": {
			Login:     "sjawhar",
			Tokens:    auth.Tokens{AccessToken: "old", RefreshToken: "r1"},
			Addressed: map[string]string{"sjawhar/legion#1": "2026-09-04T00:00:00Z"},
		},
	}}
	cfg := &ProxyConfig{
		Login: "sjawhar",
		Users: store,
		RefreshFn: func(_ context.Context, _ *auth.Tokens) (*auth.Tokens, error) {
			return &auth.Tokens{AccessToken: "new", RefreshToken: "r2"}, nil
		},
	}
	if _, err := refreshAndStore(context.Background(), cfg, &auth.Tokens{RefreshToken: "r1"}); err != nil {
		t.Fatalf("refreshAndStore: %v", err)
	}
	got := store.users["sjawhar"]
	if got.Tokens.AccessToken != "new" {
		t.Errorf("tokens not persisted: %+v", got.Tokens)
	}
	if got.Addressed["sjawhar/legion#1"] == "" {
		t.Errorf("addressed map lost on refresh: %+v", got.Addressed)
	}
	if cfg.Tokens == nil || cfg.Tokens.AccessToken != "new" {
		t.Errorf("cfg.Tokens not updated")
	}
}

func TestRefreshAndStoreRefusesLoggedOutUser(t *testing.T) {
	cfg := &ProxyConfig{
		Login: "ghost",
		Users: &memUserStore{users: map[string]*auth.User{}},
		RefreshFn: func(_ context.Context, _ *auth.Tokens) (*auth.Tokens, error) {
			return &auth.Tokens{AccessToken: "new"}, nil
		},
	}
	if _, err := refreshAndStore(context.Background(), cfg, &auth.Tokens{}); err == nil {
		t.Fatal("expected an error when the user record is gone")
	}
}
