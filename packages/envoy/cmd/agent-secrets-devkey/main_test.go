// packages/envoy/cmd/agent-secrets-devkey/main_test.go
//
// This test mounts the real broker handlers (api.Register) on a real Postgres test store — no
// fakes — and drives the devkey binary's own run() function directly (in-process, exactly the
// argv a subprocess would receive) for both halves of its job: "register --seed" to bootstrap an
// approver identity the same way scripts/dev-broker.sh does, and "approve" to decide a real
// pending credential-request record with a real WebAuthn assertion. The requester side (a signed
// request object over a live session enrollment) is built directly with record.Sign and
// proof.Sign — the same two calls a fixed agent-secrets client sends (AGENTC-393 Task 10, a
// sibling lane not yet landed in this workspace) — since devkey only ever plays the approver.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/approvers"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

const (
	testOrigin  = "https://devkey.test"
	testUIToken = "test-ui-token-0123456789abcdef"
	testSource  = "dev1/agent-secrets/AGENT_SECRETS_PROOF_APPROVAL"
	testValue   = "dev-secret-value"
)

// devkeyTestServer is a live broker HTTP server (real handlers, real Postgres) seeded with
// devkey's own "register --seed" output for login "sjawhar" — the same break-glass path
// scripts/dev-broker.sh drives — so this test exercises the CLI's seed logic itself, not a
// shortcut that bypasses it.
type devkeyTestServer struct {
	URL      string
	Store    *store.Store
	StateDir string
}

