// packages/envoy/internal/brokerclient/device.go

package brokerclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// SignInScopes are the only scopes a person's sign-in asks for: who they are and their address.
// `agent-secrets login` asks for nothing else, so the grant a person gives reaches no mail, no
// drive and no calendar of theirs.
var SignInScopes = []string{"openid", "email"}

// idTokenAlgorithms are the signature algorithms a person's ID token may be signed with, which
// go-jose requires a parser to name. Nothing here verifies a signature — the broker is the
// verifier, and this reads the address claim alone to say whose sign-in a record is — so the
// list is Google's own asymmetric set rather than a judgement of its own.
var idTokenAlgorithms = []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.ES512, jose.PS256, jose.PS384, jose.PS512}

// DeviceFlow is Google's device authorization grant (RFC 8628) as `agent-secrets login` runs it:
// the machine asks for a code, the person types it in on a device of their own, and the machine
// is answered the person's ID token and a refresh token it keeps. It is x/oauth2's own
// DeviceAuth and DeviceAccessToken against the endpoints the issuer's OpenID discovery document
// names, so the same code runs against Google and against a dev stack's issuer.
type DeviceFlow struct {
	// Issuer is the OpenID provider: https://accounts.google.com, or a dev stack's own.
	Issuer string
	// ClientID and ClientSecret are the device-flow client registered at that issuer. Google
	// registers a limited-input device client as confidential, so the secret is sent; it is no
	// secret of the person's, and the broker checks the audience of the token it mints rather
	// than trusting this client.
	ClientID, ClientSecret string
	// HTTP is the client every call goes through; nil is http.DefaultClient.
	HTTP *http.Client

	mu     sync.Mutex
	config *oauth2.Config
}

// SignIn is what a completed device grant leaves a machine: the person who signed in, the ID
// token of that moment, and the refresh token that mints the next one. `agent-secrets login`
// keeps the refresh token beside the box key, readable by that person's own user alone.
type SignIn struct {
	// Person is the `email` claim of the ID token, the address the broker records the sign-in
	// under.
	Person string
	// IDToken is the person's Google identity token, the subject of the broker's exchange.
	IDToken string
	// RefreshToken mints the next ID token with no step by the person.
	RefreshToken string
}

// discover reads the issuer's discovery document once per DeviceFlow and builds the x/oauth2
// client from it. An issuer that names no device authorization endpoint serves no device grant,
// which is a refusal of its own: a person cannot sign in there with no browser.
func (d *DeviceFlow) discover(ctx context.Context) (*oauth2.Config, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config != nil {
		return d.config, nil
	}
	provider, err := gooidc.NewProvider(d.context(ctx), d.Issuer)
	if err != nil {
		return nil, fmt.Errorf("read the sign-in issuer %s: %w", d.Issuer, err)
	}
	endpoint := provider.Endpoint()
	if endpoint.DeviceAuthURL == "" || endpoint.TokenURL == "" {
		return nil, fmt.Errorf("the sign-in issuer %s names no device_authorization_endpoint or token_endpoint, so it serves no device sign-in", d.Issuer)
	}
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	d.config = &oauth2.Config{ClientID: d.ClientID, ClientSecret: d.ClientSecret, Endpoint: endpoint, Scopes: SignInScopes}
	return d.config, nil
}

// context is ctx carrying the HTTP client x/oauth2 and go-oidc send their requests with.
func (d *DeviceFlow) context(ctx context.Context) context.Context {
	if d.HTTP == nil {
		return ctx
	}
	return context.WithValue(ctx, oauth2.HTTPClient, d.HTTP)
}

// Start asks the issuer for a device code and a user code: what the person types in, and where.
func (d *DeviceFlow) Start(ctx context.Context) (*oauth2.DeviceAuthResponse, error) {
	config, err := d.discover(ctx)
	if err != nil {
		return nil, err
	}
	answer, err := config.DeviceAuth(d.context(ctx))
	if err != nil {
		return nil, fmt.Errorf("ask %s for a sign-in code: %w", d.Issuer, err)
	}
	if answer.DeviceCode == "" || answer.UserCode == "" {
		return nil, fmt.Errorf("%s answered no device code to poll or no code for the person to type", d.Issuer)
	}
	return answer, nil
}

