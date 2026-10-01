// packages/envoy/internal/broker/brokertest/rig.go

// Package brokertest mounts the real broker (api.Register) — real Postgres store, real
// enroll/machine-login/request services — on an httptest.Server, for any package that needs to
// drive a genuine broker end to end rather than a hand-rolled fake. It is the extraction
// api_test.go's own newTestServer names as its reference pattern. Every consumer of a full
// real-broker rig imports this package instead of duplicating the wiring.
package brokertest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const uiToken = "brokertest-ui-token-0123456789ab"

// podAudience is the fixed audience every Rig's pod verifier is constructed for, and the value
// MintPodToken signs pod tokens against — arbitrary, since a Rig both mints and verifies them
// itself; chosen only to read clearly in a failure message.
const podAudience = "legion-broker-pod"

// Rig is a live broker HTTP server (real handlers, real Postgres) plus a direct handle to its
// store — enough to drive a machine login, a box or host enrollment, and an agent-secret request
// end to end over genuine HTTP, and to reach into the store directly for what the API itself has
// no route to do (e.g. forcing a launcher credential's expiry). A human decision is a UI-route
// call naming Operator as the approver, the body Dispatch's server sends.
type Rig struct {
	// URL is the broker's own httptest.Server base URL: every proof's htu and every request
	// object's aud must check against this one value.
	URL string
	// Operator is the approver login ("sjawhar"), also named as the box operator in the fixture
	// rules file below.
	Operator string
	// OperatorFile already holds Operator, trimmed — ready to hand to a helper.Broker as its
	// OperatorFile field.
	OperatorFile string
	Store        *store.Store
	// podIssuer and podKey back MintPodToken: a local OIDC issuer this Rig's own pod verifier
	// (wired into its enroll.Service by NewRig) trusts, and the signing key MintPodToken mints
	// under.
	podIssuer *oidctest.Issuer
	podKey    *oidctest.Key
}

// NewRig writes a rules file naming login "sjawhar" as a test secret's approver and as a box
// operator, wires a real pod verifier (a local OIDC issuer trusted by an enroll.K8sPodVerifier,
// mirroring cmd/broker/main.go's own wiring), and mounts api.Register on an httptest.Server —
// wired exactly as cmd/broker/main.go and api_test.go's newTestServer wire it. It skips t when
// BROKER_TEST_DATABASE_URL is unset (storetest.Open's own contract).
func NewRig(t *testing.T) *Rig {
	t.Helper()
	st := storetest.Open(t)
	operator := "sjawhar"
	rulesYAML := `version: 1
secrets:
  TEST_SECRET:
    source: example/agent-secrets/TEST_SECRET
    owner: ` + operator + `
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: ` + operator + `, decision: approval, approver: operator}
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
		Store: st, Rules: cur, Secrets: secrets.Fake{"example/agent-secrets/TEST_SECRET": "test-secret-v1"},
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
		PublicURL: srv.URL, UIToken: uiToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach,
		Proof: &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
	})

	operatorFile := filepath.Join(t.TempDir(), "operator")
	if err := os.WriteFile(operatorFile, []byte(operator), 0o600); err != nil {
		t.Fatal(err)
	}

	return &Rig{
		URL: srv.URL, Operator: operator, OperatorFile: operatorFile,
		Store:     st,
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
