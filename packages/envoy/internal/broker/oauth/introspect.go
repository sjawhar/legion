package oauth

import (
	"context"
	"net/http"
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
	// Active is whether the token is live: known to the broker, not revoked, not expired.
	// Everything else is set only when it is.
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
// 2.2 asks for. An error is returned only where the broker itself failed — a storage outage —
// because answering that as "not live" would both deny a sound token and hide the outage.
func (p *Provider) Introspect(ctx context.Context, token string) (Introspection, error) {
	_, request, err := p.oauth.IntrospectToken(ctx, token, oauth2.AccessToken, oauthstore.NewExchangeSession())
	if err != nil {
		if oauth2.ErrorToRFC6749Error(err).CodeField >= http.StatusInternalServerError {
			return Introspection{}, err
		}
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
