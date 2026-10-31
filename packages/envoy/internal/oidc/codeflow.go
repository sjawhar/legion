package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// tokenTimeout bounds one call to the issuer's token endpoint, a code exchange or a refresh, and
// one call to its revocation endpoint.
const tokenTimeout = 10 * time.Second

// tokenClient is the HTTP client every call that carries the client's credentials is made with:
// the code flow's own calls to the token and revocation endpoints, and CheckClient's discovery
// read and token request. A redirect is answered as it stands: following a 307 or 308 would
// re-send the form, and with it a code, a refresh token or the client's secret, to wherever the
// redirect points.
func tokenClient() *http.Client {
	return &http.Client{Timeout: tokenTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// signInScopes are the scopes a sign-in asks for: an ID token (openid) naming the person.
var signInScopes = []string{gooidc.ScopeOpenID, "email", "profile"}

// ErrNonce is an ID token whose nonce is not the one the sign-in that asked for it bound: the
// token belongs to some other sign-in, so the exchange it answers is refused. It is a sentinel
// of its own, as every Verify failure is (Reason), because a caller that reports why a sign-in
// failed cannot otherwise tell this apart from a token that did not verify at all.
var ErrNonce = errors.New("oidc: the ID token's nonce is not the sign-in's")

// Session is what a code exchange or a refresh yields: the verified ID token's claims and the
// refresh token that renews them. A refresh the issuer answers without a new refresh token keeps
// the one that was used.
type Session struct {
	Claims       Claims
	RefreshToken string
}

// CodeFlow signs a person in with OpenID Connect's authorization code flow as one confidential
// client of one issuer, in one of two modes the caller picks by the methods it calls.
// Refresh-keeping mode (AuthURL, Exchange) keeps the sign-in's refresh token, renews the sign-in
// with it (Refresh) and revokes it at the issuer's revocation endpoint (Revoke): Dispatch's
// sign-in. Online mode (OnlineAuthURL, OnlineExchange) signs a person in once with PKCE and keeps
// no refresh token: the secrets broker's approval sign-in. Every ID token it returns has been
// verified against the issuer's keys, the client as audience, and its expiry.
//
// The issuer's discovery document is read when first needed, not at construction, so Dispatch
// starts and serves agents and signed-in people whose confirmation stands while the issuer is
// down: Run reads it from the start in the background, and each method reads it first when it is
// not read yet (Discover's rules bound how often). Until it has been read, AuthURL, Exchange,
// OnlineAuthURL, OnlineExchange, Refresh and Revoke fail with an error matching
// ErrIssuerUnavailable.
type CodeFlow struct {
	discovery *discovery[*codeFlowIssuer]
	// hostedDomain is the Google Workspace domain a domain-mode flow signs people in for; empty in
	// group mode, a Cognito sign-in pool's.
	hostedDomain string
	http         *http.Client
}

// codeFlowIssuer is what a code flow reads from its issuer's discovery document: the verifier of
// the ID tokens it issues to the client, the client's endpoints there, and the revocation_endpoint
// the document names, empty when it names none.
type codeFlowIssuer struct {
	verifier      *Verifier
	config        oauth2.Config
	revocationURL string
}

// NewCodeFlow returns the code flow for the client, and reads nothing: the issuer's discovery
// document is read by Discover, by Run, or by the first call that needs it. hostedDomain is the
// Google Workspace domain of a domain-mode flow (Google's issuer), which asks for offline access
// and reads the Workspace claims; empty, the flow is a Cognito sign-in pool's and does neither.
func NewCodeFlow(issuer, clientID, clientSecret, hostedDomain string) *CodeFlow {
	return &CodeFlow{
		discovery: newDiscovery(issuer, func(ctx context.Context) (*codeFlowIssuer, error) {
			return discoverCodeFlow(ctx, issuer, clientID, clientSecret)
		}),
		hostedDomain: hostedDomain,
		http:         tokenClient(),
	}
}

// discoverCodeFlow reads issuer's discovery document. An issuer whose document names no
// authorization_endpoint or token_endpoint offers no sign-in, and fails discovery. One that names
// no revocation_endpoint is accepted, and Revoke fails against it.
func discoverCodeFlow(ctx context.Context, issuer, clientID, clientSecret string) (*codeFlowIssuer, error) {
	provider, err := gooidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovering issuer %s: %w", issuer, err)
	}
	endpoint := provider.Endpoint()
	if endpoint.AuthURL == "" || endpoint.TokenURL == "" {
		return nil, fmt.Errorf("oidc: issuer %s names no authorization_endpoint or token_endpoint in its discovery document, so it offers no sign-in", issuer)
	}
	var revocation struct {
		Endpoint string `json:"revocation_endpoint"`
	}
	if err := provider.Claims(&revocation); err != nil {
		return nil, fmt.Errorf("oidc: read issuer %s's revocation_endpoint: %w", issuer, err)
	}
	return &codeFlowIssuer{
		verifier: newVerifier(provider, issuer, clientID),
		config: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     endpoint,
			Scopes:       signInScopes,
		},
		revocationURL: revocation.Endpoint,
	}, nil
}

