// metadata_test.go drives GET /.well-known/oauth-authorization-server, the broker's RFC 8414
// authorization server metadata, in both states a deployment can be in: one that serves the
// device grant and one that does not, which is a broker started with no BROKER_MACHINE_LOGIN_URL.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// deviceCodeGrant is RFC 8628's grant type, which the library spells in a package its users
// cannot import.
const deviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code"

// wireMetadata is the document as a client reads it, declared here rather than shared with the
// broker's own type: a test that read the server's struct would pass whatever the server wrote.
type wireMetadata struct {
	Issuer                            string   `json:"issuer"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	DeviceAuthorizationEndpoint       string   `json:"device_authorization_endpoint"`
	IntrospectionEndpoint             string   `json:"introspection_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	DPoPSigningAlgValuesSupported     []string `json:"dpop_signing_alg_values_supported"`
	SubjectTokenTypesSupported        []string `json:"subject_token_types_supported"`
}

// metadata reads the document with no credential at all, as a client that holds none does.
func (ts *testServer) metadata(t *testing.T) wireMetadata {
	t.Helper()
	status, body := ts.req(t, http.MethodGet, "/.well-known/oauth-authorization-server", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /.well-known/oauth-authorization-server = %d: %s", status, body)
	}
	return decode[wireMetadata](t, body)
}

// TestTheMetadataDocumentNamesTheBrokersOwnEndpoints: the document is public, its issuer is the
// broker's own public URL, every endpoint it names is built from that URL, and it says the
// device grant, the refresh grant and the token exchange are served, that a client presents no
// credential at the token endpoint, and which subject tokens the exchange takes.
func TestTheMetadataDocumentNamesTheBrokersOwnEndpoints(t *testing.T) {
	ts := newTestServer(t)
	document := ts.metadata(t)

	for _, row := range []struct{ name, got, want string }{
		{"issuer", document.Issuer, ts.URL},
		{"token_endpoint", document.TokenEndpoint, ts.URL + "/v1/oauth/token"},
		{"device_authorization_endpoint", document.DeviceAuthorizationEndpoint, ts.URL + "/v1/oauth/device_authorization"},
		{"introspection_endpoint", document.IntrospectionEndpoint, ts.URL + "/v1/oauth/introspect"},
		{"revocation_endpoint", document.RevocationEndpoint, ts.URL + "/v1/oauth/revoke"},
	} {
		if row.got != row.want {
			t.Errorf("%s = %q, want %q: every URL in the document is built from the broker's own public URL", row.name, row.got, row.want)
		}
	}
	wantGrants := []string{deviceCodeGrant, "refresh_token", "urn:ietf:params:oauth:grant-type:token-exchange"}
	if !slices.Equal(document.GrantTypesSupported, wantGrants) {
		t.Errorf("grant_types_supported = %v, want %v", document.GrantTypesSupported, wantGrants)
	}
	if len(document.ResponseTypesSupported) != 0 {
		t.Errorf("response_types_supported = %v, want none: the broker has no authorization endpoint", document.ResponseTypesSupported)
	}
	if !slices.Equal(document.TokenEndpointAuthMethodsSupported, []string{"none"}) {
		t.Errorf("token_endpoint_auth_methods_supported = %v, want [none]: neither registered client presents a credential", document.TokenEndpointAuthMethodsSupported)
	}
	if !slices.Contains(document.DPoPSigningAlgValuesSupported, "ES256") {
		t.Errorf("dpop_signing_alg_values_supported = %v, want the algorithms a DPoP proof may be signed with, ES256 among them: every token this broker issues is bound to one", document.DPoPSigningAlgValuesSupported)
	}
	// The rig's exchange is told about a cluster and GitHub and about no Google issuer, so it
	// names those two kinds and the access token a dev box session presents, and no other.
	wantSubjects := []string{
		oauthwire.SubjectTokenTypeGitHub, oauthwire.SubjectTokenTypeKubernetes, oauthwire.SubjectTokenTypeAccessToken,
	}
	slices.Sort(wantSubjects)
	if !slices.Equal(document.SubjectTokenTypesSupported, wantSubjects) {
		t.Errorf("subject_token_types_supported = %v, want %v: the types this deployment registered, and not one it would refuse", document.SubjectTokenTypesSupported, wantSubjects)
	}
}

