// packages/envoy/internal/broker/airlockproxy/azure_test.go
//
// The azure kind, end to end: the exchange the broker makes itself at the stand-in token
// endpoint, the placeholder access token the command is answered, the swap of the real token
// into a later request, and the second pod that holds no token of its own and exchanges for
// itself. The stand-in plays both parts of the bound host - the token endpoint and the API
// service - because that is the shape of the design: the proxy reads one of them and streams
// the other.
package airlockproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// The azure-login secret behind the airlock: the federated token the broker holds, the
// placeholder the command holds in its place, and what the exchange asks for.
const (
	azureName          = "AZURE_ADMIN"
	azureTokenPath     = "/the-tenant-id/oauth2/v2.0/token"
	azureScope         = "https://management.example.com/.default"
	azureClientID      = "11111111-2222-3333-4444-555555555555"
	azureRealFederated = "the.real.federated-token"
	azureRealAccess    = "the-real-access-token"
	azureNextAccess    = "the-second-real-access-token"
	azureRealIDToken   = "the-real-id-token"
	azureLifetime      = 3599
)

var azureFederatedPlaceholder = requests.PlaceholderPrefix + "azureFederatedazureFederated0001"

// azureRules is the rules file of an azure-login secret: one entry of kind azure, bound to the
// one host, whose access token travels in Authorization as every other token does.
func azureRules() airlock.Rules {
	return airlock.Rules{Secrets: []airlock.SecretRule{{
		Name: azureName, Hosts: []string{boundHost}, Headers: []string{"authorization"}, Kind: "azure",
	}}}
}

// newAzureRig is the rig with the azure-login secret as the grant's one name, and the stand-in
// token endpoint behind the bound host: the stand-in package's Microsoft endpoint, which
// answers an exchange that carries the real federated token and records each form it was sent.
// It answers token requests alone, so the same service plays the API these rows then call with
// the access token: that is the shape of the design, since the proxy reads one of them and
// streams the other.
func newAzureRig(t *testing.T, o Options) (*rig, *airlocktest.MicrosoftStand) {
	t.Helper()
	o.Rules = azureRules()
	r := newRig(t, o)
	r.grants.connections[azureFederatedPlaceholder] = requests.AirlockConnection{
		GrantID: testGrantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
		Names: map[string]requests.AirlockGrantName{
			azureName: {Kind: "azure-login", Slots: map[string]string{requests.SlotAzureFederated: azureFederatedPlaceholder}},
		},
	}
	// No expiry on the federated token itself: what these rows are about is the life of the
	// access token the proxy exchanges it for, which is the endpoint's own expires_in.
	r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{
		azureName: {Kind: "azure-login", Slots: map[string]string{requests.SlotAzureFederated: azureRealFederated}},
	})
	// One token for every exchange, as a real endpoint answers one identity's calls: a row
	// about a second token says so itself. The endpoint takes the real federated token as its
	// assertion and refuses any other, as Microsoft refuses an assertion that is not the
	// application's.
	stand := airlocktest.NewMicrosoftStand(airlocktest.MicrosoftOptions{
		Tokens:    []string{azureRealAccess},
		IDToken:   azureRealIDToken,
		Assertion: azureRealFederated,
		Lifetime:  azureLifetime * time.Second,
	})
	r.answerWith(stand.Answer)
	return r, stand
}

// tokenRequest is one token request a command makes: the form az and the azure-identity library
// send, with the client_assertion the caller names.
func tokenRequest(t *testing.T, r *rig, client *http.Client, assertion string) (*http.Response, string) {
	t.Helper()
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {azureClientID},
		"scope":                 {azureScope},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	return r.do(client, http.MethodPost, "https://"+boundHost+azureTokenPath, form.Encode(),
		"Content-Type", "application/x-www-form-urlencoded")
}

