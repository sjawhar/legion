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

// signInScopes are the scopes a sign-in asks for: an ID token (openid) naming the person.
var signInScopes = []string{gooidc.ScopeOpenID, "email", "profile"}

// Session is what a code exchange or a refresh yields: the verified ID token's claims and the
// refresh token that renews them. A refresh the issuer answers without a new refresh token keeps
// the one that was used.
type Session struct {
	Claims       Claims
	RefreshToken string
}

// The class of every Confirm failure that is the signed-in person's rather than the issuer's or
// the network's. A caller names the class on its own page and never repeats the token.
var (
	ErrNonce           = errors.New("oidc: the ID token's nonce is not the sign-in's")
	ErrEmailUnverified = errors.New("oidc: the ID token's email is not verified")
	ErrHostedDomain    = errors.New("oidc: the ID token names another hosted domain")
	ErrFutureSignIn    = errors.New("oidc: the ID token says the person signed in later than now")
)

// authTimeSkew is how far past now an ID token's auth_time may sit before Confirm refuses it: the
// clock difference between the issuer and this process, never a sign-in that has not happened.
const authTimeSkew = time.Minute

// authTimeClaim is the OpenID Connect claims request (Core 5.5) asking for auth_time in the ID
// token. Google returns auth_time only when a request's claims parameter names it, and honours
// neither max_age nor prompt=login (developers.google.com/identity/openid-connect/reference), so
// this is the one way to learn when the person last authenticated.
const authTimeClaim = `{"id_token":{"auth_time":{"essential":true}}}`

// CodeFlow signs a person in with OpenID Connect's authorization code flow as one confidential
// client of one issuer, renews the sign-in with its refresh token, and revokes a refresh token at
// the issuer's revocation endpoint. Every ID token it returns has been verified against the
// issuer's keys, the client as audience, and its expiry.
//
// A flow NewConfirmFlow built proves one fresh sign-in instead of opening a session
// (ConfirmURL and Confirm): it asks for no refresh token, so it never calls Exchange, Refresh or
// Revoke, and keeps nothing of the person beyond the claims it answers with.
type CodeFlow struct {
	verifier *Verifier
	config   oauth2.Config
	// revocationURL is the revocation_endpoint the issuer's discovery document names; empty when it
	// names none.
	revocationURL string
	// hostedDomain is the Google Workspace domain a domain-mode flow signs people in for; empty in
	// group mode, a Cognito sign-in pool's.
	hostedDomain string
	// offline is whether AuthURL asks Google for a refresh token (access_type=offline with
	// prompt=consent): a sign-in that has to renew itself later, never a confirmation, which is
	// spent at its one exchange.
	offline bool
	http    *http.Client
}

// NewCodeFlow reads the issuer's discovery document now and returns the code flow for the client.
// An issuer whose discovery names no authorization_endpoint or token_endpoint offers no sign-in,
// and is refused. One that names no revocation_endpoint is accepted, and Revoke fails against it.
// hostedDomain is the Google Workspace domain of a domain-mode flow (Google's issuer), which asks
// for offline access and reads the Workspace claims; empty, the flow is a Cognito sign-in pool's
// and does neither.
func NewCodeFlow(ctx context.Context, issuer, clientID, clientSecret, hostedDomain string) (*CodeFlow, error) {
	return newCodeFlow(ctx, issuer, clientID, clientSecret, hostedDomain, hostedDomain != "")
}

// NewConfirmFlow reads the issuer's discovery document now and returns a flow that proves one
// fresh sign-in of one person as that client, rather than opening a session: ConfirmURL sends a
// browser to the issuer asking for a recent authentication and no offline access, and Confirm
// verifies what comes back and keeps nothing. hostedDomain is the Workspace domain every
// confirmed person must be in, which Confirm checks on the verified token.
func NewConfirmFlow(ctx context.Context, issuer, clientID, clientSecret, hostedDomain string) (*CodeFlow, error) {
	return newCodeFlow(ctx, issuer, clientID, clientSecret, hostedDomain, false)
}

