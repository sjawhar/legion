// api_test.go mounts the real broker handlers (api.Register) on a real Postgres test store — no
// fakes: every dependency (enroll.Service, requests.Machine, machine.Service) is the genuine
// article, wired exactly as cmd/broker/main.go wires it, with real signed request/proof JWS
// objects. A human decision is a UI-route call naming the approver's login, the body Dispatch's
// server sends. It rebuilds route-level coverage for every row of routes_table.go's table.
package api_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/ratelimit"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	testUIToken = "test-ui-token-0123456789abcdef"
	// testApprover owns DEEL_API_KEY, a human-tier secret, so every request for it is an approval
	// request to them; they are also every machine login's login_hint below.
	testApprover = "sami@example.com"
)

// testServer is a live broker HTTP server (real handlers, real Postgres) plus a direct handle to
// its store — needed to seed fixtures (an enrollment, a launcher credential) the API itself has
// no route to create directly — the secrets.Local standing in for Secrets Manager, which a test
// writes to as a person would, and the local OIDC issuer its pod verifier trusts, which mints the
// projected service-account tokens a pod enrollment presents.
type testServer struct {
	URL       string
	Store     *store.Store
	Secrets   *secrets.Local
	podIssuer *oidctest.Issuer
	podKey    *oidctest.Key
	// ServiceLogins is the broker's live BROKER_SERVICE_LOGINS, daemonAccount and
	// otherDaemonAccount each logging in as legion-daemon; a test removes an entry to take an
	// account off it.
	ServiceLogins map[string]string
}

// podAudience is the audience the test server's pod verifier checks and podToken mints for.
const podAudience = "legion-broker-pod"

// daemonAccount and otherDaemonAccount are the ServiceAccounts the test server's
// BROKER_SERVICE_LOGINS lets log in as legion-daemon with their tokens, as two Legion daemons' own
// accounts do.
const (
	daemonAccount      = "system:serviceaccount:legion-system:legion-daemon-test"
	otherDaemonAccount = "system:serviceaccount:legion-system:legion-daemon-other"
)

// newTestServer holds three secrets: DEEL_API_KEY, human-tier, which testApprover owns and so
// approves for every requester; WORKER_TOKEN, shared and agent-tier, which every session and pod
// gets at once; and SHARED_KEY, shared and human-tier, which anyone signed in approves. It wires a
// real pod verifier against a local OIDC issuer, as cmd/broker/main.go does when
// BROKER_K8S_OIDC_ISSUER is set, and mounts api.Register on an httptest.Server so every proof's
// htu and every request object's aud have one real, consistent PublicURL to check against.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	return newTestServerWith(t, func(*api.Deps) {})
}

