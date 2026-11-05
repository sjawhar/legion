package oauth

import (
	"context"
	"net/http"
	"net/url"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// authMethodNone is the 'token_endpoint_auth_method' of a request carrying no client credential.
// The library spells it in a package its users cannot import.
const authMethodNone = "none"

// exchangeClientAuth is the broker's client authentication. It is the library's own strategy in
// every case but two: the exchange client, at the token-exchange grant and at the revocation
// endpoint, carrying no client credential at all.
//
// The library offers that caller no path. It refuses a public client the token-exchange grant
// (handler/rfc8693/flow_token_exchange.go) and refuses the 'none' method to a confidential one
// (client_authentication_strategy.go), so a caller holding no secret cannot authenticate however
// its client is registered. RFC 8693 Section 2.1 leaves the choice to the deployment: "The
// supported methods of client authentication and whether or not to allow unauthenticated or
// unidentified clients are deployment decisions that are at the discretion of the authorization
// server." Shipping a client secret to every pod is the shared secret this whole change removes,
// and keying client authentication instead would lock out the continuous-integration caller,
// which has no enrolled key.
//
// So the exchange client is answered as authenticated when, and only when, it presents nothing:
// the subject token is the credential the request is judged on, and the DPoP proof the broker
// enforces binds the issued token to the caller's own key. Every other request — another client
// id presenting nothing, this client on any other grant, this client presenting a credential of
// any kind, right or wrong — goes to the library and is answered as it always was.
type exchangeClientAuth struct {
	// store answers the exchange client when this strategy accepts a credential-less exchange.
	store *oauthstore.Store
	// delegate is the library's own strategy, which answers every other request.
	delegate oauth2.ClientAuthenticationStrategy
}

// newExchangeClientAuth is the strategy on store, delegating to the library's default built the
// way oauth2.Fosite builds it when a provider configures none.
func newExchangeClientAuth(store *oauthstore.Store, config *oauth2.Config) *exchangeClientAuth {
	return &exchangeClientAuth{
		store:    store,
		delegate: &oauth2.DefaultClientAuthenticationStrategy{Store: store, Config: config},
	}
}

// AuthenticateClient answers the exchange client for a credential-less token exchange and for a
// credential-less revocation of one of its own tokens, and hands every other request to the
// library.
func (s *exchangeClientAuth) AuthenticateClient(ctx context.Context, r *http.Request, form url.Values, strategy oauth2.EndpointClientAuthStrategy) (oauth2.Client, string, error) {
	if !credentialLessExchange(r, form) && !credentialLessRevocation(r, form, strategy) {
		return s.delegate.AuthenticateClient(ctx, r, form, strategy)
	}
	client, err := s.store.GetClient(ctx, oauthwire.ExchangeClientID)
	if err != nil {
		return nil, "", oauth2.ErrInvalidClient.WithWrap(err).WithDebugError(err)
	}
	return client, authMethodNone, nil
}

// credentialLessExchange reports whether this is the one token request the broker authenticates
// itself: the exchange client, exactly the token-exchange grant, and no credential of any kind.
func credentialLessExchange(r *http.Request, form url.Values) bool {
	return form.Get("grant_type") == string(oauth2.GrantTypeTokenExchange) && namesTheExchangeClientWithoutCredential(r, form)
}

// credentialLessRevocation reports whether this is a revocation request naming the exchange
// client and presenting no credential of any kind.
//
// The library offers this caller no path here either, for the same reason it offers none at the
// token endpoint: the exchange client is registered confidential so that the library will grant
// it the token exchange at all, and a confidential client is refused the 'none' method
// (client_authentication_strategy.go), so a caller that holds no secret cannot authenticate as
// it. RFC 7009 Section 2.1 requires the client to be authenticated, which is what this answers,
// but what the request is really judged on is not a client credential at all: the route the
// provider serves this from is an accountAuth route, so the broker has already verified the
// caller's own Kubernetes ServiceAccount token and checked that its listed name carries the
// power to end one of the broker's tokens (internal/broker/api, relaysDecisions) — and handed
// the provider the request without that header, so what reaches here carries no Authorization of
// any kind (withoutCallerToken). The client id here only says which client's tokens the request
// may end, and the library still refuses a token issued to any other client
// (ErrUnauthorizedClient).
//
// The endpoint is read off the strategy the library hands this call rather than guessed from the
// form, because that value is what the library itself used to decide which endpoint's rules
// apply: a form naming no grant type reaches the token endpoint too.
func credentialLessRevocation(r *http.Request, form url.Values, strategy oauth2.EndpointClientAuthStrategy) bool {
	if _, ok := strategy.(*oauth2.RevocationEndpointClientAuthStrategy); !ok {
		return false
	}
	return namesTheExchangeClientWithoutCredential(r, form)
}

// namesTheExchangeClientWithoutCredential reports whether the request names the exchange client
// in its form and carries no credential of any kind — no Authorization header, no client secret
// in the form, and no client assertion. Any Authorization header at all disqualifies it, rather
// than only a Basic one: a request that tried to authenticate and got it wrong must be refused by
// the library, not waved through here.
func namesTheExchangeClientWithoutCredential(r *http.Request, form url.Values) bool {
	if r == nil || r.Header.Get("Authorization") != "" {
		return false
	}
	if form.Get("client_id") != oauthwire.ExchangeClientID {
		return false
	}
	return form.Get("client_secret") == "" && form.Get("client_assertion") == "" && form.Get("client_assertion_type") == ""
}
