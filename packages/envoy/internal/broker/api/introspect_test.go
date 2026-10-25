// introspect_test.go drives POST /v1/oauth/introspect against the real broker (brokertest's rig,
// which runs the OAuth provider): a token the broker just issued reads back live, with what the
// broker knows about it, and a string it never issued reads back as simply not live.
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
