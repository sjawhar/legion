package oidc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	flowClientID     = "dispatch-client"
	flowClientSecret = "dispatch-client-secret"
	flowRedirect     = "https://dispatch.example/auth/callback"
	flowUsername     = "ExampleIdP_a.b+c@d.example"
	flowDomain       = "example.com"
)

// newCodeFlowFor starts a code-flow issuer for flowClientID and discovers it in group mode, the
// Cognito sign-in pool's.
func newCodeFlowFor(t *testing.T) (*oidctest.Issuer, *CodeFlow) {
	t.Helper()
	return newCodeFlowForDomain(t, "")
}

// newCodeFlowForDomain starts a code-flow issuer for flowClientID and discovers it for the hosted
// domain, empty for group mode.
func newCodeFlowForDomain(t *testing.T, domain string) (*oidctest.Issuer, *CodeFlow) {
	t.Helper()
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), flowClientID, flowClientSecret)
	flow, err := NewCodeFlow(context.Background(), issuer.URL(), flowClientID, flowClientSecret, domain)
	if err != nil {
		t.Fatalf("NewCodeFlow(%s): %v", issuer.URL(), err)
	}
	return issuer, flow
}

// googleClaims is a Google ID token's claims for an account of the Workspace domain flowDomain,
// with emailVerified as its email_verified claim.
func googleClaims(emailVerified any) map[string]any {
	return map[string]any{
		"sub":            "110169484474386276334",
		"email":          "A.Person@example.com",
		"email_verified": emailVerified,
		"hd":             flowDomain,
	}
}

// exchange runs one sign-in through flow as the person claims names, and returns its session.
func exchange(t *testing.T, issuer *oidctest.Issuer, flow *CodeFlow, claims map[string]any) Session {
	t.Helper()
	issuer.SignInAs(claims)
	code, _ := issuer.Authorize(t, flow.AuthURL(flowRedirect, "state-1", "nonce-1"))
	session, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	return session
}

// In domain mode the session carries the Google ID token's email, hd and sub, and email_verified
// as true only for the JSON true or the string "true": any other value or shape is false, never an
// error, so a token Google shapes differently is refused by the rule rather than failing the read.
func TestCodeFlowInDomainModeReadsTheWorkspaceClaims(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  bool
	}{
		{"true", true, true},
		{`"true"`, "true", true},
		{`"false"`, "false", false},
		{"false", false, false},
		{"a number", 1, false},
		{"null", nil, false},
		{"an object", map[string]any{"value": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer, flow := newCodeFlowForDomain(t, flowDomain)
			session := exchange(t, issuer, flow, googleClaims(tc.value))
			claims := session.Claims
			if claims.Email != "A.Person@example.com" || claims.HostedDomain != flowDomain || claims.Subject != "110169484474386276334" || claims.EmailVerified != tc.want {
				t.Errorf("claims = %#v, want the email, hd and sub read, and email_verified %t", claims, tc.want)
			}
		})
	}
	t.Run("an email and hd that are not strings", func(t *testing.T) {
		issuer, flow := newCodeFlowForDomain(t, flowDomain)
		claims := googleClaims(true)
		claims["email"], claims["hd"] = 7, []string{flowDomain}
		if session := exchange(t, issuer, flow, claims); session.Claims.Email != "" || session.Claims.HostedDomain != "" {
			t.Errorf("claims = %#v, want no email and no hd", session.Claims)
		}
	})
	t.Run("no email_verified, email or hd", func(t *testing.T) {
		issuer, flow := newCodeFlowForDomain(t, flowDomain)
		session := exchange(t, issuer, flow, map[string]any{"sub": "110169484474386276334"})
		if session.Claims.Email != "" || session.Claims.HostedDomain != "" || session.Claims.EmailVerified {
			t.Errorf("claims = %#v, want none of the Workspace claims", session.Claims)
		}
	})
}

