package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	sessionMaxAgeSeconds = 30 * 24 * 60 * 60
	sessionCookieName    = "dsession"
)

// LoadOrCreateSigningKey reads the per-server HMAC key. If missing, a fresh
// 32-byte base64url key is generated and written with mode 0600.
func LoadOrCreateSigningKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return string(data), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	key := base64.RawURLEncoding.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create signing key dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return "", fmt.Errorf("write signing key: %w", err)
	}
	return key, nil
}

// LoadSigningKey returns DISPATCH_SIGNING_KEY when set, otherwise falls
// back to LoadOrCreateSigningKey(path). The env-var path is the production
// shape (key sourced from a secrets manager and injected into the
// container env) — the file path is the local-dev shape.
//
// Whichever source wins, the resulting key MUST be stable across deploys
// or every dsession cookie invalidates whenever a container rolls.
func LoadSigningKey(path string) (string, error) {
	if key := os.Getenv("DISPATCH_SIGNING_KEY"); key != "" {
		return key, nil
	}
	return LoadOrCreateSigningKey(path)
}

func sign(payload, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// IssueSessionCookie returns a Set-Cookie header value for a 30-day session.
// When env DISPATCH_INSECURE_COOKIE is unset, the Secure flag is added.
func IssueSessionCookie(login string, generation int64, signingKey string) string {
	expiry := time.Now().Add(time.Duration(sessionMaxAgeSeconds) * time.Second).UnixMilli()
	payload := fmt.Sprintf("%s.%d.%d", login, generation, expiry)
	value := fmt.Sprintf("%s.%s", payload, sign(payload, signingKey))
	attrs := []string{
		fmt.Sprintf("%s=%s", sessionCookieName, value),
		"HttpOnly",
		"Path=/",
		"SameSite=Strict",
		fmt.Sprintf("Max-Age=%d", sessionMaxAgeSeconds),
	}
	if os.Getenv("DISPATCH_INSECURE_COOKIE") == "" {
		attrs = append(attrs, "Secure")
	}
	return strings.Join(attrs, "; ")
}

// ClearSessionCookie returns a Set-Cookie header value that immediately
// invalidates the dsession cookie.
func ClearSessionCookie() string {
	attrs := []string{
		fmt.Sprintf("%s=", sessionCookieName),
		"HttpOnly",
		"Path=/",
		"SameSite=Strict",
		"Max-Age=0",
	}
	if os.Getenv("DISPATCH_INSECURE_COOKIE") == "" {
		attrs = append(attrs, "Secure")
	}
	return strings.Join(attrs, "; ")
}

// Session is the signed browser session identity and its server-revocable
// generation.
type Session struct {
	Login      string
	Generation int64
}

// VerifySessionCookie validates the dsession cookie value. Returns the login on
// success, or empty string if invalid/expired.
func VerifySessionCookie(value, signingKey string) string {
	session, ok := VerifySession(value, signingKey)
	if !ok {
		return ""
	}
	return session.Login
}

// VerifySession validates the dsession cookie value and returns its signed
// session generation on success.
func VerifySession(value, signingKey string) (Session, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return Session{}, false
	}
	login, generationText, expiryStr, signature := parts[0], parts[1], parts[2], parts[3]
	if login == "" {
		return Session{}, false
	}
	generation, err := strconv.ParseInt(generationText, 10, 64)
	if err != nil || generation < 0 {
		return Session{}, false
	}
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil || expiry <= time.Now().UnixMilli() {
		return Session{}, false
	}
	payload := fmt.Sprintf("%s.%s.%s", login, generationText, expiryStr)
	expected := sign(payload, signingKey)
	// Constant-time compare on the raw hex strings; both come from hex.EncodeToString.
	sigBytes, err := hex.DecodeString(signature)
	if err != nil {
		return Session{}, false
	}
	expectedBytes, err := hex.DecodeString(expected)
	if err != nil || !hmac.Equal(sigBytes, expectedBytes) {
		return Session{}, false
	}
	return Session{Login: login, Generation: generation}, true
}

// SessionFromRequest returns the valid signed session identity. Identity
// implementations verify the generation against their server-side store.
func SessionFromRequest(r *http.Request, signingKey string) (Session, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return Session{}, false
	}
	return VerifySession(cookie.Value, signingKey)
}

// SessionStore persists the generation that makes signed browser sessions
// revocable. Login ensures a row; authentication reads only.
type SessionStore interface {
	EnsureSession(ctx context.Context, login string) (int64, error)
	CurrentSessionGeneration(ctx context.Context, login string) (generation int64, found bool, err error)
	RevokeSessions(ctx context.Context, login string) error
}

// SessionLogin returns the valid session login or an empty string when the
// request has no valid session. Identity implementations map the empty result
// to the shared authorization error response.
func SessionLogin(r *http.Request, signingKey string) string {
	session, ok := SessionFromRequest(r, signingKey)
	if !ok {
		return ""
	}
	return session.Login
}

// HasSessionCookie reports whether a request selected cookie authentication.
func HasSessionCookie(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	return err == nil && cookie.Value != ""
}
