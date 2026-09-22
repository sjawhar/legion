// Package oidctest is a local OIDC issuer for tests of the code that verifies
// projected service-account tokens: an httptest server answering discovery and
// JWKS for keys the test controls, plus the minting side a Kubernetes pod's
// token would come from. It is an ordinary package rather than a _test.go file
// so the three packages that need it — internal/oidc, internal/dispatch/api and
// cmd/listener — can all import it; Go cannot share test-only code across
// packages.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// TB is the part of *testing.T this package uses. Taking the interface keeps
// the testing package out of a non-test build.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Key is one RSA signing key of the issuer. A key exists as soon as it is
// generated; it is only verifiable once Publish puts it in the served set.
type Key struct {
	kid  string
	priv *rsa.PrivateKey
}

// KeyID is the key's `kid`, which a test needs to mint a foreign key under the
// same id — the one shape where the key set resolves and only the signature fails.
func (k *Key) KeyID() string { return k.kid }

// NewKey generates a signing key the issuer does not serve until Publish.
func NewKey(t TB, kid string) *Key {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key %s: %v", kid, err)
	}
	return &Key{kid: kid, priv: priv}
}

// Issuer serves a discovery document and a key set, both under the test's control.
type Issuer struct {
	server *httptest.Server

	mu              sync.Mutex
	issuerClaim     string
	discoveryStatus int
	served          []jose.JSONWebKey
	jwksRequests    int
	keysDelay       time.Duration
}

// New starts an issuer serving no keys yet. The server is closed at cleanup.
func New(t TB) *Issuer {
	t.Helper()
	issuer := &Issuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", issuer.handleDiscovery)
	mux.HandleFunc("/keys", issuer.handleKeys)
	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)
	issuer.issuerClaim = issuer.server.URL
	return issuer
}

// URL is the issuer's base URL, which is also the `iss` it advertises and mints
// until AdvertiseIssuer says otherwise.
func (i *Issuer) URL() string { return i.server.URL }

// PublishKey generates a key and serves it — the common case, for a test that
// only needs one working signing key.
func (i *Issuer) PublishKey(t TB, kid string) *Key {
	t.Helper()
	key := NewKey(t, kid)
	i.Publish(key)
	return key
}

// Publish adds a key to the served set.
func (i *Issuer) Publish(key *Key) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.served = append(i.served, jose.JSONWebKey{
		Key:       key.priv.Public(),
		KeyID:     key.kid,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	})
}

// AdvertiseIssuer makes the discovery document name a different issuer than the
// URL it is served from.
func (i *Issuer) AdvertiseIssuer(issuer string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.issuerClaim = issuer
}

// FailDiscovery makes the discovery document answer with an HTTP status.
func (i *Issuer) FailDiscovery(status int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.discoveryStatus = status
}

// DelayKeys makes the key-set endpoint wait before answering. A test about a
// caller that goes away mid-verification needs this: go-oidc selects between
// the caller's ctx.Done() and its own inflight fetch, and against a local
// issuer that answers in microseconds both cases are ready at once, so Go
// picks between them at random. Over a network the fetch is the slow one,
// which is the case the test means to describe.
func (i *Issuer) DelayKeys(d time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keysDelay = d
}

// JWKSFetches counts key-set reads, so a test can prove a refresh happened.
func (i *Issuer) JWKSFetches() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.jwksRequests
}

// Claims is a valid projected service-account token's claim set, for a test to
// spoil one claim at a time.
func (i *Issuer) Claims(subject, audience string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": i.URL(),
		"sub": subject,
		"aud": []string{audience},
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

// Mint signs claims with key, producing the compact token a pod would present.
func (i *Issuer) Mint(t TB, key *Key, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key.priv, KeyID: key.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign claims: %v", err)
	}
	raw, err := signed.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize token: %v", err)
	}
	return raw
}

func (i *Issuer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	status, issuer := i.discoveryStatus, i.issuerClaim
	i.mu.Unlock()
	if status != 0 {
		http.Error(w, "discovery unavailable", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                i.server.URL + "/auth",
		"token_endpoint":                        i.server.URL + "/token",
		"jwks_uri":                              i.server.URL + "/keys",
		"id_token_signing_alg_values_supported": []string{string(jose.RS256)},
	})
}

func (i *Issuer) handleKeys(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	i.jwksRequests++
	set := jose.JSONWebKeySet{Keys: append([]jose.JSONWebKey(nil), i.served...)}
	delay := i.keysDelay
	i.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(set)
}