// A refresh in domain mode reads the claims as Google holds them now, so the hourly confirmation
// applies the domain rule to the refreshed token.
func TestCodeFlowRefreshInDomainModeReadsTheCurrentWorkspaceClaims(t *testing.T) {
	issuer, flow := newCodeFlowForDomain(t, flowDomain)
	session := exchange(t, issuer, flow, googleClaims(true))
	issuer.UpdateGrants(func(claims map[string]any) {
		claims["hd"] = "other.example"
		claims["email_verified"] = "false"
	})
	refreshed, err := flow.Refresh(context.Background(), session.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.Claims.HostedDomain != "other.example" || refreshed.Claims.EmailVerified || refreshed.Claims.Email != "A.Person@example.com" {
		t.Errorf("refreshed claims = %#v, want hd other.example and email_verified false", refreshed.Claims)
	}
}

// Group mode reads none of the Workspace claims, whatever the token carries: a Cognito ID token's
// email is one the person can write.
func TestCodeFlowInGroupModeReadsNoWorkspaceClaims(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	claims := personClaims("dispatch-members")
	claims["email_verified"], claims["hd"] = true, flowDomain
	session := exchange(t, issuer, flow, claims)
	if session.Claims.Email != "" || session.Claims.EmailVerified || session.Claims.HostedDomain != "" {
		t.Errorf("claims = %#v, want no Workspace claims in group mode", session.Claims)
	}
	if session.Claims.Username != flowUsername || !reflect.DeepEqual(session.Claims.Groups, []string{"dispatch-members"}) {
		t.Errorf("claims = %#v, want the Cognito username and groups", session.Claims)
	}
}

// In domain mode the authorization request asks Google for offline access, so the exchange returns
// a refresh token, with consent each time, so a second sign-in returns one too, and names the
// domain as a hint; group mode sends none of the three.
func TestCodeFlowAuthURLAsksGoogleForOfflineAccessInTheDomain(t *testing.T) {
	_, domainFlow := newCodeFlowForDomain(t, flowDomain)
	_, groupFlow := newCodeFlowFor(t)
	query := func(flow *CodeFlow) url.Values {
		location, err := url.Parse(flow.AuthURL(flowRedirect, "state-1", "nonce-1"))
		if err != nil {
			t.Fatal(err)
		}
		return location.Query()
	}
	domain := query(domainFlow)
	for name, want := range map[string]string{"access_type": "offline", "prompt": "consent", "hd": flowDomain, "client_id": flowClientID, "nonce": "nonce-1", "scope": "openid email profile"} {
		if got := domain.Get(name); got != want {
			t.Errorf("domain mode: %s = %q, want %q", name, got, want)
		}
	}
	group := query(groupFlow)
	for _, name := range []string{"access_type", "prompt", "hd"} {
		if group.Has(name) {
			t.Errorf("group mode: the authorization request carries %s=%q", name, group.Get(name))
		}
	}
}

// personClaims is a Cognito ID token's claims for a person signed in through the Google provider:
// its email claim is one the person could have written, unlike its username.
func personClaims(groups ...string) map[string]any {
	return map[string]any{
		"sub":              "f3c2a7e0-person",
		"cognito:username": flowUsername,
		"cognito:groups":   groups,
		"email":            "someone-else@d.example",
		"identities":       []map[string]any{{"providerName": "ExampleIdP", "providerType": "SAML", "primary": "true"}},
	}
}

func TestCodeFlowExchangesACodeForTheSignedInPerson(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.SignInAs(personClaims("dispatch-members", "other-group"))

	authURL := flow.AuthURL(flowRedirect, "state-1", "nonce-1")
	code, state := issuer.Authorize(t, authURL)
	if state != "state-1" {
		t.Fatalf("authorization returned state %q, want state-1", state)
	}
	session, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if session.Claims.Username != flowUsername {
		t.Errorf("Username = %q, want %q", session.Claims.Username, flowUsername)
	}
	if want := []string{"dispatch-members", "other-group"}; !reflect.DeepEqual(session.Claims.Groups, want) {
		t.Errorf("Groups = %q, want %q", session.Claims.Groups, want)
	}
	if want := []string{"ExampleIdP"}; !reflect.DeepEqual(session.Claims.Providers, want) {
		t.Errorf("Providers = %q, want %q", session.Claims.Providers, want)
	}
	if session.Claims.Subject != "f3c2a7e0-person" || session.Claims.Issuer != issuer.URL() {
		t.Errorf("claims = %#v, want the person's subject from %s", session.Claims, issuer.URL())
	}
	if session.RefreshToken == "" {
		t.Fatal("Exchange returned no refresh token")
	}
}

func TestCodeFlowAuthURLAsksForTheCodeWithTheClientsScopes(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	authURL := flow.AuthURL(flowRedirect, "state-1", "nonce-1")
	for _, want := range []string{
		issuer.URL() + "/authorize?",
		"client_id=" + flowClientID,
		"response_type=code",
		"scope=openid+email+profile",
		"state=state-1",
		"nonce=nonce-1",
		"redirect_uri=https%3A%2F%2Fdispatch.example%2Fauth%2Fcallback",
	} {
		if !strings.Contains(authURL, want) {
			t.Errorf("AuthURL %q does not carry %q", authURL, want)
		}
	}
}

// The nonce is what binds the ID token to the browser that started this sign-in: a code another
// sign-in minted, replayed into this callback, carries that sign-in's nonce.
func TestCodeFlowExchangeRefusesAnIDTokenForAnotherNonce(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.SignInAs(personClaims("dispatch-members"))
	code, _ := issuer.Authorize(t, flow.AuthURL(flowRedirect, "state-1", "nonce-of-another-sign-in"))
	if _, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1"); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("Exchange with another sign-in's nonce: err = %v, want a nonce refusal", err)
	}
}

