// token_revocation_test.go drives the REAL compiled broker's RFC 7009 revocation route over HTTP,
// on a real Postgres, against a local OpenID issuer standing in for the cluster's. An exchanged
// access token — a pod's, the one kind no credential revocation can reach, since it carries no
// credential id — is ended through POST /v1/oauth/revoke and stops introspecting at once, while
// the pod's other token, issued by another exchange, goes on working; a machine login's own access
// token is ended the same way and the login it belongs to still refreshes. Every way in that must
// not work is refused: the introspection-only caller cannot end a token, nor can a caller naming
// the wrong client, and a token the broker never issued is answered 200 having ended nothing.
// Nothing here is a unit test — the binary is the one cmd/broker builds, the schema is a fresh one
// on the test server, and every request crosses a socket.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/url"
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
	// revokePath is the RFC 7009 revocation route this test drives.
	revokePath = "/v1/oauth/revoke"
	// deviceCodeGrantType and refreshGrantType are the two grants a machine login polls and then
	// renews with, which the library spells in a package its users cannot import.
	deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	refreshGrantType    = "refresh_token"
	// revocationSession is the session name the pod of this test writes under, its own so that
	// nothing it revokes can disturb another test's claim on a name.
	revocationSession = "example-revocation"
	// revocationPodUID is the pod whose tokens this test ends, and revocationHost the machine
	// whose login it ends one token of.
	revocationPodUID = "pod-uid-revocation"
	revocationHost   = "example-host-revocation"
)

// TestTheBinaryRevokesOneOfItsOwnAccessTokens is the live proof that a token the broker issued can
// be ended before its own expiry: the real binary on a fresh schema, a pod's token exchanged
// twice, one of the two access tokens ended through POST /v1/oauth/revoke by the dispatch caller,
// and introspection answering active false for that one and active true for the other from the
// moment the revocation commits. The refusals around it are the point of the route's authority:
// the caller listed for introspection alone is refused by the power check and the token stays
// live, a revocation naming the wrong client is refused and the token stays live, and a token
// nobody issued is answered 200 (RFC 7009 Section 2.2) having ended nothing. Ending a token
// twice, and ending one under the optional access_token hint, are each answered 200 too. It
// skips when BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestTheBinaryRevokesOneOfItsOwnAccessTokens(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")

	key := proofKey(t)
	dispatchToken := accountToken(t, cluster, clusterKey, apiAccount)
	listenerToken := accountToken(t, cluster, clusterKey, listenerAccount)
	registrarToken := accountToken(t, cluster, clusterKey, registrarAccount)

	broker := startBroker(t, brokerOptions{Env: exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds)})
	broker.Await(t, "token exchange")
	live := newLiveBroker(broker.Addr, key)

	live.registered(t, mintPodToken(t, cluster, clusterKey, podAccount, enrolmentAudience, revocationPodUID), revocationSession, registrarToken)
	podToken := mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, revocationPodUID)
	// Two exchanges of the same standing pod token: two access tokens of two requests, so that
	// what the revocation ends can be told from what it leaves alone.
	ending := live.exchanged(t, exchangeForm(podToken, revocationSession))
	keeping := live.exchanged(t, exchangeForm(podToken, revocationSession))
	if ending == keeping {
		t.Fatal("the two exchanges answered the same access token, so this test could not tell an ended token from a kept one")
	}
	live.requireActive(t, ending, dispatchToken, true, "the exchange has just minted it")
	live.requireActive(t, keeping, dispatchToken, true, "the exchange has just minted it")

	t.Run("the introspection-only caller cannot end a token", func(t *testing.T) {
		status, body, _ := live.revoke(t, ending, oauthwire.ExchangeClientID, "", listenerToken)
		if status != http.StatusForbidden || body["code"] != lacksPower {
			t.Fatalf("the listener caller's revocation = %d %v, want 403 %s: a caller listed to read a token back must not be able to end one", status, body, lacksPower)
		}
		refusal, _ := body["error"].(string)
		for _, want := range []string{"listener", "POST /v1/oauth/revoke"} {
			if !strings.Contains(refusal, want) {
				t.Errorf("the refusal reads %q, which does not name %q: it must say which caller was refused which route", refusal, want)
			}
		}
		live.requireActive(t, ending, dispatchToken, true, "the power check refused the revocation before the provider saw it")
	})

	t.Run("a revocation naming the wrong client ends nothing", func(t *testing.T) {
		status, body, _ := live.revoke(t, ending, oauthwire.ClientID, "", dispatchToken)
		if status != http.StatusBadRequest || body["error"] != "unauthorized_client" {
			t.Fatalf("a revocation of an exchanged token naming %s = %d %v, want 400 unauthorized_client: RFC 7009 Section 2.1 lets a client end only the tokens it was issued", oauthwire.ClientID, status, body)
		}
		live.requireActive(t, ending, dispatchToken, true, "the token was issued to another client than the request named")
	})

	t.Run("a token the broker never issued is answered 200 and ends nothing", func(t *testing.T) {
		status, _, answer := live.revoke(t, "a-token-nobody-issued", oauthwire.ExchangeClientID, "", dispatchToken)
		if status != http.StatusOK || len(bytes.TrimSpace(answer)) != 0 {
			t.Fatalf("a revocation of a string nobody was issued = %d %q, want 200 with no body: RFC 7009 Section 2.2 answers an unknown token as it answers a revoked one, so the answer tells a caller holding a string at random nothing", status, answer)
		}
		live.requireActive(t, ending, dispatchToken, true, "a revocation that found no token must end none")
	})

	t.Run("the dispatch caller ends one exchanged token and leaves the other", func(t *testing.T) {
		live.revoked(t, ending, oauthwire.ExchangeClientID, dispatchToken)
		live.requireActive(t, ending, dispatchToken, false, "the revocation has committed, and an exchanged token is reachable no other way")
		live.requireActive(t, keeping, dispatchToken, true, "the revocation ends the one token it named, not the pod's session")
	})

	t.Run("ending the same token again is answered 200", func(t *testing.T) {
		live.revoked(t, ending, oauthwire.ExchangeClientID, dispatchToken)
		live.requireActive(t, ending, dispatchToken, false, "a repeated revocation leaves it ended")
	})

	t.Run("a revocation carrying the access_token hint ends the token it names", func(t *testing.T) {
		status, body, answer := live.revoke(t, keeping, oauthwire.ExchangeClientID, "access_token", dispatchToken)
		if status != http.StatusOK || len(bytes.TrimSpace(answer)) != 0 {
			t.Fatalf("a revocation carrying token_type_hint = %d %v, want 200 with no body: the hint says which kind to look for first and refuses nothing", status, body)
		}
		live.requireActive(t, keeping, dispatchToken, false, "the hinted revocation ends the token as an unhinted one does")
	})
}

