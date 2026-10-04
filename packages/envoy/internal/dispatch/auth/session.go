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
	// sessionFormat leads every signed payload. The person's email is base64url-encoded so its
	// dots cannot be read as the payload's separators; a cookie of the earlier shape, a bare
	// GitHub login with three fields after it, has a part count of its own and never verifies.
	sessionFormat = "2"
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
	key, err := NewSigningKey()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create signing key dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return "", fmt.Errorf("write signing key: %w", err)
	}
	return key, nil
}

// NewSigningKey returns a fresh 32-byte base64url key: the key of a process that must not share
// cookies with any other (cmd/dispatch's dev sign-in).
func NewSigningKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func sign(payload, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// IssueSessionCookie returns a Set-Cookie header value for a 30-day session of the person named
// by email, Secure when secure is true (cmd/dispatch: DISPATCH_INSECURE_COOKIE unset).
func IssueSessionCookie(email string, generation int64, signingKey string, secure bool) string {
	expiry := time.Now().Add(time.Duration(sessionMaxAgeSeconds) * time.Second).UnixMilli()
	payload := sessionPayload(base64.RawURLEncoding.EncodeToString([]byte(email)), strconv.FormatInt(generation, 10), strconv.FormatInt(expiry, 10))
	value := fmt.Sprintf("%s.%s", payload, sign(payload, signingKey))
	attrs := []string{
		fmt.Sprintf("%s=%s", sessionCookieName, value),
		"HttpOnly",
		"Path=/",
		"SameSite=Strict",
		fmt.Sprintf("Max-Age=%d", sessionMaxAgeSeconds),
	}
	if secure {
		attrs = append(attrs, "Secure")
	}
	return strings.Join(attrs, "; ")
}

func sessionPayload(encodedEmail, generation, expiry string) string {
	return strings.Join([]string{sessionFormat, encodedEmail, generation, expiry}, ".")
}

// ClearSessionCookie returns a Set-Cookie header value that immediately
// invalidates the dsession cookie, Secure when secure is true.
func ClearSessionCookie(secure bool) string {
	attrs := []string{
		fmt.Sprintf("%s=", sessionCookieName),
		"HttpOnly",
		"Path=/",
		"SameSite=Strict",
		"Max-Age=0",
	}
	if secure {
		attrs = append(attrs, "Secure")
	}
	return strings.Join(attrs, "; ")
}

// Session is the signed browser session identity, the person's email, and its server-revocable
// generation.
type Session struct {
	Login      string
	Generation int64
}

// VerifySession validates the dsession cookie value and returns its signed
// session generation on success.
func VerifySession(value, signingKey string) (Session, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 5 || parts[0] != sessionFormat {
		return Session{}, false
	}
	encodedEmail, generationText, expiryStr, signature := parts[1], parts[2], parts[3], parts[4]
	generation, err := strconv.ParseInt(generationText, 10, 64)
	if err != nil || generation < 0 {
		return Session{}, false
	}
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil || expiry <= time.Now().UnixMilli() {
		return Session{}, false
	}
	expected := sign(sessionPayload(encodedEmail, generationText, expiryStr), signingKey)
	// Constant-time compare on the raw hex strings; both come from hex.EncodeToString.
	sigBytes, err := hex.DecodeString(signature)
	if err != nil {
		return Session{}, false
	}
	expectedBytes, err := hex.DecodeString(expected)
	if err != nil || !hmac.Equal(sigBytes, expectedBytes) {
		return Session{}, false
	}
	email, err := base64.RawURLEncoding.DecodeString(encodedEmail)
	if err != nil || len(email) == 0 {
		return Session{}, false
	}
	return Session{Login: string(email), Generation: generation}, true
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

// HasSessionCookie reports whether a request selected cookie authentication.
func HasSessionCookie(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	return err == nil && cookie.Value != ""
}
