package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	flowClientID     = "dispatch-client"
	flowClientSecret = "dispatch-client-secret"
	flowRedirect     = "https://dispatch.example/auth/callback"
	flowUsername     = "ExampleIdP_a.b+c@d.example"
)

// newCodeFlowFor starts a code-flow issuer for flowClientID and discovers it.
func newCodeFlowFor(t *testing.T) (*oidctest.Issuer, *CodeFlow) {
	t.Helper()
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), flowClientID, flowClientSecret)
	flow, err := NewCodeFlow(context.Background(), issuer.URL(), flowClientID, flowClientSecret)
	if err != nil {
		t.Fatalf("NewCodeFlow(%s): %v", issuer.URL(), err)
	}
	return issuer, flow
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

// A revocation the issuer does not accept is a RevocationError naming its HTTP status and the OAuth
// error code its body names, if any, and never the token.
func TestCodeFlowRevokeReportsTheIssuersRefusalWithoutTheToken(t *testing.T) {
	for _, refusal := range []struct {
		status int
		code   string
	}{{http.StatusBadRequest, "invalid_request"}, {http.StatusServiceUnavailable, ""}} {
		issuer, flow := newCodeFlowFor(t)
		issuer.FailRevocation(refusal.status, refusal.code)
		token := "plain-refresh-token-" + refusal.code
		err := flow.Revoke(context.Background(), token)
		var refused *RevocationError
		if !errors.As(err, &refused) || refused.Status != refusal.status || refused.Code != refusal.code {
			t.Errorf("Revoke against an issuer answering %d %q: err = %#v, want a RevocationError with that status and code", refusal.status, refusal.code, err)
			continue
		}
		if strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", refusal.status)) {
			t.Errorf("Revoke's error %q carries the token or does not name HTTP %d", err, refusal.status)
		}
	}
}

// An issuer whose discovery names no revocation endpoint cannot revoke: Revoke says so, and no
// endpoint is guessed.
func TestCodeFlowRevokeFailsAgainstAnIssuerWithNoRevocationEndpoint(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), flowClientID, flowClientSecret)
	issuer.OmitRevocationEndpoint()
	flow, err := NewCodeFlow(context.Background(), issuer.URL(), flowClientID, flowClientSecret)
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
	_, err := NewCodeFlow(context.Background(), issuer.URL(), flowClientID, flowClientSecret)
	if err == nil || !strings.Contains(err.Error(), "token_endpoint") {
		t.Fatalf("NewCodeFlow against an issuer with no code-flow endpoints: err = %v, want a refusal naming token_endpoint", err)
	}
}

func TestDiscoverCodeFlowRefusesAnIssuerWhoseDiscoveryFails(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.FailDiscovery(503)
	_, err := DiscoverCodeFlow(context.Background(), issuer.URL(), flowClientID, flowClientSecret, time.Second)
	if err == nil || !strings.Contains(err.Error(), issuer.URL()) {
		t.Fatalf("DiscoverCodeFlow against a failing issuer: err = %v, want one naming the issuer", err)
	}
}
