// packages/envoy/internal/brokerclient/pool_test.go
//
// The workload identity pool's whole path, which is an acceptance clause of
// dispatch://INFRA-958: the credential this machine already holds is traded at Google's security
// token service for a federated access token, that token is spent on one IAM Credentials
// generateIdToken with includeEmail — the broker looks a machine up by the token's email claim,
// so a mint without it names no account at all — and the identity token that mint answers is
// what reaches the broker's exchange.

package brokerclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPoolIdentityMintsThroughTheSecurityTokenServiceAndIAMCredentials(t *testing.T) {
	const (
		externalToken  = "the.credential.this.machine.holds"
		federatedToken = "federated-access-token"
		mintedToken    = "google.identity.token.minted.for.the.pool"
		poolAudience   = "//iam.googleapis.com/projects/000000000000/locations/global/workloadIdentityPools/example-pool/providers/example-provider"
		serviceAccount = "example-fleet@example-project.iam.gserviceaccount.com"
		clientID       = "example-machine-client-id"
	)
	var (
		securityTokenRequest map[string]any
		mintRequest          map[string]any
		mintPath, mintBearer string
	)

	securityTokenService := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		securityTokenRequest = readJSONBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"` + federatedToken + `","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
	})
	iamCredentials := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		mintPath, mintBearer = r.URL.Path, r.Header.Get("Authorization")
		mintRequest = readJSONBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"` + mintedToken + `"}`))
	})

	external := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(external, []byte(externalToken+"\n"), 0o600); err != nil {
		t.Fatalf("write the external credential: %v", err)
	}
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 200, map[string]any{
			"access_token": "dispatch-token-for-the-pool-machine", "token_type": "DPoP",
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 3600,
		}
	})
	client, err := New(Config{
		BrokerURL: broker.URL(),
		Key:       testKey(t),
		Subject: &PoolIdentity{
			Subject:                 NewProjectedToken(external),
			Audience:                poolAudience,
			ServiceAccount:          serviceAccount,
			TokenAudience:           clientID,
			SecurityTokenServiceURL: securityTokenService + "/v1/token",
			IAMCredentialsURL:       iamCredentials,
		},
		Session: "example-fleet-session",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token, err := client.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token.AccessToken != "dispatch-token-for-the-pool-machine" {
		t.Errorf("the client held %q, want the token the broker answered", token.AccessToken)
	}

	for _, field := range []struct{ name, want string }{
		{"audience", poolAudience},
		{"grantType", "urn:ietf:params:oauth:grant-type:token-exchange"},
		{"requestedTokenType", "urn:ietf:params:oauth:token-type:access_token"},
		{"scope", "https://www.googleapis.com/auth/cloud-platform"},
		{"subjectTokenType", "urn:ietf:params:oauth:token-type:jwt"},
		{"subjectToken", externalToken},
	} {
		if got, _ := securityTokenRequest[field.name].(string); got != field.want {
			t.Errorf("the security token service was sent %s %q, want %q", field.name, got, field.want)
		}
	}

	if want := "/v1/projects/-/serviceAccounts/" + serviceAccount + ":generateIdToken"; mintPath != want {
		t.Errorf("generateIdToken was called at %s, want %s: the managed identity the pool lets this credential act as", mintPath, want)
	}
	if mintBearer != "Bearer "+federatedToken {
		t.Errorf("generateIdToken was called with %q, want the federated access token the security token service answered", mintBearer)
	}
	if mintRequest["includeEmail"] != true {
		t.Errorf("generateIdToken was asked with includeEmail %v, want true: the broker looks a machine up by the token's email claim", mintRequest["includeEmail"])
	}
	if got, _ := mintRequest["audience"].(string); got != clientID {
		t.Errorf("generateIdToken was asked for the audience %q, want the client id the broker checks", got)
	}

	requests := broker.Requests()
	if len(requests) != 1 {
		t.Fatalf("the broker saw %d trades, want one", len(requests))
	}
	if got := requests[0].Form.Get("subject_token"); got != mintedToken {
		t.Errorf("the trade carried the subject token %q, want the one generateIdToken minted", got)
	}
	if got := requests[0].Form.Get("subject_token_type"); got != "urn:envoy:params:oauth:token-type:google-identity" {
		t.Errorf("the trade presented the minted token as %q, want a machine's Google identity token", got)
	}
}

// TestPoolIdentityRefusedByTheSecurityTokenServiceNamesIt: a pool that refuses this machine's
// credential is a failure before the broker is asked at all, and the error says where it
// happened rather than reading as a broker that is down.
func TestPoolIdentityRefusedByTheSecurityTokenServiceNamesIt(t *testing.T) {
	securityTokenService := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_grant","error_description":"the subject token is not this provider's"}`, http.StatusBadRequest)
	})
	external := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(external, []byte("external\n"), 0o600); err != nil {
		t.Fatalf("write the external credential: %v", err)
	}
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "never", "token_type": "DPoP", "expires_in": 60}
	})
	client, err := New(Config{
		BrokerURL: broker.URL(), Key: testKey(t),
		Subject: &PoolIdentity{
			Subject: NewProjectedToken(external), Audience: "//iam.googleapis.com/example",
			ServiceAccount: "example-fleet@example-project.iam.gserviceaccount.com", TokenAudience: "example-client",
			SecurityTokenServiceURL: securityTokenService + "/v1/token", IAMCredentialsURL: "http://127.0.0.1:1",
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "security token service") {
		t.Fatalf("Token = %v, want a refusal naming the security token service", err)
	}
	if len(broker.Requests()) != 0 {
		t.Error("the client traded at the broker with no identity token minted")
	}
}

func readJSONBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		t.Fatalf("read the request body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("read the request body as JSON: %v", err)
	}
	return body
}
