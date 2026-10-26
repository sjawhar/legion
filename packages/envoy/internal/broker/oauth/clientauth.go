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

// exchangeClientAuth is the broker's client authentication at the token endpoint. It is the
// library's own strategy in every case but one: the exchange client, at the token-exchange grant,
// carrying no client credential at all.
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

// AuthenticateClient answers the exchange client for a credential-less token exchange and hands
// every other request to the library.
func (s *exchangeClientAuth) AuthenticateClient(ctx context.Context, r *http.Request, form url.Values, strategy oauth2.EndpointClientAuthStrategy) (oauth2.Client, string, error) {
	if !credentialLessExchange(r, form) {
		return s.delegate.AuthenticateClient(ctx, r, form, strategy)
	}
	client, err := s.store.GetClient(ctx, oauthwire.ExchangeClientID)
	if err != nil {
		return nil, "", oauth2.ErrInvalidClient.WithWrap(err).WithDebugError(err)
	}
	return client, authMethodNone, nil
}

// credentialLessExchange reports whether this is the one request the broker authenticates itself:
// the exchange client, exactly the token-exchange grant, and no credential of any kind — no
// Authorization header, no client secret in the form, and no client assertion. Any Authorization
// header at all disqualifies it, rather than only a Basic one: a request that tried to
// authenticate and got it wrong must be refused by the library, not waved through here.
func credentialLessExchange(r *http.Request, form url.Values) bool {
	if r == nil || r.Header.Get("Authorization") != "" {
		return false
	}
	if form.Get("client_id") != oauthwire.ExchangeClientID {
		return false
	}
	if form.Get("grant_type") != string(oauth2.GrantTypeTokenExchange) {
		return false
	}
	return form.Get("client_secret") == "" && form.Get("client_assertion") == "" && form.Get("client_assertion_type") == ""
}
