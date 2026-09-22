package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	testAudience = "dispatch"
	testSubject  = "system:serviceaccount:legion:legion-worker"
)

// testKey is one RSA signing key of the local issuer. A key exists as soon as
// it is generated; it is only verifiable once publish puts it in the served set.
type testKey struct {
	kid  string
	priv *rsa.PrivateKey
}

// testIssuer is a local OIDC issuer: an httptest server answering
// /.well-known/openid-configuration and /keys for keys the test controls.
type testIssuer struct {
	server *httptest.Server

	mu              sync.Mutex
	issuerClaim     string
	discoveryStatus int
	served          []jose.JSONWebKey
	jwksRequests    int
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	issuer := &testIssuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", issuer.handleDiscovery)
	mux.HandleFunc("/keys", issuer.handleKeys)
	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)
	issuer.issuerClaim = issuer.server.URL
	return issuer
}

func (ti *testIssuer) url() string { return ti.server.URL }

// advertiseIssuer makes the discovery document name a different issuer than the
// URL it is served from.
func (ti *testIssuer) advertiseIssuer(issuer string) {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	ti.issuerClaim = issuer
}

// failDiscovery makes the discovery document answer with an HTTP status.
func (ti *testIssuer) failDiscovery(status int) {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	ti.discoveryStatus = status
}

func (ti *testIssuer) publish(key *testKey) {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	ti.served = append(ti.served, jose.JSONWebKey{
		Key:       key.priv.Public(),
		KeyID:     key.kid,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	})
}

func (ti *testIssuer) jwksFetches() int {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	return ti.jwksRequests
}

// claims is a valid projected service-account token's claim set, for a test to
// spoil one claim at a time.
func (ti *testIssuer) claims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": ti.url(),
		"sub": testSubject,
		"aud": []string{testAudience},
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

func (ti *testIssuer) mint(t *testing.T, key *testKey, claims map[string]any) string {
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

func (ti *testIssuer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	ti.mu.Lock()
	status, issuer := ti.discoveryStatus, ti.issuerClaim
	ti.mu.Unlock()
	if status != 0 {
		http.Error(w, "discovery unavailable", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                ti.server.URL + "/auth",
		"token_endpoint":                        ti.server.URL + "/token",
		"jwks_uri":                              ti.server.URL + "/keys",
		"id_token_signing_alg_values_supported": []string{string(jose.RS256)},
	})
}

func (ti *testIssuer) handleKeys(w http.ResponseWriter, _ *http.Request) {
	ti.mu.Lock()
	ti.jwksRequests++
	set := jose.JSONWebKeySet{Keys: append([]jose.JSONWebKey(nil), ti.served...)}
	ti.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(set)
}

func newTestKey(t *testing.T, kid string) *testKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key %s: %v", kid, err)
	}
	return &testKey{kid: kid, priv: priv}
}
