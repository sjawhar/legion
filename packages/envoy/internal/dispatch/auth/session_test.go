package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func cookieValue(t *testing.T, setCookie string) string {
	t.Helper()
	parts := strings.SplitN(setCookie, ";", 2)
	if !strings.HasPrefix(parts[0], "dsession=") {
		t.Fatalf("expected dsession= prefix, got %q", parts[0])
	}
	return strings.TrimPrefix(parts[0], "dsession=")
}

func signedCookie(login string, expiry int64, key string) string {
	payload := fmt.Sprintf("%s.%d", login, expiry)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return fmt.Sprintf("%s.%s", payload, hex.EncodeToString(mac.Sum(nil)))
}

func TestIssueAndVerifySessionCookie(t *testing.T) {
	t.Setenv("DISPATCH_INSECURE_COOKIE", "")
	setCookie := IssueSessionCookie("sjawhar", "signing-key")
	for _, frag := range []string{"dsession=", "HttpOnly", "Path=/", "SameSite=Strict", "Max-Age=2592000", "Secure"} {
		if !strings.Contains(setCookie, frag) {
			t.Errorf("set-cookie %q missing fragment %q", setCookie, frag)
		}
	}
	login := VerifySessionCookie(cookieValue(t, setCookie), "signing-key")
	if login != "sjawhar" {
		t.Errorf("verify: got %q want sjawhar", login)
	}
}

func TestIssueSessionCookieInsecureFlag(t *testing.T) {
	t.Setenv("DISPATCH_INSECURE_COOKIE", "1")
	setCookie := IssueSessionCookie("sjawhar", "signing-key")
	if strings.Contains(setCookie, "Secure") {
		t.Errorf("insecure mode should omit Secure: %q", setCookie)
	}
}

func TestVerifySessionCookieTampered(t *testing.T) {
	t.Setenv("DISPATCH_INSECURE_COOKIE", "1")
	value := cookieValue(t, IssueSessionCookie("sjawhar", "signing-key"))
	tampered := value[:len(value)-1] + "a"
	if value[len(value)-1] == 'a' {
		tampered = value[:len(value)-1] + "b"
	}
	if VerifySessionCookie(tampered, "signing-key") != "" {
		t.Errorf("tampered cookie should not verify")
	}
}

func TestVerifySessionCookieExpired(t *testing.T) {
	expired := time.Now().Add(-time.Hour).UnixMilli()
	value := signedCookie("sjawhar", expired, "signing-key")
	if VerifySessionCookie(value, "signing-key") != "" {
		t.Errorf("expired cookie should not verify")
	}
}

func TestRequireSessionMissingCookie(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://localhost/auth/whoami", nil)
	w := httptest.NewRecorder()
	login := RequireSession(w, req, "signing-key")
	if login != "" {
		t.Errorf("expected empty login, got %q", login)
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestRequireSessionValidCookie(t *testing.T) {
	t.Setenv("DISPATCH_INSECURE_COOKIE", "1")
	cookie := cookieValue(t, IssueSessionCookie("sjawhar", "signing-key"))
	req := httptest.NewRequest(http.MethodGet, "http://localhost/auth/whoami", nil)
	req.AddCookie(&http.Cookie{Name: "dsession", Value: cookie})
	w := httptest.NewRecorder()
	login := RequireSession(w, req, "signing-key")
	if login != "sjawhar" {
		t.Errorf("got login %q, want sjawhar", login)
	}
}