// TestTheBinaryRevokesAMachineLoginsAccessTokenAndLeavesItsLogin is the proof that the same route
// reaches the other kind of token the broker issues, and reaches no further: a person's machine
// login, approved through the real binary, has its access token ended under its own client id and
// stops introspecting at once, while the login itself still refreshes into a token that is live.
// RFC 7009 Section 2.1 makes ending the refresh token beside an access token a MAY, and the broker
// ends the family — and with it the launcher credential the family names — only for a caller that
// presents that family's own refresh token, so revoking one token of a dev box session does not
// log that machine out.
func TestTheBinaryRevokesAMachineLoginsAccessTokenAndLeavesItsLogin(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")

	key := proofKey(t)
	dispatchToken := accountToken(t, cluster, clusterKey, apiAccount)

	broker := startBroker(t, brokerOptions{Env: exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds)})
	broker.Await(t, "token exchange")
	live := newLiveBroker(broker.Addr, key)

	access, refresh := live.machineLogin(t, key, dispatchToken)
	live.requireActive(t, access, dispatchToken, true, "the device code grant has just issued it")

	live.revoked(t, access, oauthwire.ClientID, dispatchToken)
	live.requireActive(t, access, dispatchToken, false, "the machine login's access token was ended through the same route, under its own client id")

	status, body := live.post(t, live.dpop, "/v1/oauth/token", url.Values{
		"grant_type": {refreshGrantType}, "refresh_token": {refresh}, "client_id": {oauthwire.ClientID},
	}, "")
	renewed, _ := body["access_token"].(string)
	if status != http.StatusOK || renewed == "" {
		t.Fatalf("the machine login refreshes after one of its access tokens was ended = %d %v, want 200 with an access token: ending an access token ends that token, not the login behind it", status, body)
	}
	live.requireActive(t, renewed, dispatchToken, true, "the login is still live, so its refresh mints a token that is")
	live.requireActive(t, access, dispatchToken, false, "the refresh does not bring an ended token back")
}

