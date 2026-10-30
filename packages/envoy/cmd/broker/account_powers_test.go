// account_powers_test.go drives the REAL compiled broker with two accounts in
// BROKER_API_ACCOUNTS — one calling as dispatch, one as listener — over HTTP, on a real Postgres,
// against a local OpenID issuer standing in for the cluster's. Being listed is not being allowed:
// the caller's name says which of the broker's named-caller routes that account reaches, so the
// introspection-only caller is refused every route Dispatch's server relays a person's decisions
// through, and the dispatch caller still reaches all eleven. Nothing here is a unit test — the
// binary is the one cmd/broker builds and every request crosses a socket.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// listenerAccount is the second listed ServiceAccount, calling as listener: the Envoy
	// listener, which reads one of the broker's access tokens back and relays no person's
	// decision. apiAccount (token_exchange_test.go) is the first, calling as dispatch.
	listenerAccount = "system:serviceaccount:example-ns:example-listener"
	// powersApprover is the person a relayed decision would name, and unknownID an id no record,
	// machine login or grant has: the routes under test are reached, never satisfied, so the
	// answer this test reads is the authority's and not a decision's.
	powersApprover = "ada@example.com"
	unknownID      = "00000000-0000-4000-8000-000000000000"
	// lacksPower is the refusal a listed caller gets on a route its name does not carry, as
	// against ACCOUNT_NOT_LISTED for an account the broker does not list at all.
	lacksPower = "ACCOUNT_LACKS_POWER"
)

// TestAListedAccountReachesOnlyItsOwnRoutes lists two callers and drives every route accountAuth
// authenticates from both. The listener caller, listed for introspection alone, is refused each
// of the ten relay routes — approve, deny, both revoke-by-approver and the RFC 7009 token
// revocation among them — 403 ACCOUNT_LACKS_POWER naming itself and the route, and still reads a
// token back; the dispatch caller reaches all eleven.
func TestAListedAccountReachesOnlyItsOwnRoutes(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster := oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "signing-key")
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + cluster.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
		"BROKER_API_ACCOUNTS=dispatch=" + apiAccount + " listener=" + listenerAccount,
	}})
	live := newPlainBroker(broker.Addr)
	dispatchToken := accountToken(t, cluster, clusterKey, apiAccount)
	listenerToken := accountToken(t, cluster, clusterKey, listenerAccount)

	// Each row is one route accountAuth authenticates, with a request that reaches its handler:
	// pattern is what the broker names in a refusal, path what this test calls.
	for _, route := range []struct {
		method, pattern, path, contentType, body string
		// relays is true for the ten routes Dispatch's server relays a person's decisions and
		// reads through, false for introspection, the one route the listener caller may call.
		relays bool
	}{
		{method: http.MethodPost, pattern: "/v1/oauth/introspect", path: "/v1/oauth/introspect",
			contentType: "application/x-www-form-urlencoded", body: "token=a-token-nobody-issued"},
		// The revocation body names no client, so the dispatch caller it reaches is refused by
		// the provider's own client authentication rather than answered 200 with an empty body:
		// what this walk reads is the authority, and callBroker reads a JSON answer.
		{method: http.MethodPost, pattern: "/v1/oauth/revoke", path: "/v1/oauth/revoke",
			contentType: "application/x-www-form-urlencoded", body: "token=a-token-nobody-issued", relays: true},
		{method: http.MethodGet, pattern: "/v1/pending", path: "/v1/pending?approver=" + powersApprover, relays: true},
		{method: http.MethodGet, pattern: "/v1/credential-requests/{record}", path: "/v1/credential-requests/" + unknownID, relays: true},
		{method: http.MethodPost, pattern: "/v1/credential-requests/{record}/approve", path: "/v1/credential-requests/" + unknownID + "/approve",
			contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, relays: true},
		{method: http.MethodPost, pattern: "/v1/credential-requests/{record}/deny", path: "/v1/credential-requests/" + unknownID + "/deny",
			contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, relays: true},
		{method: http.MethodPost, pattern: "/v1/machine-logins/lookup", path: "/v1/machine-logins/lookup",
			contentType: "application/json", body: `{"code":"EXAM-PLE1"}`, relays: true},
		{method: http.MethodGet, pattern: "/v1/launcher-credentials", path: "/v1/launcher-credentials?approver=" + powersApprover, relays: true},
		{method: http.MethodPost, pattern: "/v1/launcher-credentials/{id}/revoke-by-approver", path: "/v1/launcher-credentials/" + unknownID + "/revoke-by-approver",
			contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, relays: true},
		{method: http.MethodGet, pattern: "/v1/grants", path: "/v1/grants?approver=" + powersApprover, relays: true},
		{method: http.MethodPost, pattern: "/v1/grants/{id}/revoke-by-approver", path: "/v1/grants/" + unknownID + "/revoke-by-approver",
			contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, relays: true},
	} {
		t.Run("as listener: "+route.method+" "+route.pattern, func(t *testing.T) {
			status, body := live.callAs(t, route.method, route.path, listenerToken, route.contentType, route.body)
			if !route.relays {
				// Introspection is the listener's own route: it answers about the token, and the
				// token nobody issued is simply not live.
				if status != http.StatusOK || body["active"] != false {
					t.Fatalf("%s %s as listener = %d %v, want 200 and an inactive token: the caller listed for introspection must still reach it", route.method, route.pattern, status, body)
				}
				return
			}
			if status != http.StatusForbidden || body["code"] != lacksPower {
				t.Fatalf("%s %s as listener = %d %v, want 403 %s: a caller listed for introspection alone must not relay a person's decisions", route.method, route.pattern, status, body, lacksPower)
			}
			refusal, _ := body["error"].(string)
			for _, want := range []string{"listener", route.method + " " + route.pattern} {
				if !strings.Contains(refusal, want) {
					t.Errorf("the refusal reads %q, which does not name %q: it must say which caller was refused which route", refusal, want)
				}
			}
			if strings.Contains(refusal, listenerToken) {
				t.Error("the refusal repeats the caller's ServiceAccount token")
			}
		})
		t.Run("as dispatch: "+route.method+" "+route.pattern, func(t *testing.T) {
			// The dispatch caller holds both powers, so no route refuses it for its name. A route
			// may well answer 400 or 404 — this test sets up no record, no machine login and no
			// grant for the ids and bodies it sends — so what is asserted is that the answer is
			// not the authority's refusal: neither "this caller may not call that route" nor
			// "this account is not listed".
			status, body := live.callAs(t, route.method, route.path, dispatchToken, route.contentType, route.body)
			if code, _ := body["code"].(string); status == http.StatusForbidden && (code == lacksPower || code == "ACCOUNT_NOT_LISTED") {
				t.Fatalf("%s %s as dispatch = %d %v, want anything but the authority's refusal: the dispatch caller reaches all eleven routes as it did before", route.method, route.pattern, status, body)
			}
		})
	}
}

// callAs sends one request to the broker as the caller whose ServiceAccount token is bearer, and
// answers the status and the decoded body, the caller's token named in the transcript rather than
// repeated.
func (l *liveBroker) callAs(t *testing.T, method, path, bearer, contentType, body string) (int, map[string]any) {
	t.Helper()
	status, answer := l.send(t, l.plain, brokerCall{
		method: method, path: path, contentType: contentType,
		body: body, bearer: bearer, shown: body + " (as <account-token>)",
	})
	var decoded map[string]any
	if err := json.Unmarshal(answer, &decoded); err != nil {
		t.Fatalf("%s %s answered %d %s, which is not JSON: %v", method, path, status, answer, err)
	}
	return status, decoded
}
