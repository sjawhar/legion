// Package oidctest is a local OIDC issuer for tests of the code that verifies
// OIDC tokens: an httptest server answering discovery and JWKS for keys the
// test controls, the minting side a Kubernetes pod's projected service-account
// token would come from, and, once EnableCodeFlow arms it, the authorization,
// token and revocation endpoints a person's sign-in goes through
// (authorization code, refresh token, RFC 7009 revocation). It is an ordinary
// package rather than a _test.go file so the packages that need it —
// internal/oidc, internal/dispatch/api, internal/dispatch/routes,
// internal/dispatch/store, cmd/dispatch and cmd/listener — can all import it;
// Go cannot share test-only code across packages.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	omitCodeFlow    bool
	omitRevocation  bool
	codeFlow        *codeFlow
}

// codeFlow is the state of the authorization and token endpoints: the one
// client they serve, the key ID tokens are signed with, the sign-ins queued for
// the next authorization requests, and the codes and refresh tokens issued.
type codeFlow struct {
	key          *Key
	clientID     string
	clientSecret string
	queued       []map[string]any
	codes        map[string]codeGrant
	grants       map[string]*refreshGrant
	refreshes    int
	revocation   revocation
}

// revocation is the state of the revocation endpoint: every token the client
// sent it, in order, whatever it answered; the status and OAuth error code it
// answers instead of revoking, when FailRevocation set one; and how long it
// waits before answering.
type revocation struct {
	requests []string
	status   int
	code     string
	delay    time.Duration
}

type codeGrant struct {
	claims      map[string]any
	nonce       string
	redirectURI string
}

// refreshGrant is one refresh token: the claims each refresh's ID token
// carries, which UpdateGrants changes the way a pool changes a user, and
// whether RevokeGrants has ended it.
type refreshGrant struct {
	claims  map[string]any
	revoked bool
}

// New starts an issuer serving no keys yet. The server is closed at cleanup.
func New(t TB) *Issuer {
	t.Helper()
	issuer := &Issuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", issuer.handleDiscovery)
	mux.HandleFunc("/keys", issuer.handleKeys)
	mux.HandleFunc("GET /authorize", issuer.handleAuthorize)
	mux.HandleFunc("POST /token", issuer.handleToken)
	mux.HandleFunc("POST /revoke", issuer.handleRevoke)
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

// EnableCodeFlow arms the authorization and token endpoints for one confidential
// client, signing every ID token they issue with key (which the caller
// publishes).
func (i *Issuer) EnableCodeFlow(key *Key, clientID, clientSecret string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow = &codeFlow{
		key:          key,
		clientID:     clientID,
		clientSecret: clientSecret,
		codes:        map[string]codeGrant{},
		grants:       map[string]*refreshGrant{},
	}
}

// OmitCodeFlowEndpoints makes the discovery document name no authorization or
// token endpoint, the shape of an issuer that offers no sign-in.
func (i *Issuer) OmitCodeFlowEndpoints() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.omitCodeFlow = true
}

// OmitRevocationEndpoint makes the discovery document name no revocation
// endpoint, the shape of an issuer that offers no token revocation.
func (i *Issuer) OmitRevocationEndpoint() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.omitRevocation = true
}

// FailRevocation makes the revocation endpoint answer status, with code as the
// body's OAuth error when code is not empty, instead of revoking; a status of 0
// makes it revoke again. Cognito answers 400 invalid_request to a client with
// revocation turned off.
func (i *Issuer) FailRevocation(status int, code string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.revocation.status, i.codeFlow.revocation.code = status, code
}

// DelayRevocation makes the revocation endpoint wait d before answering, or
// until the client hangs up: a pool that accepts the connection and does not
// answer.
func (i *Issuer) DelayRevocation(d time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.revocation.delay = d
}

// RevocationRequests is every token the client has sent the revocation
// endpoint, in order, whatever it answered.
func (i *Issuer) RevocationRequests() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.codeFlow.revocation.requests...)
}

// SignInAs queues the claims the next authorization request signs in with: the
// person who completes the sign-in at the identity provider. The issuer adds
// iss, aud, iat, exp and the request's nonce when it mints the ID token.
func (i *Issuer) SignInAs(claims map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.queued = append(i.codeFlow.queued, maps.Clone(claims))
}

// UpdateGrants changes the claims every refresh token issued so far answers
// with, as a pool does when a user's groups change.
func (i *Issuer) UpdateGrants(change func(claims map[string]any)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, grant := range i.codeFlow.grants {
		change(grant.claims)
	}
}

// RevokeGrants ends every refresh token issued so far: the next refresh with any
// of them answers invalid_grant, as a pool does for a disabled or deleted user.
func (i *Issuer) RevokeGrants() {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, grant := range i.codeFlow.grants {
		grant.revoked = true
	}
}

// Refreshes counts the refresh-token grants the token endpoint has answered,
// successful or not.
func (i *Issuer) Refreshes() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.codeFlow.refreshes
}

// Authorize follows authURL the way a browser does through the identity
// provider's sign-in, and returns the code and state the issuer redirects back
// with. The person signed in is the one SignInAs queued.
func (i *Issuer) Authorize(t TB, authURL string) (code, state string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("GET %s: %v", authURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("GET %s: status %d, want 302", authURL, response.StatusCode)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect %q: %v", response.Header.Get("Location"), err)
	}
	return location.Query().Get("code"), location.Query().Get("state")
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
	raw, err := sign(key, claims)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return raw
}

