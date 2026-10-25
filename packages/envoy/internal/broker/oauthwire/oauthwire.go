// Package oauthwire is the wire contract of a machine login's OAuth device grant that the broker
// and the agent-secrets helper share: the client the helper logs in as, the scope it asks for, and
// the fields the broker adds to a token answer. It imports nothing, so the helper reads it without
// the broker's storage.
package oauthwire

const (
	// ClientID is the OAuth client the helper logs a machine in as: the provider's one client.
	ClientID = "agent-secrets-helper"
	// Scope is the one scope a machine login asks for and is granted.
	Scope = "machine"
	// ExtraCredentialID is the token answer's field naming the launcher credential the login holds;
	// the login's session carries it under the same key.
	ExtraCredentialID = "credential_id"
	// ExtraCredentialExpiresAt is the token answer's field holding that credential's expiry
	// (RFC 3339).
	ExtraCredentialExpiresAt = "credential_expires_at"
)