// newTestServerWith is newTestServer with adjust applied to the api.Deps it registers, for a test
// that needs one dependency wired otherwise, such as a tighter rate limit.
func newTestServerWith(t *testing.T, adjust func(*api.Deps)) *testServer {
	t.Helper()
	st := storetest.Open(t)

	local := secrets.NewLocal(
		policytest.Secret("DEEL_API_KEY", testApprover, policy.TierHuman, "deel-v1"),
		policytest.Secret("WORKER_TOKEN", policy.OwnerShared, policy.TierAgent, "worker-v1"),
		policytest.Secret("SHARED_KEY", policy.OwnerShared, policy.TierHuman, "shared-v1"),
	)
	cur := policytest.Current(t, local)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	podVerifier, err := oidc.New(context.Background(), issuer.URL(), podAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	serviceLogins := map[string]string{daemonAccount: "legion-daemon", otherDaemonAccount: "legion-daemon"}
	enr := &enroll.Service{Store: st, Lease: time.Hour, Pod: enroll.K8sPodVerifier{Verifier: podVerifier}, ServiceLogins: serviceLogins}
	enr.Chain = enroll.NewChainVerifier(st, srv.URL, time.Minute)

	reqMachine := &requests.Machine{
		Store: st, Policy: cur, Secrets: policytest.Store(local),
		PendingTTL: 12 * time.Hour,
		Audience:   srv.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	reqMachine.Chain = requests.NewChainVerifier(st, srv.URL, time.Minute)
	provider := brokertest.NewOAuthProvider(t, st)
	mach := &machine.Service{
		Store: st, Enroll: enr, Policy: cur,
		Audience: srv.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		OAuth: provider, Replay: enr.Replay,
	}

	deps := api.Deps{
		PublicURL: srv.URL, UIToken: testUIToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach,
		OAuth: provider, DeviceLogin: mach.LoginDevice, TokenHooks: mach.TokenHooks(),
		Proof:  &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: cur, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN,
	}
	adjust(&deps)
	api.Register(mux, deps)

	return &testServer{URL: srv.URL, Store: st, Secrets: local, podIssuer: issuer, podKey: podKey, ServiceLogins: serviceLogins}
}

// newSessionEnrollment inserts a live session enrollment (and its backing launcher_credentials
// row) directly, mirroring requests/machine_test.go's own newEnrollment: enroll.Service.Create is
// exercised end to end by the launcher-proof enrollment routes themselves (see
// TestMachineLoginApprovalMintsAKeyBoundLauncherCredentialForEnrollment), so session-route tests
// need only a live row to authenticate a session proof against.
func (ts *testServer) newSessionEnrollment(t *testing.T, runtimeID, operator string) (id string, key *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	key = newSigningKey(t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	credentialID := uuid.New()
	if _, err := ts.Store.Pool.Exec(ctx, `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,'test-host',$3,'{}'::jsonb, now() + interval '30 days')`, credentialID, operator, thumbprint); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	id = uuid.NewString()
	if _, err := ts.Store.Pool.Exec(ctx, `insert into enrollments (id, kind, runtime_id, operator, thumbprint, launcher_credential_id, lease_expires_at)
		values ($1,'session',$2,$3,$4,$5, now() + interval '1 hour')`, id, runtimeID, operator, thumbprint, credentialID); err != nil {
		t.Fatalf("insert enrollments: %v", err)
	}
	return id, key
}

// newSigningKey is brokertest.NewSigningKey, under the name every test file of this package
// calls it by: one P-256 key helper for the whole repository, kept in the package that already
// owns the broker's test rig. It is a variable rather than a wrapper function so that a failure
// inside it is still reported against the caller's line, as that helper's own t.Helper makes it.
var newSigningKey = brokertest.NewSigningKey

// --- HTTP helpers ---

// req issues method against path with an optional body (marshaled to JSON) and headers, returning
// the decoded status and body bytes. It never follows the caller into asserting a status: every
// test does that itself so a mismatch names the actual body.
func (ts *testServer) req(t *testing.T, method, path string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	request, err := http.NewRequest(method, ts.URL+path, reader)
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
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, respBody
}

func (ts *testServer) ui(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	return ts.req(t, method, path, map[string]string{"Authorization": "Bearer " + testUIToken}, body)
}

func (ts *testServer) session(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, method, path string, body any) (int, []byte) {
	t.Helper()
	p, err := proof.Sign(key, enrollmentID, method, ts.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	return ts.req(t, method, path, map[string]string{"Proof": p}, body)
}

func (ts *testServer) launcher(t *testing.T, key *ecdsa.PrivateKey, launcherID, method, path string, body any) (int, []byte) {
	t.Helper()
	p, err := proof.SignLauncher(key, launcherID, method, ts.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.SignLauncher: %v", err)
	}
	return ts.req(t, method, path, map[string]string{"Proof": p}, body)
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %s: %v (body: %s)", "response", err, body)
	}
	return v
}

// --- wire-shape mirrors of the shared broker contract, for decoding response bodies ---

type wireError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

type wireEnrollmentInfo struct {
	Kind      string  `json:"kind"`
	RuntimeID string  `json:"runtime_id"`
	Operator  string  `json:"operator"`
	Slot      *string `json:"slot"`
}

type wireDecided struct {
	Event        string    `json:"event"`
	At           time.Time `json:"at"`
	CredentialID *string   `json:"credential_id"`
}

// wireSession is a record's or a pending entry's "session" field (LEGION-587): the request's own
// override and its requesting enrollment's id, read independently.
type wireSession struct {
	Request    *string `json:"request"`
	Enrollment *string `json:"enrollment"`
}

type wireRecord struct {
	RecordID        string              `json:"record_id"`
	Kind            string              `json:"kind"`
	State           string              `json:"state"`
	Approver        string              `json:"approver"`
	Enrollment      *wireEnrollmentInfo `json:"enrollment"`
	Session         wireSession         `json:"session"`
	Identifiers     []string            `json:"identifiers"`
	Service         *string             `json:"service"`
	Reason          string              `json:"reason"`
	LifetimeSeconds *int                `json:"lifetime_seconds"`
	RulesVersion    string              `json:"rules_version"`
	ExpiresAt       time.Time           `json:"expires_at"`
	RequestedAt     time.Time           `json:"requested_at"`
	Decided         *wireDecided        `json:"decided"`
}

type wireCreateRequestResponse struct {
	RequestID string  `json:"request_id"`
	State     string  `json:"state"`
	GrantID   *string `json:"grant_id"`
	RecordID  *string `json:"record_id"`
	Coalesced bool    `json:"coalesced"`
}

type wireRequestDecision struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

type wireRequestStatus struct {
	State     string               `json:"state"`
	GrantID   *string              `json:"grant_id"`
	RecordID  *string              `json:"record_id"`
	DecidedAt *time.Time           `json:"decided_at"`
	Decision  *wireRequestDecision `json:"decision"`
}

type wirePendingEntry struct {
	RecordID    string      `json:"record_id"`
	Kind        string      `json:"kind"`
	Identifiers []string    `json:"identifiers"`
	RequestedAt time.Time   `json:"requested_at"`
	Session     wireSession `json:"session"`
}

type wireApproverGrant struct {
	GrantID    string             `json:"grant_id"`
	Granted    string             `json:"granted"`
	RecordID   *string            `json:"record_id"`
	Enrollment wireEnrollmentInfo `json:"enrollment"`
	Names      []string           `json:"names"`
	Approver   *string            `json:"approver"`
	ExpiresAt  *time.Time         `json:"expires_at"`
	CreatedAt  time.Time          `json:"created_at"`
}

// --- signing helpers ---

func signAgentSecretRequest(t *testing.T, key *ecdsa.PrivateKey, audience, reason string, names ...string) string {
	t.Helper()
	details := make([]record.AuthorizationDetail, len(names))
	for i, n := range names {
		details[i] = record.AuthorizationDetail{Type: "agent_secret", Identifier: n, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(key, audience, details, reason, "", nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	return compact
}

func signAgentSecretRequestFor(t *testing.T, key *ecdsa.PrivateKey, audience, reason string, lifetime int, names ...string) string {
	t.Helper()
	details := make([]record.AuthorizationDetail, len(names))
	for i, n := range names {
		details[i] = record.AuthorizationDetail{Type: "agent_secret", Identifier: n, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(key, audience, details, reason, "", &lifetime, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	return compact
}

// signMachineLoginRequest signs a machine login's request object for one launcher_credential
// detail on host, naming service ("" for a person's machine) and carrying loginHint ("" for none).
func signMachineLoginRequest(t *testing.T, key *ecdsa.PrivateKey, audience, loginHint, host, service string) string {
	t.Helper()
	compact, err := record.Sign(key, audience, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: host, Service: service},
	}, "", loginHint, nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	return compact
}

// --- tests ---

// TestHealthzIsPublic also pins what a broker with no Deps.Draining answers, the nil every test
// and every in-process rig leaves it: a process that never stops is never draining, so the ping
// alone decides.
func TestHealthzIsPublic(t *testing.T) {
	ts := newTestServer(t)
	status, _ := ts.req(t, http.MethodGet, "/healthz", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", status)
	}
}

// TestHealthzFailsWhileDrainingAndNothingElseDoes pins the drain a stopping pod answers with. The
// code in the body is what is asserted, not the status alone, which a broker that cannot reach
// Postgres answers too. A terminating pod leaves its Service's EndpointSlice concurrently with
// the kubelet beginning termination, so the whole point of failing the health check is that the
// pod goes on serving the requests still being routed to it: GET /v1/settings, a public route of
// the same mux, must answer exactly as it always does while /healthz is refusing.
func TestHealthzFailsWhileDrainingAndNothingElseDoes(t *testing.T) {
	var draining atomic.Bool
	ts := newTestServerWith(t, func(d *api.Deps) { d.Draining = draining.Load })

	if status, body := ts.req(t, http.MethodGet, "/healthz", nil, nil); status != http.StatusOK {
		t.Fatalf("GET /healthz before the drain = %d %s, want 200", status, body)
	}
	draining.Store(true)
	status, body := ts.req(t, http.MethodGet, "/healthz", nil, nil)
	if status != http.StatusServiceUnavailable || decode[wireError](t, body).Code != "SHUTTING_DOWN" {
		t.Fatalf("GET /healthz while draining = %d %s, want 503 SHUTTING_DOWN", status, body)
	}
	if status, body := ts.req(t, http.MethodGet, "/v1/settings", nil, nil); status != http.StatusOK {
		t.Fatalf("GET /v1/settings while draining = %d %s, want the 200 it always answers", status, body)
	}
}

// TestUIRouteWithoutBearerIsUnauthorized pins uiAuth's own contract: no bearer, or the wrong one,
// is 401 UI_INVALID — never silently open.
func TestUIRouteWithoutBearerIsUnauthorized(t *testing.T) {
	ts := newTestServer(t)
	for name, headers := range map[string]map[string]string{
		"no header":    {},
		"wrong bearer": {"Authorization": "Bearer wrong-token"},
		"empty bearer": {"Authorization": "Bearer "},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := ts.req(t, http.MethodGet, "/v1/pending?approver=sjawhar", headers, nil)
			if status != http.StatusUnauthorized {
				t.Fatalf("GET /v1/pending (%s) = %d, want 401: %s", name, status, body)
			}
			werr := decode[wireError](t, body)
			if werr.Code != "UI_INVALID" {
				t.Fatalf("code = %q, want UI_INVALID", werr.Code)
			}
		})
	}
}

// TestEnrollmentRouteWithOldBearerHeaderIsLauncherInvalid pins that the v8 bearer credential no
// longer authenticates the enrollment routes at all: only a launcher proof (Proof header, "lid"
// claim) does now.
func TestEnrollmentRouteWithOldBearerHeaderIsLauncherInvalid(t *testing.T) {
	ts := newTestServer(t)
	status, body := ts.req(t, http.MethodPost, "/v1/enrollments",
		map[string]string{"Authorization": "Bearer some-v8-style-launcher-token", "Content-Type": "application/json"},
		map[string]any{"kind": "session", "runtime_id": "session-1", "operator": "sjawhar", "thumbprint": "irrelevant"})
	if status != http.StatusUnauthorized {
		t.Fatalf("POST /v1/enrollments (bearer) = %d, want 401: %s", status, body)
	}
	werr := decode[wireError](t, body)
	if werr.Code != "LAUNCHER_INVALID" {
		t.Fatalf("code = %q, want LAUNCHER_INVALID", werr.Code)
	}
}

// deviceLogin asks POST /v1/oauth/device_authorization for a machine login signed by key, naming
// loginHint, host and service, with dpop_jkt key's thumbprint, as the helper does. It answers the
// status and the decoded body: the device code and user code, or OAuth's error.
func (ts *testServer) deviceLogin(t *testing.T, key *ecdsa.PrivateKey, loginHint, host, service string) (int, map[string]any) {
	t.Helper()
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"client_id": {oauthwire.ClientID}, "scope": {"machine"}, "dpop_jkt": {jkt},
		"request": {signMachineLoginRequest(t, key, ts.URL, loginHint, host, service)},
	}
	resp, err := http.Post(ts.URL+"/v1/oauth/device_authorization", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode the device authorization's answer (%d): %v", resp.StatusCode, err)
	}
	return resp.StatusCode, body
}

// openMachineLogin opens a person's machine login for key on host through the device grant and
// answers its user code, the confirmation code the person types into Dispatch.
func (ts *testServer) openMachineLogin(t *testing.T, key *ecdsa.PrivateKey, loginHint, host string) string {
	t.Helper()
	status, body := ts.deviceLogin(t, key, loginHint, host, "")
	code, _ := body["user_code"].(string)
	if status != http.StatusOK || code == "" {
		t.Fatalf("POST /v1/oauth/device_authorization = %d %v, want 200 and a user code", status, body)
	}
	return code
}

// mintLauncherCredential drives a full typed-code machine login through the HTTP API — the device
// authorization, Dispatch's lookup by code and the approver's approval — and returns the resulting
// launcher credential's id and the machine's own signing key, for tests that just need a live
// launcher credential to sign proofs with.
func (ts *testServer) mintLauncherCredential(t *testing.T, loginHint, host string) (credentialID string, key *ecdsa.PrivateKey) {
	t.Helper()
	key = newSigningKey(t)
	code := ts.openMachineLogin(t, key, loginHint, host)
	_, body := ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	looked := decode[wireRecord](t, body)
	_, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve",
		map[string]any{"approver": loginHint, "code": code})
	approved := decode[struct {
		CredentialID *string `json:"credential_id"`
	}](t, body)
	if approved.CredentialID == nil || *approved.CredentialID == "" {
		t.Fatalf("approve machine login: answered %s, want a credential_id", body)
	}
	return *approved.CredentialID, key
}

// TestSessionProofRejectedOnLauncherAuthRoute pins authenticate()'s authLauncher guard: a VALID
// session proof (an "eid" claim, signed by a live enrollment's own key) is refused with
// 401 LAUNCHER_INVALID exactly as an unsigned bearer is — it must never be treated as a launcher
// proof merely because it verifies. Without this guard the handler would receive an empty
// enroll.Credential and either fail differently downstream or, worse, succeed with the zero value.
func TestSessionProofRejectedOnLauncherAuthRoute(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), "sjawhar")

	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "session", "runtime_id": "session-other", "operator": "sjawhar", "thumbprint": "irrelevant",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("POST /v1/enrollments (valid session proof) = %d, want 401: %s", status, body)
	}
	werr := decode[wireError](t, body)
	if werr.Code != "LAUNCHER_INVALID" {
		t.Fatalf("code = %q, want LAUNCHER_INVALID", werr.Code)
	}
}

// TestLauncherProofRejectedOnSessionAuthRoute pins the mirror guard on authProof: a VALID
// launcher proof (an "lid" claim, signed by a live launcher credential's own key) is refused with
// 401 PROOF_INVALID — it must never be treated as a session proof merely because it verifies.
func TestLauncherProofRejectedOnSessionAuthRoute(t *testing.T) {
	ts := newTestServer(t)
	credentialID, machineKey := ts.mintLauncherCredential(t, "sjawhar", "example-host-devbox")

	status, body := ts.launcher(t, machineKey, credentialID, http.MethodPost, "/v1/requests", map[string]any{
		"request": "irrelevant", "session_id": nil,
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("POST /v1/requests (valid launcher proof) = %d, want 401: %s", status, body)
	}
	werr := decode[wireError](t, body)
	if werr.Code != "PROOF_INVALID" {
		t.Fatalf("code = %q, want PROOF_INVALID", werr.Code)
	}
}

// TestMachineLoginApprovalMintsAKeyBoundLauncherCredentialForEnrollment drives the whole typed-code
// machine-login flow through the HTTP API end to end, then uses the resulting launcher credential
// to exercise both launcher-proof enrollment routes: deviceAuthorization, lookupMachineLogin,
// approveRecord dispatching to the machine kind (refusing any login but the record's approver),
// createEnrollment, and deleteEnrollment.
func TestMachineLoginApprovalMintsAKeyBoundLauncherCredentialForEnrollment(t *testing.T) {
	ts := newTestServer(t)
	machineKey := newSigningKey(t)
	login := struct{ Code string }{ts.openMachineLogin(t, machineKey, testApprover, "example-host-devbox")}

	status, body := ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": login.Code})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/machine-logins/lookup = %d: %s", status, body)
	}
	looked := decode[wireRecord](t, body)
	if looked.Kind != "launcher_credential" || looked.State != "pending" || looked.Approver != testApprover {
		t.Fatalf("lookup = %+v, want kind=launcher_credential state=pending approver=%s", looked, testApprover)
	}

	status, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+looked.RecordID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/credential-requests/{machine record} = %d: %s", status, body)
	}
	plainRead := decode[wireRecord](t, body)
	if plainRead.Enrollment != nil {
		t.Fatalf("machine record enrollment = %+v, want nil (no requesting enrollment)", plainRead.Enrollment)
	}
	if plainRead.LifetimeSeconds == nil || *plainRead.LifetimeSeconds != int((7*24*time.Hour).Seconds()) {
		t.Fatalf("machine record lifetime_seconds = %v, want the credential's week", plainRead.LifetimeSeconds)
	}

	// The code selects the login, but only its approver (the login_hint) decides it.
	status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve",
		map[string]any{"approver": "mallory", "code": login.Code})
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
		t.Fatalf("approve machine record as mallory = %d %s, want 403 NOT_APPROVER", status, body)
	}

	status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve",
		map[string]any{"approver": testApprover, "code": login.Code})
	if status != http.StatusOK {
		t.Fatalf("approve machine record = %d: %s", status, body)
	}
	approved := decode[struct {
		State        string  `json:"state"`
		GrantID      *string `json:"grant_id"`
		CredentialID *string `json:"credential_id"`
	}](t, body)
	if approved.State != "approved" || approved.GrantID != nil || approved.CredentialID == nil || *approved.CredentialID == "" {
		t.Fatalf("approve response = %+v, want state=approved grant_id=nil credential_id set", approved)
	}
	credentialID := *approved.CredentialID

	var minted time.Time
	if err := ts.Store.Pool.QueryRow(context.Background(), `select expires_at from launcher_credentials where id=$1`, credentialID).Scan(&minted); err != nil {
		t.Fatalf("read the minted credential's expiry: %v", err)
	}
	if want := time.Now().Add(7 * 24 * time.Hour); minted.Before(want.Add(-time.Minute)) || minted.After(want) {
		t.Fatalf("minted credential expires %s, want CredentialLifetime from the approval, about %s", minted, want)
	}
	_, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+looked.RecordID, nil)
	if decided := decode[wireRecord](t, body).Decided; decided == nil || decided.CredentialID == nil || *decided.CredentialID != credentialID {
		t.Fatalf("decided machine record = %+v, want its decision to name credential %s", decided, credentialID)
	}

	// The minted launcher credential now authenticates POST /v1/enrollments and DELETE
	// /v1/enrollments/{id} via a launcher proof signed by the machine's own key.
	sessionKey := newSigningKey(t)
	sessionThumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	status, body = ts.launcher(t, machineKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "session", "runtime_id": "session-" + t.Name(), "operator": testApprover, "thumbprint": sessionThumbprint,
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/enrollments (launcher proof) = %d, want 201: %s", status, body)
	}
	created := decode[struct {
		EnrollmentID string `json:"enrollment_id"`
	}](t, body)
	if created.EnrollmentID == "" {
		t.Fatalf("created enrollment has no id: %s", body)
	}

	status, body = ts.launcher(t, machineKey, credentialID, http.MethodDelete, "/v1/enrollments/"+created.EnrollmentID, nil)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE /v1/enrollments/{id} = %d, want 204: %s", status, body)
	}
}