func sign(key *Key, claims map[string]any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key.priv, KeyID: key.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", fmt.Errorf("new signer: %w", err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		return "", fmt.Errorf("sign claims: %w", err)
	}
	return signed.CompactSerialize()
}

func (i *Issuer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	status, issuer, omitCodeFlow, omitRevocation := i.discoveryStatus, i.issuerClaim, i.omitCodeFlow, i.omitRevocation
	i.mu.Unlock()
	if status != 0 {
		http.Error(w, "discovery unavailable", status)
		return
	}
	document := map[string]any{
		"issuer":                                issuer,
		"jwks_uri":                              i.server.URL + "/keys",
		"id_token_signing_alg_values_supported": []string{string(jose.RS256)},
	}
	if !omitCodeFlow {
		document["authorization_endpoint"] = i.server.URL + "/authorize"
		document["token_endpoint"] = i.server.URL + "/token"
	}
	if !omitRevocation {
		document["revocation_endpoint"] = i.server.URL + "/revoke"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(document)
}

func (i *Issuer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	i.mu.Lock()
	defer i.mu.Unlock()
	flow := i.codeFlow
	switch {
	case flow == nil:
		http.Error(w, "code flow not enabled", http.StatusNotFound)
		return
	case query.Get("client_id") != flow.clientID || query.Get("response_type") != "code" || query.Get("redirect_uri") == "":
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	case len(flow.queued) == 0:
		http.Error(w, "no sign-in queued (SignInAs)", http.StatusBadRequest)
		return
	}
	claims := flow.queued[0]
	flow.queued = flow.queued[1:]
	code := randomValue()
	flow.codes[code] = codeGrant{claims: claims, nonce: query.Get("nonce"), redirectURI: query.Get("redirect_uri")}
	target, err := url.Parse(query.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	values := target.Query()
	values.Set("code", code)
	values.Set("state", query.Get("state"))
	target.RawQuery = values.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// handleToken answers the two grants a confidential client makes, as Cognito's
// token endpoint does: a code exchange returns an ID token, an access token and
// a refresh token; a refresh returns a fresh ID token and access token and no
// new refresh token.
func (i *Issuer) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	flow := i.codeFlow
	if flow == nil {
		http.Error(w, "code flow not enabled", http.StatusNotFound)
		return
	}
	clientID, clientSecret, basic := r.BasicAuth()
	if !basic {
		clientID, clientSecret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if clientID != flow.clientID || clientSecret != flow.clientSecret {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	var (
		claims  map[string]any
		refresh string
	)
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		grant, ok := flow.codes[r.PostForm.Get("code")]
		delete(flow.codes, r.PostForm.Get("code"))
		if !ok || grant.redirectURI != r.PostForm.Get("redirect_uri") {
			tokenError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		claims = maps.Clone(grant.claims)
		if grant.nonce != "" {
			claims["nonce"] = grant.nonce
		}
		refresh = randomValue()
		flow.grants[refresh] = &refreshGrant{claims: maps.Clone(grant.claims)}
	case "refresh_token":
		flow.refreshes++
		grant, ok := flow.grants[r.PostForm.Get("refresh_token")]
		if !ok || grant.revoked {
			tokenError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		claims = maps.Clone(grant.claims)
	default:
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	now := time.Now()
	claims["iss"] = i.issuerClaim
	claims["aud"] = flow.clientID
	claims["iat"] = now.Add(-time.Minute).Unix()
	claims["exp"] = now.Add(time.Hour).Unix()
	idToken, err := sign(flow.key, claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body := map[string]any{
		"access_token": randomValue(),
		"id_token":     idToken,
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
	if refresh != "" {
		body["refresh_token"] = refresh
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// handleRevoke answers a revocation as Cognito's /oauth2/revoke does for a
// client with a secret: the client authenticates with HTTP Basic, the body
// carries the token, and a token the pool does not know, or has revoked
// already, is answered 200 like one it revokes now. A revoked refresh token's
// next refresh answers invalid_grant.
func (i *Issuer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	i.mu.Lock()
	flow := i.codeFlow
	if flow == nil {
		i.mu.Unlock()
		http.Error(w, "code flow not enabled", http.StatusNotFound)
		return
	}
	clientID, clientSecret, basic := r.BasicAuth()
	if !basic || clientID != flow.clientID || clientSecret != flow.clientSecret {
		i.mu.Unlock()
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	token := r.PostForm.Get("token")
	if token == "" {
		i.mu.Unlock()
		tokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	flow.revocation.requests = append(flow.revocation.requests, token)
	status, code, delay := flow.revocation.status, flow.revocation.code, flow.revocation.delay
	i.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		if code == "" {
			http.Error(w, http.StatusText(status), status)
		} else {
			tokenError(w, status, code)
		}
		return
	}
	i.mu.Lock()
	if grant, ok := flow.grants[token]; ok {
		grant.revoked = true
	}
	i.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func tokenError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func randomValue() string {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		panic(errors.New("oidctest: crypto/rand failed: " + err.Error()))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
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
