// handlers_metadata.go: GET /.well-known/oauth-authorization-server — the broker's RFC 8414
// authorization server metadata, the one document a client reads before it holds any credential
// to learn where this broker's endpoints are and which grants it serves. It is public for that
// reason, and it describes only this broker as it is configured: a deployment that serves no
// device grant (BROKER_MACHINE_LOGIN_URL unset) names neither the device code nor the device
// authorization endpoint, and one told about no Google issuer names no Google subject token
// type.
package api

import "net/http"

// The paths of the routes the metadata document names, each the pattern of a row in routes()
// (routes_table.go, which must spell the pattern itself: the broker's generated reference reads
// those rows as source). metadata_paths_test.go checks every one of these against that table, so
// the document can never name an endpoint this broker does not mount, and Register reads
// deviceAuthorizationPath to leave that row off a broker with no device grant.
const (
	metadataPath            = "/.well-known/oauth-authorization-server"
	deviceAuthorizationPath = "/v1/oauth/device_authorization"
	tokenPath               = "/v1/oauth/token"
	introspectionPath       = "/v1/oauth/introspect"
	revocationPath          = "/v1/oauth/revoke"
)

// authMethodNone is the one client authentication method this broker's token endpoint takes: the
// machine-login client is public and the exchange client presents no credential of its own, the
// subject token and the DPoP proof being what the request is judged on. RFC 8414 says a server
// that names no token_endpoint_auth_methods_supported supports client_secret_basic, which this
// one does not, so the member is written rather than left to that default.
const authMethodNone = "none"

// authorizationServerMetadata is GET /.well-known/oauth-authorization-server's answer, this
// broker's RFC 8414 metadata. Every member is either one RFC 8414 requires, one whose default
// would be wrong for this broker, or the address of an endpoint a client could not otherwise
// find; nothing here is a claim about the broker that some other part of it could contradict,
// since the grants, the token types and the DPoP algorithms are read from the provider that
// serves them.
type authorizationServerMetadata struct {
	// The broker's own issuer identifier (BROKER_PUBLIC_URL), which is the URL this document is
	// published under and the audience every signed proof and request object names. RFC 8414
	// requires it.
	Issuer string `json:"issuer"`
	// The token endpoint: the device code grant, the refresh grant and the RFC 8693 token
	// exchange. RFC 8414 requires it of a server that serves a grant using it, as all three do.
	TokenEndpoint string `json:"token_endpoint"`
	// The device authorization endpoint (RFC 8628), absent from a broker that serves no device
	// grant, which is one started with no BROKER_MACHINE_LOGIN_URL: there is then no page for a
	// person to type a code into, the route is not mounted, and grant_types_supported names no
	// device code either.
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint,omitempty"`
	// The introspection endpoint (RFC 7662), where a resource server listed in
	// BROKER_API_ACCOUNTS reads one of the broker's access tokens back.
	IntrospectionEndpoint string `json:"introspection_endpoint"`
	// The revocation endpoint (RFC 7009), where a caller listed for that power ends one of the
	// broker's tokens.
	RevocationEndpoint string `json:"revocation_endpoint"`
	// The grant types this broker serves: always the refresh token grant and the RFC 8693 token
	// exchange, and the RFC 8628 device code only while the broker serves the device grant. RFC
	// 8414 makes the member optional with a default of the authorization code and implicit
	// grants, neither of which this broker has, so leaving it out would say something untrue.
	GrantTypesSupported []string `json:"grant_types_supported"`
	// The response types this broker supports, which is none: it has no authorization endpoint
	// at all, since nobody signs in to it through a browser to get a token — a person's part in
	// a machine login is typing a code, and every other caller presents an identity it already
	// holds. RFC 8414 requires the member, so it is written empty rather than omitted.
	ResponseTypesSupported []string `json:"response_types_supported"`
	// How a client authenticates at the token endpoint: "none" and nothing else (authMethodNone).
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	// The algorithms a DPoP proof (RFC 9449) may be signed with, the member RFC 9449 Section 5.1
	// defines. Every token this broker issues is DPoP-bound and a request with no acceptable
	// proof gets none, so a client that cannot sign with one of these gets nothing at all.
	DPoPSigningAlgValuesSupported []string `json:"dpop_signing_alg_values_supported"`
	// The token types this broker's RFC 8693 exchange takes as a subject token, which is what a
	// caller puts in subject_token_type. RFC 8693 registers no metadata member for them and RFC
	// 8414 names nothing of the kind, so this is the broker's own member, named after RFC 8693's
	// own subject_token_type parameter and RFC 8414's <thing>_supported convention. A caller
	// that does not know it ignores it, as RFC 8414 says an unknown member must be; one that
	// does learns which identities this deployment accepts without trying each in turn.
	SubjectTokenTypesSupported []string `json:"subject_token_types_supported"`
}

// authorizationServerMetadata answers the broker's RFC 8414 metadata, built from the broker's own
// public URL — BROKER_PUBLIC_URL as the broker resolved it at boot (Deps.PublicURL), the same
// base every other absolute URL the broker writes is built from, and the one address its callers
// reach it at — and from the provider that actually serves the endpoints, so the document and
// the routes cannot disagree. The row is mounted only with that provider, as the OAuth rows are.
func (s *server) authorizationServerMetadata(w http.ResponseWriter, _ *http.Request) {
	document := authorizationServerMetadata{
		Issuer:                            s.deps.PublicURL,
		TokenEndpoint:                     s.deps.PublicURL + tokenPath,
		IntrospectionEndpoint:             s.deps.PublicURL + introspectionPath,
		RevocationEndpoint:                s.deps.PublicURL + revocationPath,
		GrantTypesSupported:               s.deps.OAuth.GrantTypes(),
		ResponseTypesSupported:            []string{},
		TokenEndpointAuthMethodsSupported: []string{authMethodNone},
		DPoPSigningAlgValuesSupported:     s.deps.OAuth.DPoPSigningAlgorithms(),
		SubjectTokenTypesSupported:        s.deps.OAuth.SubjectTokenTypes(),
	}
	if s.deps.OAuth.DeviceGrant() {
		document.DeviceAuthorizationEndpoint = s.deps.PublicURL + deviceAuthorizationPath
	}
	writeJSON(w, http.StatusOK, document)
}