// machineLogin logs one machine in through the real binary the way agent-secrets-helper does: a
// device authorization naming the person, the dispatch caller looking the code up and approving it
// as that person, and the device code polled with a DPoP proof of the machine's key. It answers the
// access token and the refresh token the broker issued.
func (l *liveBroker) machineLogin(t *testing.T, key *ecdsa.PrivateKey, bearer string) (access, refresh string) {
	t.Helper()
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	request, err := record.Sign(key, l.base, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: revocationHost},
	}, "", person, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, body := l.post(t, l.plain, "/v1/oauth/device_authorization", url.Values{
		"client_id": {oauthwire.ClientID}, "scope": {oauthwire.Scope},
		"dpop_jkt": {thumbprint}, "request": {request},
	}, "")
	deviceCode, _ := body["device_code"].(string)
	userCode, _ := body["user_code"].(string)
	if status != http.StatusOK || deviceCode == "" || userCode == "" {
		t.Fatalf("the machine login's device authorization = %d %v, want 200 with a device code and a user code", status, body)
	}
	status, body = l.postJSON(t, "/v1/machine-logins/lookup", map[string]any{"code": userCode}, bearer)
	recordID, _ := body["record_id"].(string)
	if status != http.StatusOK || recordID == "" {
		t.Fatalf("looking the machine login up by its code = %d %v, want 200 with a record id", status, body)
	}
	status, body = l.postJSON(t, "/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"approver": person, "code": userCode}, bearer)
	if status != http.StatusOK {
		t.Fatalf("%s approves her own machine login = %d %v, want 200", person, status, body)
	}
	status, body = l.post(t, l.dpop, "/v1/oauth/token", url.Values{
		"grant_type": {deviceCodeGrantType}, "device_code": {deviceCode}, "client_id": {oauthwire.ClientID},
	}, "")
	access, _ = body["access_token"].(string)
	refresh, _ = body["refresh_token"].(string)
	if status != http.StatusOK || access == "" || refresh == "" {
		t.Fatalf("the approved machine polls its device code = %d %v, want 200 with an access token and a refresh token", status, body)
	}
	return access, refresh
}

// exchanged exchanges form and answers the access token the broker minted, failing the test unless
// it minted one: a step a test makes on the way to a revocation, not the thing it is proving.
func (l *liveBroker) exchanged(t *testing.T, form url.Values) string {
	t.Helper()
	status, body := l.exchange(t, form)
	token, _ := body["access_token"].(string)
	if status != http.StatusOK || token == "" {
		t.Fatalf("the exchange = %d %v, want 200 with an access token", status, body)
	}
	return token
}

// revoke ends one of the broker's own tokens the way Dispatch's server does: its own
// ServiceAccount token on the Authorization header, no proof, and the RFC 7009 form — the token,
// the client it was issued to, and the hint when one is given. An empty bearer sends no header. It
// answers the status, the decoded body (nil when the answer carries none, which is what a
// successful revocation answers) and the raw answer, so a caller can hold the route to RFC 7009's
// own "200 and no body".
func (l *liveBroker) revoke(t *testing.T, token, clientID, hint, bearer string) (int, map[string]any, []byte) {
	t.Helper()
	form := url.Values{"token": {token}, "client_id": {clientID}}
	if hint != "" {
		form.Set("token_type_hint", hint)
	}
	status, answer := l.send(t, l.plain, brokerCall{
		method: http.MethodPost, path: revokePath, contentType: formContentType,
		body: form.Encode(), bearer: bearer, shown: shownForm(form),
	})
	var body map[string]any
	if len(bytes.TrimSpace(answer)) > 0 {
		if err := json.Unmarshal(answer, &body); err != nil {
			t.Fatalf("POST %s answered %d %s, which is neither empty nor JSON: %v", revokePath, status, answer, err)
		}
	}
	return status, body, answer
}

// revoked ends token as the caller whose ServiceAccount token is bearer and fails the test unless
// the broker answered RFC 7009's own 200 with no body.
func (l *liveBroker) revoked(t *testing.T, token, clientID, bearer string) {
	t.Helper()
	status, body, answer := l.revoke(t, token, clientID, "", bearer)
	if status != http.StatusOK || len(bytes.TrimSpace(answer)) != 0 {
		t.Fatalf("the revocation = %d %v, want 200 with no body", status, body)
	}
}

// requireActive reads token back as the caller whose ServiceAccount token is bearer and fails the
// test unless introspection answers the liveness want, saying why that is what it must answer.
func (l *liveBroker) requireActive(t *testing.T, token, bearer string, want bool, because string) {
	t.Helper()
	status, body := l.introspect(t, token, bearer)
	if status != http.StatusOK || body["active"] != want {
		t.Fatalf("introspection = %d %v, want 200 with active %t: %s", status, body, want, because)
	}
}