func TestCodeFlowExchangeRefusesACodeTheIssuerRefuses(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.SignInAs(personClaims("dispatch-members"))
	code, _ := issuer.Authorize(t, flow.AuthURL(flowRedirect, "state-1", "nonce-1"))
	if _, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1"); err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	if _, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1"); err == nil {
		t.Fatal("a code exchanged twice was accepted the second time")
	}
}

func TestCodeFlowRefreshReadsThePersonsCurrentClaims(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.SignInAs(personClaims("dispatch-members"))
	code, _ := issuer.Authorize(t, flow.AuthURL(flowRedirect, "state-1", "nonce-1"))
	session, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	refreshed, err := flow.Refresh(context.Background(), session.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.Claims.Username != flowUsername || !reflect.DeepEqual(refreshed.Claims.Groups, []string{"dispatch-members"}) {
		t.Errorf("refreshed claims = %#v, want the same person in dispatch-members", refreshed.Claims)
	}
	// Cognito answers a refresh without a new refresh token; the one that worked stays the one to use.
	if refreshed.RefreshToken != session.RefreshToken {
		t.Errorf("refreshed refresh token = %q, want the original %q", refreshed.RefreshToken, session.RefreshToken)
	}

	issuer.UpdateGrants(func(claims map[string]any) { claims["cognito:groups"] = []string{"other-group"} })
	removed, err := flow.Refresh(context.Background(), session.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh after the group change: %v", err)
	}
	if !reflect.DeepEqual(removed.Claims.Groups, []string{"other-group"}) {
		t.Errorf("Groups after removal = %q, want [other-group]", removed.Claims.Groups)
	}

	issuer.RevokeGrants()
	if _, err := flow.Refresh(context.Background(), session.RefreshToken); err == nil {
		t.Fatal("Refresh with a revoked refresh token succeeded")
	}
}

// Revoke sends the refresh token to the revocation endpoint the issuer's discovery names, as the
// client, and the issuer ends it: the next refresh with it is refused.
func TestCodeFlowRevokeEndsTheRefreshToken(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.SignInAs(personClaims("dispatch-members"))
	code, _ := issuer.Authorize(t, flow.AuthURL(flowRedirect, "state-1", "nonce-1"))
	session, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if err := flow.Revoke(context.Background(), session.RefreshToken); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := issuer.RevocationRequests(); !reflect.DeepEqual(got, []string{session.RefreshToken}) {
		t.Errorf("the revocation endpoint was sent %q, want the refresh token once", got)
	}
	if _, err := flow.Refresh(context.Background(), session.RefreshToken); err == nil {
		t.Fatal("Refresh with the revoked refresh token succeeded")
	}
}

// A revocation the issuer does not accept is an error naming its HTTP status and the OAuth error
// code its body names, if any, and never the token.
func TestCodeFlowRevokeReportsTheIssuersRefusalWithoutTheToken(t *testing.T) {
	for _, refusal := range []struct {
		status int
		code   string
		want   string
	}{{http.StatusBadRequest, "invalid_request", "HTTP 400 invalid_request"}, {http.StatusServiceUnavailable, "", "HTTP 503"}} {
		issuer, flow := newCodeFlowFor(t)
		issuer.FailRevocation(refusal.status, refusal.code)
		token := "plain-refresh-token-" + refusal.code
		err := flow.Revoke(context.Background(), token)
		if err == nil || !strings.Contains(err.Error(), refusal.want) || strings.Contains(err.Error(), token) {
			t.Errorf("Revoke against an issuer answering %d %q: err = %v, want one naming %s and not the token", refusal.status, refusal.code, err, refusal.want)
		}
	}
}

// A revocation endpoint that redirects is answered as a refusal, never followed: following a 307
// would re-send the token to the redirect's target.
func TestCodeFlowRevokeDoesNotFollowARedirect(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Add(1) }))
	t.Cleanup(target.Close)
	issuer, flow := newCodeFlowFor(t)
	issuer.RedirectRevocation(target.URL + "/elsewhere")
	if err := flow.Revoke(context.Background(), "plain-refresh-token"); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Errorf("Revoke against a redirecting endpoint: err = %v, want one naming HTTP 307", err)
	}
	if n := received.Load(); n != 0 {
		t.Errorf("the redirect's target received %d requests, want none", n)
	}
}

