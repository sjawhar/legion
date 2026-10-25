// Package oidctest is a local OIDC issuer for tests of the code that verifies
// OIDC tokens: an httptest server answering discovery and JWKS for keys the
// test controls, the minting side a Kubernetes pod's projected service-account
// token would come from, and, once EnableCodeFlow arms it, the authorization,
// token and revocation endpoints a person's sign-in goes through
// (authorization code with optional PKCE S256, refresh token, RFC 7009
// revocation). It is an ordinary
// package rather than a _test.go file so the packages that need it —
// internal/oidc, internal/dispatch/api, internal/dispatch/routes,
// internal/dispatch/store, cmd/dispatch and cmd/listener — can all import it;
// Go cannot share test-only code across packages. cmd/dev-oidc-issuer serves it
// outside any test, as the local development stand-in for a cluster's issuer.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
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
	mux    *http.ServeMux

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
	// omitRefresh makes a code exchange answer with no refresh token
	// (OmitRefreshTokens).
	omitRefresh bool
	// spoil changes the claims of every ID token the token endpoint mints,
	// after the issuer has filled them in (SpoilIDTokens).
	spoil func(claims map[string]any)
	// unqueued chooses the claims of an authorization request that finds
	// none queued (SignInUnqueued); nil, such a request is refused.
	unqueued func(query url.Values) (map[string]any, error)
}

// revocation is the state of the revocation endpoint: every token the client
// sent it, in order, whatever it answered; the status and OAuth error code it
// answers instead of revoking, when FailRevocation set one; where it redirects
// instead, when RedirectRevocation set it; and how long it waits before
// answering.
type revocation struct {
	requests []string
	status   int
	code     string
	redirect string
	delay    time.Duration
}

// codeGrant is one authorization code: the person it signs in, the nonce and
// redirect URI of the authorization request that issued it, and the PKCE S256
// code_challenge that request carried, empty when it carried none.
type codeGrant struct {
	claims      map[string]any
	nonce       string
	redirectURI string
	challenge   string
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
	return newIssuer(t, nil)
}

// NewOnListener is New serving on listener rather than on a random loopback
// port, for cmd/dev-oidc-issuer, whose caller may fix the issuer's address.
// The listener is closed with the server at cleanup.
func NewOnListener(t TB, listener net.Listener) *Issuer {
	t.Helper()
	return newIssuer(t, listener)
}

// newIssuer starts the issuer on listener, or on a random loopback port when
// listener is nil.
func newIssuer(t TB, listener net.Listener) *Issuer {
	t.Helper()
	issuer := &Issuer{mux: http.NewServeMux()}
	issuer.mux.HandleFunc("/.well-known/openid-configuration", issuer.handleDiscovery)
	issuer.mux.HandleFunc("/keys", issuer.handleKeys)
	issuer.mux.HandleFunc("GET /authorize", issuer.handleAuthorize)
	issuer.mux.HandleFunc("POST /token", issuer.handleToken)
	issuer.mux.HandleFunc("POST /revoke", issuer.handleRevoke)
	if listener == nil {
		issuer.server = httptest.NewServer(issuer.mux)
	} else {
		issuer.server = &httptest.Server{Listener: listener, Config: &http.Server{Handler: issuer.mux}}
		issuer.server.Start()
	}
	t.Cleanup(issuer.server.Close)
	issuer.issuerClaim = issuer.server.URL
	return issuer
}

// Handle serves handler at pattern (an http.ServeMux pattern) beside the
// issuer's own endpoints, for cmd/dev-oidc-issuer's control endpoints. A
// pattern that conflicts with one already served panics, as ServeMux does.
func (i *Issuer) Handle(pattern string, handler http.HandlerFunc) {
	i.mux.HandleFunc(pattern, handler)
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

// RedirectRevocation makes the revocation endpoint answer 307 to target
// instead of revoking: a client that follows it re-sends the token there.
func (i *Issuer) RedirectRevocation(target string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.revocation.redirect = target
}

// RevocationRequests is every token the client has sent the revocation
// endpoint, in order, whatever it answered.
func (i *Issuer) RevocationRequests() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.codeFlow.revocation.requests...)
}

