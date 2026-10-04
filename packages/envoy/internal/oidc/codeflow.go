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

// CodeFlow signs a person in with OpenID Connect's authorization code flow as one confidential
// client of one issuer, renews the sign-in with its refresh token, and revokes a refresh token at
// the issuer's revocation endpoint. Every ID token it returns has been verified against the
// issuer's keys, the client as audience, and its expiry.
type CodeFlow struct {
	verifier *Verifier
	config   oauth2.Config
	// revocationURL is the revocation_endpoint the issuer's discovery document names; empty when it
	// names none.
	revocationURL string
	http          *http.Client
}

// NewCodeFlow reads the issuer's discovery document now and returns the code flow for the client.
// An issuer whose discovery names no authorization_endpoint or token_endpoint offers no sign-in,
// and is refused. One that names no revocation_endpoint is accepted, and Revoke fails against it.
func NewCodeFlow(ctx context.Context, issuer, clientID, clientSecret string) (*CodeFlow, error) {
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
		// A redirect is answered as it stands: following a 307 or 308 would re-send the form, and
		// with it a code or a refresh token, to wherever the redirect points.
		http: &http.Client{Timeout: tokenTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

// DiscoverCodeFlow is NewCodeFlow under a deadline on the discovery read, as Discover is for the
// pod-token verifier.
func DiscoverCodeFlow(ctx context.Context, issuer, clientID, clientSecret string, timeout time.Duration) (*CodeFlow, error) {
	discovery, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return NewCodeFlow(discovery, issuer, clientID, clientSecret)
}

// AuthURL is where a browser starts the sign-in: the issuer's authorization endpoint asking for a
// code to be sent to redirectURL with state, and an ID token carrying nonce.
func (f *CodeFlow) AuthURL(redirectURL, state, nonce string) string {
	config := f.config
	config.RedirectURL = redirectURL
	return config.AuthCodeURL(state, gooidc.Nonce(nonce))
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
		return Session{}, errors.New("oidc: the ID token's nonce is not the sign-in's")
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

func (f *CodeFlow) session(ctx context.Context, token *oauth2.Token) (Session, error) {
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return Session{}, errors.New("oidc: the issuer's token response carries no id_token")
	}
	claims, err := f.verifier.Verify(ctx, raw)
	if err != nil {
		return Session{}, fmt.Errorf("oidc: ID token rejected (%s): %w", Reason(err), err)
	}
	return Session{Claims: claims, RefreshToken: token.RefreshToken}, nil
}

// tokenContext hands oauth2 the bounded client its token-endpoint calls use; its default client has
// no timeout.
func (f *CodeFlow) tokenContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, f.http)
}
