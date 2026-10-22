package oauthstore

import (
	"context"
	"time"

	"authelia.com/provider/oauth2"
)

// ClientID is the provider's one client: the agent-secrets helper logging a machine in. It is a
// public client, so it authenticates with no secret; the DPoP key its device request names binds
// every token it is issued.
const ClientID = "agent-secrets-helper"

// GetClient returns the helper's client for ClientID and oauth2.ErrNotFound for any other id.
func (s *Store) GetClient(_ context.Context, id string) (oauth2.Client, error) {
	if id != ClientID {
		return nil, oauth2.ErrNotFound
	}
	return &oauth2.DefaultClient{
		ID:         ClientID,
		Public:     true,
		GrantTypes: []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
		Scopes:     []string{"machine"},
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
