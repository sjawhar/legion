package oauthstore

import (
	"context"
	"time"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// GetClient returns the provider's one client for oauthwire.ClientID, and oauth2.ErrNotFound for
// any other id: the agent-secrets helper logging a machine in. It is a public client, so it
// authenticates with no secret; the DPoP key its device request names binds every token it is
// issued.
func (s *Store) GetClient(_ context.Context, id string) (oauth2.Client, error) {
	if id != oauthwire.ClientID {
		return nil, oauth2.ErrNotFound
	}
	return &oauth2.DefaultClient{
		ID:         oauthwire.ClientID,
		Public:     true,
		GrantTypes: []string{string(oauth2.GrantTypeDeviceCode), string(oauth2.GrantTypeRefreshToken)},
		Scopes:     []string{oauthwire.Scope},
	}, nil
}

// ClientAssertionJWTValid reports every jti as unknown: the one client is public and never
// authenticates with a client assertion.
func (s *Store) ClientAssertionJWTValid(context.Context, string) error { return nil }

// SetClientAssertionJWT is oauth2.ErrNotFound: no client authenticates with a client assertion.
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
