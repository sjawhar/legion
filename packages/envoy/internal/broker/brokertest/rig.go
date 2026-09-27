// packages/envoy/internal/broker/brokertest/rig.go

// Package brokertest mounts the real broker (api.Register) — real Postgres store, real
// enroll/machine-login/approvers services, a real software WebAuthn authenticator, a seeded
// approver key — on an httptest.Server, for any package that needs to drive a genuine broker end
// to end rather than a hand-rolled fake. It is the extraction api_test.go's own newTestServer
// names as its reference pattern: written fresh from reading that function, never by editing it
// (Plan A's own file, out of bounds). Every consumer of a full real-broker rig — this task's
// contract test, and later ones — imports this package instead of duplicating the wiring.
package brokertest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"github.com/sjawhar/envoy/internal/broker/webauthntest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// Origin and RPID are the fixed WebAuthn origin/rpId every Rig's approver key and every
// ceremony's PublicKeyCredential*Options are built against — a caller signs an Authenticator
// assertion with these two values.
const (
	Origin = "https://dispatch.test"
	RPID   = "dispatch.test"
)

const (
	testAAGUID = "ee882879-721c-4913-9775-3dfcce97072a"
	uiToken    = "brokertest-ui-token-0123456789ab"
)

// podAudience is the fixed audience every Rig's pod verifier is constructed for, and the value
// MintPodToken signs pod tokens against — arbitrary, since a Rig both mints and verifies them
// itself; chosen only to read clearly in a failure message.
const podAudience = "legion-broker-pod"

// Rig is a live broker HTTP server (real handlers, real Postgres) plus direct handles to its
// store and a real WebAuthn software authenticator seeded as Operator's approver key — enough to
// drive a machine login, a box or host enrollment, and an agent-secret request end to end over
// genuine HTTP, and to reach into the store directly for what the API itself has no route to do
// (e.g. forcing a launcher credential's expiry).
type Rig struct {
	// URL is the broker's own httptest.Server base URL: every proof's htu and every request
	// object's aud must check against this one value.
	URL string
	// Operator is the seeded approver login ("sjawhar"), also named as the box operator in the
	// fixture rules file below.
	Operator string
	// OperatorFile already holds Operator, trimmed — ready to hand to a helper.Broker as its
	// OperatorFile field.
	OperatorFile string
	Store        *store.Store
	Approvers    *approvers.Service
	CA           *webauthntest.CA
	// Approver is Operator's seeded real WebAuthn key: the software authenticator that produces
	// real assertions for the human-approval half of a machine login or a credential request.
	Approver *webauthntest.Authenticator
	// podIssuer and podKey back MintPodToken: a local OIDC issuer this Rig's own pod verifier
	// (wired into its enroll.Service by NewRig) trusts, and the signing key MintPodToken mints
	// under.
	podIssuer *oidctest.Issuer
	podKey    *oidctest.Key
}

// NewRig seeds one approver key for login "sjawhar", a rules file naming it as a test secret's
// approver and as a box operator, wires a real pod verifier (a local OIDC issuer trusted by an
// enroll.K8sPodVerifier, mirroring cmd/broker/main.go's own wiring), and mounts api.Register on
// an httptest.Server — wired exactly as cmd/broker/main.go and api_test.go's newTestServer wire
// it. It skips t when BROKER_TEST_DATABASE_URL is unset (storetest.Open's own contract).
func NewRig(t *testing.T) *Rig {
	t.Helper()
	st := storetest.Open(t)

	ca := webauthntest.NewCA(t)
	auth := ca.NewAuthenticator(t, uuid.MustParse(testAAGUID))
	operator := "sjawhar"
	nonce := strings.Repeat("a", 64)
	challenge := record.RegisterChallenge(operator, nonce)
	entry := approvers.KeyEntry{
		CredentialID:   base64.RawURLEncoding.EncodeToString(auth.CredentialID),
		ChallengeNonce: nonce,
		Registration:   auth.Register(t, RPID, Origin, challenge[:]),
		Seed:           true,
	}
	if _, err := st.Pool.Exec(context.Background(), `insert into approver_key_seeds (login, credential_id) values ($1,$2)`, operator, entry.CredentialID); err != nil {
		t.Fatalf("insert approver_key_seeds: %v", err)
	}
	approversSvc := &approvers.Service{Store: st, Verifier: &approvers.Verifier{
		Roots: ca.Pool(), Origin: Origin, AAGUIDs: map[uuid.UUID]bool{uuid.MustParse(testAAGUID): true},
	}}
	if err := approversSvc.Reconcile(context.Background(), map[string][]approvers.KeyEntry{operator: {entry}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rulesYAML := `version: 1
secrets:
  TEST_SECRET:
    source: dev1/agent-secrets/TEST_SECRET
    owner: ` + operator + `
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: ` + operator + `, decision: approval, approver: operator}
approvers:
  origin: ` + Origin + `
  aaguids: ["` + testAAGUID + `"]
  logins:
    ` + operator + `:
      keys:
        - credential_id: "` + entry.CredentialID + `"
          registration:
            challenge_nonce: "` + entry.ChallengeNonce + `"
            response: ` + string(entry.Registration) + `
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

	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	podVerifier, err := oidc.New(context.Background(), issuer.URL(), podAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}

	enr := &enroll.Service{Store: st, Lease: time.Hour, Pod: enroll.K8sPodVerifier{Verifier: podVerifier}}
	enr.Chain = enroll.NewChainVerifier(st, approversSvc, srv.URL, time.Minute)

	reqMachine := &requests.Machine{
		Store: st, Rules: cur, Secrets: secrets.Fake{"dev1/agent-secrets/TEST_SECRET": "test-secret-v1"},
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
		PublicURL: srv.URL, UIOrigin: Origin, UIToken: uiToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach, Approvers: approversSvc,
		Proof: &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
	})

	operatorFile := filepath.Join(t.TempDir(), "operator")
	if err := os.WriteFile(operatorFile, []byte(operator), 0o600); err != nil {
		t.Fatal(err)
	}

	return &Rig{
		URL: srv.URL, Operator: operator, OperatorFile: operatorFile,
		Store: st, Approvers: approversSvc, CA: ca, Approver: auth,
		podIssuer: issuer, podKey: podKey,
	}
}

// Req issues method against path on the real broker with an optional body (marshaled to JSON)
// and headers, returning the decoded status and body bytes. It never asserts a status itself: a
// caller does that so a mismatch names the actual body.
func (r *Rig) Req(t *testing.T, method, path string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, r.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
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

// UI is Req with the shared UI bearer token every human/operator-facing route requires
// (Dispatch's own server credential in production; this Rig's fixed fixture token here).
func (r *Rig) UI(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	return r.Req(t, method, path, map[string]string{"Authorization": "Bearer " + uiToken}, body)
}

// MintPodToken mints a projected service-account token bound to podUID — the shape
// enroll.K8sPodVerifier.Verify reads — signed by this Rig's own local OIDC issuer, which the
// Rig's enroll.Service trusts as its PodVerifier (wired in NewRig). Mirrors
// internal/broker/enroll/enroll_test.go's own mintPodToken helper.
func (r *Rig) MintPodToken(t *testing.T, podUID string) string {
	t.Helper()
	claims := r.podIssuer.Claims("system:serviceaccount:legion:worker", podAudience)
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(raw, &merged); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	merged["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	return r.podIssuer.Mint(t, r.podKey, merged)
}
