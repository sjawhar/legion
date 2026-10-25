// packages/envoy/cmd/agent-secrets-devrelay/main_test.go
//
// This test mounts the real broker handlers (api.Register) on a real Postgres test store — no
// fakes — and drives the devrelay binary's own run() function directly (in-process, exactly the
// argv a subprocess would receive) for both decisions the dev stack needs a human for: the typed-
// code machine login that enrolls a box, and the approval of that box's secret request. The
// machine and requester sides (signed request objects, launcher and session proofs) are built
// directly with record.Sign, proof.SignLauncher and proof.Sign — the calls agent-secrets and its
// helper make — since devrelay only ever plays the approving human's relay.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// dispatchAccount is the ServiceAccount the test broker's BROKER_API_ACCOUNTS names as the
	// caller "dispatch", and dispatchAudience the audience its projected token carries: devrelay
	// stands in for Dispatch's server, so it presents that account's token.
	dispatchAccount  = "system:serviceaccount:example-ns:example-dispatch"
	dispatchAudience = "agent-secrets"
	testApprover     = "sami@example.com"
	testValue        = "dev-secret-value"
)

// newDevrelayTestServer is a live broker HTTP server (real handlers, real Postgres) holding one
// human-tier secret the box's operator owns and so approves, as scripts/dev-broker.sh does. It
// answers its URL and the ServiceAccount token devrelay calls it with.
func newDevrelayTestServer(t *testing.T) (brokerURL, callerToken string) {
	t.Helper()
	st := storetest.Open(t)
	local := secrets.NewLocal(policytest.Secret("AGENT_SECRETS_PROOF_APPROVAL", testApprover, policy.TierHuman, testValue))
	cur := policytest.Current(t, local)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	enr := &enroll.Service{Store: st, Lease: time.Hour}
	enr.Chain = enroll.NewChainVerifier(st, srv.URL, time.Minute)
	reqMachine := &requests.Machine{
		Store: st, Policy: cur, Secrets: policytest.Store(local),
		PendingTTL: 12 * time.Hour,
		Audience:   srv.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	reqMachine.Chain = requests.NewChainVerifier(st, srv.URL, time.Minute)
	// devrelay calls the broker as the listed caller does in production: with its own projected
	// ServiceAccount token, verified against a local OIDC issuer standing in for the cluster's.
	issuer := oidctest.New(t)
	issuerKey := issuer.PublishKey(t, "signing-key")
	accountTokens, err := oidc.New(context.Background(), issuer.URL(), dispatchAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	provider := brokertest.NewOAuthProvider(t, st, issuer)
	mach := &machine.Service{
		Store: st, Enroll: enr, Policy: cur,
		Audience: srv.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		OAuth: provider, Replay: enr.Replay,
	}
	api.Register(mux, api.Deps{
		PublicURL:     srv.URL,
		APIAccounts:   map[string]string{dispatchAccount: "dispatch"},
		AccountTokens: enroll.K8sPodVerifier{Verifier: accountTokens},
		Enroll:        enr, Machine: reqMachine, MachineLogin: mach,
		OAuth: provider, DeviceLogin: mach.LoginDevice, TokenHooks: mach.TokenHooks(),
		Proof:  &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: cur, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN,
	})
	// The caller's token lives well inside the hour one may, as a projected token does.
	callerClaims := issuer.Claims(dispatchAccount, dispatchAudience)
	callerClaims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	return srv.URL, issuer.Mint(t, issuerKey, callerClaims)
}

// req issues method against base+path with an optional JSON body and headers, returning the
// status and raw body bytes.
func req(t *testing.T, method, url string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, raw
}

// devrelay runs the command in-process with args and the broker connection flags, failing t on a
// nonzero exit, and decodes its stdout into T. Its --ui-token is the caller's own ServiceAccount
// token, the bearer the broker's listed caller presents.
func devrelay[T any](t *testing.T, brokerURL, callerToken string, args ...string) T {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args = append(args, "--broker", brokerURL, "--ui-token", callerToken, "--login", testApprover)
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("agent-secrets-devrelay %v: exit %d\nstdout: %s\nstderr: %s", args, code, stdout.String(), stderr.String())
	}
	var out T
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode devrelay output: %v\n%s", err, stdout.String())
	}
	return out
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatalf("proof.NewKey: %v", err)
	}
	return key
}