// placeholderTokenOf reads the access token out of the answer the command was given and fails t
// unless the answer is Microsoft's own shape with a placeholder where the token was.
func placeholderTokenOf(t *testing.T, body string) string {
	t.Helper()
	var answer map[string]any
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("the answer to the exchange is no JSON object: %q", body)
	}
	token, _ := answer["access_token"].(string)
	if !strings.HasPrefix(token, requests.PlaceholderPrefix) {
		t.Fatalf("the answer's access_token = %q; want a placeholder", token)
	}
	if answer["token_type"] != "Bearer" || answer["expires_in"] != float64(azureLifetime) {
		t.Errorf("the answer = %v; want the endpoint's own token_type and expires_in", answer)
	}
	for _, never := range []string{azureRealAccess, azureNextAccess, azureRealIDToken, azureRealFederated} {
		if strings.Contains(body, never) {
			t.Fatalf("the answer carries a real token: %q", body)
		}
	}
	if _, held := answer["id_token"]; held {
		t.Errorf("the answer carries an id_token: %v; want every other token of the endpoint's answer left out", answer)
	}
	return token
}

// TestAnAzureTokenRequestIsExchangedByTheBrokerAndItsTokenSwappedIn: the command's assertion is
// a placeholder, the exchange the endpoint sees carries the real federated token, the command
// is answered a placeholder access token, and the request it then makes with that placeholder
// reaches the service carrying the real access token - from the token this pod already holds,
// so one exchange serves both.
func TestAnAzureTokenRequestIsExchangedByTheBrokerAndItsTokenSwappedIn(t *testing.T) {
	r, stand := newAzureRig(t, Options{})
	answer, body := tokenRequest(t, r, r.client(azureFederatedPlaceholder), azureFederatedPlaceholder)
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the exchange answered %d %q; want 200", answer.StatusCode, body)
	}
	placeholder := placeholderTokenOf(t, body)

	exchanges := stand.Exchanges()
	if len(exchanges) != 1 {
		t.Fatalf("the endpoint was sent %d exchanges; want one", len(exchanges))
	}
	if got := exchanges[0].Get("client_assertion"); got != azureRealFederated {
		t.Fatalf("the endpoint was sent the assertion %q; want the real federated token", got)
	}
	if exchanges[0].Get("scope") != azureScope || exchanges[0].Get("client_id") != azureClientID {
		t.Errorf("the endpoint was sent %v; want the command's own scope and client_id", exchanges[0])
	}
	if mints := r.grants.accessMints(); mints != 1 {
		t.Errorf("the broker was asked for %d access placeholders; want one", mints)
	}

	// A later connection, which is what a command makes: the token endpoint and the API are
	// different hosts in production, and the placeholder this mints reaches a connection opened
	// after it, as the broker's own row does.
	r.get(r.client(azureFederatedPlaceholder), "https://"+boundHost+"/v1/subscriptions", "Authorization", "Bearer "+placeholder)
	seen := r.requests()
	if len(seen) != 2 {
		t.Fatalf("the host saw %d calls; want the exchange and the API call", len(seen))
	}
	if got, want := seen[1].Header.Get("Authorization"), "Bearer "+azureRealAccess; got != want {
		t.Fatalf("the service saw Authorization %q; want %q", got, want)
	}
	if later := stand.Exchanges(); len(later) != 1 {
		t.Errorf("the endpoint was sent %d exchanges; want the one, since this pod holds the token", len(later))
	}
}

// TestASecondPodExchangesTheSlotForItself: the proxy runs in several pods and a command's
// connections are spread over them, so the pod that serves a request carrying an access
// placeholder is not the pod that minted it. That pod does the exchange for itself from what
// the slot carries - the endpoint, the scope and the application - and swaps the same real
// token in.
func TestASecondPodExchangesTheSlotForItself(t *testing.T) {
	r, stand := newAzureRig(t, Options{})
	_, body := tokenRequest(t, r, r.client(azureFederatedPlaceholder), azureFederatedPlaceholder)
	placeholder := placeholderTokenOf(t, body)

	second := secondPod(t, r)
	r.get(r.clientAt(second, azureFederatedPlaceholder), "https://"+boundHost+"/v1/subscriptions", "Authorization", "Bearer "+placeholder)

	seen := r.requests()
	if len(seen) != 3 {
		t.Fatalf("the host saw %d calls; want the first pod's exchange, the second pod's, and the API call", len(seen))
	}
	if got, want := seen[2].Header.Get("Authorization"), "Bearer "+azureRealAccess; got != want {
		t.Fatalf("the service saw Authorization %q; want %q, the token the second pod exchanged for itself", got, want)
	}
	exchanges := stand.Exchanges()
	if len(exchanges) != 2 {
		t.Fatalf("the endpoint was sent %d exchanges; want one per pod", len(exchanges))
	}
	if got := exchanges[1].Get("client_assertion"); got != azureRealFederated {
		t.Errorf("the second pod's exchange carried the assertion %q; want the real federated token", got)
	}
	if got, want := exchanges[1].Get("scope"), azureScope; got != want {
		t.Errorf("the second pod asked for the scope %q; want %q, the one the slot carries", got, want)
	}
	if got, want := exchanges[1].Get("client_id"), azureClientID; got != want {
		t.Errorf("the second pod exchanged as %q; want %q, the application the slot carries", got, want)
	}
	if mints := r.grants.accessMints(); mints != 1 {
		t.Errorf("the broker was asked for %d access placeholders; want the one the first pod minted", mints)
	}
}