// TestTheHandBuiltMachineLoginRoutesAreGone pins the removal of the login helpers before the
// device grant used: POST /v1/launcher-credentials answers the router's 405 (Dispatch's GET on the
// same path remains) and GET /v1/launcher-credentials/{pending} its 404, neither opening a record,
// while the routes Dispatch and the Legion daemon call under the same prefix still answer as they
// did.
func TestTheHandBuiltMachineLoginRoutesAreGone(t *testing.T) {
	ts := newTestServer(t)
	request := signMachineLoginRequest(t, newSigningKey(t), ts.URL, testApprover, "example-host-devbox", "")
	if status, body := ts.req(t, http.MethodPost, "/v1/launcher-credentials", nil, map[string]any{"request": request}); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/launcher-credentials = %d %s, want 405", status, body)
	}
	for _, pending := range []string{"some-pending-id", uuid.NewString()} {
		if status, body := ts.req(t, http.MethodGet, "/v1/launcher-credentials/"+pending, nil, nil); status != http.StatusNotFound {
			t.Fatalf("GET /v1/launcher-credentials/%s = %d %s, want 404", pending, status, body)
		}
	}
	if got := ts.column(t, `select count(*)::text from credential_requests`); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("credential-request records after the removed routes = %v, want none", got)
	}
	if status, body := ts.ui(t, http.MethodGet, "/v1/launcher-credentials?approver="+testApprover, nil); status != http.StatusOK {
		t.Fatalf("Dispatch's GET /v1/launcher-credentials = %d %s, want 200", status, body)
	}
	if status, body := ts.ui(t, http.MethodPost, "/v1/launcher-credentials/"+uuid.NewString()+"/revoke-by-approver", map[string]any{"approver": testApprover}); status != http.StatusNotFound || decode[wireError](t, body).Code != "NOT_FOUND" {
		t.Fatalf("Dispatch's revoke of an unknown login = %d %s, want its own 404 NOT_FOUND", status, body)
	}
	if status, body := ts.tokenLogin(t, "not-a-token", ts.daemonRequest(t, newSigningKey(t))); status != http.StatusUnauthorized {
		t.Fatalf("the daemon's POST /v1/launcher-credentials/token = %d %s, want its own 401", status, body)
	}
}

