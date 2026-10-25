package oauth

import (
	"context"
	"errors"
	"strings"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/broker/oauthstore"
)

// Introspection is what the broker knows about one of its access tokens, the RFC 7662 answer
// Dispatch reads a token back through. This provider's tokens are opaque, so everything here
// comes from the row the token is stored in rather than from the token's own bytes, and a token
// the broker has revoked or let expire is simply not live.
//
// The JSON names are RFC 7662's where it has one and the broker's own where it does not; the
// route serialises this struct and adds no field of its own.
type Introspection struct {
	// Active is whether the token is live: one of the broker's access tokens, not revoked, not
	// expired. Everything else is set only when it is.
	Active bool `json:"active"`
	// Session is the session the token's writes are recorded as.
	Session string `json:"session,omitempty"`
	// Person is the person the session acts for.
	Person string `json:"person,omitempty"`
	// ProjectScope is what Project means: ProjectScopeNamed or ProjectScopePerson. A machine
	// login's own token, which no exchange decided for, carries neither this nor Project.
	ProjectScope string `json:"project_scope,omitempty"`
	// Project is the one project key a ProjectScopeNamed token may write.
	Project string `json:"project,omitempty"`
	// Scope is the OAuth scope the token was granted, space-separated as RFC 7662 spells it.
	Scope string `json:"scope,omitempty"`
	// ExpiresAt is when the token expires, in seconds since the epoch (RFC 7662's 'exp'), so a
	// caller that remembers an affirmative answer can keep it no longer than the token lives.
	ExpiresAt int64 `json:"exp,omitempty"`
	// ClientID is the OAuth client the token was issued to.
	ClientID string `json:"client_id,omitempty"`
	// Thumbprint is the JWK thumbprint of the key the token is DPoP-bound to (RFC 9449's 'jkt').
	// It is what makes a stolen token useless: whoever reads this answer checks the caller's
	// proof against it.
	Thumbprint string `json:"jkt,omitempty"`
}

// Introspect is what the broker knows about token, for the introspection route to answer with.
//
// It is built on the library's IntrospectToken rather than NewIntrospectionRequest: that one
// authenticates its caller as an OAuth client, and this route's caller is Dispatch, authenticated
// by its own Kubernetes service-account token before the provider is reached.
//
// A token the broker does not know, one it has revoked and one that has expired are all answered
// as simply not live, with no error and nothing else disclosed, which is what RFC 7662 Section
// 2.2 asks for. So is any token that is not an access token, a machine login's refresh token
// among them: the answer carries no token type, so a refresh token answered live — it outlives
// the access token issued beside it — would pass for a credential no request can present. The
// library takes oauth2.AccessToken only as a hint and falls through to the refresh-token store
// when the access lookup misses, so the kind it reports finding is checked here rather than
// trusted to the hint. An error is returned only where the broker itself failed — a storage
// outage, or a row it could not decode — because answering that as "not live" would both deny a
// sound token and hide the outage; notLive is how the two are told apart.
func (p *Provider) Introspect(ctx context.Context, token string) (Introspection, error) {
	kind, request, err := p.oauth.IntrospectToken(ctx, token, oauth2.AccessToken, oauthstore.NewExchangeSession())
	if err != nil {
		if notLive(err) {
			return Introspection{}, nil
		}
		return Introspection{}, err
	}
	if kind != oauth2.AccessToken {
		return Introspection{}, nil
	}
	answer := Introspection{Active: true, Scope: strings.Join(request.GetGrantedScopes(), " ")}
	if client := request.GetClient(); client != nil {
		answer.ClientID = client.GetID()
	}
	session, ok := request.GetSession().(*oauthstore.ExchangeSession)
	if !ok || session.DefaultSession == nil {
		return answer, nil
	}
	answer.Thumbprint = session.GetDPoPJWKThumbprint()
	if expires := session.GetExpiresAt(oauth2.AccessToken); !expires.IsZero() {
		answer.ExpiresAt = expires.Unix()
	}
	answer.Session, _ = session.Extra[ClaimSession].(string)
	answer.Person, _ = session.Extra[ClaimPerson].(string)
	answer.ProjectScope, _ = session.Extra[ClaimProjectScope].(string)
	answer.Project, _ = session.Extra[ClaimProject].(string)
	return answer, nil
}

// notLiveCauses are the library's own sentinels for a token it found wanting: a string with no
// signature to look up (oauth2.ErrNotFound, which the library wraps itself), a row the store
// answers revoked (oauth2.ErrInactiveToken), a token past its expiry, and one whose bytes do not
// check against the secret. errors.Is matches an *oauth2.RFC6749Error on its error code and
// status, and each of these has its own.
var notLiveCauses = []error{
	oauth2.ErrNotFound, oauth2.ErrInactiveToken, oauth2.ErrTokenExpired,
	oauth2.ErrInvalidTokenFormat, oauth2.ErrTokenSignatureMismatch,
}

// notLive reports whether err, an IntrospectToken failure, is the library deciding the token is
// not live, rather than the broker failing to read it.
//
// The library's status cannot tell them apart: it wraps every failed read of a token's row, a lost
// connection or a row that does not decode alike, in oauth2.ErrRequestUnauthorized, a 401, the
// same error it answers a revoked token with. So the failure is told apart by its cause. It is not
// live when the chain holds one of notLiveCauses, or when it is the library's own
// ErrRequestUnauthorized wrapping nothing at all: the store answers a row it does not have
// oauth2.ErrNotFound and nothing else does (oauthstore's hydrate keeps the sentinel out of a row
// it cannot decode), the library takes that as "not mine" from every introspector, and
// IntrospectToken then answers that bare error. Anything else wraps an error of the store's own —
// a read that failed, or a row it could not decode — and is the broker's failure, never a
// confident "not live": a cause this does not recognise is answered as an outage rather than as a
// negative.
func notLive(err error) bool {
	for _, cause := range notLiveCauses {
		if errors.Is(err, cause) {
			return true
		}
	}
	return errors.Is(err, oauth2.ErrRequestUnauthorized) && errors.Unwrap(oauth2.ErrorToRFC6749Error(err)) == nil
}