// An issuer whose discovery names no revocation endpoint cannot revoke: Revoke says so, and no
// endpoint is guessed.
func TestCodeFlowRevokeFailsAgainstAnIssuerWithNoRevocationEndpoint(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), flowClientID, flowClientSecret)
	issuer.OmitRevocationEndpoint()
	flow, err := NewCodeFlow(context.Background(), issuer.URL(), flowClientID, flowClientSecret, "")
	if err != nil {
		t.Fatalf("NewCodeFlow: %v", err)
	}
	if err := flow.Revoke(context.Background(), "plain-refresh-token"); err == nil || !strings.Contains(err.Error(), "revocation_endpoint") {
		t.Fatalf("Revoke against an issuer with no revocation endpoint: err = %v, want one naming revocation_endpoint", err)
	}
	if got := issuer.RevocationRequests(); len(got) != 0 {
		t.Errorf("the issuer's /revoke was sent %q, want nothing", got)
	}
}

func TestNewCodeFlowRefusesAnIssuerWithNoCodeFlowEndpoints(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.PublishKey(t, "signing-key")
	issuer.OmitCodeFlowEndpoints()
	_, err := NewCodeFlow(context.Background(), issuer.URL(), flowClientID, flowClientSecret, "")
	if err == nil || !strings.Contains(err.Error(), "token_endpoint") {
		t.Fatalf("NewCodeFlow against an issuer with no code-flow endpoints: err = %v, want a refusal naming token_endpoint", err)
	}
}

func TestDiscoverCodeFlowRefusesAnIssuerWhoseDiscoveryFails(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.FailDiscovery(503)
	_, err := DiscoverCodeFlow(context.Background(), issuer.URL(), flowClientID, flowClientSecret, "", time.Second)
	if err == nil || !strings.Contains(err.Error(), issuer.URL()) {
		t.Fatalf("DiscoverCodeFlow against a failing issuer: err = %v, want one naming the issuer", err)
	}
}

// onlineSignIn is an online-mode sign-in hinting the account of googleClaims, with a fresh PKCE
// verifier and nonce as its ID token's nonce.
func onlineSignIn(nonce string) OnlineSignIn {
	return OnlineSignIn{State: "state-1", Nonce: nonce, Verifier: oauth2.GenerateVerifier(), LoginHint: "a.person@example.com"}
}

// onlineAuthURLQuery is the query of flow's online-mode authorization URL for signIn.
func onlineAuthURLQuery(t *testing.T, flow *CodeFlow, signIn OnlineSignIn) url.Values {
	t.Helper()
	location, err := url.Parse(flow.OnlineAuthURL(flowRedirect, signIn))
	if err != nil {
		t.Fatal(err)
	}
	return location.Query()
}

