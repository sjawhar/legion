package auth

import (
	"context"
	"crypto/hkdf"
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

// fingerprintLabel is the message a key's fingerprint is the HMAC of (SigningKeys.Fingerprints):
// its one purpose, so a fingerprint equals no MAC the key makes for anything else.
const fingerprintLabel = "dispatch signing key fingerprint v1"

// SigningKeys is the set of keys session cookies are signed with and the refresh tokens Dispatch
// stores are sealed under (DISPATCH_SIGNING_KEY): the current key first, which signs every new
// cookie and seals every new refresh token, then the keys still accepted, each of which verifies a
// cookie and opens a seal. A rotation adds the new key second, moves it first, and later drops the
// old one, so a cookie or a seal made under either key stays good while every process moves. The
// zero value is the empty set, which BuildAppContext refuses.
type SigningKeys struct {
	keys []string
}

// ParseSigningKeys parses DISPATCH_SIGNING_KEY: one key, or several separated by whitespace, the
// first the current key, refused as the shared agent token setting is refused (an empty or
// repeated entry, named by its position and never its value).
func ParseSigningKeys(setting string) (SigningKeys, error) {
	entries, err := parseSettingEntries(setting)
	if err != nil {
		return SigningKeys{}, err
	}
	return SigningKeys{keys: entries}, nil
}

// SingleSigningKey is the set of the one key key, read whole (a key kept in a file, or one
// generated for this process), or the empty set when key is empty.
func SingleSigningKey(key string) SigningKeys {
	if key == "" {
		return SigningKeys{}
	}
	return SigningKeys{keys: []string{key}}
}

// Empty reports whether the set holds no key.
func (k SigningKeys) Empty() bool {
	return len(k.keys) == 0
}

// Keys returns every key of the set, the current one first, in a slice of the caller's own.
func (k SigningKeys) Keys() []string {
	return append([]string(nil), k.keys...)
}

// Fingerprints names each key of the set, the current one first, without revealing it: the
// lowercase hex of the first 16 bytes of HMAC-SHA256 under the key of fingerprintLabel. Two
// processes compare them to tell whether they hold the same keys.
func (k SigningKeys) Fingerprints() []string {
	fingerprints := make([]string, 0, len(k.keys))
	for _, key := range k.keys {
		fingerprints = append(fingerprints, hex.EncodeToString(mac(fingerprintLabel, key)[:16]))
	}
	return fingerprints
}

// LoadOrCreateSigningKey reads the per-server HMAC key, a set of the one key the file holds. If
// missing, a fresh 32-byte base64url key is generated and written with mode 0600.
func LoadOrCreateSigningKey(path string) (SigningKeys, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return SingleSigningKey(string(data)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return SigningKeys{}, err
	}
	key, err := NewSigningKey()
	if err != nil {
		return SigningKeys{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return SigningKeys{}, fmt.Errorf("create signing key dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return SigningKeys{}, fmt.Errorf("write signing key: %w", err)
	}
	return SingleSigningKey(key), nil
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

func mac(payload, key string) []byte {
	hash := hmac.New(sha256.New, []byte(key))
	hash.Write([]byte(payload))
	return hash.Sum(nil)
}

// SignFor returns the lowercase hex HMAC-SHA256 of payload under a key derived from the set's
// current key for purpose alone (purposeMAC). A value signed for one purpose therefore verifies
// neither for another purpose nor as a session cookie, whose MAC is made under the signing key
// itself, and a session cookie verifies for no purpose. Each caller names its own purpose, a
// label no other use of the signing key shares. keys must not be empty.
func (k SigningKeys) SignFor(purpose, payload string) string {
	return hex.EncodeToString(purposeMAC(purpose, payload, k.keys[0]))
}

// VerifyFor reports whether signature is SignFor(purpose, payload) under any key of the set, so a
// value signed before a rotation stays good while its own lifetime lasts.
func (k SigningKeys) VerifyFor(purpose, payload, signature string) bool {
	sigBytes, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	// As in VerifySession: each key's MAC is compared in constant time and every key is tried
	// whichever matches, so the timing gives away no byte of any MAC.
	matched := false
	for _, key := range k.keys {
		matched = hmac.Equal(sigBytes, purposeMAC(purpose, payload, key)) || matched
	}
	return matched
}

// purposeMAC is payload's HMAC-SHA256 under the key HKDF-SHA256 derives from the signing key key
// with purpose as its info, as the refresh-token seal derives its own key.
func purposeMAC(purpose, payload, key string) []byte {
	derived, err := hkdf.Key(sha256.New, []byte(key), nil, purpose, sha256.Size)
	if err != nil {
		// HKDF-SHA256 refuses only a length beyond 255 hashes; sha256.Size never is.
		panic(fmt.Sprintf("dispatch: derive the %q key from the signing key: %v", purpose, err))
	}
	hash := hmac.New(sha256.New, derived)
	hash.Write([]byte(payload))
	return hash.Sum(nil)
}

// IssueSessionCookie returns a Set-Cookie header value for a 30-day session of the person named
// by email, signed with keys' current key, Secure when secure is true (cmd/dispatch:
// DISPATCH_INSECURE_COOKIE unset). keys must not be empty.
func IssueSessionCookie(email string, generation int64, keys SigningKeys, secure bool) string {
	expiry := time.Now().Add(time.Duration(sessionMaxAgeSeconds) * time.Second).UnixMilli()
	payload := sessionPayload(base64.RawURLEncoding.EncodeToString([]byte(email)), strconv.FormatInt(generation, 10), strconv.FormatInt(expiry, 10))
	value := fmt.Sprintf("%s.%s", payload, hex.EncodeToString(mac(payload, keys.keys[0])))
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

// VerifySession validates the dsession cookie value and returns its signed session generation on
// success. A cookie signed with any key of keys verifies, so one signed before a rotation stays
// good until it expires.
func VerifySession(value string, keys SigningKeys) (Session, bool) {
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
	sigBytes, err := hex.DecodeString(signature)
	if err != nil {
		return Session{}, false
	}
	// Each key's MAC is compared in constant time, and every key is tried whichever matches, so
	// the timing gives away no byte of any MAC.
	payload := sessionPayload(encodedEmail, generationText, expiryStr)
	matched := false
	for _, key := range keys.keys {
		matched = hmac.Equal(sigBytes, mac(payload, key)) || matched
	}
	if !matched {
		return Session{}, false
	}
	email, err := base64.RawURLEncoding.DecodeString(encodedEmail)
	if err != nil || len(email) == 0 {
		return Session{}, false
	}
	return Session{Login: string(email), Generation: generation}, true
}

// SessionFromRequest returns the valid signed session identity, its cookie signed with any key of
// keys. Identity implementations verify the generation against their server-side store.
func SessionFromRequest(r *http.Request, keys SigningKeys) (Session, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return Session{}, false
	}
	return VerifySession(cookie.Value, keys)
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