// Discover reads the issuer's discovery document unless it has been read. Concurrent calls share
// one read, bounded by its own ten seconds whatever ctx allows (ctx ends only this caller's wait),
// and for five seconds after a read fails every call fails at once with that failure rather than
// reading again. Its error matches ErrIssuerUnavailable.
func (f *CodeFlow) Discover(ctx context.Context) error {
	_, err := f.discovery.discover(ctx)
	return err
}

// Discovered reports whether the issuer's discovery document has been read.
func (f *CodeFlow) Discovered() bool { return f.discovery.done() }

// Run reads the issuer's discovery document in the background from the process's start until it
// succeeds or ctx ends: at once, then after waits of one second doubling to thirty while reads
// fail, logging a failure at ERROR at most once a minute.
func (f *CodeFlow) Run(ctx context.Context) { f.discovery.run(ctx) }

// AuthURL is where a browser starts the sign-in: the issuer's authorization endpoint asking for a
// code to be sent to redirectURL with state, and an ID token carrying nonce. A domain-mode flow
// also asks Google for offline access with consent, since Google returns a refresh token only for
// access_type=offline and, after the first exchange, only with prompt=consent; and it names the
// domain as hd, a hint for Google's account chooser that the callback does not rely on. It fails
// with ErrIssuerUnavailable while the issuer is not discovered.
func (f *CodeFlow) AuthURL(ctx context.Context, redirectURL, state, nonce string) (string, error) {
	issuer, err := f.discovery.discover(ctx)
	if err != nil {
		return "", err
	}
	config := issuer.config
	config.RedirectURL = redirectURL
	options := []oauth2.AuthCodeOption{gooidc.Nonce(nonce)}
	if f.hostedDomain != "" {
		options = append(options, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"), oauth2.SetAuthURLParam("hd", f.hostedDomain))
	}
	return config.AuthCodeURL(state, options...), nil
}

// Exchange trades the code the issuer redirected to redirectURL for the signed-in person's ID
// token and refresh token. The ID token must carry nonce, the value the sign-in that sent the
// browser to AuthURL bound it to. It fails with ErrIssuerUnavailable while the issuer is not
// discovered.
func (f *CodeFlow) Exchange(ctx context.Context, redirectURL, code, nonce string) (Session, error) {
	issuer, err := f.discovery.discover(ctx)
	if err != nil {
		return Session{}, err
	}
	config := issuer.config
	config.RedirectURL = redirectURL
	token, err := config.Exchange(f.tokenContext(ctx), code)
	if err != nil {
		return Session{}, fmt.Errorf("oidc: exchange code: %w", err)
	}
	session, err := f.session(ctx, issuer, token)
	if err != nil {
		return Session{}, err
	}
	if nonce == "" || session.Claims.Nonce != nonce {
		return Session{}, errors.New("oidc: the ID token's nonce is not the sign-in's")
	}
	if session.RefreshToken == "" {
		return Session{}, errors.New("oidc: the issuer returned no refresh token")
	}
	return session, nil
}

// OnlineSignIn is one sign-in in online mode, the values the caller generated for it and keeps
// until the callback: the same value goes to OnlineAuthURL and then to OnlineExchange.
type OnlineSignIn struct {
	// State is the value the issuer hands back to the callback, which the caller compares.
	State string
	// Nonce is the value the ID token must carry, binding it to this sign-in.
	Nonce string
	// Verifier is the PKCE code verifier (RFC 7636, oauth2.GenerateVerifier): the authorization
	// request carries its S256 challenge and the exchange the verifier, so a code is worth nothing
	// to whoever intercepts it without the verifier.
	Verifier string
	// LoginHint, when set, names the account the issuer's chooser should offer.
	LoginHint string
	// SelectAccount asks the issuer to show its account chooser (prompt=select_account), for a
	// person switching to another account.
	SelectAccount bool
}

// OnlineAuthURL is where a browser starts an online-mode sign-in: the issuer's authorization
// endpoint asking for a code to be sent to redirectURL with signIn's state, an ID token carrying
// its nonce, and PKCE with its verifier's S256 challenge. It asks for no offline access and no
// consent, since an online sign-in keeps no refresh token: the person is signed in once, and signs
// in again when the caller's own session ends. It names the hosted domain as hd, as AuthURL does,
// when the flow has one. It fails with ErrIssuerUnavailable while the issuer is not discovered.
func (f *CodeFlow) OnlineAuthURL(ctx context.Context, redirectURL string, signIn OnlineSignIn) (string, error) {
	issuer, err := f.discovery.discover(ctx)
	if err != nil {
		return "", err
	}
	config := issuer.config
	config.RedirectURL = redirectURL
	options := []oauth2.AuthCodeOption{gooidc.Nonce(signIn.Nonce), oauth2.S256ChallengeOption(signIn.Verifier)}
	if f.hostedDomain != "" {
		options = append(options, oauth2.SetAuthURLParam("hd", f.hostedDomain))
	}
	if signIn.LoginHint != "" {
		options = append(options, oauth2.SetAuthURLParam("login_hint", signIn.LoginHint))
	}
	if signIn.SelectAccount {
		options = append(options, oauth2.SetAuthURLParam("prompt", "select_account"))
	}
	return config.AuthCodeURL(signIn.State, options...), nil
}

// OnlineExchange trades the code the issuer redirected to redirectURL, with signIn's PKCE
// verifier, for the signed-in person's verified ID token claims (the Workspace claims too, when
// the flow has a hosted domain). The ID token must carry signIn's nonce, and fails with ErrNonce
// when it carries another. A refresh token the issuer returns anyway is dropped, never returned:
// an online sign-in keeps none. It fails with ErrIssuerUnavailable while the issuer is not
// discovered.
func (f *CodeFlow) OnlineExchange(ctx context.Context, redirectURL, code string, signIn OnlineSignIn) (Claims, error) {
	if signIn.Nonce == "" || signIn.Verifier == "" {
		return Claims{}, errors.New("oidc: an online sign-in needs its nonce and PKCE verifier")
	}
	issuer, err := f.discovery.discover(ctx)
	if err != nil {
		return Claims{}, err
	}
	config := issuer.config
	config.RedirectURL = redirectURL
	token, err := config.Exchange(f.tokenContext(ctx), code, oauth2.VerifierOption(signIn.Verifier))
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: exchange code: %w", err)
	}
	session, err := f.session(ctx, issuer, token)
	if err != nil {
		return Claims{}, err
	}
	if session.Claims.Nonce != signIn.Nonce {
		return Claims{}, ErrNonce
	}
	return session.Claims, nil
}

