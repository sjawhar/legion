// Package githubappjwt parses a GitHub App's private key and signs the JWT a GitHub App
// authenticates as, for the two callers that act as a GitHub App: Dispatch's GitHub client
// (internal/dispatch/githubapp) and the secrets broker's github-app minter
// (internal/broker/secrets). Its errors are plain sentinels that never quote the key.
package githubappjwt

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// The ways ParseKey refuses a key. None of them carries any of the key's text.
var (
	// ErrNoPEM is text holding no PEM block.
	ErrNoPEM = errors.New("no PEM block found")
	// ErrNotPrivateKey is a PEM block that is neither a PKCS1 nor a PKCS8 private key.
	ErrNotPrivateKey = errors.New("not a PKCS1 or PKCS8 private key")
	// ErrNotRSA is a PKCS8 private key of another algorithm than RSA, which GitHub does not issue.
	ErrNotRSA = errors.New("private key is not RSA")
)

// ParseKey reads a GitHub App's private key: the PEM GitHub's "Generate a private key" downloads,
// in PKCS1 form, or the same key in PKCS8.
func ParseKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, ErrNoPEM
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, ErrNotPrivateKey
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, ErrNotRSA
	}
	return key, nil
}

// Sign makes the RS256 JWT a GitHub App authenticates with: iss is the App's client ID or numeric
// App ID (GitHub accepts either), iat is backdated a minute against clock skew, and exp is nine
// minutes on, inside GitHub's ten-minute cap.
func Sign(iss string, key *rsa.PrivateKey, now time.Time) (string, error) {
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": iss,
	})
	if err != nil {
		return "", fmt.Errorf("encode JWT claims: %w", err)
	}
	signing := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) +
		"." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app JWT: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