// TestTheMetadataDocumentIsJSONForAnyCaller pins that the route takes no credential and answers
// JSON: it is what a client reads before it holds anything to present.
func TestTheMetadataDocumentIsJSONForAnyCaller(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET the metadata with no Authorization header = %d %q, want 200 application/json", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&fields); err != nil {
		t.Fatalf("decode the document: %v", err)
	}
}

// withoutTheDeviceGrant wires the OAuth provider with no verification URL, which is what a
// broker started with no BROKER_MACHINE_LOGIN_URL builds: the same provider in every other way,
// on the same database.
func withoutTheDeviceGrant(t *testing.T) func(*api.Deps) {
	return func(d *api.Deps) {
		store := oauthstore.New(d.MachineLogin.Store.Pool)
		store.Reused = enroll.RevokeReusedCredential
		secret, err := store.Secret(context.Background())
		if err != nil {
			t.Fatalf("Secret: %v", err)
		}
		provider, err := oauth.New(oauth.Config{
			Secret: secret, DeviceLifetime: 15 * time.Minute, AccessLifetime: 15 * time.Minute,
			RefreshLifetime: time.Hour, ProofSkew: time.Minute,
		}, store)
		if err != nil {
			t.Fatalf("oauth.New with no verification URL: %v", err)
		}
		if provider.DeviceGrant() {
			t.Fatal("a provider built with no verification URL serves the device grant, want none")
		}
		d.OAuth = provider
		d.MachineLogin.OAuth = provider
	}
}

// TestWithNoDeviceGrantTheRouteIsGoneAndTheDocumentSaysSo: the two observable halves of a broker
// that serves no device grant — the device authorization route is not mounted at all, so its
// path is a 404, and the metadata names neither the device code nor the device authorization
// endpoint — while the token endpoint, introspection and revocation are mounted as ever.
func TestWithNoDeviceGrantTheRouteIsGoneAndTheDocumentSaysSo(t *testing.T) {
	ts := newTestServerWith(t, withoutTheDeviceGrant(t))

	status, body := ts.req(t, http.MethodPost, "/v1/oauth/device_authorization", nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("POST /v1/oauth/device_authorization with no device grant = %d %s, want 404: the row is not mounted", status, body)
	}

	document := ts.metadata(t)
	if document.DeviceAuthorizationEndpoint != "" {
		t.Errorf("device_authorization_endpoint = %q, want none: the broker serves no device grant", document.DeviceAuthorizationEndpoint)
	}
	if slices.Contains(document.GrantTypesSupported, deviceCodeGrant) {
		t.Errorf("grant_types_supported = %v, want no device code", document.GrantTypesSupported)
	}
	wantGrants := []string{"refresh_token", "urn:ietf:params:oauth:grant-type:token-exchange"}
	if !slices.Equal(document.GrantTypesSupported, wantGrants) {
		t.Errorf("grant_types_supported = %v, want %v: everything but the device grant is served as ever", document.GrantTypesSupported, wantGrants)
	}
	if document.TokenEndpoint != ts.URL+"/v1/oauth/token" || document.IntrospectionEndpoint != ts.URL+"/v1/oauth/introspect" || document.RevocationEndpoint != ts.URL+"/v1/oauth/revoke" {
		t.Errorf("the document names %q, %q and %q, want the token, introspection and revocation endpoints: only the device grant goes", document.TokenEndpoint, document.IntrospectionEndpoint, document.RevocationEndpoint)
	}
	if status, body := ts.req(t, http.MethodPost, "/v1/oauth/introspect", nil, nil); status == http.StatusNotFound {
		t.Fatalf("POST /v1/oauth/introspect with no device grant = %d %s, want a mounted route", status, body)
	}
}

// TestWithNoProviderThereIsNoMetadata: a broker wired with no OAuth provider serves neither the
// OAuth routes nor a document that would name them.
func TestWithNoProviderThereIsNoMetadata(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) { d.OAuth = nil })
	if status, body := ts.req(t, http.MethodGet, "/.well-known/oauth-authorization-server", nil, nil); status != http.StatusNotFound {
		t.Fatalf("GET the metadata with no provider = %d %s, want 404", status, body)
	}
}