// Refresh renews a sign-in with its refresh token, returning the person's claims as the issuer
// holds them now. Its error matches ErrGrantRefused when the token endpoint answered 400 or 401
// with the OAuth error invalid_grant (RFC 6749 section 5.2): the refresh token has expired, been
// revoked, or is not this client's, so the sign-in is over. Every other failure matches
// ErrIssuerUnavailable and says nothing about the sign-in: the issuer not discovered, no answer or
// a timeout, a 5xx or a 429, any other OAuth error (invalid_client among them: the client's own
// configuration, not the person), or an answer without an ID token that verifies.
func (f *CodeFlow) Refresh(ctx context.Context, refreshToken string) (Session, error) {
	issuer, err := f.discovery.discover(ctx)
	if err != nil {
		return Session{}, err
	}
	token, err := issuer.config.TokenSource(f.tokenContext(ctx), &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		if grantRefused(err) {
			return Session{}, fmt.Errorf("oidc: refresh: %w: %w", ErrGrantRefused, err)
		}
		return Session{}, fmt.Errorf("oidc: refresh: %w: %w", ErrIssuerUnavailable, err)
	}
	session, err := f.session(ctx, issuer, token)
	if err != nil {
		return Session{}, fmt.Errorf("oidc: refresh: %w: %w", ErrIssuerUnavailable, err)
	}
	return session, nil
}

