// account_powers_test.go drives the REAL compiled broker with three accounts in
// BROKER_API_ACCOUNTS — one calling as dispatch, one as listener, one as access-review — over
// HTTP, on a real Postgres, against a local OpenID issuer standing in for the cluster's. Being
// listed is not being allowed: the caller's name says which of the broker's thirteen
// named-caller routes that account reaches, so the introspection-only caller is refused every
// route Dispatch's server relays a person's decisions through, the review caller reads the
// grant log and is refused the other twelve, and the dispatch caller reaches all twelve of its
// own and is refused the review's. Nothing here is a unit test — the binary is the one
// cmd/broker builds and every request crosses a socket.
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
	// decision. reviewAccount is the third, calling as access-review: the identity a quarterly
	// access review reads the broker's audit trail as, which decides nothing. apiAccount
	// (token_exchange_test.go) is the first, calling as dispatch.
	listenerAccount = "system:serviceaccount:example-ns:example-listener"
	reviewAccount   = "system:serviceaccount:example-ns:example-access-review"
	// The three caller names those accounts are listed under, which the broker names in a
	// refusal and this test drives every route from.
	dispatchCaller = "dispatch"
	listenerCaller = "listener"
	reviewCaller   = "access-review"
	// introspection is the one route two names carry: the listener's own, which dispatch
	// reaches too, since the name that relays a person's decisions reads a token back as well.
	// grantLogRoute is the review's own, which carries a closed period because the review
	// caller's answer there is a page and not a refusal.
	introspection = "/v1/oauth/introspect"
	grantLogRoute = "/v1/grant-log"
	grantLogQuery = grantLogRoute + "?since=2026-01-01T00:00:00Z&until=2026-01-02T00:00:00Z"
	// powersApprover is the person a relayed decision would name, and unknownID an id no record,
	// machine login or grant has: the routes under test are reached, never satisfied, so the
	// answer this test reads is the authority's and not a decision's.
	powersApprover = "ada@example.com"
	unknownID      = "00000000-0000-4000-8000-000000000000"
	// unknownRecord is that same id nothing has, in a record id's own shape: a lowercase-hex
	// sha256 hash. The two decide routes authenticate their own caller after they have read the
	// record's kind, so an id of any other shape is refused 400 before the authority this test
	// reads is reached; every other route refuses the caller first and takes either shape.
	unknownRecord = "0000000000000000000000000000000000000000000000000000000000000000"
	// lacksPower is the refusal a listed caller gets on a route its name does not carry, as
	// against ACCOUNT_NOT_LISTED for an account the broker does not list at all.
	lacksPower = "ACCOUNT_LACKS_POWER"
)

