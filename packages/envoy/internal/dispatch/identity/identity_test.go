package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

type testSessionStore struct {
	generations            map[string]int64
	currentGenerationCalls int
	ensureSessionCalls     int
}

func (s *testSessionStore) CurrentSessionGeneration(_ context.Context, login string) (int64, bool, error) {
	s.currentGenerationCalls++
	generation, found := s.generations[login]
	return generation, found, nil
}

func (s *testSessionStore) EnsureSession(_ context.Context, login string) (int64, error) {
	s.ensureSessionCalls++
	generation, found := s.generations[login]
	if !found {
		s.generations[login] = 0
	}
	return generation, nil
}

func (s *testSessionStore) RevokeSessions(_ context.Context, login string) error {
	s.generations[login]++
	return nil
}

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
	sessions := &testSessionStore{generations: map[string]int64{"sjawhar": 0}}
	identity := CookieIdentity{SigningKey: "signing-key", AllowedLogins: allowed, Sessions: sessions}
	if _, err := identity.Login(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("missing cookie error: got %v, want ErrNoIdentity", err)
	}

	cookie := strings.SplitN(auth.IssueSessionCookie("sjawhar", 0, "signing-key"), ";", 2)[0]
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

	if _, err := (CookieIdentity{SigningKey: "signing-key", AllowedLogins: allowed}).Login(req); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("cookie identity without session store error: got %v, want ErrNoIdentity", err)
	}

	delete(allowed, "sjawhar")
	if _, err := identity.Login(req); !errors.Is(err, ErrLoginNotAllowed) {
		t.Errorf("revoked cookie error: got %v, want ErrLoginNotAllowed", err)
	}
}

func TestCookieIdentityReadsGenerationWithoutWriting(t *testing.T) {
	allowed := map[string]struct{}{"sjawhar": {}}
	sessions := &testSessionStore{generations: map[string]int64{"sjawhar": 0}}
	cookie := strings.SplitN(auth.IssueSessionCookie("sjawhar", 0, "signing-key"), ";", 2)[0]
	name, value, ok := strings.Cut(cookie, "=")
	if !ok {
		t.Fatalf("invalid session cookie %q", cookie)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: name, Value: value})

	login, err := (CookieIdentity{SigningKey: "signing-key", AllowedLogins: allowed, Sessions: sessions}).Login(request)
	if err != nil || login != "sjawhar" {
		t.Fatalf("cookie login = %q, %v; want sjawhar, nil", login, err)
	}
	if sessions.ensureSessionCalls != 0 {
		t.Fatalf("cookie identity created the session row %d time(s)", sessions.ensureSessionCalls)
	}
	if sessions.currentGenerationCalls != 1 {
		t.Fatalf("cookie identity generation reads = %d, want 1", sessions.currentGenerationCalls)
	}
}

func TestCookieIdentityRejectsCookieWithoutSessionRow(t *testing.T) {
	allowed := map[string]struct{}{"sjawhar": {}}
	sessions := &testSessionStore{generations: map[string]int64{}}
	cookie := strings.SplitN(auth.IssueSessionCookie("sjawhar", 0, "signing-key"), ";", 2)[0]
	name, value, ok := strings.Cut(cookie, "=")
	if !ok {
		t.Fatalf("invalid session cookie %q", cookie)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: name, Value: value})

	if _, err := (CookieIdentity{SigningKey: "signing-key", AllowedLogins: allowed, Sessions: sessions}).Login(request); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("cookie without session row error = %v, want ErrNoIdentity", err)
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

	cookie := strings.SplitN(auth.IssueSessionCookie("Xodarap", 0, "signing-key"), ";", 2)[0]
	name, value, ok := strings.Cut(cookie, "=")
	if !ok {
		t.Fatalf("invalid session cookie %q", cookie)
	}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: name, Value: value})
	login, err = CookieIdentity{
		SigningKey: "signing-key", AllowedLogins: allowed, Sessions: &testSessionStore{generations: map[string]int64{"Xodarap": 0}},
	}.Login(req)
	if err != nil {
		t.Fatalf("cookie login: %v", err)
	}
	if login != "Xodarap" {
		t.Errorf("cookie login: got %q, want the login as presented", login)
	}
}
