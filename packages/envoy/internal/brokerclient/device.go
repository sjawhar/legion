// packages/envoy/internal/brokerclient/device.go

package brokerclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// GoogleIssuer is the issuer a person signs in at when nothing names another: Google's own,
// whose OpenID configuration names its device authorization and token endpoints.
const GoogleIssuer = "https://accounts.google.com"

// signInScopes are the only scopes a sign-in asks for: who the person is, and their address.
// Nothing here reads a calendar, a mailbox or a drive, and a client that asked for more would be
// a client a person has to trust with more.
var signInScopes = []string{"openid", "email"}

// Provider is the two endpoints of the identity provider a person signs in at.
type Provider struct {
	DeviceAuthURL string
	TokenURL      string
}

// Discover reads issuer's OpenID configuration (RFC 8414 / OpenID Connect Discovery) for the
// device authorization and token endpoints. Google's own document names both, and so does the
// repository's test issuer, so one variable names a provider and the client finds the rest.
func Discover(ctx context.Context, issuer string, client *http.Client) (Provider, error) {
	if client == nil {
		client = http.DefaultClient
	}
	documentURL := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, documentURL, nil)
	if err != nil {
		return Provider{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Provider{}, fmt.Errorf("read the sign-in provider's configuration at %s: %w", documentURL, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Provider{}, err
	}
	if resp.StatusCode/100 != 2 {
		return Provider{}, fmt.Errorf("the sign-in provider's configuration at %s answered HTTP %d", documentURL, resp.StatusCode)
	}
	var document struct {
		DeviceAuthURL string `json:"device_authorization_endpoint"`
		TokenURL      string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return Provider{}, fmt.Errorf("the sign-in provider's configuration at %s is not a JSON object: %w", documentURL, err)
	}
	if document.DeviceAuthURL == "" || document.TokenURL == "" {
		return Provider{}, fmt.Errorf("the sign-in provider at %s serves no device authorization grant: its configuration names %q and %q",
			issuer, document.DeviceAuthURL, document.TokenURL)
	}
	return Provider{DeviceAuthURL: document.DeviceAuthURL, TokenURL: document.TokenURL}, nil
}

// SignIn is Google's device authorization grant (RFC 8628) as `agent-secrets login` runs it: the
// machine asks for a code, a person types that code in on a device of their own, and the machine
// is handed the person's identity token and a refresh token for the next one.
type SignIn struct {
	Provider     Provider
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
}

// PersonSignIn is what a sign-in leaves behind: the person who signed in, read from the identity
// token's `email` claim, the identity token itself, and the refresh token the next one is minted
// with.
type PersonSignIn struct {
	Person       string
	IDToken      string
	RefreshToken string
}

// config is the OAuth client of the sign-in, which x/oauth2 runs both halves of the device grant
// with.
func (s *SignIn) config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     s.ClientID,
		ClientSecret: s.ClientSecret,
		Scopes:       signInScopes,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: s.Provider.DeviceAuthURL,
			TokenURL:      s.Provider.TokenURL,
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

func (s *SignIn) client() *http.Client {
	if s.HTTP == nil {
		return http.DefaultClient
	}
	return s.HTTP
}

// Run asks for a device code, hands it to tell — which prints the code and the page to type it
// in at — and then polls until the person decides, at the interval the provider named. It
// returns the sign-in, or the provider's refusal: a person who denied it, or a code nobody used
// in time.
func (s *SignIn) Run(ctx context.Context, tell func(*oauth2.DeviceAuthResponse)) (*PersonSignIn, error) {
	cfg := s.config()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.client())
	auth, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("ask %s for a device code: %w", s.Provider.DeviceAuthURL, err)
	}
	if auth.UserCode == "" || auth.VerificationURI == "" {
		return nil, fmt.Errorf("%s answered no user code or no verification page", s.Provider.DeviceAuthURL)
	}
	tell(auth)
	token, err := cfg.DeviceAccessToken(ctx, auth)
	if err != nil {
		return nil, fmt.Errorf("wait for the sign-in to be approved: %w", err)
	}
	idToken, _ := token.Extra("id_token").(string)
	if idToken == "" {
		return nil, errors.New("the sign-in answered no id_token: the client must be one whose tokens carry the person's identity")
	}
	person, err := PersonOf(idToken)
	if err != nil {
		return nil, err
	}
	if token.RefreshToken == "" {
		return nil, errors.New("the sign-in answered no refresh_token: without one this machine could not mint the person's next identity token, and would ask them to sign in again within the hour")
	}
	return &PersonSignIn{Person: person, IDToken: idToken, RefreshToken: token.RefreshToken}, nil
}

// PersonOf is the `email` claim of an identity token, the address the broker knows the person
// by. Nothing here verifies the token: the broker checks the signature, the issuer, the
// audience, the expiry and whether the provider holds the address verified, and this read is
// only so a machine can say whose sign-in it is keeping.
func PersonOf(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", errors.New("the identity token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("read the identity token's claims: %w", err)
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("read the identity token's claims: %w", err)
	}
	if claims.Email == "" {
		return "", errors.New("the identity token carries no email claim, so it names no person")
	}
	return strings.ToLower(claims.Email), nil
}

// PersonSource is a signed-in person's own Google identity token, minted again for every trade
// from the refresh token the sign-in left behind. It is the subject of the person half of the
// exchange, where the machine's own token is the actor.
//
// The mint is x/oauth2's refresh, which is the flow it implements: a token source over the
// refresh token, asked for a token it always finds spent, so every call is a fresh request whose
// answer carries a fresh identity token.
type PersonSource struct {
	Provider     Provider
	ClientID     string
	ClientSecret string
	RefreshToken string
	// Person is who the record says signed in, for the messages a refusal prints.
	Person string
	HTTP   *http.Client
}

func (s *PersonSource) TokenType() string { return oauthwire.SubjectTokenTypePerson }

func (s *PersonSource) Name() string { return s.Person + "'s sign-in on this machine" }

// Token mints the person's next identity token. A refresh the provider refuses (the person
// revoked the sign-in at Google, or it lapsed unused) is returned as it came: the person signs
// in again, and nothing here does it for them.
func (s *PersonSource) Token(ctx context.Context) (string, error) {
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	cfg := &oauth2.Config{
		ClientID:     s.ClientID,
		ClientSecret: s.ClientSecret,
		Scopes:       signInScopes,
		Endpoint:     oauth2.Endpoint{TokenURL: s.Provider.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
	}
	// An expiry in the past is what makes x/oauth2 spend the refresh token rather than hand back
	// the access token it was given, which is not the token this source is for.
	spent := &oauth2.Token{RefreshToken: s.RefreshToken, Expiry: time.Unix(1, 0)}
	token, err := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, client), spent).Token()
	if err != nil {
		return "", fmt.Errorf("mint %s's identity token at %s: %w", s.Person, s.Provider.TokenURL, err)
	}
	idToken, _ := token.Extra("id_token").(string)
	if idToken == "" {
		return "", fmt.Errorf("%s answered no id_token for %s's sign-in", s.Provider.TokenURL, s.Person)
	}
	return idToken, nil
}
