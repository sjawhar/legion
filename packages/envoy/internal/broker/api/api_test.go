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
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	testUIToken = "test-ui-token-0123456789abcdef"
	// testApprover is DEEL_API_KEY's approver (approver: operator for a box sjawhar operates) and
	// every machine login's login_hint below.
	testApprover = "sjawhar"
)

// testServer is a live broker HTTP server (real handlers, real Postgres) plus a direct handle to
// its store — needed to seed fixtures (an enrollment, a launcher credential) the API itself has
// no route to create directly — and the local OIDC issuer its pod verifier trusts, which mints the
// projected service-account tokens a pod enrollment presents.
type testServer struct {
	URL       string
	Store     *store.Store
	podIssuer *oidctest.Issuer
	podKey    *oidctest.Key
}

// podAudience is the audience the test server's pod verifier checks and podToken mints for.
const podAudience = "legion-broker-pod"

// newTestServer writes a rules file naming the box operator sjawhar as DEEL_API_KEY's approver,
// and login sjawhar for a pod of service account legion:worker, which WORKER_TOKEN is automatic
// for; wires a real pod verifier against a local OIDC issuer, as cmd/broker/main.go does when
// BROKER_K8S_OIDC_ISSUER is set; and mounts api.Register on an httptest.Server so every proof's
// htu and every request object's aud have one real, consistent PublicURL to check against.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	st := storetest.Open(t)

	rulesYAML := `version: 1
secrets:
  DEEL_API_KEY:
    source: example/agent-secrets/DEEL_API_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
      - {kind: pod, service_account: 'system:serviceaccount:legion:worker', decision: approval, approver: "login:sjawhar"}
  WORKER_TOKEN:
    source: example/agent-secrets/WORKER_TOKEN
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: pod, service_account: 'system:serviceaccount:legion:worker', decision: automatic}
`
	rulesPath := t.TempDir() + "/rules.yaml"
	if err := os.WriteFile(rulesPath, []byte(rulesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cur, err := rules.NewCurrent(context.Background(), rules.FileLoader{Path: rulesPath}, time.Hour, func(error) {})
	if err != nil {
		t.Fatalf("rules.NewCurrent: %v", err)
	}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	podVerifier, err := oidc.New(context.Background(), issuer.URL(), podAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	enr := &enroll.Service{Store: st, Lease: time.Hour, Pod: enroll.K8sPodVerifier{Verifier: podVerifier}}
	enr.Chain = enroll.NewChainVerifier(st, srv.URL, time.Minute)

	reqMachine := &requests.Machine{
		Store: st, Rules: cur, Secrets: secrets.Fake{"example/agent-secrets/DEEL_API_KEY": "deel-v1", "example/agent-secrets/WORKER_TOKEN": "worker-v1"},
		MaxGrant: time.Hour, PendingTTL: 12 * time.Hour,
		Audience: srv.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	reqMachine.Chain = requests.NewChainVerifier(st, srv.URL, time.Minute)
	mach := &machine.Service{
		Store: st, Enroll: enr, Rules: cur,
		Audience: srv.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		Replay: enr.Replay,
	}

	api.Register(mux, api.Deps{
		PublicURL: srv.URL, UIToken: testUIToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach,
		Proof: &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
	})

	return &testServer{URL: srv.URL, Store: st, podIssuer: issuer, podKey: podKey}
}

// newSessionEnrollment inserts a live box enrollment (and its backing launcher_credentials row)
// directly, mirroring requests/machine_test.go's own newEnrollment: enroll.Service.Create is
// exercised end to end by the launcher-proof enrollment routes themselves (see
// TestMachineLoginApprovalMintsAKeyBoundLauncherCredentialForEnrollment), so session-route tests
// need only a live row to authenticate a session proof against.
func (ts *testServer) newSessionEnrollment(t *testing.T, kind, runtimeID, operator string) (id string, key *ecdsa.PrivateKey) {
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
		values ($1,$2,$3,$4,$5,$6, now() + interval '1 hour')`, id, kind, runtimeID, operator, thumbprint, credentialID); err != nil {
		t.Fatalf("insert enrollments: %v", err)
	}
	return id, key
}

func newSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

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

// --- wire-shape mirrors of the shared broker contract
// (dispatch://AGENTC-393/artifact/plan-overview-md), for decoding response bodies ---

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

type wireRecord struct {
	RecordID        string              `json:"record_id"`
	Kind            string              `json:"kind"`
	State           string              `json:"state"`
	Approver        string              `json:"approver"`
	Enrollment      *wireEnrollmentInfo `json:"enrollment"`
	Identifiers     []string            `json:"identifiers"`
	Service         *string             `json:"service"`
	Reason          string              `json:"reason"`
	LifetimeSeconds int                 `json:"lifetime_seconds"`
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
	RecordID    string    `json:"record_id"`
	Kind        string    `json:"kind"`
	Identifiers []string  `json:"identifiers"`
	RequestedAt time.Time `json:"requested_at"`
}

type wireApproverGrant struct {
	GrantID    string             `json:"grant_id"`
	RecordID   *string            `json:"record_id"`
	Enrollment wireEnrollmentInfo `json:"enrollment"`
	Names      []string           `json:"names"`
	Approver   string             `json:"approver"`
	ExpiresAt  time.Time          `json:"expires_at"`
	CreatedAt  time.Time          `json:"created_at"`
}

// --- signing helpers ---

func signAgentSecretRequest(t *testing.T, key *ecdsa.PrivateKey, audience, reason string, names ...string) string {
	t.Helper()
	details := make([]record.AuthorizationDetail, len(names))
	for i, n := range names {
		details[i] = record.AuthorizationDetail{Type: "agent_secret", Identifier: n, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(key, audience, details, reason, "", time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	return compact
}

func signMachineLoginRequest(t *testing.T, key *ecdsa.PrivateKey, audience, loginHint, host string) string {
	t.Helper()
	compact, err := record.Sign(key, audience, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: host},
	}, "", loginHint, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	return compact
}

// --- tests ---

func TestHealthzIsPublic(t *testing.T) {
	ts := newTestServer(t)
	status, _ := ts.req(t, http.MethodGet, "/healthz", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", status)
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
		map[string]any{"kind": "box", "runtime_id": "box-1", "operator": "sjawhar", "thumbprint": "irrelevant"})
	if status != http.StatusUnauthorized {
		t.Fatalf("POST /v1/enrollments (bearer) = %d, want 401: %s", status, body)
	}
	werr := decode[wireError](t, body)
	if werr.Code != "LAUNCHER_INVALID" {
		t.Fatalf("code = %q, want LAUNCHER_INVALID", werr.Code)
	}
}

// mintLauncherCredential drives a full typed-code machine-login round trip through the HTTP API
// and returns the resulting launcher credential's id and the machine's own signing key — the same
// setup TestMachineLoginApprovalMintsAKeyBoundLauncherCredentialForEnrollment exercises route by
// route, factored out here for tests that just need a live launcher credential to sign proofs
// with.
func (ts *testServer) mintLauncherCredential(t *testing.T, loginHint, host string) (credentialID string, key *ecdsa.PrivateKey) {
	t.Helper()
	key = newSigningKey(t)
	return ts.approveMachineLogin(t, signMachineLoginRequest(t, key, ts.URL, loginHint, host), loginHint), key
}

// approveMachineLogin posts a signed machine-login request object, looks its record up by the
// typed code as the UI does, approves it as approver, and returns the minted launcher credential's
// id.
func (ts *testServer) approveMachineLogin(t *testing.T, compact, approver string) string {
	t.Helper()
	_, body := ts.req(t, http.MethodPost, "/v1/launcher-credentials", nil, map[string]any{"request": compact})
	login := decode[struct {
		Code string `json:"code"`
	}](t, body)
	_, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": login.Code})
	looked := decode[wireRecord](t, body)
	_, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve",
		map[string]any{"approver": approver, "code": login.Code})
	approved := decode[struct {
		CredentialID *string `json:"credential_id"`
	}](t, body)
	if approved.CredentialID == nil || *approved.CredentialID == "" {
		t.Fatalf("approve machine login: answered %s, want a credential_id", body)
	}
	return *approved.CredentialID
}

// TestSessionProofRejectedOnLauncherAuthRoute pins authenticate()'s authLauncher guard: a VALID
// session proof (an "eid" claim, signed by a live enrollment's own key) is refused with
// 401 LAUNCHER_INVALID exactly as an unsigned bearer is — it must never be treated as a launcher
// proof merely because it verifies. Without this guard the handler would receive an empty
// enroll.Credential and either fail differently downstream or, worse, succeed with the zero value.
func TestSessionProofRejectedOnLauncherAuthRoute(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "box", "box-"+t.Name(), "sjawhar")

	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "box", "runtime_id": "box-other", "operator": "sjawhar", "thumbprint": "irrelevant",
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
// to exercise both launcher-proof enrollment routes: machineLogin, readMachineLogin (pending and
// issued states), lookupMachineLogin, approveRecord dispatching to the machine kind (refusing any
// login but the record's approver), createEnrollment, and deleteEnrollment.
func TestMachineLoginApprovalMintsAKeyBoundLauncherCredentialForEnrollment(t *testing.T) {
	ts := newTestServer(t)
	machineKey := newSigningKey(t)
	compact := signMachineLoginRequest(t, machineKey, ts.URL, "sjawhar", "example-host-devbox")

	status, body := ts.req(t, http.MethodPost, "/v1/launcher-credentials", nil, map[string]any{"request": compact})
	if status != http.StatusAccepted {
		t.Fatalf("POST /v1/launcher-credentials = %d, want 202: %s", status, body)
	}
	login := decode[struct {
		PendingID string `json:"pending_id"`
		Code      string `json:"code"`
	}](t, body)
	if login.PendingID == "" || login.Code == "" {
		t.Fatalf("machine login response = %+v, want both fields set", login)
	}

	status, body = ts.req(t, http.MethodGet, "/v1/launcher-credentials/"+login.PendingID, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/launcher-credentials/{pending} (pending) = %d: %s", status, body)
	}
	pendingRead := decode[struct {
		State     string     `json:"state"`
		ExpiresAt *time.Time `json:"expires_at"`
	}](t, body)
	if pendingRead.State != "pending" || pendingRead.ExpiresAt != nil {
		t.Fatalf("pending read = %+v, want state pending and no expires_at", pendingRead)
	}

	status, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": login.Code})
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

	status, body = ts.req(t, http.MethodGet, "/v1/launcher-credentials/"+login.PendingID, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/launcher-credentials/{pending} (issued) = %d: %s", status, body)
	}
	issuedRead := decode[struct {
		State        string     `json:"state"`
		CredentialID string     `json:"credential_id"`
		ExpiresAt    *time.Time `json:"expires_at"`
	}](t, body)
	if issuedRead.State != "issued" || issuedRead.CredentialID != credentialID {
		t.Fatalf("issued read = %+v, want state=issued credential_id=%s", issuedRead, credentialID)
	}
	// The machine learns when its credential expires: there is no renewal, so it must log in again
	// before then.
	var minted time.Time
	if err := ts.Store.Pool.QueryRow(context.Background(), `select expires_at from launcher_credentials where id=$1`, credentialID).Scan(&minted); err != nil {
		t.Fatalf("read the minted credential's expiry: %v", err)
	}
	if issuedRead.ExpiresAt == nil || !issuedRead.ExpiresAt.Equal(minted) {
		t.Fatalf("issued read expires_at = %v, want the minted credential's %s", issuedRead.ExpiresAt, minted)
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
		"kind": "box", "runtime_id": "box-" + t.Name(), "operator": "sjawhar", "thumbprint": sessionThumbprint,
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
	machineKey := newSigningKey(t)
	compact := signMachineLoginRequest(t, machineKey, ts.URL, "sjawhar", "example-host-devbox")
	_, body := ts.req(t, http.MethodPost, "/v1/launcher-credentials", nil, map[string]any{"request": compact})
	login := decode[struct {
		PendingID string `json:"pending_id"`
		Code      string `json:"code"`
	}](t, body)

	_, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": login.Code})
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
// creation (session proof), the UI's pending list and record read (with an enrollment and no code
// required), approval by the record's approver, the session's own status/values reads, the UI's
// grant list, and human revocation, refused for any login but the approver's.
func TestAgentSecretRequestLifecycle(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "box", "box-"+t.Name(), "sjawhar")

	compact := signAgentSecretRequest(t, sessionKey, ts.URL, "need it for the demo", "DEEL_API_KEY")
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d, want 200: %s", status, body)
	}
	created := decode[wireCreateRequestResponse](t, body)
	if created.State != "pending" || created.RecordID == nil || *created.RecordID == "" {
		t.Fatalf("create response = %+v, want state=pending with a record_id", created)
	}
	requestID, recordID := created.RequestID, *created.RecordID

	status, body = ts.ui(t, http.MethodGet, "/v1/pending?approver=sjawhar", nil)
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
	if readBack.Enrollment == nil || readBack.Enrollment.Kind != "box" || readBack.Enrollment.Operator != "sjawhar" || readBack.Enrollment.Slot != nil {
		t.Fatalf("record enrollment = %+v, want kind=box operator=sjawhar and no slot", readBack.Enrollment)
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

	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/requests/"+requestID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/requests/{id} = %d: %s", status, body)
	}
	statusResp := decode[wireRequestStatus](t, body)
	if statusResp.State != "granted" || statusResp.GrantID == nil || *statusResp.GrantID != grantID {
		t.Fatalf("request status = %+v, want granted with grant_id %s", statusResp, grantID)
	}
	if statusResp.Decision == nil || statusResp.Decision.By != "sjawhar" {
		t.Fatalf("request decision = %+v, want by=sjawhar", statusResp.Decision)
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

	status, body = ts.ui(t, http.MethodGet, "/v1/grants?approver=sjawhar", nil)
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
	if len(listed.Names) != 1 || listed.Names[0] != "DEEL_API_KEY" || listed.Approver != testApprover || listed.Enrollment.Slot != nil {
		t.Fatalf("listed grant = %+v, want names [DEEL_API_KEY] approved by %s on an enrollment with no slot", listed, testApprover)
	}

	// Revoking by approver takes the grant's approver or its enrollment's operator (both sjawhar
	// here); any other login is refused.
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

	status, body = ts.ui(t, http.MethodGet, "/v1/grants?approver=sjawhar", nil)
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
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "box", "box-"+t.Name(), "sjawhar")

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

// TestCreateRequestWithUnknownSecretNameIs400UnknownSecret pins the shared broker contract's
// "identifier must name a rule's secret (else 400 UNKNOWN_SECRET at record time)"
// (dispatch://AGENTC-393/artifact/plan-overview-md) over real HTTP: a request naming
// a secret no rule mentions is refused, not folded into an ordinary deny decision.
func TestCreateRequestWithUnknownSecretNameIs400UnknownSecret(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "box", "box-"+t.Name(), "sjawhar")
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

// TestDecisionsTakeOnlyTheApproversLogin pins that only the record's approver decides it, and
// only through Dispatch: the approver's own login sent without the UI bearer is 401 UI_INVALID,
// another login's approve and deny are both 403 NOT_APPROVER, a missing approver is 400
// APPROVER_REQUIRED, all of them leave the record pending, the approver's deny succeeds, and a
// second decision on the now-terminal record is 409 RECORD_TERMINAL for its approver and still 403
// NOT_APPROVER for another login.
func TestDecisionsTakeOnlyTheApproversLogin(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "box", "box-"+t.Name(), "sjawhar")
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
	denied := decode[struct {
		State string `json:"state"`
	}](t, body)
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

// TestCancelRequestThenCancelAgainIsTerminal covers POST /v1/requests/{id}/cancel's happy path and
// its own terminal guard.
func TestCancelRequestThenCancelAgainIsTerminal(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "box", "box-"+t.Name(), "sjawhar")
	compact := signAgentSecretRequest(t, sessionKey, ts.URL, "", "DEEL_API_KEY")
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)

	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests/"+created.RequestID+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel = %d: %s", status, body)
	}
	cancelled := decode[struct {
		State string `json:"state"`
	}](t, body)
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
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "box", "box-"+t.Name(), "sjawhar")
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
	revoked := decode[struct {
		State string `json:"state"`
	}](t, body)
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
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "box", "box-"+t.Name(), "sjawhar")

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
	if self.EnrollmentID != enrollmentID || self.Kind != "box" || self.Operator != "sjawhar" {
		t.Fatalf("self = %+v, want id=%s kind=box operator=sjawhar", self, enrollmentID)
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

	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "box", "box-"+t.Name(), "sjawhar")
	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/not-a-uuid/values", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("POST values with a bad grant id = %d, want 400: %s", status, body)
	}
	werr = decode[wireError](t, body)
	if werr.Code != "GRANT_ID_INPUT" {
		t.Fatalf("code = %q, want GRANT_ID_INPUT", werr.Code)
	}
}