// TestMachineLoginLookupUnknownCodeIsNoSuchCode pins the 404 NO_SUCH_CODE contract for a code that
// names no pending machine login.
func TestMachineLoginLookupUnknownCodeIsNoSuchCode(t *testing.T) {
	ts := newTestServer(t)
	status, body := ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": "ZZZZ-ZZZZ"})
	if status != http.StatusNotFound {
		t.Fatalf("lookup unknown code = %d, want 404: %s", status, body)
	}
	werr := decode[wireError](t, body)
	if werr.Code != "NO_SUCH_CODE" {
		t.Fatalf("code = %q, want NO_SUCH_CODE", werr.Code)
	}
}

// TestApproveMachineRecordWithoutCodeIsCodeRequired pins the 400 CODE_REQUIRED contract: a machine
// record cannot be decided without the confirmation code, even by its own approver.
func TestApproveMachineRecordWithoutCodeIsCodeRequired(t *testing.T) {
	ts := newTestServer(t)
	login := struct{ Code string }{ts.openMachineLogin(t, newSigningKey(t), testApprover, "example-host-devbox")}

	_, body := ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": login.Code})
	looked := decode[wireRecord](t, body)

	status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve",
		map[string]any{"approver": testApprover})
	if status != http.StatusBadRequest {
		t.Fatalf("approve without code = %d, want 400: %s", status, body)
	}
	werr := decode[wireError](t, body)
	if werr.Code != "CODE_REQUIRED" {
		t.Fatalf("code = %q, want CODE_REQUIRED", werr.Code)
	}
}