// grantRefused reports whether err is the token endpoint's 400 or 401 naming invalid_grant.
func grantRefused(err error) bool {
	var retrieve *oauth2.RetrieveError
	if !errors.As(err, &retrieve) || retrieve.ErrorCode != "invalid_grant" || retrieve.Response == nil {
		return false
	}
	return retrieve.Response.StatusCode == http.StatusBadRequest || retrieve.Response.StatusCode == http.StatusUnauthorized
}

// Revoke revokes refreshToken at the revocation endpoint the issuer's discovery document names
// (RFC 7009), as the client with HTTP Basic, as Cognito's /oauth2/revoke requires of a client with
// a secret; the issuer ends every token it issued from it. An issuer answers 200 for a token it
// has revoked already or does not know, so that is success too. Any other answer, a redirect
// included, is an error naming the HTTP status and the OAuth error code the body names, if any;
// the error never carries the token.
func (f *CodeFlow) Revoke(ctx context.Context, refreshToken string) error {
	issuer, err := f.discovery.discover(ctx)
	if err != nil {
		return err
	}
	if issuer.revocationURL == "" {
		return errors.New("oidc: revoke: the issuer names no revocation_endpoint in its discovery document")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer.revocationURL, strings.NewReader(url.Values{"token": {refreshToken}}.Encode()))
	if err != nil {
		return fmt.Errorf("oidc: revoke: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// The token endpoint's encoding (oauth2's AuthStyleInHeader): RFC 6749 section 2.3.1.
	request.SetBasicAuth(url.QueryEscape(issuer.config.ClientID), url.QueryEscape(issuer.config.ClientSecret))
	response, err := f.http.Do(request)
	if err != nil {
		return fmt.Errorf("oidc: revoke: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	var body struct {
		Error string `json:"error"`
	}
	// A body that is not an OAuth error (a proxy's page) names no code; the status still stands.
	if json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&body) != nil || body.Error == "" {
		return fmt.Errorf("oidc: revoke: the issuer answered HTTP %d", response.StatusCode)
	}
	return fmt.Errorf("oidc: revoke: the issuer answered HTTP %d %s", response.StatusCode, body.Error)
}

// session verifies the ID token of a token-endpoint answer and returns its claims with the refresh
// token. A domain-mode flow also reads the Workspace claims from the verified token (Claims.Email,
// EmailVerified, HostedDomain); Verify, which every other token kind goes through, never does.
func (f *CodeFlow) session(ctx context.Context, issuer *codeFlowIssuer, token *oauth2.Token) (Session, error) {
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return Session{}, errors.New("oidc: the issuer's token response carries no id_token")
	}
	claims, err := issuer.verifier.Verify(ctx, raw)
	if err != nil {
		return Session{}, fmt.Errorf("oidc: ID token rejected (%s): %w", Reason(err), err)
	}
	if f.hostedDomain != "" {
		if err := readWorkspaceClaims(raw, &claims); err != nil {
			return Session{}, fmt.Errorf("oidc: ID token rejected (malformed): %w", err)
		}
	}
	return Session{Claims: claims, RefreshToken: token.RefreshToken}, nil
}

// workspaceClaims are the claims of a Google ID token a domain-mode session reads, each kept as the
// JSON it was written in, so a value of a shape Google does not document reads as absent or false
// rather than failing the sign-in.
type workspaceClaims struct {
	Email         json.RawMessage `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	HostedDomain  json.RawMessage `json:"hd"`
}

// readWorkspaceClaims reads raw's email, email_verified and hd into claims: email and hd when each
// is a JSON string, and email_verified as true only for the JSON true or the string "true", Google
// documenting both. raw is a token Verify accepted, so its payload is a JSON object.
func readWorkspaceClaims(raw string, claims *Claims) error {
	payload, err := tokenPayload(raw)
	if err != nil {
		return err
	}
	var workspace workspaceClaims
	if err := json.Unmarshal(payload, &workspace); err != nil {
		return fmt.Errorf("payload is not a claim set: %w", err)
	}
	claims.Email = jsonString(workspace.Email)
	claims.HostedDomain = jsonString(workspace.HostedDomain)
	claims.EmailVerified = string(workspace.EmailVerified) == "true" || jsonString(workspace.EmailVerified) == "true"
	return nil
}

// jsonString is value when it is a JSON string, and empty for any other value or none.
func jsonString(value json.RawMessage) string {
	var text string
	if json.Unmarshal(value, &text) != nil {
		return ""
	}
	return text
}

// tokenContext hands oauth2 the bounded client its token-endpoint calls use; its default client has
// no timeout.
func (f *CodeFlow) tokenContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, f.http)
}