// Wait polls the issuer until the person has typed the code in, and answers the sign-in that
// leaves. x/oauth2 polls at the interval the issuer asked for and stops when the code expires.
func (d *DeviceFlow) Wait(ctx context.Context, authorization *oauth2.DeviceAuthResponse) (*SignIn, error) {
	config, err := d.discover(ctx)
	if err != nil {
		return nil, err
	}
	token, err := config.DeviceAccessToken(d.context(ctx), authorization)
	if err != nil {
		return nil, fmt.Errorf("wait for the sign-in at %s: %w", d.Issuer, err)
	}
	return d.signIn(token, true)
}

// ErrSignInEnded is a kept refresh token the issuer no longer honours: the person revoked the
// grant at Google, or it expired. It is a sentinel because it is as final as a revoke the
// broker records — no retry and no other caller cures it — so every reader of a sign-in falls
// back to this machine's own authority on it rather than asking the issuer again at every
// enrolment (SignInAuthority.Current).
var ErrSignInEnded = errors.New("the issuer no longer honours this sign-in")

// Refresh mints a fresh ID token from a kept refresh token, with no step by the person. A
// refresh token the issuer no longer honours — the person revoked the grant, or it expired — is
// ErrSignInEnded, a sign-in that is over, which names the command that makes another.
func (d *DeviceFlow) Refresh(ctx context.Context, refreshToken string) (*SignIn, error) {
	config, err := d.discover(ctx)
	if err != nil {
		return nil, err
	}
	token, err := config.TokenSource(d.context(ctx), &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		var refused *oauth2.RetrieveError
		if errors.As(err, &refused) && refused.ErrorCode == "invalid_grant" {
			return nil, fmt.Errorf("%s %w; run: %s", d.Issuer, ErrSignInEnded, SignInCommand)
		}
		return nil, fmt.Errorf("renew the sign-in at %s: %w", d.Issuer, err)
	}
	// A refresh answers no new refresh token, so the kept one stays the one to keep.
	return d.signIn(token, false)
}

// signIn reads the ID token out of an answer and names the person it was minted for. needRefresh
// is the first grant's rule: a sign-in a machine cannot renew is no sign-in to keep.
func (d *DeviceFlow) signIn(token *oauth2.Token, needRefresh bool) (*SignIn, error) {
	idToken, _ := token.Extra("id_token").(string)
	if idToken == "" {
		return nil, fmt.Errorf("%s answered no id_token: the sign-in asked for the %q scope and must be answered one", d.Issuer, "openid")
	}
	if needRefresh && token.RefreshToken == "" {
		return nil, fmt.Errorf("%s answered no refresh token, so this machine could not renew the sign-in without the person again", d.Issuer)
	}
	person, err := PersonOf(idToken)
	if err != nil {
		return nil, err
	}
	return &SignIn{Person: person, IDToken: idToken, RefreshToken: token.RefreshToken}, nil
}

// PersonOf is the `email` claim of an ID token: whose sign-in a record is. It verifies nothing —
// the broker checks the signature, the audience, the domain and email_verified, and refuses a
// token that fails any of them — so what it reads is a label on a local record and never a
// decision.
func PersonOf(idToken string) (string, error) {
	parsed, err := jose.ParseSigned(idToken, idTokenAlgorithms)
	if err != nil {
		return "", fmt.Errorf("read the identity token the sign-in answered: %w", err)
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(parsed.UnsafePayloadWithoutVerification(), &claims); err != nil {
		return "", fmt.Errorf("read the identity token's claims: %w", err)
	}
	if !strings.Contains(claims.Email, "@") {
		return "", errors.New("the identity token carries no email claim of an address's shape, so it names no person")
	}
	return strings.ToLower(claims.Email), nil
}

// PersonIdentity is a person's own Google identity token as the subject of the broker's
// exchange: their kept refresh token spent for a fresh ID token at every trade, so a sign-in
// that a person or Google has ended stops the machine at the next renewal rather than at the
// next hour.
type PersonIdentity struct {
	// Flow is the device-flow client the sign-in was made with, which mints the next ID token.
	Flow *DeviceFlow
	// RefreshToken is what `agent-secrets login` kept beside the box key.
	RefreshToken string
	// Person is whose sign-in it is, for a refusal to name.
	Person string
}

func (p *PersonIdentity) Token(ctx context.Context) (string, string, error) {
	signIn, err := p.Flow.Refresh(ctx, p.RefreshToken)
	if err != nil {
		return "", "", err
	}
	return signIn.IDToken, oauthwire.SubjectTokenTypePerson, nil
}

func (p *PersonIdentity) Describe() string {
	return p.Person + "'s sign-in on this machine"
}
