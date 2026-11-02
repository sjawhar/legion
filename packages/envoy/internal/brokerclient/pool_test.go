// packages/envoy/internal/brokerclient/pool_test.go

package brokerclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// The names this test's stubs use. None is a deployment's: the pool, the provider and the
// managed identity are example names, as every fixture in this repository is.
const (
	poolAudience      = "//iam.googleapis.com/projects/000000000000/locations/global/workloadIdentityPools/example-pool/providers/example-provider"
	poolIdentity      = "000000000000000000000"
	poolTokenAudience = "example-broker-audience"
	poolMintedToken   = "example-minted-identity-token"
	poolAccessToken   = "example-federated-access-token"
)

// TestPoolSourceMintsThroughTheSecurityTokenServiceAndGenerateIdToken drives the pool source
// against a stub Google security token service and a stub IAM Credentials generateIdToken, and
// holds it to the two things the broker depends on: the mint asks for includeEmail, without
// which the token carries no address and names no fleet, and the minted token is what reaches
// the broker's exchange, under the Google identity type.
func TestPoolSourceMintsThroughTheSecurityTokenServiceAndGenerateIdToken(t *testing.T) {
	var (
		federation url.Values
		mint       map[string]any
		mintPath   string
		bearer     string
	)
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		federation = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"` + poolAccessToken + `","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer sts.Close()
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mintPath, bearer = r.URL.Path, r.Header.Get("Authorization")
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err := json.Unmarshal(body, &mint); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"` + poolMintedToken + `"}`))
	}))
	defer iam.Close()

	stub := newStubBroker(t)
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	source := &PoolSource{
		Audience:                poolTokenAudience,
		PoolAudience:            poolAudience,
		Identity:                poolIdentity,
		Subject:                 &FileSource{Path: writeTokenFile(t, "example-pool-credential")},
		SecurityTokenServiceURL: sts.URL + "/v1/token",
		IAMCredentialsURL:       iam.URL + "/v1",
	}
	client := &Client{BrokerURL: stub.URL(), Key: key, Session: "example-pool-session", Subject: source}
	token, err := client.Trade(t.Context())
	if err != nil {
		t.Fatalf("the pool source's trade: %v", err)
	}
	if token.AccessToken == "" {
		t.Fatal("the trade answered no access token")
	}

	// The security token service half: the pool's provider as the audience, and the credential
	// it trusts as the subject.
	for field, want := range map[string]string{
		"grant_type":           GrantTypeTokenExchange,
		"audience":             poolAudience,
		"subject_token":        "example-pool-credential",
		"subject_token_type":   jwtTokenType,
		"requested_token_type": accessTokenType,
		"scope":                cloudPlatformScope,
	} {
		if got := federation.Get(field); got != want {
			t.Errorf("the security token service was asked %s=%q; want %q", field, got, want)
		}
	}

	// The IAM Credentials half: generateIdToken for the managed identity its client id names,
	// authenticated with what the security token service answered, asking for the email claim.
	if want := "/v1/projects/-/serviceAccounts/" + poolIdentity + ":generateIdToken"; mintPath != want {
		t.Errorf("the mint was asked at %q; want %q, the managed identity named by its client id", mintPath, want)
	}
	if bearer != "Bearer "+poolAccessToken {
		t.Errorf("the mint was authenticated with %q; want the access token the security token service answered", bearer)
	}
	if mint["includeEmail"] != true {
		t.Fatalf("the mint asked includeEmail=%v; without it the token carries no email claim and the broker can place no machine by it", mint["includeEmail"])
	}
	if mint["audience"] != poolTokenAudience {
		t.Errorf("the mint asked for the audience %v; want %q, the one the broker checks", mint["audience"], poolTokenAudience)
	}

	// And the minted token is what the broker was given.
	form, _ := stub.call(t, 0)
	if form.Get("subject_token") != poolMintedToken {
		t.Fatalf("the exchange carried the subject token %q; want %q, the token generateIdToken minted", form.Get("subject_token"), poolMintedToken)
	}
	if form.Get("subject_token_type") != oauthwire.SubjectTokenTypeGoogle {
		t.Fatalf("the exchange presented it as %q; a pool-minted token is a Google identity token (%q)", form.Get("subject_token_type"), oauthwire.SubjectTokenTypeGoogle)
	}
}

// TestPoolSourceCarriesTheMintsRefusalWhole: a mint the service refuses stops the trade with
// what that service said, so an operator reads Google's own reason rather than "no token".
func TestPoolSourceCarriesTheMintsRefusalWhole(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"the subject token is not from a trusted provider"}`))
	}))
	defer sts.Close()
	source := &PoolSource{
		Audience: poolTokenAudience, PoolAudience: poolAudience, Identity: poolIdentity,
		Subject:                 &FileSource{Path: writeTokenFile(t, "example-pool-credential")},
		SecurityTokenServiceURL: sts.URL + "/v1/token",
		IAMCredentialsURL:       "http://127.0.0.1:1/v1",
	}
	_, err := source.Token(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not from a trusted provider") {
		t.Fatalf("the pool source answered %v; want the security token service's own refusal", err)
	}
}
