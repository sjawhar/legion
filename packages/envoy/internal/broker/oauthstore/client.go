package oauthstore

import (
	"context"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc8693"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// The client type every registration here is, which the token exchange reads through
// rfc8693.Client: which subject, actor and requested token types a client may name, and which
// client may exchange a token issued to it. A plain *oauth2.DefaultClient answers none of those.
var _ rfc8693.Client = (*oauth2.DefaultRegisteredClient)(nil)

// actorTokenTypeNone is the one actor token type the exchange client is registered for, and it
// is a name no token type is registered under, so every actor token a request carries is
// refused. A registered client says which actor token types it may present by listing them, and
// the library reads an empty list as no restriction at all, so this is how a client says none.
const actorTokenTypeNone = "urn:envoy:params:oauth:token-type:no-actor-token" //nolint:gosec // a token type's name, not a token

// authMethodNone is the 'token_endpoint_auth_method' of a public client, which presents no
// credentials of its own. A registration leaving it unset is read as client_secret_basic, the
// default OpenID Connect Dynamic Client Registration states, which a public client could never
// satisfy. The library spells it in a package its users cannot import.
const authMethodNone = "none"

// GetClient returns one of the provider's two clients, and oauth2.ErrNotFound for any other id:
// the agent-secrets helper logging a machine in, and the caller exchanging a subject token for a
// Dispatch access token.
func (s *Store) GetClient(_ context.Context, id string) (oauth2.Client, error) {
	switch id {
	case oauthwire.ClientID:
		return machineClient(), nil
	case oauthwire.ExchangeClientID:
		return exchangeClient(), nil
	}
	return nil, oauth2.ErrNotFound
}

// machineClient is the agent-secrets helper logging a machine in. It is a public client, so it
// authenticates with no secret; the DPoP key its device request names binds every token it is
// issued.
//
// Its tokens are the one kind this provider issues that may itself be exchanged: a dev box
// session presents the access token its machine login holds, and the library asks the client the
// token was issued to whether that is permitted. It permits the exchange client and no other, so
// no caller holding one of these tokens can exchange it anywhere else.
func machineClient() *oauth2.DefaultRegisteredClient {
	return &oauth2.DefaultRegisteredClient{
		DefaultClient: &oauth2.DefaultClient{
			ID:         oauthwire.ClientID,
			Public:     true,
			GrantTypes: []string{string(oauth2.GrantTypeDeviceCode), string(oauth2.GrantTypeRefreshToken)},
			Scopes:     []string{oauthwire.Scope},
		},
		TokenEndpointAuthMethod:         authMethodNone,
		TokenExchangePermittedClientIDs: []string{oauthwire.ExchangeClientID},
	}
}

// exchangeClient is the client every exchanging caller presents: a pod, a workflow run, and a dev
// box session. It is registered confidential because the library refuses a public client the
// token-exchange grant, but it presents no client credential at all: the provider's own
// authentication strategy answers it for a credential-less exchange, and the subject token and
// the DPoP proof are what the request is judged on. A secret here would be one more shared
// secret in every pod, which is what this exchange exists to remove.
//
// It holds the token-exchange grant and no other, may present exactly the three kinds of subject
// token this broker accepts, and may ask for an access token and nothing else: a caller that
// asked for a JSON Web Token would be asking the broker to sign a claim set of its own, which it
// does not do here. It permits no actor token — actorTokenTypeNone says how — so no caller may
// exchange one identity's token while acting as another.
func exchangeClient() *oauth2.DefaultRegisteredClient {
	return &oauth2.DefaultRegisteredClient{
		DefaultClient: &oauth2.DefaultClient{
			ID:         oauthwire.ExchangeClientID,
			GrantTypes: []string{string(oauth2.GrantTypeTokenExchange)},
			Scopes:     []string{oauthwire.Scope},
		},
		TokenExchangeSubjectTokenTypes: []string{
			oauthwire.SubjectTokenTypeKubernetes,
			oauthwire.SubjectTokenTypeGitHub,
			oauthwire.SubjectTokenTypeAccessToken,
		},
		TokenExchangeRequestTokenTypes: []string{oauthwire.SubjectTokenTypeAccessToken},
		TokenExchangeActorTokenTypes:   []string{actorTokenTypeNone},
	}
}

// ClientAssertionJWTValid reports every jti as unknown: no registered client authenticates with a
// client assertion, the machine-login client being public and the exchange client authenticating
// with its secret.
func (s *Store) ClientAssertionJWTValid(context.Context, string) error { return nil }

// SetClientAssertionJWT is oauth2.ErrNotFound: no registered client authenticates with a client
// assertion, so there is never one to remember.
func (s *Store) SetClientAssertionJWT(context.Context, string, time.Time) error {
	return oauth2.ErrNotFound
}

// CreateClient is oauth2.ErrNotFound: the provider registers no clients.
func (s *Store) CreateClient(context.Context, oauth2.Client) error { return oauth2.ErrNotFound }

// UpdateClient is oauth2.ErrNotFound: the provider registers no clients.
func (s *Store) UpdateClient(context.Context, string, oauth2.Client) error {
	return oauth2.ErrNotFound
}

// DeleteClient is oauth2.ErrNotFound: the provider registers no clients.
func (s *Store) DeleteClient(context.Context, string) error { return oauth2.ErrNotFound }