// An online-mode authorization request carries PKCE S256 for the sign-in's verifier, its nonce,
// state and login hint, and the domain as hd; it asks for no offline access and no consent, so
// Google shows no consent screen and issues no refresh token. prompt=select_account is sent only
// when the caller asks for an account switch, and login_hint only when the caller names one.
func TestCodeFlowOnlineAuthURLCarriesPKCEAndAsksForNoOfflineAccess(t *testing.T) {
	_, flow := newCodeFlowForDomain(t, flowDomain)
	signIn := onlineSignIn("nonce-1")
	query := onlineAuthURLQuery(t, flow, signIn)
	for name, want := range map[string]string{
		"code_challenge":        oauth2.S256ChallengeFromVerifier(signIn.Verifier),
		"code_challenge_method": "S256",
		"nonce":                 "nonce-1",
		"state":                 "state-1",
		"login_hint":            "a.person@example.com",
		"hd":                    flowDomain,
		"client_id":             flowClientID,
		"response_type":         "code",
		"redirect_uri":          flowRedirect,
		"scope":                 "openid email profile",
	} {
		if got := query.Get(name); got != want {
			t.Errorf("online mode: %s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"access_type", "prompt"} {
		if query.Has(name) {
			t.Errorf("online mode: the authorization request carries %s=%q", name, query.Get(name))
		}
	}

	switching := onlineSignIn("nonce-2")
	switching.LoginHint, switching.SelectAccount = "", true
	query = onlineAuthURLQuery(t, flow, switching)
	if got := query.Get("prompt"); got != "select_account" {
		t.Errorf("an account switch: prompt = %q, want select_account", got)
	}
	if query.Has("login_hint") || query.Has("access_type") {
		t.Errorf("an account switch with no hint: the request carries login_hint=%q access_type=%q", query.Get("login_hint"), query.Get("access_type"))
	}

	_, groupFlow := newCodeFlowFor(t)
	if query := onlineAuthURLQuery(t, groupFlow, onlineSignIn("nonce-3")); query.Has("hd") {
		t.Errorf("online mode with no hosted domain: the request carries hd=%q", query.Get("hd"))
	}
}

// An issuer that answers a code exchange with no refresh token, as Google does without offline
// access, signs the person in in online mode, which reads the Workspace claims and the nonce from
// the verified ID token; offline mode still refuses that answer. The flow is built by
// DiscoverCodeFlow, as a caller that discovers the issuer at its first sign-in builds it.
func TestCodeFlowOnlineExchangeSignsInWithNoRefreshToken(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), flowClientID, flowClientSecret)
	issuer.OmitRefreshTokens()
	flow, err := DiscoverCodeFlow(context.Background(), issuer.URL(), flowClientID, flowClientSecret, flowDomain, time.Second)
	if err != nil {
		t.Fatalf("DiscoverCodeFlow: %v", err)
	}

	issuer.SignInAs(googleClaims(true))
	signIn := onlineSignIn("nonce-1")
	code, state := issuer.Authorize(t, flow.OnlineAuthURL(flowRedirect, signIn))
	if state != signIn.State {
		t.Fatalf("authorization returned state %q, want %q", state, signIn.State)
	}
	claims, err := flow.OnlineExchange(context.Background(), flowRedirect, code, signIn)
	if err != nil {
		t.Fatalf("OnlineExchange with no refresh token: %v", err)
	}
	if claims.Subject != "110169484474386276334" || claims.Email != "A.Person@example.com" || !claims.EmailVerified || claims.HostedDomain != flowDomain || claims.Nonce != "nonce-1" || claims.Issuer != issuer.URL() {
		t.Errorf("claims = %#v, want the Workspace account's verified claims with nonce-1", claims)
	}

	issuer.SignInAs(googleClaims(true))
	code, _ = issuer.Authorize(t, flow.AuthURL(flowRedirect, "state-2", "nonce-2"))
	if _, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-2"); err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("offline Exchange with no refresh token: err = %v, want a refusal naming the missing refresh token", err)
	}
}