func newCodeFlow(ctx context.Context, issuer, clientID, clientSecret, hostedDomain string, offline bool) (*CodeFlow, error) {
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
	return &CodeFlow{
		verifier: newVerifier(provider, issuer, clientID),
		config: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     endpoint,
			Scopes:       signInScopes,
		},
		revocationURL: revocation.Endpoint,
		hostedDomain:  hostedDomain,
		offline:       offline,
		// A redirect is answered as it stands: following a 307 or 308 would re-send the form, and
		// with it a code or a refresh token, to wherever the redirect points.
		http: &http.Client{Timeout: tokenTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

// DiscoverCodeFlow is NewCodeFlow under a deadline on the discovery read, as Discover is for the
// pod-token verifier.
func DiscoverCodeFlow(ctx context.Context, issuer, clientID, clientSecret, hostedDomain string, timeout time.Duration) (*CodeFlow, error) {
	discovery, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return NewCodeFlow(discovery, issuer, clientID, clientSecret, hostedDomain)
}

// DiscoverConfirmFlow is NewConfirmFlow under a deadline on the discovery read, as
// DiscoverCodeFlow is for a sign-in.
func DiscoverConfirmFlow(ctx context.Context, issuer, clientID, clientSecret, hostedDomain string, timeout time.Duration) (*CodeFlow, error) {
	discovery, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return NewConfirmFlow(discovery, issuer, clientID, clientSecret, hostedDomain)
}

// AuthURL is where a browser starts the sign-in: the issuer's authorization endpoint asking for a
// code to be sent to redirectURL with state, and an ID token carrying nonce. A domain-mode
// sign-in also asks Google for offline access with consent, since Google returns a refresh token
// only for access_type=offline and, after the first exchange, only with prompt=consent; and every
// flow that names a domain names it as hd, a hint for Google's account chooser that the callback
// does not rely on.
func (f *CodeFlow) AuthURL(redirectURL, state, nonce string) string {
	config := f.config
	config.RedirectURL = redirectURL
	options := []oauth2.AuthCodeOption{gooidc.Nonce(nonce)}
	if f.offline {
		options = append(options, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
	}
	if f.hostedDomain != "" {
		options = append(options, oauth2.SetAuthURLParam("hd", f.hostedDomain))
	}
	return config.AuthCodeURL(state, options...)
}

// ConfirmURL is where a browser starts a confirmation: the issuer's authorization endpoint asking
// for a code to be sent to redirectURL with state, and an ID token carrying nonce. It asks for
// nothing to keep — no offline access, so the issuer returns no refresh token — and asks, through
// the claims parameter, for auth_time, when the person last authenticated to the issuer, which
// Confirm then reports. It names loginHint as login_hint and the domain as hd, the account the
// person is asked to pick, and prompt=select_account, so the issuer shows its account chooser on
// every confirmation rather than answering for whichever account the browser holds first. None
// of that forces a new authentication: an issuer may answer from a session the browser already
// has, Google among them. Confirm relies on none of it, and checks the email it gets back itself.
func (f *CodeFlow) ConfirmURL(redirectURL, state, nonce, loginHint string) string {
	config := f.config
	config.RedirectURL = redirectURL
	options := []oauth2.AuthCodeOption{
		gooidc.Nonce(nonce),
		oauth2.SetAuthURLParam("prompt", "select_account"),
		oauth2.SetAuthURLParam("claims", authTimeClaim),
	}
	if loginHint != "" {
		options = append(options, oauth2.SetAuthURLParam("login_hint", loginHint))
	}
	if f.hostedDomain != "" {
		options = append(options, oauth2.SetAuthURLParam("hd", f.hostedDomain))
	}
	return config.AuthCodeURL(state, options...)
}

// AuthorizationOrigin is the origin (scheme://host[:port]) of the issuer's authorization endpoint,
// as its discovery document names it: where a browser goes when ConfirmURL or AuthURL sends it
// there. A page whose form answers with a redirect to it has to allow it in its
// content-security policy's form-action, since a browser checks that directive on the redirect
// too, not only on the form's own target.
func (f *CodeFlow) AuthorizationOrigin() string {
	endpoint, err := url.Parse(f.config.Endpoint.AuthURL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return ""
	}
	return endpoint.Scheme + "://" + endpoint.Host
}

// Confirm trades the code the issuer redirected to redirectURL for the person who signed in, and
// keeps nothing: no refresh token is asked for, and the ID token is dropped once its claims are
// read. It is the whole of what a confirmation proves, and every check is here — the ID token
// verified against the issuer's keys, this client as its audience and its own expiry (Verify);
// nonce, the value the page that sent the browser to ConfirmURL bound it to (ErrNonce); a
// verified email (ErrEmailUnverified); the flow's Workspace domain, compared on the token's own hd
// claim rather than on the email's spelling (ErrHostedDomain); and an auth_time that is not later
// than now (ErrFutureSignIn), which an issuer that keeps a sane clock never sends. It asks nothing
// of how recent the sign-in is: Google cannot be made to authenticate the person again, so a
// freshness bound would refuse every person who was already signed in. Claims.AuthTime is zero
// for a token that carries no auth_time, which the caller records as such. The caller compares
// the email with whoever it expected.
func (f *CodeFlow) Confirm(ctx context.Context, redirectURL, code, nonce string, now time.Time) (Claims, error) {
	config := f.config
	config.RedirectURL = redirectURL
	token, err := config.Exchange(f.tokenContext(ctx), code)
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: exchange code: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return Claims{}, errors.New("oidc: the issuer's token response carries no id_token")
	}
	claims, err := f.verifier.Verify(ctx, raw)
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: ID token rejected (%s): %w", Reason(err), err)
	}
	if err := readWorkspaceClaims(raw, &claims); err != nil {
		return Claims{}, fmt.Errorf("oidc: ID token rejected (malformed): %w", err)
	}
	switch {
	case nonce == "" || claims.Nonce != nonce:
		return Claims{}, ErrNonce
	case !claims.EmailVerified || claims.Email == "":
		return Claims{}, ErrEmailUnverified
	case f.hostedDomain != "" && claims.HostedDomain != f.hostedDomain:
		return Claims{}, ErrHostedDomain
	case claims.AuthTime.After(now.Add(authTimeSkew)):
		return Claims{}, ErrFutureSignIn
	}
	return claims, nil
}

// Exchange trades the code the issuer redirected to redirectURL for the signed-in person's ID
// token and refresh token. The ID token must carry nonce, the value the sign-in that sent the
// browser to AuthURL bound it to.
func (f *CodeFlow) Exchange(ctx context.Context, redirectURL, code, nonce string) (Session, error) {
	config := f.config
	config.RedirectURL = redirectURL
	token, err := config.Exchange(f.tokenContext(ctx), code)
	if err != nil {
		return Session{}, fmt.Errorf("oidc: exchange code: %w", err)
	}
	session, err := f.session(ctx, token)
	if err != nil {
		return Session{}, err
	}
	if nonce == "" || session.Claims.Nonce != nonce {
		return Session{}, ErrNonce
	}
	if session.RefreshToken == "" {
		return Session{}, errors.New("oidc: the issuer returned no refresh token")
	}
	return session, nil
}

// Refresh renews a sign-in with its refresh token, returning the person's claims as the issuer
// holds them now. An error is any refresh the issuer did not answer with a verified ID token: a
// revoked or expired refresh token, a person the issuer no longer has, or no answer at all.
func (f *CodeFlow) Refresh(ctx context.Context, refreshToken string) (Session, error) {
	token, err := f.config.TokenSource(f.tokenContext(ctx), &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		return Session{}, fmt.Errorf("oidc: refresh: %w", err)
	}
	return f.session(ctx, token)
}

// Revoke revokes refreshToken at the revocation endpoint the issuer's discovery document names
// (RFC 7009), as the client with HTTP Basic, as Cognito's /oauth2/revoke requires of a client with
// a secret; the issuer ends every token it issued from it. An issuer answers 200 for a token it
// has revoked already or does not know, so that is success too. Any other answer, a redirect
// included, is an error naming the HTTP status and the OAuth error code the body names, if any;
// the error never carries the token.
func (f *CodeFlow) Revoke(ctx context.Context, refreshToken string) error {
	if f.revocationURL == "" {
		return errors.New("oidc: revoke: the issuer names no revocation_endpoint in its discovery document")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.revocationURL, strings.NewReader(url.Values{"token": {refreshToken}}.Encode()))
	if err != nil {
		return fmt.Errorf("oidc: revoke: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// The token endpoint's encoding (oauth2's AuthStyleInHeader): RFC 6749 section 2.3.1.
	request.SetBasicAuth(url.QueryEscape(f.config.ClientID), url.QueryEscape(f.config.ClientSecret))
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
func (f *CodeFlow) session(ctx context.Context, token *oauth2.Token) (Session, error) {
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return Session{}, errors.New("oidc: the issuer's token response carries no id_token")
	}
	claims, err := f.verifier.Verify(ctx, raw)
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

// workspaceClaims are the claims of a Google ID token a domain-mode session or a confirmation
// reads, each kept as the JSON it was written in, so a value of a shape Google does not document
// reads as absent or false rather than failing the sign-in.
type workspaceClaims struct {
	Email         json.RawMessage `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	HostedDomain  json.RawMessage `json:"hd"`
	AuthTime      json.RawMessage `json:"auth_time"`
}

// readWorkspaceClaims reads raw's email, email_verified, hd and auth_time into claims: email and
// hd when each is a JSON string, email_verified as true only for the JSON true or the string
// "true", Google documenting both, and auth_time when it is a JSON number of seconds since the
// epoch, which is what an issuer answers a claims request for it with (CodeFlow.ConfirmURL). raw
// is a token Verify accepted, so its payload is a JSON object.
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
	claims.AuthTime = jsonSeconds(workspace.AuthTime)
	return nil
}

// jsonSeconds is the moment value names as a JSON number of seconds since the epoch, and the zero
// time for any other value or none.
func jsonSeconds(value json.RawMessage) time.Time {
	var seconds int64
	if json.Unmarshal(value, &seconds) != nil {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
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