// TestAgentSecretRequestLifecycle drives an approval-needing agent_secret request end to end:
// creation (session proof, naming an explicit session_id override), the UI's pending list and
// record read (with an enrollment, its session, and no code required), approval by the record's
// approver, the session's own status/values reads, the UI's grant list, and human revocation,
// refused for any login but the approver's.
func TestAgentSecretRequestLifecycle(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)

	const sessionID = "sess-agent-secret-lifecycle"
	compact := signAgentSecretRequest(t, sessionKey, ts.URL, "need it for the demo", "DEEL_API_KEY")
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": sessionID})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d, want 200: %s", status, body)
	}
	created := decode[wireCreateRequestResponse](t, body)
	if created.State != "pending" || created.RecordID == nil || *created.RecordID == "" {
		t.Fatalf("create response = %+v, want state=pending with a record_id", created)
	}
	requestID, recordID := created.RequestID, *created.RecordID

	status, body = ts.ui(t, http.MethodGet, "/v1/pending?approver="+testApprover, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/pending = %d: %s", status, body)
	}
	pendingList := decode[struct {
		Pending []wirePendingEntry `json:"pending"`
	}](t, body)
	found := false
	for _, p := range pendingList.Pending {
		if p.RecordID == recordID {
			found = true
			if p.Kind != "agent_secret" || len(p.Identifiers) != 1 || p.Identifiers[0] != "DEEL_API_KEY" {
				t.Fatalf("pending entry = %+v, want kind=agent_secret identifiers=[DEEL_API_KEY]", p)
			}
			if p.Session.Request == nil || *p.Session.Request != sessionID || p.Session.Enrollment != nil {
				t.Fatalf("pending entry session = %+v, want request=%s and no enrollment session_id (this box enrolled none)", p.Session, sessionID)
			}
		}
	}
	if !found {
		t.Fatalf("GET /v1/pending = %+v, want record %s listed", pendingList, recordID)
	}

	status, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/credential-requests/{id} = %d: %s", status, body)
	}
	readBack := decode[wireRecord](t, body)
	if readBack.State != "pending" || readBack.Approver != testApprover {
		t.Fatalf("record read = %+v, want pending with approver %s", readBack, testApprover)
	}
	if readBack.Enrollment == nil || readBack.Enrollment.Kind != "session" || readBack.Enrollment.Operator != testApprover || readBack.Enrollment.Slot != nil {
		t.Fatalf("record enrollment = %+v, want kind=session operator=%s and no slot", readBack.Enrollment, testApprover)
	}
	if readBack.LifetimeSeconds != nil {
		t.Fatalf("record lifetime_seconds = %d, want null for a request naming no lifetime (until the session ends)", *readBack.LifetimeSeconds)
	}
	if readBack.Session.Request == nil || *readBack.Session.Request != sessionID || readBack.Session.Enrollment != nil {
		t.Fatalf("record session = %+v, want request=%s and no enrollment session_id", readBack.Session, sessionID)
	}

	status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"approver": testApprover})
	if status != http.StatusOK {
		t.Fatalf("approve = %d: %s", status, body)
	}
	approved := decode[struct {
		State        string  `json:"state"`
		GrantID      *string `json:"grant_id"`
		CredentialID *string `json:"credential_id"`
	}](t, body)
	if approved.State != "approved" || approved.GrantID == nil || *approved.GrantID == "" || approved.CredentialID != nil {
		t.Fatalf("approve response = %+v, want state=approved grant_id set credential_id=nil", approved)
	}
	grantID := *approved.GrantID

	status, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/credential-requests/{id} (after approval) = %d: %s", status, body)
	}
	decidedRead := decode[wireRecord](t, body)
	if decidedRead.State != "approved" || decidedRead.Decided == nil || decidedRead.Decided.Event != "approved" || decidedRead.Decided.CredentialID != nil {
		t.Fatalf("decided record read = %+v, want state=approved with its approved event and a null credential_id", decidedRead)
	}
	if decidedRead.Session.Request == nil || *decidedRead.Session.Request != sessionID {
		t.Fatalf("decided record session = %+v, want request=%s still named after approval", decidedRead.Session, sessionID)
	}

	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/requests/"+requestID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/requests/{id} = %d: %s", status, body)
	}
	statusResp := decode[wireRequestStatus](t, body)
	if statusResp.State != "granted" || statusResp.GrantID == nil || *statusResp.GrantID != grantID {
		t.Fatalf("request status = %+v, want granted with grant_id %s", statusResp, grantID)
	}
	if statusResp.Decision == nil || statusResp.Decision.By != testApprover {
		t.Fatalf("request decision = %+v, want by=%s", statusResp.Decision, testApprover)
	}

	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/grants/{id}/values = %d: %s", status, body)
	}
	values := decode[struct {
		Values map[string]string `json:"values"`
	}](t, body)
	if values.Values["DEEL_API_KEY"] != "deel-v1" {
		t.Fatalf("values = %+v, want DEEL_API_KEY=deel-v1", values.Values)
	}

	status, body = ts.ui(t, http.MethodGet, "/v1/grants?approver="+testApprover, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/grants = %d: %s", status, body)
	}
	grantList := decode[struct {
		Grants []wireApproverGrant `json:"grants"`
	}](t, body)
	var listed *wireApproverGrant
	for i, g := range grantList.Grants {
		if g.GrantID == grantID {
			listed = &grantList.Grants[i]
		}
	}
	if listed == nil {
		t.Fatalf("GET /v1/grants = %+v, want grant %s listed", grantList, grantID)
	}
	if len(listed.Names) != 1 || listed.Names[0] != "DEEL_API_KEY" || listed.Granted != "approval" || listed.Approver == nil || *listed.Approver != testApprover ||
		listed.RecordID == nil || *listed.RecordID != recordID || listed.Enrollment.Slot != nil {
		t.Fatalf("listed grant = %+v, want names [DEEL_API_KEY] granted on approval by %s, resting on record %s, on an enrollment with no slot", listed, testApprover, recordID)
	}
	if listed.ExpiresAt != nil {
		t.Fatalf("listed grant expires_at = %s, want null for a grant that lasts until its session ends", listed.ExpiresAt)
	}

	// Revoking by approver takes the grant's approver or its enrollment's operator (both
	// testApprover here); any other login is refused.
	status, body = ts.ui(t, http.MethodPost, "/v1/grants/"+grantID+"/revoke-by-approver",
		map[string]any{"approver": "mallory"})
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
		t.Fatalf("revoke-by-approver as mallory = %d %s, want 403 NOT_APPROVER", status, body)
	}
	status, body = ts.ui(t, http.MethodPost, "/v1/grants/"+grantID+"/revoke-by-approver",
		map[string]any{"approver": testApprover})
	if status != http.StatusOK {
		t.Fatalf("revoke-by-approver = %d: %s", status, body)
	}

	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
	if status != http.StatusForbidden {
		t.Fatalf("values after revoke = %d, want 403: %s", status, body)
	}
	werr := decode[wireError](t, body)
	if werr.Code != "GRANT_NOT_LIVE" {
		t.Fatalf("code = %q, want GRANT_NOT_LIVE", werr.Code)
	}

	status, body = ts.ui(t, http.MethodGet, "/v1/grants?approver="+testApprover, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/grants (after revoke) = %d: %s", status, body)
	}
	grantList = decode[struct {
		Grants []wireApproverGrant `json:"grants"`
	}](t, body)
	for _, g := range grantList.Grants {
		if g.GrantID == grantID {
			t.Fatalf("revoked grant %s still listed: %+v", grantID, grantList)
		}
	}
}

