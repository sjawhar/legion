// device_grant_test.go drives the REAL compiled broker in both states BROKER_MACHINE_LOGIN_URL
// puts it in, over HTTP on a real Postgres: with no page for a person to type a code into the
// broker starts, serves no device authorization route at all and says so in its RFC 8414
// metadata; with one it serves the device grant as it always has and names it there. It also
// drives the other kind of listed caller, a machine holding a Google identity token, through the
// introspection route the Envoy listener reads tokens back on. Nothing here is a unit test — the
// binary is the one cmd/broker builds and every request crosses a socket.
package main

import (
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// deviceCodeGrant is RFC 8628's grant type, the one the metadata names only while the
	// broker serves the device grant.
	deviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code"
	// metadataPath is where RFC 8414 says an authorization server publishes its metadata.
	metadataPath = "/.well-known/oauth-authorization-server"
	// deviceHost is the machine a login in these rows is for, and devicePerson the person it
	// names.
	deviceHost   = "example-host-devbox"
	devicePerson = "ada@example.com"
	// listenerVMAccount is the Google service account BROKER_API_ACCOUNTS names as the caller
	// listener in the Google row below — the Envoy listener on a VM — and strangerVMAccount one
	// the setting does not name. No real account appears: this repository is public.
	listenerVMAccount = "example-listener@example-project.iam.gserviceaccount.com"
	strangerVMAccount = "example-nobody@example-project.iam.gserviceaccount.com"
)

// wireMetadata is the broker's RFC 8414 document as a client reads it off the wire.
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

// readMetadata GETs the broker's metadata with no credential, as a client that holds none does,
// and answers the document and the transcript line a failure reads.
func readMetadata(t *testing.T, addr string) wireMetadata {
	t.Helper()
	response, err := http.Get("http://" + addr + metadataPath)
	if err != nil {
		t.Fatalf("GET %s: %v", metadataPath, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("> GET %s\n< %d %s", metadataPath, response.StatusCode, body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: the document is public", metadataPath, response.StatusCode)
	}
	var document wireMetadata
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode the metadata: %v", err)
	}
	return document
}

// startDeviceAuthorization posts one device authorization as agent-secrets-helper does: the
// machine-login client, the machine's key thumbprint, and a request object signed by that key
// naming the host and the person who approves it.
func startDeviceAuthorization(t *testing.T, addr string, key *ecdsa.PrivateKey) (int, []byte) {
	t.Helper()
	base := "http://" + addr
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	request, err := record.Sign(key, base, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: deviceHost},
	}, "", devicePerson, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"client_id": {oauthwire.ClientID}, "scope": {oauthwire.Scope},
		"dpop_jkt": {thumbprint}, "request": {request},
	}
	response, err := http.Post(base+"/v1/oauth/device_authorization", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("> POST /v1/oauth/device_authorization\n< %d %s", response.StatusCode, body)
	return response.StatusCode, body
}

// emptySecretsFile writes the fake store a broker with no secrets reads.
func emptySecretsFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(path, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheBinaryWithNoMachineLoginURLServesNoDeviceGrant: the real binary starts with no
// BROKER_MACHINE_LOGIN_URL — the setting is no longer required — and both halves of refusing the
// device grant hold. The device authorization route is not mounted, so its path is a plain 404,
// and the metadata names neither the device code nor a device authorization endpoint, while
// everything else the provider serves is named as ever.
func TestTheBinaryWithNoMachineLoginURLServesNoDeviceGrant(t *testing.T) {
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
	}})
	requireHealthy(t, broker.Addr)
	if !strings.Contains(broker.Await(t, "machine login device grant"), "machine login device grant state=off") {
		t.Fatalf("the boot log does not report the device grant off:\n%s", broker.Log())
	}

	status, body := startDeviceAuthorization(t, broker.Addr, proofKey(t))
	if status != http.StatusNotFound {
		t.Fatalf("POST /v1/oauth/device_authorization on a broker with no machine-login page = %d %s, want 404: the row is not mounted", status, body)
	}

	document := readMetadata(t, broker.Addr)
	if document.DeviceAuthorizationEndpoint != "" {
		t.Errorf("device_authorization_endpoint = %q, want none", document.DeviceAuthorizationEndpoint)
	}
	if slices.Contains(document.GrantTypesSupported, deviceCodeGrant) {
		t.Errorf("grant_types_supported = %v, want no device code", document.GrantTypesSupported)
	}
	base := "http://" + broker.Addr
	if document.Issuer != base || document.TokenEndpoint != base+"/v1/oauth/token" ||
		document.IntrospectionEndpoint != base+"/v1/oauth/introspect" || document.RevocationEndpoint != base+"/v1/oauth/revoke" {
		t.Errorf("the document reads %+v, want every endpoint built from %s: only the device grant goes", document, base)
	}
	if !slices.Contains(document.SubjectTokenTypesSupported, oauthwire.SubjectTokenTypeAccessToken) {
		t.Errorf("subject_token_types_supported = %v, want at least the access token the exchange always takes", document.SubjectTokenTypesSupported)
	}
}

