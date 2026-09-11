package identity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

func TestHeaderIdentityReturnsAllowedLogin(t *testing.T) {
	identity := HeaderIdentity{
		Header:        "X-Dispatch-User",
		AllowedLogins: map[string]struct{}{"sjawhar": {}},
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Dispatch-User", "sjawhar")
	login, err := identity.Login(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if login != "sjawhar" {
		t.Errorf("login: got %q, want sjawhar", login)
	}
}

func TestHeaderIdentityRejectsMissingAndUnlistedLogins(t *testing.T) {
	identity := HeaderIdentity{
		Header:        "X-Dispatch-User",
		AllowedLogins: map[string]struct{}{"sjawhar": {}},
	}

	if _, err := identity.Login(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("missing header error: got %v, want ErrNoIdentity", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Dispatch-User", "mallory")
	if _, err := identity.Login(req); !errors.Is(err, ErrLoginNotAllowed) {
		t.Errorf("unlisted login error: got %v, want ErrLoginNotAllowed", err)
	}
}

func TestCookieIdentityReturnsLoginOnlyForAllowedValidCookie(t *testing.T) {
	allowed := map[string]struct{}{"sjawhar": {}}
	identity := CookieIdentity{SigningKey: "signing-key", AllowedLogins: allowed}
	if _, err := identity.Login(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("missing cookie error: got %v, want ErrNoIdentity", err)
	}

	cookie := strings.SplitN(auth.IssueSessionCookie("sjawhar", "signing-key"), ";", 2)[0]
	name, value, ok := strings.Cut(cookie, "=")
	if !ok {
		t.Fatalf("invalid session cookie %q", cookie)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: name, Value: value})
	login, err := identity.Login(req)
	if err != nil {
		t.Fatalf("valid cookie: %v", err)
	}
	if login != "sjawhar" {
		t.Errorf("login: got %q, want sjawhar", login)
	}

	delete(allowed, "sjawhar")
	if _, err := identity.Login(req); !errors.Is(err, ErrLoginNotAllowed) {
		t.Errorf("revoked cookie error: got %v, want ErrLoginNotAllowed", err)
	}
}

func TestWriteErrorMapsIdentitySentinels(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "no identity", err: ErrNoIdentity, want: http.StatusUnauthorized},
		{name: "not allowed", err: ErrLoginNotAllowed, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteError(w, tc.err)
			if w.Code != tc.want {
				t.Errorf("status: got %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestAllowedLoginsMatchGitHubCasing(t *testing.T) {
	// The allowlist is stored lower-case; GitHub returns the user's display casing at sign-in.
	allowed := map[string]struct{}{"xodarap": {}}

	header := HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: allowed}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Dispatch-User", "Xodarap")
	login, err := header.Login(req)
	if err != nil {
		t.Fatalf("header login: %v", err)
	}
	if login != "Xodarap" {
		t.Errorf("header login: got %q, want the login as presented", login)
	}

	cookie := strings.SplitN(auth.IssueSessionCookie("Xodarap", "signing-key"), ";", 2)[0]
	name, value, ok := strings.Cut(cookie, "=")
	if !ok {
		t.Fatalf("invalid session cookie %q", cookie)
	}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: name, Value: value})
	login, err = CookieIdentity{SigningKey: "signing-key", AllowedLogins: allowed}.Login(req)
	if err != nil {
		t.Fatalf("cookie login: %v", err)
	}
	if login != "Xodarap" {
		t.Errorf("cookie login: got %q, want the login as presented", login)
	}
}
