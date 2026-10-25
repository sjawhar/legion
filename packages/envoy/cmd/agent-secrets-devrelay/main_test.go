// packages/envoy/cmd/agent-secrets-devrelay/main_test.go
//
// This test mounts the real broker handlers (api.Register) on a real Postgres test store — no
// fakes — and walks the dev stack's two decisions a human makes: devrelay's own run() function
// (in-process, exactly the argv a subprocess would receive) relays the typed-code machine login
// that enrolls a box, and the box's secret request is approved by the operator signed in to the
// broker's own approval page (brokertest.Decide), which no relay can stand in for. The machine
// and requester sides (signed request objects, launcher and session proofs) are built directly
// with record.Sign, proof.SignLauncher and proof.Sign — the calls agent-secrets and its helper
// make.
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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
// human-tier secret the box's operator owns and so approves, as scripts/dev-broker.sh does, with
// its approval page signing people in at a local issuer. It answers its URL, the ServiceAccount
// token devrelay calls it with, and that issuer.
func newDevrelayTestServer(t *testing.T) (brokerURL, callerToken string, issuer *oidctest.Issuer) {
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
	issuer = oidctest.New(t)
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
		// People sign in to the approval page at the same local issuer, which stands in for every
		// provider.
		Approval: brokertest.NewApprovalPage(t, brokertest.SignIn(issuer, issuerKey), srv.URL, st, reqMachine),
	})
	// The caller's token lives well inside the hour one may, as a projected token does.
	callerClaims := issuer.Claims(dispatchAccount, dispatchAudience)
	callerClaims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	return srv.URL, issuer.Mint(t, issuerKey, callerClaims), issuer
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
// nonzero exit, and decodes its stdout into T. Its --account-token-file holds the caller's own
// ServiceAccount token, the bearer the broker's listed caller presents.
func devrelay[T any](t *testing.T, brokerURL, callerToken string, args ...string) T {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args = append(args, "--broker", brokerURL, "--account-token-file", tokenFile(t, callerToken), "--login", testApprover)
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

// tokenFile writes token to a file of t's own and answers its path, as dev-oidc-issuer keeps a
// listed caller's token for devrelay on the local stack.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestATokenFileItCannotUseSendsNothing pins that a missing or empty account token file is an
// error naming the file, before any request: the broker is never sent an empty bearer to refuse.
func TestATokenFileItCannotUseSendsNothing(t *testing.T) {
	var reached atomic.Bool
	broker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	t.Cleanup(broker.Close)
	missing := filepath.Join(t.TempDir(), "missing")
	empty := tokenFile(t, "")
	for _, tc := range []struct{ file, says string }{
		{missing, "read the account token: open " + missing},
		{empty, "the account token file " + empty + " is empty"},
	} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"machine-approve", "--code", "KQ7M-X4PZ", "--broker", broker.URL, "--account-token-file", tc.file, "--login", testApprover}, &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), tc.says) {
			t.Errorf("--account-token-file %s: exit %d, stderr %q; want 1 saying %q", tc.file, code, stderr.String(), tc.says)
		}
	}
	if reached.Load() {
		t.Error("the broker was called with a token file devrelay could not use")
	}
}

// TestDevStackEnrollsRequestsApprovesAndReleases runs the dev stack's whole story end to end: a
// machine logs in and devrelay approves its typed code by the operator's login, the resulting
// launcher credential enrolls a box, the box asks for an approval-required secret, the operator
// signs in to the broker's own page and approves that record there (no relay can decide it), and
// the grant releases the fake secrets store's value.
func TestDevStackEnrollsRequestsApprovesAndReleases(t *testing.T) {
	brokerURL, callerToken, issuer := newDevrelayTestServer(t)

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
		RequestID  string  `json:"request_id"`
		State      string  `json:"state"`
		RecordID   *string `json:"record_id"`
		ApproveURL string  `json:"approve_url"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.State != "pending" || created.RecordID == nil {
		t.Fatalf("create response = %s (%v), want a pending request with a record_id", body, err)
	}
	if page := brokerURL + "/approve/" + *created.RecordID; created.ApproveURL != page {
		t.Fatalf("create response approve_url = %q, want the record's page %s", created.ApproveURL, page)
	}

	// --- approve: the operator signs in to the broker's page and approves the record there ---
	brokertest.Decide(t, brokerURL, issuer, testApprover, *created.RecordID, true)
	statusProof, err := proof.Sign(sessionKey, enrolled.EnrollmentID, http.MethodGet, brokerURL+"/v1/requests/"+created.RequestID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, body = req(t, http.MethodGet, brokerURL+"/v1/requests/"+created.RequestID, map[string]string{"Proof": statusProof}, nil)
	var approved struct {
		State   string  `json:"state"`
		GrantID *string `json:"grant_id"`
	}
	if err := json.Unmarshal(body, &approved); err != nil || status != http.StatusOK || approved.State != "granted" || approved.GrantID == nil {
		t.Fatalf("GET /v1/requests/{id} after the page's approval = %d %s (%v), want state=granted with a grant_id", status, body, err)
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