// secondPod is another broker pod: a Server of its own, with a cache of its own and no token in
// it, given the same grants, the same rules and the same way out as the rig's. It answers the
// address this returns.
func secondPod(t *testing.T, r *rig) string {
	t.Helper()
	second := New(Options{
		Rules:         azureRules(),
		Grants:        r.grants,
		CA:            r.authority,
		Log:           slog.New(r.logs),
		Dial:          r.dial,
		Now:           r.proxy.now,
		Resolve:       r.resolve,
		UpstreamRoots: r.service.Authority().Pool(),
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the second pod's proxy port: %v", err)
	}
	go func() {
		if err := second.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("the second pod stopped serving: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		second.Shutdown(ctx)
	})
	return listener.Addr().String()
}

// TestATokenRequestWithoutThisGrantsPlaceholderIsRefused: an assertion that is not the
// placeholder is a credential the command holds of its own, and the airlock carries no such
// thing - nothing reaches the endpoint.
func TestATokenRequestWithoutThisGrantsPlaceholderIsRefused(t *testing.T) {
	r, stand := newAzureRig(t, Options{})
	answer, body := tokenRequest(t, r, r.client(azureFederatedPlaceholder), "an.assertion.of.its.own")
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "client_assertion is not this grant's placeholder") {
		t.Fatalf("the token request answered %d %q; want 403 naming the assertion it refused", answer.StatusCode, body)
	}
	if sent := stand.Exchanges(); len(sent) != 0 {
		t.Fatalf("the endpoint was sent %d exchanges; want none", len(sent))
	}
	if mints := r.grants.accessMints(); mints != 0 {
		t.Errorf("the broker was asked for %d access placeholders; want none", mints)
	}
}

// TestAnExchangeTheEndpointRefusesKeepsItsStatus: the command has to read the endpoint's own
// verdict - an application that may not have that scope answers 400, not a proxy error - and
// the words name the exchange, so an operator reading them knows which call failed.
func TestAnExchangeTheEndpointRefusesKeepsItsStatus(t *testing.T) {
	r, stand := newAzureRig(t, Options{})
	stand.Refuse(http.StatusBadRequest)
	answer, body := tokenRequest(t, r, r.client(azureFederatedPlaceholder), azureFederatedPlaceholder)
	if answer.StatusCode != http.StatusBadRequest {
		t.Fatalf("the exchange answered %d %q; want the endpoint's own 400", answer.StatusCode, body)
	}
	if !strings.Contains(body, "the token endpoint refused the exchange of this grant's federated token") {
		t.Fatalf("the exchange answered %q; want words naming the exchange", body)
	}
	if sent := stand.Exchanges(); len(sent) != 1 {
		t.Errorf("the endpoint was sent %d exchanges; want the one it refused", len(sent))
	}
	if mints := r.grants.accessMints(); mints != 0 {
		t.Errorf("the broker minted %d access placeholders for a refused exchange; want none", mints)
	}
}

