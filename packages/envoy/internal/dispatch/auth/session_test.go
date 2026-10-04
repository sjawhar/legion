package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

func sign256(payload, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func signedCookie(email string, generation, expiry int64, key string) string {
	payload := fmt.Sprintf("2.%s.%d.%d", base64.RawURLEncoding.EncodeToString([]byte(email)), generation, expiry)
	return payload + "." + sign256(payload, key)
}

func TestIssueAndVerifySessionCookie(t *testing.T) {
	setCookie := IssueSessionCookie("sami@example.com", 7, "signing-key", true)
	for _, frag := range []string{"dsession=", "HttpOnly", "Path=/", "SameSite=Strict", "Max-Age=2592000", "Secure"} {
		if !strings.Contains(setCookie, frag) {
			t.Errorf("set-cookie %q missing fragment %q", setCookie, frag)
		}
	}
	session, ok := VerifySession(cookieValue(t, setCookie), "signing-key")
	if !ok || session.Login != "sami@example.com" || session.Generation != 7 {
		t.Errorf("verify: got %#v valid=%t, want sami@example.com generation 7", session, ok)
	}
}

// An email holds the characters the payload's separators and a cookie value cannot: dots, a plus,
// an at sign. The cookie carries any of them back unchanged.
func TestSessionCookieRoundTripsAnyEmail(t *testing.T) {
	for _, email := range []string{"a.b+c@d.example", "first.last@sub.d.example", "o'neil=x;y@d.example"} {
		session, ok := VerifySession(cookieValue(t, IssueSessionCookie(email, 3, "signing-key", false)), "signing-key")
		if !ok || session.Login != email || session.Generation != 3 {
			t.Errorf("round trip of %q: got %#v valid=%t", email, session, ok)
		}
	}
}

// A cookie Dispatch issued before people were named by email (`<login>.<generation>.<expiry>.<sig>`)
// is signed with the same key, and still does not verify: no GitHub login comes back as a person.
func TestVerifySessionRefusesTheLoginCookieShape(t *testing.T) {
	payload := fmt.Sprintf("%s.%d.%d", "sjawhar", 0, time.Now().Add(time.Hour).UnixMilli())
	if session, ok := VerifySession(payload+"."+sign256(payload, "signing-key"), "signing-key"); ok {
		t.Errorf("login-shaped cookie verified as %#v", session)
	}
}

func TestIssueSessionCookieInsecureFlag(t *testing.T) {
	setCookie := IssueSessionCookie("sami@example.com", 0, "signing-key", false)
	if strings.Contains(setCookie, "Secure") {
		t.Errorf("insecure mode should omit Secure: %q", setCookie)
	}
}

func TestVerifySessionRejectsTamperedCookie(t *testing.T) {
	value := cookieValue(t, IssueSessionCookie("sami@example.com", 0, "signing-key", false))
	tampered := value[:len(value)-1] + "a"
	if value[len(value)-1] == 'a' {
		tampered = value[:len(value)-1] + "b"
	}
	if _, ok := VerifySession(tampered, "signing-key"); ok {
		t.Errorf("tampered cookie should not verify")
	}
	other := signedCookie("mallory@example.com", 0, time.Now().Add(time.Hour).UnixMilli(), "another-key")
	if _, ok := VerifySession(other, "signing-key"); ok {
		t.Errorf("cookie signed with another key should not verify")
	}
}

func TestVerifySessionRejectsExpiredCookie(t *testing.T) {
	fresh := signedCookie("sami@example.com", 0, time.Now().Add(time.Hour).UnixMilli(), "signing-key")
	if _, ok := VerifySession(fresh, "signing-key"); !ok {
		t.Fatalf("an unexpired cookie built like the issuer's does not verify: %q", fresh)
	}
	expired := signedCookie("sami@example.com", 0, time.Now().Add(-time.Hour).UnixMilli(), "signing-key")
	if _, ok := VerifySession(expired, "signing-key"); ok {
		t.Errorf("expired cookie should not verify")
	}
}

func TestNewSigningKeyIsFreshEachCall(t *testing.T) {
	first, err := NewSigningKey()
	if err != nil {
		t.Fatalf("first key: %v", err)
	}
	second, err := NewSigningKey()
	if err != nil {
		t.Fatalf("second key: %v", err)
	}
	if first == second {
		t.Fatalf("two calls returned the same key %q", first)
	}
	for _, key := range []string{first, second} {
		decoded, err := base64.RawURLEncoding.DecodeString(key)
		if err != nil || len(decoded) != 32 {
			t.Errorf("key %q decodes to %d bytes (err %v), want 32 bytes of base64url", key, len(decoded), err)
		}
	}
}
