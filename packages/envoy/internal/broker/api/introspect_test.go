// introspect_test.go drives POST /v1/oauth/introspect against the real broker (brokertest's rig,
// which runs the OAuth provider): a token the broker just issued reads back live, with what the
// broker knows about it, a string it never issued and a token it revoked read back as simply not
// live, and a token it cannot read for a storage outage is refused 503 rather than answered.
package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
)

// wireIntrospection is the route's answer as a caller reads it: RFC 7662's active flag and, for a
// live token, the claims the broker knows.
type wireIntrospection struct {
	Active     bool   `json:"active"`
	Session    string `json:"session"`
	Person     string `json:"person"`
	Project    string `json:"project"`
	Scope      string `json:"scope"`
	ExpiresAt  int64  `json:"exp"`
	ClientID   string `json:"client_id"`
	Thumbprint string `json:"jkt"`
}

// introspect posts token to the introspection route as the listed caller does, answering the
// status and the decoded body.
func introspect(t *testing.T, rig *brokertest.Rig, token, bearer string) (int, wireIntrospection) {
	t.Helper()
	form := url.Values{"token": {token}}
	request, err := http.NewRequest(http.MethodPost, rig.URL+"/v1/oauth/introspect", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST /v1/oauth/introspect: %v", err)
	}
	defer response.Body.Close()
	var answer wireIntrospection
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
			t.Fatalf("decode introspection: %v", err)
		}
	}
	return response.StatusCode, answer
}

// TestIntrospectionAnswersALiveTokenAndAnUnknownOne drives a whole machine login over the device
// grant, introspects the access token it was issued — which must read back live, bound to the
// machine's own key and naming the OAuth client — and then introspects a string the broker never
// issued, which must read back as not live and disclose nothing else.
func TestIntrospectionAnswersALiveTokenAndAnUnknownOne(t *testing.T) {
	rig := brokertest.NewRig(t)
	_, tok, _ := approvedLogin(t, rig)

	status, answer := introspect(t, rig, tok.AccessToken, rig.UIToken)
	if status != http.StatusOK || !answer.Active {
		t.Fatalf("introspecting a live token = %d %+v, want 200 and active", status, answer)
	}
	if answer.ClientID == "" || answer.Thumbprint == "" || answer.ExpiresAt == 0 {
		t.Fatalf("a live token's answer = %+v, want its client, its key thumbprint and its expiry", answer)
	}

	status, answer = introspect(t, rig, "not-a-token-the-broker-ever-issued", rig.UIToken)
	if status != http.StatusOK || answer.Active {
		t.Fatalf("introspecting an unknown token = %d %+v, want 200 and not active", status, answer)
	}
	if answer != (wireIntrospection{}) {
		t.Fatalf("an unknown token's answer = %+v, want nothing but active false", answer)
	}
}

// TestIntrospectionAnswersARevokedTokenAsNotLive: once a person revokes the machine, its access
// token reads back as simply not live, a 200 like an unknown token's, not as the broker's failure.
func TestIntrospectionAnswersARevokedTokenAsNotLive(t *testing.T) {
	rig := brokertest.NewRig(t)
	_, tok, credentialID := approvedLogin(t, rig)
	if status, body := rig.UI(t, http.MethodPost, "/v1/launcher-credentials/"+credentialID+"/revoke-by-approver", map[string]any{"approver": rig.Operator}); status != http.StatusOK {
		t.Fatalf("revoke = %d: %s", status, body)
	}
	status, answer := introspect(t, rig, tok.AccessToken, rig.UIToken)
	if status != http.StatusOK || answer != (wireIntrospection{}) {
		t.Fatalf("introspecting a revoked token = %d %+v, want 200 and nothing but active false", status, answer)
	}
}

// TestIntrospectionAnswersAStorageOutage503: a live token the broker cannot read back, because
// its storage is gone, is refused 503 DATABASE_UNAVAILABLE. The library wraps a failed read in a
// 401-coded error, the same it answers a revoked token with, so answering by that status would
// tell a resource server a sound token is not valid when the truth is the broker could not tell.
func TestIntrospectionAnswersAStorageOutage503(t *testing.T) {
	rig := brokertest.NewRig(t)
	_, tok, _ := approvedLogin(t, rig)
	if status, answer := introspect(t, rig, tok.AccessToken, rig.UIToken); status != http.StatusOK || !answer.Active {
		t.Fatalf("introspecting a live token = %d %+v, want 200 and active", status, answer)
	}

	rig.Store.Close()
	form := url.Values{"token": {tok.AccessToken}}
	request, err := http.NewRequest(http.MethodPost, rig.URL+"/v1/oauth/introspect", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+rig.UIToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST /v1/oauth/introspect: %v", err)
	}
	defer response.Body.Close()
	var refusal wireError
	if err := json.NewDecoder(response.Body).Decode(&refusal); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable || refusal.Code != "DATABASE_UNAVAILABLE" {
		t.Fatalf("introspecting a live token with the store closed = %d %+v, want 503 DATABASE_UNAVAILABLE, not a confident negative", response.StatusCode, refusal)
	}
}

// TestIntrospectionTakesOnlyAListedCallersToken pins that the route is behind the same credential
// the routes Dispatch's server calls are: no bearer at all, and a sound ServiceAccount token of an
// account BROKER_API_ACCOUNTS does not name, are both refused.
func TestIntrospectionTakesOnlyAListedCallersToken(t *testing.T) {
	rig := brokertest.NewRig(t)
	if status, _ := introspect(t, rig, "anything", ""); status != http.StatusUnauthorized {
		t.Fatalf("introspecting with no caller token = %d, want 401", status)
	}
	stranger := rig.MintAccountToken(t, "system:serviceaccount:example-ns:example-stranger", time.Hour)
	if status, _ := introspect(t, rig, "anything", stranger); status != http.StatusForbidden {
		t.Fatalf("introspecting as an unlisted account = %d, want 403", status)
	}
}
