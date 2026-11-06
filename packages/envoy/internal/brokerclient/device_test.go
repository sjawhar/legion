// packages/envoy/internal/brokerclient/device_test.go
//
// A person's sign-in, which `agent-secrets login` runs: Google's device authorization grant
// against a real OpenID issuer (internal/oidc/oidctest, the issuer the broker's own tests mint
// against and the one cmd/dev-oidc-issuer serves), asking for openid and email and nothing else,
// and the refresh token it leaves minting the next identity token with no step by the person.

package brokerclient

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

func TestDeviceSignInAsksForWhoTheyAreAndNothingElse(t *testing.T) {
	const (
		clientID     = "example-device-client.apps.googleusercontent.example"
		clientSecret = "example-device-client-secret"
		person       = "ada@example.com"
	)
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "google-key")
	issuer.EnableDeviceFlow(key, clientID, clientSecret)
	issuer.SignInByDefault(map[string]any{
		"sub": person, "email": person, "email_verified": true, "hd": "example.com",
	})

	asked := &recordingTransport{}
	flow := &DeviceFlow{
		Issuer: issuer.URL(), ClientID: clientID, ClientSecret: clientSecret,
		HTTP: &http.Client{Transport: asked},
	}
	ctx := context.Background()
	authorization, err := flow.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if authorization.UserCode == "" || authorization.VerificationURI == "" {
		t.Fatalf("the sign-in answered %+v, want a code for the person to type and the page to type it on", authorization)
	}
	if scopes := asked.scopes(); !slices.Equal(scopes, []string{"openid", "email"}) {
		t.Errorf("the sign-in asked for the scopes %v, want openid and email alone", scopes)
	}
	if err := issuer.ApproveDeviceCode(authorization.UserCode); err != nil {
		t.Fatalf("the person types the code in: %v", err)
	}
	authorization.Interval = 1
	signIn, err := flow.Wait(ctx, authorization)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if signIn.Person != person {
		t.Errorf("the sign-in names %q, want %q: the person the identity token's email claim names", signIn.Person, person)
	}
	if signIn.IDToken == "" || signIn.RefreshToken == "" {
		t.Fatal("the sign-in left no identity token or no refresh token, so this machine could not renew it without the person")
	}

	// The record `agent-secrets login` keeps is the refresh token, and what it buys is a fresh
	// identity token at every trade.
	identity := &PersonIdentity{Flow: flow, RefreshToken: signIn.RefreshToken, Person: person}
	token, tokenType, err := identity.Token(ctx)
	if err != nil {
		t.Fatalf("the person's identity at the next trade: %v", err)
	}
	if tokenType != oauthwire.SubjectTokenTypePerson {
		t.Errorf("a person's token is presented as %q, want %q", tokenType, oauthwire.SubjectTokenTypePerson)
	}
	if token == "" {
		t.Error("the refresh answered no identity token")
	}
	if got, err := PersonOf(token); err != nil || got != person {
		t.Errorf("the renewed token names %q (%v), want %q", got, err, person)
	}
}

// TestASignInTheIssuerNoLongerHonoursNamesTheCommandThatMakesAnother: a person who revoked the
// grant, or a refresh token Google retired, stops the machine with a sentence that says what to
// run, which is the rule every refusal a sign-in cures follows.
func TestASignInTheIssuerNoLongerHonoursNamesTheCommandThatMakesAnother(t *testing.T) {
	const clientID, clientSecret = "example-device-client", "example-device-client-secret"
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "google-key")
	issuer.EnableDeviceFlow(key, clientID, clientSecret)
	issuer.SignInByDefault(map[string]any{"sub": "ada", "email": "ada@example.com", "email_verified": true})
	flow := &DeviceFlow{Issuer: issuer.URL(), ClientID: clientID, ClientSecret: clientSecret}
	_, err := flow.Refresh(context.Background(), "a-refresh-token-this-issuer-never-issued")
	if err == nil || !strings.Contains(err.Error(), "agent-secrets login") {
		t.Fatalf("Refresh with a retired refresh token = %v, want a refusal naming `agent-secrets login`", err)
	}
}

// recordingTransport keeps the form of every request it carries, so a test reads what the
// sign-in asked the issuer for.
type recordingTransport struct {
	forms []url.Values
}

func (r *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil && request.GetBody != nil {
		if body, err := request.GetBody(); err == nil {
			defer body.Close()
			buffer := make([]byte, 1<<16)
			n, _ := body.Read(buffer)
			if form, err := url.ParseQuery(string(buffer[:n])); err == nil {
				r.forms = append(r.forms, form)
			}
		}
	}
	return http.DefaultTransport.RoundTrip(request)
}

// scopes is the scope the device authorization asked for, as its own words.
func (r *recordingTransport) scopes() []string {
	for _, form := range r.forms {
		if scope := form.Get("scope"); scope != "" {
			return strings.Fields(scope)
		}
	}
	return nil
}
