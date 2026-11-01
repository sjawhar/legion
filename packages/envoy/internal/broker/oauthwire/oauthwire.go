// Package oauthwire is the wire contract of the broker's OAuth endpoints that the broker and the
// callers it answers share: the clients they present, the scope a machine login asks for, the
// names the token exchange knows a subject token by, and the fields the broker adds to a token
// answer. It imports nothing, so the agent-secrets helper reads it without the broker's storage.
package oauthwire

const (
	// ClientID is the OAuth client the agent-secrets helper logs a machine in as, and the client
	// whose access token a dev box session exchanges for a Dispatch one.
	ClientID = "agent-secrets-helper"
	// Scope is the one scope a machine login asks for and is granted.
	Scope = "machine"
	// ExtraCredentialID is the token answer's field naming the launcher credential the login holds;
	// the login's session carries it under the same key.
	ExtraCredentialID = "credential_id"
	// ExtraCredentialExpiresAt is the token answer's field holding that credential's expiry
	// (RFC 3339).
	ExtraCredentialExpiresAt = "credential_expires_at"
	// ExchangeClientID is the OAuth client every caller asking the RFC 8693 token exchange for a
	// Dispatch access token presents: a pod, a workflow run, and a dev box session exchanging the
	// access token its machine login holds. It is registered confidential, because the library
	// refuses a public client that grant, but it presents no client credential: the subject
	// token it carries and the proof of its own key are what the broker judges the request on.
	ExchangeClientID = "dispatch-token-exchange"
	// SubjectTokenTypeKubernetes is the subject_token_type a pod presents its projected
	// Kubernetes service-account token under. RFC 8693 registers no name for one, so the broker
	// defines this one and the exchange client is the only client that may present it.
	SubjectTokenTypeKubernetes = "urn:envoy:params:oauth:token-type:kubernetes-service-account"
	// SubjectTokenTypeGitHub is the subject_token_type a continuous-integration workflow run
	// presents its GitHub token under, broker-defined for the same reason as
	// SubjectTokenTypeKubernetes.
	SubjectTokenTypeGitHub = "urn:envoy:params:oauth:token-type:github-workflow-run"
	// SubjectTokenTypeAccessToken is RFC 8693's own name for an access token: the only kind the
	// exchange mints, the kind it asks for, and the kind a dev box session presents its machine
	// login's token as. The library spells it in a package its users cannot import.
	SubjectTokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token" //nolint:gosec // a token type's name, not a token
)

// The wire names of a token's project scope, which an introspection answer's ProjectScope carries
// and the exchange writes onto the session it stores (internal/broker/oauth, ClaimProjectScope).
// They are short and fixed so a reader of either reads one word rather than the prose
// exchange.ProjectScope.String prints for an operator.
const (
	// ProjectScopeNamed is a token that may write the one project it names.
	ProjectScopeNamed = "named"
	// ProjectScopePerson is a token that may write every project its person may write, and names
	// no project.
	ProjectScopePerson = "person"
)

// Introspection is what the broker knows about one of its access tokens: the RFC 7662 answer the
// broker's introspection route serialises and a resource server reads a presented token back
// through (internal/brokertoken). The broker's tokens are opaque, so everything here comes from
// the row the token is stored in rather than from the token's own bytes, and a token the broker
// has revoked or let expire is simply not live.
//
// The JSON names are RFC 7662's where it has one and the broker's own where it does not; the
// route serialises this struct and adds no field of its own. Both ends read this one declaration,
// so a field added here reaches each of them and the compiler holds them together.
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