// TestCreateRequestWithProofTypJWSIs400 pins that a proof (not a request object) in the "request"
// field is refused: the two are disjoint JWS typ values by design.
func TestCreateRequestWithProofTypJWSIs400(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), "sjawhar")

	proofNotRequest, err := proof.Sign(sessionKey, enrollmentID, http.MethodPost, ts.URL+"/v1/requests", time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": proofNotRequest, "session_id": nil})
	if status != http.StatusBadRequest {
		t.Fatalf("POST /v1/requests (proof-typ JWS) = %d, want 400: %s", status, body)
	}
}

// TestCreateRequestWithUnknownSecretNameIs400UnknownSecret pins, over real HTTP, that a request
// naming a secret the policy does not serve is 400 UNKNOWN_SECRET at record time, not folded into
// an ordinary deny decision.
func TestCreateRequestWithUnknownSecretNameIs400UnknownSecret(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), "sjawhar")
	compact := signAgentSecretRequest(t, sessionKey, ts.URL, "need it", "NOT_A_REAL_SECRET")
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusBadRequest {
		t.Fatalf("POST /v1/requests (unknown secret) = %d, want 400: %s", status, body)
	}
	werr := decode[wireError](t, body)
	if werr.Code != "UNKNOWN_SECRET" {
		t.Fatalf("code = %q, want UNKNOWN_SECRET", werr.Code)
	}
}

func TestCreateRequestWithMaxGrantNeedsLifetime(t *testing.T) {
	ts := newTestServer(t)
	limited := policytest.KindSecret("LIMITED_KEY", testApprover, policy.TierAgent, policy.KindGoogleLogin, "value")
	limited.Tags[policy.TagMaxGrant] = "1h"
	ts.Secrets.Put(limited)
	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/LIMITED_KEY/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("reread LIMITED_KEY = %d %s", status, body)
	}
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	compact := signAgentSecretRequest(t, sessionKey, ts.URL, "need limited login", "LIMITED_KEY")
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusBadRequest {
		t.Fatalf("POST /v1/requests = %d, want 400: %s", status, body)
	}
	reply := decode[wireError](t, body)
	if reply.Code != "GRANT_REQUIRES_LIFETIME" || !strings.Contains(reply.Error, "LIMITED_KEY") || !strings.Contains(reply.Error, "1h") {
		t.Fatalf("POST /v1/requests = %+v, want GRANT_REQUIRES_LIFETIME naming LIMITED_KEY and 1h", reply)
	}
}

// TestAFailedMintIs502MintFailed: a kind secret whose minter fails, such as a Google login that
// needs a new sign-in, answers the session 502 MINT_FAILED with the minter's words, so it can tell
// its person what to fix, rather than an opaque 500; and the broker logs it at ERROR for its
// operator, naming the secret and the error's class, never the stored value.
func TestAFailedMintIs502MintFailed(t *testing.T) {
	logged := policytest.CaptureLog(t)
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.Machine.Minters = map[string]requests.Minter{
			policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) {
				return "", fmt.Errorf("%w: Google login needs a new sign-in: invalid_grant (invalid_rapt)", secrets.ErrMintRefused)
			},
		}
	})
	ts.Secrets.Put(policytest.KindSecret("LAPSED_LOGIN", testApprover, policy.TierAgent, policy.KindGoogleLogin, "stored-login"))
	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/LAPSED_LOGIN/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("reread LAPSED_LOGIN = %d %s", status, body)
	}
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "read login", "LAPSED_LOGIN"), "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	if created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %s, want an automatic grant", body)
	}
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil)
	reply := decode[wireError](t, body)
	if status != http.StatusBadGateway || reply.Code != "MINT_FAILED" || !strings.Contains(reply.Error, "invalid_rapt") || strings.Contains(reply.Error, "stored-login") {
		t.Fatalf("POST values = %d %+v, want 502 MINT_FAILED naming invalid_rapt and not the stored value", status, reply)
	}
	line := ""
	for l := range strings.Lines(logged.String()) {
		if strings.Contains(l, requests.MintFailedMessage) {
			line = l
		}
	}
	for _, want := range []string{"ERROR " + requests.MintFailedMessage, "name=LAPSED_LOGIN", "kind=google-login", "class=refused", "invalid_rapt"} {
		if !strings.Contains(line, want) {
			t.Fatalf("mint failure log line %q does not hold %q (log: %q)", line, want, logged.String())
		}
	}
	if strings.Contains(logged.String(), "stored-login") {
		t.Fatalf("the broker's log holds the stored value: %q", logged.String())
	}
}

// TestCreateRequestNeedingTwoApproversIs400MixedApprovers pins, over real HTTP, that a request for
// testApprover's human-tier secret beside a shared human-tier one, whose approver is anyone, is
// refused 400 MIXED_APPROVERS rather than recorded as one decision.
func TestCreateRequestNeedingTwoApproversIs400MixedApprovers(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	compact := signAgentSecretRequest(t, sessionKey, ts.URL, "need both", "DEEL_API_KEY", "SHARED_KEY")
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusBadRequest || decode[wireError](t, body).Code != "MIXED_APPROVERS" {
		t.Fatalf("POST /v1/requests (DEEL_API_KEY, SHARED_KEY) = %d %s, want 400 MIXED_APPROVERS", status, body)
	}
}