// OmitRefreshTokens makes every code exchange from now on answer with no
// refresh token, as Google does for a sign-in that did not ask for offline
// access.
func (i *Issuer) OmitRefreshTokens() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.omitRefresh = true
}

// SpoilIDTokens makes change rewrite the claims of every ID token the token
// endpoint mints from now on, after the issuer has set iss, aud, iat, exp and
// the nonce, so a test can spoil one of them in a token the code flow
// otherwise issues as usual. A nil change mints them unspoiled again.
func (i *Issuer) SpoilIDTokens(change func(claims map[string]any)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.spoil = change
}

// SignInAs queues the claims the next authorization request signs in with: the
// person who completes the sign-in at the identity provider. The issuer adds
// iss, aud, iat, exp and the request's nonce when it mints the ID token.
func (i *Issuer) SignInAs(claims map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.queued = append(i.codeFlow.queued, maps.Clone(claims))
}

// SignInUnqueued makes an authorization request that finds no sign-in queued
// sign in with the claims choose returns for the request's query, as an
// identity provider's own sign-in page chooses a person, from the request's
// login_hint say, rather than refusing it. An error from choose refuses the
// request 400 with its text; a nil choose refuses every such request again.
// choose runs with the issuer locked, so it must not call the issuer.
func (i *Issuer) SignInUnqueued(choose func(query url.Values) (map[string]any, error)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.unqueued = choose
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

// handleAuthorize issues a code for the next queued sign-in, or else the one
// SignInUnqueued chooses, and redirects to the request's redirect_uri with it
// and the request's state. It records the request's nonce, for the ID token,
// and its PKCE code_challenge, which only the S256 method may carry: a
// challenge by any other method, plain included, is a bad request, so a client
// that sends one never gets a code to exchange.
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
	case query.Has("code_challenge") && (query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256"):
		http.Error(w, "bad authorization request: code_challenge must be S256", http.StatusBadRequest)
		return
	case len(flow.queued) == 0 && flow.unqueued == nil:
		http.Error(w, "no sign-in queued (SignInAs)", http.StatusBadRequest)
		return
	}
	var claims map[string]any
	if len(flow.queued) > 0 {
		claims = flow.queued[0]
		flow.queued = flow.queued[1:]
	} else {
		chosen, err := flow.unqueued(query)
		if err != nil {
			http.Error(w, "bad authorization request: "+err.Error(), http.StatusBadRequest)
			return
		}
		claims = maps.Clone(chosen)
	}
	code := randomValue()
	flow.codes[code] = codeGrant{claims: claims, nonce: query.Get("nonce"), redirectURI: query.Get("redirect_uri"), challenge: query.Get("code_challenge")}
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
// a refresh token (none once OmitRefreshTokens is set); a refresh returns a
// fresh ID token and access token and no new refresh token. A code whose
// authorization request carried a PKCE challenge is exchanged only with the
// code_verifier whose S256 is that challenge (RFC 7636 section 4.6); a missing
// or wrong verifier is invalid_grant, and spends the code like any refusal.
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
		if grant.challenge != "" && s256Challenge(r.PostForm.Get("code_verifier")) != grant.challenge {
			tokenError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		claims = maps.Clone(grant.claims)
		if grant.nonce != "" {
			claims["nonce"] = grant.nonce
		}
		if !flow.omitRefresh {
			refresh = randomValue()
			flow.grants[refresh] = &refreshGrant{claims: maps.Clone(grant.claims)}
		}
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
	if flow.spoil != nil {
		flow.spoil(claims)
	}
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

// s256Challenge is the PKCE S256 code_challenge of verifier, and empty for no
// verifier, which matches no recorded challenge.
func s256Challenge(verifier string) string {
	if verifier == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
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
	status, code, redirect, delay := flow.revocation.status, flow.revocation.code, flow.revocation.redirect, flow.revocation.delay
	i.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if redirect != "" {
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
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