// TestTheBinaryWithAMachineLoginURLServesTheDeviceGrant: with the page set the device grant
// works exactly as it did — a signed device authorization is answered a device code and the user
// code a person types — and the metadata names the grant and the endpoint.
func TestTheBinaryWithAMachineLoginURLServesTheDeviceGrant(t *testing.T) {
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_MACHINE_LOGIN_URL=" + testMachineLoginURL,
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
	}})
	requireHealthy(t, broker.Addr)

	status, body := startDeviceAuthorization(t, broker.Addr, proofKey(t))
	var answer struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode the device authorization: %v", err)
	}
	if status != http.StatusOK || answer.DeviceCode == "" || answer.UserCode == "" || answer.VerificationURI != testMachineLoginURL {
		t.Fatalf("POST /v1/oauth/device_authorization = %d %s, want 200 with a device code, a user code and %s", status, body, testMachineLoginURL)
	}

	document := readMetadata(t, broker.Addr)
	if document.DeviceAuthorizationEndpoint != "http://"+broker.Addr+"/v1/oauth/device_authorization" {
		t.Errorf("device_authorization_endpoint = %q, want the route the broker just answered", document.DeviceAuthorizationEndpoint)
	}
	if !slices.Contains(document.GrantTypesSupported, deviceCodeGrant) {
		t.Errorf("grant_types_supported = %v, want the device code among them", document.GrantTypesSupported)
	}
	if !slices.Contains(document.TokenEndpointAuthMethodsSupported, "none") ||
		!slices.Contains(document.DPoPSigningAlgValuesSupported, "ES256") || len(document.ResponseTypesSupported) != 0 {
		t.Errorf("the document reads %+v, want client authentication none, the DPoP algorithms and no response type", document)
	}
}

// TestAListenerOnAVMIntrospectsWithItsGoogleIdentity: a listed caller may hold a Google identity
// token instead of a Kubernetes one, which is the Envoy listener on a VM. The real binary reads
// a token back for it on the introspection route its name carries, and refuses a machine of the
// same shape that BROKER_API_ACCOUNTS does not name 403 ACCOUNT_NOT_LISTED — the broker's list
// refusing a sound credential, not the credential failing.
func TestAListenerOnAVMIntrospectsWithItsGoogleIdentity(t *testing.T) {
	cluster, github, google := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	googleKey := google.PublishKey(t, "google-key")

	env := append(googleExchangeEnv(t, storetest.URL(t), cluster, github, google, longAccessSeconds),
		"BROKER_API_ACCOUNTS=dispatch="+apiAccount+" listener=google:"+listenerVMAccount)
	broker := startBroker(t, brokerOptions{Env: env})
	broker.Await(t, "token exchange")
	live := newPlainBroker(broker.Addr)

	identity := func(account string) string {
		t.Helper()
		claims := google.Claims(account, machineAudience)
		claims["email"] = account
		claims["email_verified"] = true
		claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
		return google.Mint(t, googleKey, claims)
	}

	status, body := live.introspect(t, "a-token-nobody-issued", identity(listenerVMAccount))
	if status != http.StatusOK || body["active"] != false {
		t.Fatalf("introspection by the listener's Google identity = %d %v, want 200 with the token not live: a listed Google account reaches the route its name carries", status, body)
	}

	status, body = live.introspect(t, "a-token-nobody-issued", identity(strangerVMAccount))
	if status != http.StatusForbidden || body["code"] != "ACCOUNT_NOT_LISTED" {
		t.Fatalf("introspection by a machine BROKER_API_ACCOUNTS does not name = %d %v, want 403 ACCOUNT_NOT_LISTED", status, body)
	}
}