// The issuer exchanges a code whose authorization request carried a PKCE challenge only for the
// verifier of that challenge: a wrong verifier, or none (offline mode's Exchange sends none), is
// refused at the issuer with invalid_grant. OnlineExchange refuses a sign-in with no verifier
// before asking the issuer.
func TestCodeFlowOnlineExchangeRefusesAWrongOrMissingVerifier(t *testing.T) {
	issuer, flow := newCodeFlowForDomain(t, flowDomain)
	authorize := func(signIn OnlineSignIn) string {
		t.Helper()
		issuer.SignInAs(googleClaims(true))
		code, _ := issuer.Authorize(t, flow.OnlineAuthURL(flowRedirect, signIn))
		return code
	}

	signIn := onlineSignIn("nonce-1")
	code := authorize(signIn)
	wrong := signIn
	wrong.Verifier = oauth2.GenerateVerifier()
	if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, wrong); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("OnlineExchange with another verifier: err = %v, want the issuer's invalid_grant", err)
	}

	code = authorize(signIn)
	if _, err := flow.Exchange(context.Background(), flowRedirect, code, signIn.Nonce); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("an exchange with no verifier for a code with a challenge: err = %v, want the issuer's invalid_grant", err)
	}

	code = authorize(signIn)
	missing := signIn
	missing.Verifier = ""
	if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, missing); err == nil || !strings.Contains(err.Error(), "verifier") {
		t.Errorf("OnlineExchange with no verifier: err = %v, want a refusal naming the verifier", err)
	}
	// The refusal spent nothing at the issuer: the right verifier still signs the person in.
	if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, signIn); err != nil {
		t.Errorf("OnlineExchange with the sign-in's own verifier: %v", err)
	}
}

// Online mode verifies the ID token as offline mode does: one minted for another sign-in's nonce,
// for another client, by another issuer, past its expiry, or under a key the issuer does not hold
// signs nobody in.
func TestCodeFlowOnlineExchangeRefusesAnIDTokenNotForThisSignIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(claims map[string]any)
		want  string
	}{
		{"another audience", func(claims map[string]any) { claims["aud"] = "another-client" }, "(audience)"},
		{"another issuer", func(claims map[string]any) { claims["iss"] = "https://issuer.internal.example" }, "(issuer)"},
		{"expired", func(claims map[string]any) {
			claims["iat"], claims["exp"] = time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
		}, "(expired)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer, flow := newCodeFlowForDomain(t, flowDomain)
			issuer.SpoilIDTokens(tc.spoil)
			issuer.SignInAs(googleClaims(true))
			signIn := onlineSignIn("nonce-1")
			code, _ := issuer.Authorize(t, flow.OnlineAuthURL(flowRedirect, signIn))
			if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, signIn); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("OnlineExchange of an ID token for %s: err = %v, want a refusal naming %s", tc.name, err, tc.want)
			}
		})
	}

	t.Run("another sign-in's nonce", func(t *testing.T) {
		issuer, flow := newCodeFlowForDomain(t, flowDomain)
		issuer.SignInAs(googleClaims(true))
		signIn := onlineSignIn("nonce-of-another-sign-in")
		code, _ := issuer.Authorize(t, flow.OnlineAuthURL(flowRedirect, signIn))
		signIn.Nonce = "nonce-1"
		if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, signIn); err == nil || !strings.Contains(err.Error(), "nonce") {
			t.Errorf("OnlineExchange with another sign-in's nonce: err = %v, want a nonce refusal", err)
		}
	})

	t.Run("a signature by a key the issuer does not hold", func(t *testing.T) {
		issuer := oidctest.New(t)
		published := issuer.PublishKey(t, "signing-key")
		// A foreign key under the published key's id: the key set resolves, and only the
		// signature fails.
		issuer.EnableCodeFlow(oidctest.NewKey(t, published.KeyID()), flowClientID, flowClientSecret)
		flow, err := NewCodeFlow(context.Background(), issuer.URL(), flowClientID, flowClientSecret, flowDomain)
		if err != nil {
			t.Fatalf("NewCodeFlow: %v", err)
		}
		issuer.SignInAs(googleClaims(true))
		signIn := onlineSignIn("nonce-1")
		code, _ := issuer.Authorize(t, flow.OnlineAuthURL(flowRedirect, signIn))
		if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, signIn); err == nil || !strings.Contains(err.Error(), "(signature)") {
			t.Errorf("OnlineExchange of a forged ID token: err = %v, want a refusal naming the signature", err)
		}
	})
}