// TestDevStackEnrollsRequestsApprovesAndReleases runs the dev stack's whole story end to end: a
// machine logs in and devrelay approves its typed code by the operator's login, the resulting
// launcher credential enrolls a box, the box asks for an approval-required secret, devrelay
// approves that record by the same login through the broker's UI route Dispatch relays to, and
// the grant releases the fake secrets store's value.
func TestDevStackEnrollsRequestsApprovesAndReleases(t *testing.T) {
	brokerURL, callerToken := newDevrelayTestServer(t)

	// --- enroll: a machine login approved by devrelay, then a box enrollment on its credential ---
	machineKey := newKey(t)
	loginRequest, err := record.Sign(machineKey, brokerURL, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: "devrelay-test-host"},
	}, "", testApprover, nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign(machine login): %v", err)
	}
	thumbprint, err := proof.Thumbprint(&machineKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"client_id": {oauthwire.ClientID}, "scope": {oauthwire.Scope}, "request": {loginRequest}, "dpop_jkt": {thumbprint}}
	resp, err := http.Post(brokerURL+"/v1/oauth/device_authorization", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var login struct {
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil || resp.StatusCode != http.StatusOK || login.UserCode == "" {
		t.Fatalf("POST /v1/oauth/device_authorization = %d, %+v (%v), want a user code", resp.StatusCode, login, err)
	}
	issued := devrelay[struct {
		State        string  `json:"state"`
		CredentialID *string `json:"credential_id"`
	}](t, brokerURL, callerToken, "machine-approve", "--code", login.UserCode)
	if issued.State != "approved" || issued.CredentialID == nil {
		t.Fatalf("machine-approve output = %+v, want state=approved with a credential_id", issued)
	}

	sessionKey := newKey(t)
	thumbprint, err = proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	launcherProof, err := proof.SignLauncher(machineKey, *issued.CredentialID, http.MethodPost, brokerURL+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, body := req(t, http.MethodPost, brokerURL+"/v1/enrollments", map[string]string{"Proof": launcherProof},
		map[string]any{"kind": "session", "runtime_id": "session-devrelay-test", "operator": testApprover, "thumbprint": thumbprint})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/enrollments = %d: %s", status, body)
	}
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	if err := json.Unmarshal(body, &enrolled); err != nil || enrolled.EnrollmentID == "" {
		t.Fatalf("decode enrollment: %v (body: %s)", err, body)
	}

	// --- request: the box asks for the approval-required secret ---
	compact, err := record.Sign(sessionKey, brokerURL, []record.AuthorizationDetail{
		{Type: "agent_secret", Identifier: "AGENT_SECRETS_PROOF_APPROVAL", Actions: []string{"inject"}},
	}, "devrelay main_test", "", nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	p, err := proof.Sign(sessionKey, enrolled.EnrollmentID, http.MethodPost, brokerURL+"/v1/requests", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, body = req(t, http.MethodPost, brokerURL+"/v1/requests", map[string]string{"Proof": p},
		map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d: %s", status, body)
	}
	var created struct {
		State    string  `json:"state"`
		RecordID *string `json:"record_id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.State != "pending" || created.RecordID == nil {
		t.Fatalf("create response = %s (%v), want a pending request with a record_id", body, err)
	}

	// --- approve: devrelay decides the record by the operator's login ---
	approved := devrelay[struct {
		State   string  `json:"state"`
		GrantID *string `json:"grant_id"`
	}](t, brokerURL, callerToken, "approve", "--record", *created.RecordID)
	if approved.State != "approved" || approved.GrantID == nil {
		t.Fatalf("approve output = %+v, want state=approved with a grant_id", approved)
	}

	// --- release: the grant's value is the fake secrets store's ---
	valuesProof, err := proof.Sign(sessionKey, enrolled.EnrollmentID, http.MethodPost, brokerURL+"/v1/grants/"+*approved.GrantID+"/values", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, body = req(t, http.MethodPost, brokerURL+"/v1/grants/"+*approved.GrantID+"/values", map[string]string{"Proof": valuesProof}, nil)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/grants/{id}/values = %d: %s", status, body)
	}
	var values struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(body, &values); err != nil {
		t.Fatalf("decode values response: %v", err)
	}
	if got := values.Values["AGENT_SECRETS_PROOF_APPROVAL"]; got != testValue {
		t.Fatalf("granted value = %q, want %q", got, testValue)
	}
}
