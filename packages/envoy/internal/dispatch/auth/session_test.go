package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

func signedCookie(login string, generation, expiry int64, key string) string {
	payload := fmt.Sprintf("%s.%d.%d", login, generation, expiry)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return fmt.Sprintf("%s.%s", payload, hex.EncodeToString(mac.Sum(nil)))
}

func TestIssueAndVerifySessionCookie(t *testing.T) {
	t.Setenv("DISPATCH_INSECURE_COOKIE", "")
	setCookie := IssueSessionCookie("sjawhar", 7, "signing-key")
	for _, frag := range []string{"dsession=", "HttpOnly", "Path=/", "SameSite=Strict", "Max-Age=2592000", "Secure"} {
		if !strings.Contains(setCookie, frag) {
			t.Errorf("set-cookie %q missing fragment %q", setCookie, frag)
		}
	}
	session, ok := VerifySession(cookieValue(t, setCookie), "signing-key")
	if !ok || session.Login != "sjawhar" || session.Generation != 7 {
		t.Errorf("verify: got %#v valid=%t, want sjawhar generation 7", session, ok)
	}
}

func TestIssueSessionCookieInsecureFlag(t *testing.T) {
	t.Setenv("DISPATCH_INSECURE_COOKIE", "1")
	setCookie := IssueSessionCookie("sjawhar", 0, "signing-key")
	if strings.Contains(setCookie, "Secure") {
		t.Errorf("insecure mode should omit Secure: %q", setCookie)
	}
}

func TestVerifySessionCookieTampered(t *testing.T) {
	t.Setenv("DISPATCH_INSECURE_COOKIE", "1")
	value := cookieValue(t, IssueSessionCookie("sjawhar", 0, "signing-key"))
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
	value := signedCookie("sjawhar", 0, expired, "signing-key")
	if VerifySessionCookie(value, "signing-key") != "" {
		t.Errorf("expired cookie should not verify")
	}
}