// TestCreateRequestWithAnotherLifetimeWhilePendingIs409RequestPending pins one pending request
// per session and secret: changing --ttl cannot create another approver Inbox card.
func TestCreateRequestWithAnotherLifetimeWhilePendingIs409RequestPending(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), "sjawhar")
	first := signAgentSecretRequestFor(t, sessionKey, ts.URL, "two minutes", 120, "DEEL_API_KEY")
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": first, "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("first POST /v1/requests = %d %s", status, body)
	}
	firstReply := decode[wireCreateRequestResponse](t, body)
	if firstReply.RecordID == nil {
		t.Fatalf("first request = %+v, want pending record", firstReply)
	}
	second := signAgentSecretRequestFor(t, sessionKey, ts.URL, "five minutes", 300, "DEEL_API_KEY")
	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": second, "session_id": nil})
	err := decode[wireError](t, body)
	if status != http.StatusConflict || err.Code != "REQUEST_PENDING" || !strings.Contains(err.Error, firstReply.RequestID) {
		t.Fatalf("second POST /v1/requests = %d %+v, want 409 REQUEST_PENDING naming %s", status, err, firstReply.RequestID)
	}
}

// TestCreateRequestWithInvalidSessionIDIs400SessionIDInput pins, over real HTTP, that a
// session_id override that is too long is refused 400 SESSION_ID_INPUT at record time
// (LEGION-587's hardening). requests.TestCreateRefusesAnInvalidSessionID (machine_test.go)
// already exercises Machine.Create's validSessionID bound directly (too-long, whitespace,
// control-character, and the exact-128 boundary); this pins the same bound at the HTTP handler
// boundary, the way UNKNOWN_SECRET and MIXED_APPROVERS are already covered above.
func TestCreateRequestWithInvalidSessionIDIs400SessionIDInput(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), "sjawhar")
	compact := signAgentSecretRequest(t, sessionKey, ts.URL, "need it", "DEEL_API_KEY")
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": strings.Repeat("a", 129)})
	if status != http.StatusBadRequest {
		t.Fatalf("POST /v1/requests (session_id too long) = %d, want 400: %s", status, body)
	}
	werr := decode[wireError](t, body)
	if werr.Code != "SESSION_ID_INPUT" {
		t.Fatalf("code = %q, want SESSION_ID_INPUT", werr.Code)
	}
}

// TestDecisionsTakeOnlyTheApproversLogin pins that only the record's approver decides it, and
// only through Dispatch: the approver's own login sent without the UI bearer is 401 UI_INVALID,
// another login's approve and deny are both 403 NOT_APPROVER, a missing approver is 400
// APPROVER_REQUIRED, all of them leave the record pending, the approver's deny succeeds, and a
// second decision on the now-terminal record is 409 RECORD_TERMINAL for its approver and still 403
// NOT_APPROVER for another login.
func TestDecisionsTakeOnlyTheApproversLogin(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), "sjawhar")
	compact := signAgentSecretRequest(t, sessionKey, ts.URL, "", "DEEL_API_KEY")
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	recordID := *created.RecordID

	status, body := ts.req(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve", nil,
		map[string]any{"approver": testApprover})
	if status != http.StatusUnauthorized || decode[wireError](t, body).Code != "UI_INVALID" {
		t.Fatalf("approve without the UI bearer = %d %s, want 401 UI_INVALID", status, body)
	}
	for _, route := range []string{"approve", "deny"} {
		status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/"+route,
			map[string]any{"approver": "mallory"})
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
			t.Fatalf("%s as mallory = %d %s, want 403 NOT_APPROVER", route, status, body)
		}
	}
	status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve", map[string]any{})
	if status != http.StatusBadRequest || decode[wireError](t, body).Code != "APPROVER_REQUIRED" {
		t.Fatalf("approve with no approver = %d %s, want 400 APPROVER_REQUIRED", status, body)
	}
	_, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID, nil)
	if stillPending := decode[wireRecord](t, body); stillPending.State != "pending" {
		t.Fatalf("record after refused decisions = %+v, want still pending", stillPending)
	}

	status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/deny",
		map[string]any{"approver": testApprover})
	if status != http.StatusOK {
		t.Fatalf("deny = %d: %s", status, body)
	}
	denied := decode[stateBody](t, body)
	if denied.State != "denied" {
		t.Fatalf("deny response = %+v, want state=denied", denied)
	}

	status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"approver": testApprover})
	if status != http.StatusConflict {
		t.Fatalf("second decision = %d, want 409: %s", status, body)
	}
	if werr := decode[wireError](t, body); werr.Code != "RECORD_TERMINAL" {
		t.Fatalf("code = %q, want RECORD_TERMINAL", werr.Code)
	}
	status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve", map[string]any{"approver": "mallory"})
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
		t.Fatalf("approve the decided record as mallory = %d %s, want 403 NOT_APPROVER", status, body)
	}
}

// TestRecordCancelledWithNoEventLeavesThePendingList pins GET /v1/pending and GET
// /v1/credential-requests/{id} against the record of a request cancelled with no terminal event on
// the record, which an enrollment that ended before the broker wrote that event left behind: the
// list leaves it out and its read says cancelled, at the time its request was, while a pending
// request's record and a pending machine login are both still listed and a denied machine login
// is not.
func TestRecordCancelledWithNoEventLeavesThePendingList(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), "sjawhar")
	create := func(reason string) wireCreateRequestResponse {
		t.Helper()
		_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
			map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, reason, "DEEL_API_KEY"), "session_id": nil})
		created := decode[wireCreateRequestResponse](t, body)
		if created.State != "pending" || created.RecordID == nil {
			t.Fatalf("create %q = %s, want a pending request with a record", reason, body)
		}
		return created
	}

	ended := create("asked before its session ended")
	cancelledAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := ts.Store.Pool.Exec(context.Background(), `update requests set state='cancelled', decided_at=$2, decided_by='broker',
		decision_detail='the requesting enrollment ended: its lease lapsed' where id=$1`, ended.RequestID, cancelledAt); err != nil {
		t.Fatalf("cancel the request with no record event: %v", err)
	}
	pending := create("still waiting")
	machineLogin := func(host string) (recordID, code string) {
		t.Helper()
		code = ts.openMachineLogin(t, newSigningKey(t), testApprover, host)
		_, body := ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
		return decode[wireRecord](t, body).RecordID, code
	}
	login, _ := machineLogin("example-host-devbox")
	denied, deniedCode := machineLogin("example-host-denied")
	if status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+denied+"/deny",
		map[string]any{"approver": testApprover, "code": deniedCode}); status != http.StatusOK {
		t.Fatalf("deny the second machine login = %d: %s", status, body)
	}

	status, body := ts.ui(t, http.MethodGet, "/v1/pending?approver="+testApprover, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/pending = %d: %s", status, body)
	}
	var listed []string
	for _, entry := range decode[struct {
		Pending []wirePendingEntry `json:"pending"`
	}](t, body).Pending {
		listed = append(listed, entry.RecordID)
	}
	if want := []string{login, *pending.RecordID}; !slices.Equal(listed, want) {
		t.Fatalf("GET /v1/pending lists %v, want the machine login and the pending request's record, newest first: %v", listed, want)
	}

	_, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+*ended.RecordID, nil)
	if read := decode[wireRecord](t, body); read.State != "cancelled" || read.Decided == nil || read.Decided.Event != "cancelled" || !read.Decided.At.Equal(cancelledAt) {
		t.Fatalf("record of the cancelled request = %s, want state cancelled, decided cancelled at %s", body, cancelledAt)
	}
	_, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+*pending.RecordID, nil)
	if read := decode[wireRecord](t, body); read.State != "pending" || read.Decided != nil {
		t.Fatalf("record of the pending request = %s, want pending and undecided", body)
	}
}