// TestAListedAccountReachesOnlyItsOwnRoutes lists three callers and drives every route
// accountAuth authenticates from each of them. The listener caller, listed for introspection
// alone, is refused each of the eleven relay routes — approve, deny, both revoke-by-approver,
// the RFC 7009 token revocation and both person sign-in routes among them — and the grant log
// too, 403 ACCOUNT_LACKS_POWER naming itself and the route, and still reads a token back. The
// access-review caller reads the grant log and is refused the other twelve the same way. The
// dispatch caller reaches all twelve of its own, the introspection route included, and is
// refused the grant log: the broker's audit trail is not another of the powers the name that
// decides already holds.
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
		"BROKER_API_ACCOUNTS=dispatch=" + apiAccount + " listener=" + listenerAccount + " access-review=" + reviewAccount,
	}})
	live := newPlainBroker(broker.Addr)
	// Each listed caller: the name the broker knows it by and that account's own token.
	callers := []struct{ name, token string }{
		{dispatchCaller, accountToken(t, cluster, clusterKey, apiAccount)},
		{listenerCaller, accountToken(t, cluster, clusterKey, listenerAccount)},
		{reviewCaller, accountToken(t, cluster, clusterKey, reviewAccount)},
	}

	// Each row is one route accountAuth authenticates, with a request that reaches its handler:
	// pattern is what the broker names in a refusal, path what this test calls.
	for _, route := range []struct {
		method, pattern, path, contentType, body string
		// caller is the listed caller the route belongs to: the name whose own powers carry it.
		// Every other listed caller is refused it, except that dispatch reaches introspection
		// too, since the name that relays a person's decisions reads a token back as well.
		caller string
	}{
		{method: http.MethodPost, pattern: introspection, path: introspection,
			contentType: "application/x-www-form-urlencoded", body: "token=a-token-nobody-issued", caller: listenerCaller},
		// The revocation body names no client, so the dispatch caller it reaches is refused by
		// the provider's own client authentication rather than answered 200 with an empty body:
		// what this walk reads is the authority, and callBroker reads a JSON answer.
		{method: http.MethodPost, pattern: "/v1/oauth/revoke", path: "/v1/oauth/revoke",
			contentType: "application/x-www-form-urlencoded", body: "token=a-token-nobody-issued", caller: dispatchCaller},
		{method: http.MethodGet, pattern: "/v1/pending", path: "/v1/pending?approver=" + powersApprover, caller: dispatchCaller},
		{method: http.MethodPost, pattern: "/v1/credential-requests/{record}/approve", path: "/v1/credential-requests/" + unknownRecord + "/approve",
			contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, caller: dispatchCaller},
		{method: http.MethodPost, pattern: "/v1/credential-requests/{record}/deny", path: "/v1/credential-requests/" + unknownRecord + "/deny",
			contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, caller: dispatchCaller},
		{method: http.MethodPost, pattern: "/v1/machine-logins/lookup", path: "/v1/machine-logins/lookup",
			contentType: "application/json", body: `{"code":"EXAM-PLE1"}`, caller: dispatchCaller},
		{method: http.MethodGet, pattern: "/v1/launcher-credentials", path: "/v1/launcher-credentials?approver=" + powersApprover, caller: dispatchCaller},
		{method: http.MethodPost, pattern: "/v1/launcher-credentials/{id}/revoke-by-approver", path: "/v1/launcher-credentials/" + unknownID + "/revoke-by-approver",
			contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, caller: dispatchCaller},
		{method: http.MethodGet, pattern: "/v1/grants", path: "/v1/grants?approver=" + powersApprover, caller: dispatchCaller},
		{method: http.MethodPost, pattern: "/v1/grants/{id}/revoke-by-approver", path: "/v1/grants/" + unknownID + "/revoke-by-approver",
			contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, caller: dispatchCaller},
		{method: http.MethodGet, pattern: "/v1/person-signins", path: "/v1/person-signins?person=" + powersApprover, caller: dispatchCaller},
		{method: http.MethodPost, pattern: "/v1/person-signins/{id}/revoke", path: "/v1/person-signins/" + unknownID + "/revoke",
			contentType: "application/json", body: `{"person":"` + powersApprover + `"}`, caller: dispatchCaller},
		// The review's own route, called for a period that closed long ago, so the answer the
		// review gets here is a page of the grant log rather than a refusal of its period.
		{method: http.MethodGet, pattern: grantLogRoute, path: grantLogQuery, caller: reviewCaller},
	} {
		for _, who := range callers {
			t.Run("as "+who.name+": "+route.method+" "+route.pattern, func(t *testing.T) {
				status, body := live.callAs(t, route.method, route.path, who.token, route.contentType, route.body)
				if who.name != route.caller && !(route.pattern == introspection && who.name == dispatchCaller) {
					if status != http.StatusForbidden || body["code"] != lacksPower {
						t.Fatalf("%s %s as %s = %d %v, want 403 %s: a listed caller reaches the routes its own name carries and no others",
							route.method, route.pattern, who.name, status, body, lacksPower)
					}
					refusal, _ := body["error"].(string)
					for _, want := range []string{who.name, route.method + " " + route.pattern} {
						if !strings.Contains(refusal, want) {
							t.Errorf("the refusal reads %q, which does not name %q: it must say which caller was refused which route", refusal, want)
						}
					}
					if strings.Contains(refusal, who.token) {
						t.Error("the refusal repeats the caller's ServiceAccount token")
					}
					return
				}
				switch route.pattern {
				case introspection:
					// Introspection answers about the token, and the token nobody issued is
					// simply not live.
					if status != http.StatusOK || body["active"] != false {
						t.Fatalf("%s %s as %s = %d %v, want 200 and an inactive token: a caller listed to read a token back must reach it",
							route.method, route.pattern, who.name, status, body)
					}
				case grantLogRoute:
					// The grant log answers a page, empty on a broker nothing has happened on
					// yet, which is still a page and not a refusal.
					if status != http.StatusOK || body["events"] == nil {
						t.Fatalf("%s %s as %s = %d %v, want 200 and a page of events: the review reads the closed period it asked for",
							route.method, route.pattern, who.name, status, body)
					}
				default:
					// A route may well answer 400 or 404 — this test sets up no record, no
					// machine login and no grant for the ids and bodies it sends — so what is
					// asserted is that the answer is not the authority's refusal: neither "this
					// caller may not call that route" nor "this account is not listed".
					if code, _ := body["code"].(string); status == http.StatusForbidden && (code == lacksPower || code == "ACCOUNT_NOT_LISTED") {
						t.Fatalf("%s %s as %s = %d %v, want anything but the authority's refusal: this route is the caller's own",
							route.method, route.pattern, who.name, status, body)
					}
				}
			})
		}
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