// TestAnAccessTokenNearItsExpiryIsExchangedAgain: a call the proxy carries can run for minutes,
// so a token within mintAgainBefore of its expiry is exchanged again rather than handed to a
// call that then fails on it - the same rule a minted value follows.
func TestAnAccessTokenNearItsExpiryIsExchangedAgain(t *testing.T) {
	clock := &testClock{now: time.Now()}
	r, stand := newAzureRig(t, Options{Now: clock.read})
	// A token of its own for the second exchange, so what leaves says which exchange it came
	// from.
	stand.Answers(azureRealAccess, azureNextAccess)
	_, body := tokenRequest(t, r, r.client(azureFederatedPlaceholder), azureFederatedPlaceholder)
	placeholder := placeholderTokenOf(t, body)
	client := r.client(azureFederatedPlaceholder)

	// Well inside the token's life: the one this pod holds.
	clock.set(clock.read().Add(time.Hour - 10*time.Minute))
	r.get(client, "https://"+boundHost+"/v1/subscriptions", "Authorization", "Bearer "+placeholder)
	seen := r.requests()
	if got, want := seen[len(seen)-1].Header.Get("Authorization"), "Bearer "+azureRealAccess; got != want {
		t.Fatalf("the service saw Authorization %q; want %q, the token this pod holds", got, want)
	}
	if sent := stand.Exchanges(); len(sent) != 1 {
		t.Fatalf("the endpoint was sent %d exchanges; want the one, since the token is not near its expiry", len(sent))
	}

	// Within five minutes of the expiry: exchanged again, and the new token is what leaves.
	clock.set(clock.read().Add(6 * time.Minute))
	r.get(client, "https://"+boundHost+"/v1/subscriptions", "Authorization", "Bearer "+placeholder)
	seen = r.requests()
	if got, want := seen[len(seen)-1].Header.Get("Authorization"), "Bearer "+azureNextAccess; got != want {
		t.Fatalf("the service saw Authorization %q; want %q, the token of the second exchange", got, want)
	}
	if sent := stand.Exchanges(); len(sent) != 2 {
		t.Fatalf("the endpoint was sent %d exchanges; want a second one for the token about to end", len(sent))
	}
	if mints := r.grants.accessMints(); mints != 1 {
		t.Errorf("the broker was asked for %d access placeholders; want the one, since the scope is the same", mints)
	}
}

// TestAnAccessSlotCarriesTheExchangeItWasMadeFor: the slot is what a pod that holds no token
// reads the exchange out of, so what it carries - and that it carries it unambiguously - is the
// whole of the two-pod case.
func TestAnAccessSlotCarriesTheExchangeItWasMadeFor(t *testing.T) {
	made := azureExchange{Endpoint: "https://" + boundHost + azureTokenPath, Scope: azureScope, ClientID: azureClientID}
	slot := azureAccessSlot(made)
	if !strings.HasPrefix(slot, requests.SlotAzureAccessPrefix) {
		t.Fatalf("the slot %q does not open with %q", slot, requests.SlotAzureAccessPrefix)
	}
	if !isAzureAccessSlot(slot) {
		t.Fatalf("%q does not read as an access slot", slot)
	}
	if again := azureAccessSlot(made); again != slot {
		t.Fatalf("one exchange encoded to %q and then to %q; want one slot, so two pods name one", slot, again)
	}
	read, ok := azureExchangeOf(slot)
	if !ok || read != made {
		t.Fatalf("the slot read back as %+v, %v; want %+v", read, ok, made)
	}
	for _, no := range []string{"value", requests.SlotAzureFederated, requests.SlotAzureAccessPrefix + "not base64url!", requests.SlotAzureAccessPrefix + "e30"} {
		if read, ok := azureExchangeOf(no); ok {
			t.Errorf("%q read as the exchange %+v; want no exchange", no, read)
		}
	}
}

// TestAnAccessPlaceholderTheBrokerDoesNotKnowIsRefused: a request carrying a placeholder whose
// slot names no exchange reaches no service, because there is nothing to exchange for it.
func TestAnAccessPlaceholderTheBrokerDoesNotKnowIsRefused(t *testing.T) {
	r, stand := newAzureRig(t, Options{})
	brokenSlot := requests.SlotAzureAccessPrefix + "not-the-encoding"
	strange := requests.PlaceholderPrefix + "strangeAccessStrangeAccess000001"
	r.grants.connections[azureFederatedPlaceholder].Names[azureName].Slots[brokenSlot] = strange
	answer, body := r.get(r.client(azureFederatedPlaceholder), "https://"+boundHost+"/v1/subscriptions", "Authorization", "Bearer "+strange)
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "names no exchange this broker made") {
		t.Fatalf("the call answered %d %q; want 403 naming the slot it could not read", answer.StatusCode, body)
	}
	if sent := stand.Exchanges(); len(sent) != 0 {
		t.Fatalf("the endpoint was sent %d exchanges; want none", len(sent))
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service was sent %d calls; want none", len(seen))
	}
}