// TestCancelRequestThenCancelAgainIsTerminal covers POST /v1/requests/{id}/cancel's happy path and
// its own terminal guard.
func TestCancelRequestThenCancelAgainIsTerminal(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), "sjawhar")
	compact := signAgentSecretRequest(t, sessionKey, ts.URL, "", "DEEL_API_KEY")
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)

	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests/"+created.RequestID+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel = %d: %s", status, body)
	}
	cancelled := decode[stateBody](t, body)
	if cancelled.State != "cancelled" {
		t.Fatalf("cancel response = %+v, want state=cancelled", cancelled)
	}

	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests/"+created.RequestID+"/cancel", nil)
	if status != http.StatusConflict {
		t.Fatalf("second cancel = %d, want 409: %s", status, body)
	}
	werr := decode[wireError](t, body)
	if werr.Code != "REQUEST_TERMINAL" {
		t.Fatalf("code = %q, want REQUEST_TERMINAL", werr.Code)
	}
}

// TestRevokeGrantBySession covers session-authenticated POST /v1/grants/{id}/revoke — a session
// ending its own grant, distinct from the UI's human revoke-by-approver route.
func TestRevokeGrantBySession(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), "sjawhar")
	compact := signAgentSecretRequest(t, sessionKey, ts.URL, "", "DEEL_API_KEY")
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	recordID := *created.RecordID

	_, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"approver": testApprover})
	approved := decode[struct {
		GrantID *string `json:"grant_id"`
	}](t, body)
	grantID := *approved.GrantID

	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/revoke", nil)
	if status != http.StatusOK {
		t.Fatalf("session revoke = %d: %s", status, body)
	}
	revoked := decode[stateBody](t, body)
	if revoked.State != "revoked" {
		t.Fatalf("revoke response = %+v, want state=revoked", revoked)
	}

	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
	if status != http.StatusForbidden {
		t.Fatalf("values after session revoke = %d, want 403: %s", status, body)
	}
}

// TestRenewAndReadSelf covers the two remaining session routes: renewing the caller's own lease
// and reading its own enrollment plus live grants.
func TestRenewAndReadSelf(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), "sjawhar")

	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/renew", nil)
	if status != http.StatusOK {
		t.Fatalf("renew = %d: %s", status, body)
	}
	renewed := decode[struct {
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}](t, body)
	if renewed.LeaseExpiresAt.Before(time.Now()) {
		t.Fatalf("renewed lease %v is not in the future", renewed.LeaseExpiresAt)
	}

	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/enrollments/self", nil)
	if status != http.StatusOK {
		t.Fatalf("read self = %d: %s", status, body)
	}
	self := decode[struct {
		EnrollmentID string `json:"enrollment_id"`
		Kind         string `json:"kind"`
		Operator     string `json:"operator"`
	}](t, body)
	if self.EnrollmentID != enrollmentID || self.Kind != "session" || self.Operator != "sjawhar" {
		t.Fatalf("self = %+v, want id=%s kind=session operator=sjawhar", self, enrollmentID)
	}
}

// TestPathValidation pins that a record id must be a lowercase-hex sha256 hash (never a UUID) and
// a grant id must be a UUID, each refused before any store read.
func TestPathValidation(t *testing.T) {
	ts := newTestServer(t)

	status, body := ts.ui(t, http.MethodGet, "/v1/credential-requests/not-a-valid-record-id", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("GET with a bad record id = %d, want 400: %s", status, body)
	}
	werr := decode[wireError](t, body)
	if werr.Code != "RECORD_ID_INPUT" {
		t.Fatalf("code = %q, want RECORD_ID_INPUT", werr.Code)
	}

	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), "sjawhar")
	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/not-a-uuid/values", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("POST values with a bad grant id = %d, want 400: %s", status, body)
	}
	werr = decode[wireError](t, body)
	if werr.Code != "GRANT_ID_INPUT" {
		t.Fatalf("code = %q, want GRANT_ID_INPUT", werr.Code)
	}
}

// TestEachPersonsMachineLoginSpendsTheirOwnBucket pins the per-login bucket's key space: a
// person's device login spends that person's bucket, even one whose login_hint is a word or a
// service's name, and a service's device-code login is refused 400 before it spends any bucket.
// With one login per bucket, each person's first login is accepted and only a second login naming
// the same person is refused.
func TestEachPersonsMachineLoginSpendsTheirOwnBucket(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.LauncherLimits = &api.LauncherLimits{
			PerAddress: ratelimit.Limit{Every: time.Hour, Burst: 100},
			PerLogin:   ratelimit.Limit{Every: time.Hour, Burst: 1},
		}
	})
	login := func(loginHint, service string) int {
		t.Helper()
		status, _ := ts.deviceLogin(t, newSigningKey(t), loginHint, "example-host-cluster", service)
		return status
	}
	for _, c := range []struct {
		name, loginHint, service string
		want                     int
	}{
		{"a service's device-code login naming a person", testApprover, "legion-daemon", http.StatusBadRequest},
		{"a person's machine login naming the word service", "service", "", http.StatusOK},
		{"a person's machine login naming a service's bare name", "other-service", "", http.StatusOK},
		{"the person the service's login named", testApprover, "", http.StatusOK},
		{"a second login naming that person", testApprover, "", http.StatusTooManyRequests},
	} {
		if got := login(c.loginHint, c.service); got != c.want {
			t.Fatalf("%s = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestAPersonsMachineLoginNamingNoOneIsRefused pins that a person's machine login must name the
// person who decides it: one with no login_hint, or one naming the sentinel, is refused 400
// invalid_request REQUEST_INVALID before any record opens.
func TestAPersonsMachineLoginNamingNoOneIsRefused(t *testing.T) {
	ts := newTestServer(t)
	for _, hint := range []string{"", record.AnyoneApprover} {
		status, body := ts.deviceLogin(t, newSigningKey(t), hint, "example-host-devbox", "")
		if desc, _ := body["error_description"].(string); status != http.StatusBadRequest || body["error"] != "invalid_request" || !strings.HasPrefix(desc, "REQUEST_INVALID: ") {
			t.Fatalf("a person's machine login with login_hint %q = %d %v, want 400 invalid_request REQUEST_INVALID", hint, status, body)
		}
	}
	if got := ts.column(t, `select count(*)::text from credential_requests`); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("credential-request records = %v, want none", got)
	}
}