func newDevkeyTestServer(t *testing.T) *devkeyTestServer {
	t.Helper()
	st := storetest.Open(t)

	stateDir := t.TempDir() + "/devkey-state"
	var seedOut, seedErr bytes.Buffer
	if code := run([]string{"register", "--seed", "--login", "sjawhar", "--origin", testOrigin, "--state", stateDir}, &seedOut, &seedErr); code != 0 {
		t.Fatalf("devkey register --seed: exit %d\nstderr: %s", code, seedErr.String())
	}
	var seed seedOutput
	if err := json.Unmarshal(seedOut.Bytes(), &seed); err != nil {
		t.Fatalf("decode seed output: %v\n%s", err, seedOut.String())
	}

	if _, err := st.Pool.Exec(context.Background(),
		`insert into approver_key_seeds (login, credential_id) values ($1,$2)`, seed.Login, seed.CredentialID); err != nil {
		t.Fatalf("insert approver_key_seeds: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(seed.CAPEM)) {
		t.Fatalf("seed output ca_pem did not parse as PEM: %s", seed.CAPEM)
	}
	approversSvc := &approvers.Service{Store: st, Verifier: &approvers.Verifier{
		Roots: pool, Origin: testOrigin, AAGUIDs: map[uuid.UUID]bool{uuid.MustParse(seed.AAGUID): true},
	}}
	entry := approvers.KeyEntry{
		CredentialID:   seed.CredentialID,
		ChallengeNonce: seed.ChallengeNonce,
		Registration:   seed.Registration,
		Seed:           true,
	}
	if err := approversSvc.Reconcile(context.Background(), map[string][]approvers.KeyEntry{seed.Login: {entry}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rulesYAML := `version: 1
secrets:
  AGENT_SECRETS_PROOF_APPROVAL:
    source: ` + testSource + `
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
approvers:
  origin: ` + testOrigin + `
  aaguids: ["` + seed.AAGUID + `"]
  logins:
    sjawhar:
      keys:
        - credential_id: "` + seed.CredentialID + `"
          registration:
            challenge_nonce: "` + seed.ChallengeNonce + `"
            response: ` + string(seed.Registration) + `
          seed: true
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

	enr := &enroll.Service{Store: st, Lease: time.Hour}
	enr.Chain = enroll.NewChainVerifier(st, approversSvc, srv.URL, time.Minute)
	reqMachine := &requests.Machine{
		Store: st, Rules: cur, Secrets: secrets.Fake{testSource: testValue},
		Approvers: approversSvc, MaxGrant: time.Hour, PendingTTL: 12 * time.Hour,
		Audience: srv.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	reqMachine.Chain = requests.NewChainVerifier(st, approversSvc, srv.URL, time.Minute)
	mach := &machine.Service{
		Store: st, Enroll: enr, Approvers: approversSvc, Rules: cur,
		Audience: srv.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		Replay: enr.Replay,
	}
	api.Register(mux, api.Deps{
		PublicURL: srv.URL, UIOrigin: testOrigin, UIToken: testUIToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach, Approvers: approversSvc,
		Proof: &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
	})

	return &devkeyTestServer{URL: srv.URL, Store: st, StateDir: stateDir}
}

// newSessionEnrollment inserts a live box enrollment (and its backing launcher_credentials row)
// directly, mirroring internal/broker/api's own test helper: enroll.Service.Create is exercised
// end to end by the launcher-proof enrollment routes themselves, elsewhere, so this test needs
// only a live row to sign a request object and a session proof against.
func (ts *devkeyTestServer) newSessionEnrollment(t *testing.T) (enrollmentID string, key *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	k, err := proof.NewKey()
	if err != nil {
		t.Fatalf("proof.NewKey: %v", err)
	}
	thumbprint, err := proof.Thumbprint(&k.PublicKey)
	if err != nil {
		t.Fatalf("proof.Thumbprint: %v", err)
	}
	credentialUUID := uuid.New()
	if _, err := ts.Store.Pool.Exec(ctx, `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,'test-host',$3,'{}'::jsonb, now() + interval '30 days')`, credentialUUID, "sjawhar", thumbprint); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	enrollmentID = uuid.NewString()
	if _, err := ts.Store.Pool.Exec(ctx, `insert into enrollments (id, kind, runtime_id, operator, thumbprint, launcher_credential_id, lease_expires_at)
		values ($1,'box','box-devkey-test','sjawhar',$2,$3, now() + interval '1 hour')`, enrollmentID, thumbprint, credentialUUID); err != nil {
		t.Fatalf("insert enrollments: %v", err)
	}
	return enrollmentID, k
}

// req issues method against ts.URL+path with an optional JSON body and headers, returning the
// decoded status and raw body bytes.
func (ts *devkeyTestServer) req(t *testing.T, method, path string, headers map[string]string, body []byte) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, raw
}

// TestRegisterAndApproveIssueAGrant is Step 1's failing (now passing) test: devkey's own
// "register --seed" bootstraps the approver identity a rules reload accepts, a signed request
// object opens a real pending credential-request record needing that approver, and devkey's own
// "approve" subcommand — built entirely on the restored identity, a live HTTP round trip, and a
// real WebAuthn assertion — decides it into a grant whose released value matches the fake secrets
// store.
func TestRegisterAndApproveIssueAGrant(t *testing.T) {
	ts := newDevkeyTestServer(t)
	enrollmentID, key := ts.newSessionEnrollment(t)

	compact, err := record.Sign(key, ts.URL, []record.AuthorizationDetail{
		{Type: "agent_secret", Identifier: "AGENT_SECRETS_PROOF_APPROVAL", Actions: []string{"inject"}},
	}, "devkey main_test smoke", "", time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	createBody, err := json.Marshal(map[string]any{"request": compact, "session_id": nil})
	if err != nil {
		t.Fatal(err)
	}
	p, err := proof.Sign(key, enrollmentID, http.MethodPost, ts.URL+"/v1/requests", time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	status, body := ts.req(t, http.MethodPost, "/v1/requests", map[string]string{"Proof": p}, createBody)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d: %s", status, body)
	}
	var created struct {
		State    string  `json:"state"`
		RecordID *string `json:"record_id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.State != "pending" || created.RecordID == nil {
		t.Fatalf("create response = %+v, want a pending request with a record_id", created)
	}

	var approveOut, approveErr bytes.Buffer
	code := run([]string{
		"approve", "--record", *created.RecordID,
		"--broker", ts.URL, "--ui-token", testUIToken, "--origin", testOrigin, "--state", ts.StateDir,
	}, &approveOut, &approveErr)
	if code != 0 {
		t.Fatalf("devkey approve: exit %d\nstdout: %s\nstderr: %s", code, approveOut.String(), approveErr.String())
	}
	var approved struct {
		State   string  `json:"state"`
		GrantID *string `json:"grant_id"`
	}
	if err := json.Unmarshal(approveOut.Bytes(), &approved); err != nil {
		t.Fatalf("decode approve output: %v\n%s", err, approveOut.String())
	}
	if approved.State != "approved" || approved.GrantID == nil {
		t.Fatalf("approve output = %s, want state=approved with a grant_id", approveOut.String())
	}

	valuesProof, err := proof.Sign(key, enrollmentID, http.MethodPost, ts.URL+"/v1/grants/"+*approved.GrantID+"/values", time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	status, body = ts.req(t, http.MethodPost, "/v1/grants/"+*approved.GrantID+"/values", map[string]string{"Proof": valuesProof}, nil)
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

// TestRegisterSeedRefusesToOverwriteAnExistingIdentity pins that a second "--seed" run against
// the same --state directory refuses rather than silently generating a new, unrelated identity
// under an approver key the rules file (and any already-inserted approver_key_seeds row) still
// names by its old credential id.
func TestRegisterSeedRefusesToOverwriteAnExistingIdentity(t *testing.T) {
	stateDir := t.TempDir() + "/devkey-state"
	var out1, err1 bytes.Buffer
	if code := run([]string{"register", "--seed", "--login", "sjawhar", "--state", stateDir}, &out1, &err1); code != 0 {
		t.Fatalf("first seed: exit %d\n%s", code, err1.String())
	}
	var out2, err2 bytes.Buffer
	code := run([]string{"register", "--seed", "--login", "sjawhar", "--state", stateDir}, &out2, &err2)
	if code == 0 {
		t.Fatalf("second seed at the same --state: want a non-zero exit, got 0: %s", out2.String())
	}
	if !strings.Contains(err2.String(), "already exists") {
		t.Fatalf("second seed error = %q, want it to mention an existing identity", err2.String())
	}
}
