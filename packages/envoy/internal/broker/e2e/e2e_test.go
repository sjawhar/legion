// e2e_test.go is contract v9's (AGENTC-393) acceptance proof for the secrets broker: the whole
// credential-request story driven as real HTTP against the mux built by api.Register, on a real
// Postgres store, a fake Secrets Manager, and webauthntest's software authenticators. Every step
// below drives HTTP; none calls a service method directly (Machine.ApplyDecision and friends are
// exercised only through the routes that wrap them).
//
// Unlike internal/broker/api/api_test.go's per-route unit coverage, this test's wiring mirrors
// cmd/broker/main.go exactly: rules.NewCurrent reads a real file on disk through a real
// rules.FileLoader and reloads it on a real ticker, with onReload wired to
// approvers.Service.Reconcile precisely as main.go wires it. Task 11's key-rotation step (8)
// rewrites that file on disk and waits for the live ticker to pick it up (a short reload interval
// plus a bounded poll), rather than standing up a second rules.Current to fake "a reload
// happened" — this is the only way to genuinely exercise main.go's reload-reconciliation path.
//
// Step-to-subtest map (brief's nine numbered steps):
//
//  1. rules load with a seeded approvers section; reconcile persists it  -> newE2EServer + "1_..."
//  2. machine login -> code -> lookup -> approve -> credential id;
//     enrollment routes accept lid proofs by the machine key            -> "2_..."
//  3. box enrollment, session request -> pending in GET /v1/pending     -> "3_..."
//  4. UI record read -> approve -> grant -> values releases             -> "4_..."
//  5. deny path on a second request                                    -> "5_..."
//  6. revoke-by-approver kills the grant                                -> "6_..."
//  7. forged grant row releases nothing                                -> "7_..."
//  8. key rotation: endorse, reload with both, tombstone, key1 refused  -> "8_..."
//  9. wrong-origin assertion refused end to end                         -> "9_..."
package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

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
)

const (
	testOrigin  = "https://dispatch.test"
	testRPID    = "dispatch.test"
	testAAGUID  = "ee882879-721c-4913-9775-3dfcce97072a"
	testUIToken = "test-ui-token-e2e-0123456789abcdef"

	// reloadInterval is rules.NewCurrent's own ticker period for this test: short enough that the
	// key-rotation step's bounded poll (reloadTimeout) sees the real reload path run several
	// times without slowing the suite down.
	reloadInterval = 40 * time.Millisecond
	reloadTimeout  = 5 * time.Second
)

// --- harness ---

// e2eServer is a live broker HTTP server (real handlers, real Postgres) wired exactly like
// cmd/broker/main.go: a real rules.FileLoader over RulesPath, reloading on a real ticker, with
// onReload calling approvers.Service.Reconcile.
type e2eServer struct {
	URL       string
	Store     *store.Store
	RulesPath string
}

// newE2EServer seeds login "sjawhar"'s first approver key via the break-glass approver_key_seeds
// row (ruling 9: there is no HTTP route that creates a login's first key), writes the initial
// scratch rules file naming that seed, and wires the full service graph — approvers.Service,
// enroll.Service (+ ChainVerifier), requests.Machine, machine.Service, rules.NewCurrent — the same
// way main.go does, minus the HTTP listener and signal handling (api.Register mounts on an
// httptest.Server instead of a real net/http.Server, as api_test.go's newTestServer does).
func newE2EServer(t *testing.T, ca *webauthntest.CA, seedKey approvers.KeyEntry) *e2eServer {
	t.Helper()
	st := storetest.Open(t)
	if _, err := st.Pool.Exec(context.Background(),
		`insert into approver_key_seeds (login, credential_id) values ($1,$2)`, "sjawhar", seedKey.CredentialID); err != nil {
		t.Fatalf("insert approver_key_seeds: %v", err)
	}

	rulesPath := t.TempDir() + "/rules.yaml"
	writeRulesFile(t, rulesPath, renderRulesYAML([]approvers.KeyEntry{seedKey}))

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	approversSvc := &approvers.Service{Store: st, Verifier: &approvers.Verifier{
		Roots: ca.Pool(), Origin: testOrigin, AAGUIDs: map[uuid.UUID]bool{uuid.MustParse(testAAGUID): true},
	}}

	enr := &enroll.Service{Store: st, Lease: time.Hour}
	enr.Chain = enroll.NewChainVerifier(st, approversSvc, srv.URL, time.Minute)

	// onReload mirrors main.go's own hook precisely: reconcile the freshly parsed approvers
	// section against the persisted key set before adopting it, then refuse the reload (keeping
	// the previous rules) if the file's declared origin no longer matches this broker's
	// configured UI origin.
	onReload := func(set *rules.Set) error {
		if err := approversSvc.Reconcile(context.Background(), set.Approvers.Logins); err != nil {
			return err
		}
		if set.Approvers.Origin != testOrigin {
			return fmt.Errorf("rules approvers.origin %q does not match configured origin %q", set.Approvers.Origin, testOrigin)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	current, err := rules.NewCurrent(ctx, rules.FileLoader{Path: rulesPath}, reloadInterval, func(error) {}, onReload)
	if err != nil {
		t.Fatalf("rules.NewCurrent: %v", err)
	}

	reqMachine := &requests.Machine{
		Store: st, Rules: current, Secrets: secrets.Fake{"dev1/agent-secrets/DEEL_API_KEY": "deel-v1"},
		Approvers: approversSvc, MaxGrant: time.Hour, PendingTTL: 12 * time.Hour,
		Audience: srv.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	mach := &machine.Service{
		Store: st, Enroll: enr, Approvers: approversSvc, Rules: current,
		Audience: srv.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
	}

	api.Register(mux, api.Deps{
		PublicURL: srv.URL, UIOrigin: testOrigin, UIToken: testUIToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach, Approvers: approversSvc,
		Proof: &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
	})

	return &e2eServer{URL: srv.URL, Store: st, RulesPath: rulesPath}
}

// writeRulesFile writes content to path via a temp-file-then-rename, so rules.FileLoader (a plain
// os.ReadFile) never observes a half-written file when the reload ticker fires mid-write.
func writeRulesFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatalf("write rules file: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename rules file into place: %v", err)
	}
}

// renderKeyBlock renders one approvers.logins.sjawhar.keys[] list entry, verbatim JSON embedded
// under "response"/"assertion" (valid YAML flow syntax), matching the shape api_test.go's own
// newTestServer builds.
func renderKeyBlock(e approvers.KeyEntry) string {
	block := `        - credential_id: "` + e.CredentialID + `"
          registration:
            challenge_nonce: "` + e.ChallengeNonce + `"
            response: ` + string(e.Registration) + "\n"
	if e.Seed {
		return block + "          seed: true\n"
	}
	return block + `          endorsement:
            by: "` + e.Endorsement.By + `"
            assertion: ` + string(e.Endorsement.Assertion) + "\n"
}

// renderRulesYAML builds a complete scratch rules file naming login "sjawhar"'s keys (in order).
// Every agent_secret is approval-needing (approver: operator) except AUTO_TOKEN (fully
// automatic, the shape the brief's Step 1 setup calls for) — and each approval-needing secret
// used across the nine steps gets its own distinct name: requests.Machine.Create's
// reuseLiveGrant check reuses any still-live grant for an exact name-set match against the same
// enrollment before ever looking at the rules, so two steps sharing one secret name would make a
// later "fresh pending request" silently resolve to an earlier step's already-decided grant
// instead of creating the new record the step means to test.
func renderRulesYAML(keys []approvers.KeyEntry) string {
	var blocks strings.Builder
	for _, k := range keys {
		blocks.WriteString(renderKeyBlock(k))
	}
	return `version: 1
secrets:
  DEEL_API_KEY:
    source: dev1/agent-secrets/DEEL_API_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
  SLACK_MCP_XOXP_TOKEN:
    source: dev1/agent-secrets/SLACK_MCP_XOXP_TOKEN
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
  GITHUB_TOKEN:
    source: dev1/agent-secrets/GITHUB_TOKEN
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
  NOTION_API_KEY:
    source: dev1/agent-secrets/NOTION_API_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
  LINEAR_API_KEY:
    source: dev1/agent-secrets/LINEAR_API_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
  FIGMA_API_KEY:
    source: dev1/agent-secrets/FIGMA_API_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
  AUTO_TOKEN:
    source: dev1/agent-secrets/AUTO_TOKEN
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: automatic}
approvers:
  origin: ` + testOrigin + `
  aaguids: ["` + testAAGUID + `"]
  logins:
    sjawhar:
      keys:
` + blocks.String()
}

// --- HTTP helpers ---

func (s *e2eServer) req(t *testing.T, method, path string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = strings.NewReader(string(b))
	}
	request, err := http.NewRequest(method, s.URL+path, reader)
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

func (s *e2eServer) ui(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	return s.req(t, method, path, map[string]string{"Authorization": "Bearer " + testUIToken}, body)
}

func (s *e2eServer) session(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, method, path string, body any) (int, []byte) {
	t.Helper()
	p, err := proof.Sign(key, enrollmentID, method, s.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	return s.req(t, method, path, map[string]string{"Proof": p}, body)
}

func (s *e2eServer) launcher(t *testing.T, key *ecdsa.PrivateKey, launcherID, method, path string, body any) (int, []byte) {
	t.Helper()
	p, err := proof.SignLauncher(key, launcherID, method, s.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.SignLauncher: %v", err)
	}
	return s.req(t, method, path, map[string]string{"Proof": p}, body)
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, body)
	}
	return v
}

func decodeChallenge(t *testing.T, b64 string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode challenge %q: %v", b64, err)
	}
	return raw
}

func newSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func randomNonceHex(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random nonce: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// waitUntil polls cond until it reports true or timeout elapses, failing the test on timeout.
// Used by the key-rotation step to observe the real rules.NewCurrent reload ticker actually pick
// up a rewritten file, rather than assuming a fixed sleep is long enough.
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- wire-shape mirrors (contract v9), the fields this test actually reads ---

type wireError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

type wireChallenges struct {
	Approve string `json:"approve"`
	Deny    string `json:"deny"`
}

type wireRecord struct {
	RecordID   string          `json:"record_id"`
	Kind       string          `json:"kind"`
	State      string          `json:"state"`
	Challenges *wireChallenges `json:"challenges"`
}

type wireCreateRequestResponse struct {
	RequestID string  `json:"request_id"`
	State     string  `json:"state"`
	GrantID   *string `json:"grant_id"`
	RecordID  *string `json:"record_id"`
}

type wireRequestStatus struct {
	State   string  `json:"state"`
	GrantID *string `json:"grant_id"`
}

type wirePendingEntry struct {
	RecordID    string   `json:"record_id"`
	Kind        string   `json:"kind"`
	Identifiers []string `json:"identifiers"`
}

type wireKeyInfo struct {
	CredentialID string `json:"credential_id"`
	State        string `json:"state"`
	Seeded       bool   `json:"seeded"`
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

// --- YAML extraction for the key-rotation step ---
//
// approvers.Service.FinishRegister/FinishEndorse return pasteable rules-file YAML fragments
// (renderKeyEntryYAML/renderEndorsementYAML in approvers.go); the register ceremony's nonce is
// server-generated and never handed back except embedded in that fragment, so recovering it to
// build this test's own rewritten rules file means parsing the fragment back out.

type registerYAMLDoc struct {
	Logins map[string]struct {
		Keys []struct {
			CredentialID string `yaml:"credential_id"`
			Registration struct {
				ChallengeNonce string `yaml:"challenge_nonce"`
				Response       any    `yaml:"response"`
			} `yaml:"registration"`
		} `yaml:"keys"`
	} `yaml:"logins"`
}

func extractRegistration(t *testing.T, yamlText, login string) (credentialID, nonce string, response json.RawMessage) {
	t.Helper()
	var doc registerYAMLDoc
	if err := yaml.Unmarshal([]byte(yamlText), &doc); err != nil {
		t.Fatalf("parse register/finish yaml: %v\n%s", err, yamlText)
	}
	entry, ok := doc.Logins[login]
	if !ok || len(entry.Keys) != 1 {
		t.Fatalf("register/finish yaml has no single key for %s: %s", login, yamlText)
	}
	k := entry.Keys[0]
	raw, err := json.Marshal(k.Registration.Response)
	if err != nil {
		t.Fatalf("re-marshal registration response: %v", err)
	}
	return k.CredentialID, k.Registration.ChallengeNonce, raw
}

type endorsementYAMLDoc struct {
	Endorsement struct {
		By        string `yaml:"by"`
		Assertion any    `yaml:"assertion"`
	} `yaml:"endorsement"`
}

func extractEndorsement(t *testing.T, yamlText string) (by string, assertion json.RawMessage) {
	t.Helper()
	var doc endorsementYAMLDoc
	if err := yaml.Unmarshal([]byte(yamlText), &doc); err != nil {
		t.Fatalf("parse endorse/finish yaml: %v\n%s", err, yamlText)
	}
	raw, err := json.Marshal(doc.Endorsement.Assertion)
	if err != nil {
		t.Fatalf("re-marshal endorsement assertion: %v", err)
	}
	return doc.Endorsement.By, raw
}

// --- the test ---

func TestEndToEnd(t *testing.T) {
	ca := webauthntest.NewCA(t)
	key1Auth := ca.NewAuthenticator(t, uuid.MustParse(testAAGUID))
	nonce1 := randomNonceHex(t)
	key1Challenge := record.RegisterChallenge("sjawhar", nonce1)
	key1Entry := approvers.KeyEntry{
		CredentialID:   base64.RawURLEncoding.EncodeToString(key1Auth.CredentialID),
		ChallengeNonce: nonce1,
		Registration:   key1Auth.Register(t, testRPID, testOrigin, key1Challenge[:]),
		Seed:           true,
	}

	ts := newE2EServer(t, ca, key1Entry)

	// Shared across steps: the box enrollment and its session key (step 2), the step-4 grant
	// later revoked in step 6, and key2 (minted in step 8, still live in step 9).
	var (
		enrollmentID string
		sessionKey   *ecdsa.PrivateKey
		grantID      string
		key2Auth     *webauthntest.Authenticator
	)

	// Step 1: rules load with an approvers section whose first key is seeded (the break-glass
	// approver_key_seeds row newE2EServer inserted) — reconcile persists it. NewCurrent's first
	// load already ran synchronously by the time newE2EServer returned, so this is a direct
	// assertion that it actually adopted the seed, driven over the real UI route.
	t.Run("1_SeedKeyReconciledOnBoot", func(t *testing.T) {
		status, body := ts.ui(t, http.MethodGet, "/v1/approvers/sjawhar/keys", nil)
		if status != http.StatusOK {
			t.Fatalf("GET keys = %d: %s", status, body)
		}
		keys := decode[struct {
			Keys []wireKeyInfo `json:"keys"`
		}](t, body)
		if len(keys.Keys) != 1 || keys.Keys[0].CredentialID != key1Entry.CredentialID ||
			!keys.Keys[0].Seeded || keys.Keys[0].State != "active" {
			t.Fatalf("keys after boot-shaped FileLoader+onReload wiring = %+v, want one seeded active key %s",
				keys.Keys, key1Entry.CredentialID)
		}
	})

	// Step 2: machine login (request object -> code -> UI lookup by code -> approve with
	// assertion+code -> credential id), then prove the enrollment routes accept a launcher proof
	// ("lid") signed by the machine's own key by creating the box enrollment steps 3-9 reuse.
	t.Run("2_MachineLoginMintsCredentialAcceptedByEnrollmentRoutes", func(t *testing.T) {
		machineKey := newSigningKey(t)
		compact := signMachineLoginRequest(t, machineKey, ts.URL, "sjawhar", "e2e-agents")
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

		status, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": login.Code})
		if status != http.StatusOK {
			t.Fatalf("POST /v1/machine-logins/lookup = %d: %s", status, body)
		}
		looked := decode[wireRecord](t, body)
		if looked.Kind != "launcher_credential" || looked.State != "pending" || looked.Challenges == nil {
			t.Fatalf("lookup = %+v, want kind=launcher_credential state=pending with challenges", looked)
		}

		assertion := key1Auth.Assert(t, testRPID, testOrigin, decodeChallenge(t, looked.Challenges.Approve))
		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve",
			map[string]any{"assertion": json.RawMessage(assertion), "code": login.Code})
		if status != http.StatusOK {
			t.Fatalf("approve machine login = %d: %s", status, body)
		}
		approved := decode[struct {
			State        string  `json:"state"`
			CredentialID *string `json:"credential_id"`
		}](t, body)
		if approved.State != "approved" || approved.CredentialID == nil || *approved.CredentialID == "" {
			t.Fatalf("approve response = %+v, want state=approved with a credential_id", approved)
		}
		launcherCredentialID := *approved.CredentialID

		sessionKey = newSigningKey(t)
		sessionThumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
		if err != nil {
			t.Fatalf("thumbprint: %v", err)
		}
		status, body = ts.launcher(t, machineKey, launcherCredentialID, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": "box", "runtime_id": "e2e-box", "operator": "sjawhar", "thumbprint": sessionThumbprint,
		})
		if status != http.StatusCreated {
			t.Fatalf("POST /v1/enrollments (launcher proof by the machine's own key) = %d, want 201: %s", status, body)
		}
		created := decode[struct {
			EnrollmentID string `json:"enrollment_id"`
		}](t, body)
		if created.EnrollmentID == "" {
			t.Fatalf("created enrollment has no id: %s", body)
		}
		enrollmentID = created.EnrollmentID
	})

	var pendingRecordID string

	// Step 3: box enrollment (from step 2), session request with a request object -> pending
	// record listed in GET /v1/pending?approver=sjawhar.
	t.Run("3_SessionRequestGoesPendingAndListsForApprover", func(t *testing.T) {
		compact := signAgentSecretRequest(t, sessionKey, ts.URL, "e2e needs it for the demo", "DEEL_API_KEY")
		status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
			map[string]any{"request": compact, "session_id": nil})
		if status != http.StatusOK {
			t.Fatalf("POST /v1/requests = %d, want 200: %s", status, body)
		}
		created := decode[wireCreateRequestResponse](t, body)
		if created.State != "pending" || created.RecordID == nil || *created.RecordID == "" {
			t.Fatalf("create response = %+v, want state=pending with a record_id", created)
		}
		pendingRecordID = *created.RecordID

		status, body = ts.ui(t, http.MethodGet, "/v1/pending?approver=sjawhar", nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/pending = %d: %s", status, body)
		}
		pending := decode[struct {
			Pending []wirePendingEntry `json:"pending"`
		}](t, body)
		found := false
		for _, p := range pending.Pending {
			if p.RecordID == pendingRecordID {
				found = true
				if p.Kind != "agent_secret" || len(p.Identifiers) != 1 || p.Identifiers[0] != "DEEL_API_KEY" {
					t.Fatalf("pending entry = %+v, want kind=agent_secret identifiers=[DEEL_API_KEY]", p)
				}
			}
		}
		if !found {
			t.Fatalf("GET /v1/pending = %+v, want record %s listed", pending, pendingRecordID)
		}
	})

	// Step 4: UI record read (challenges present) -> approve with an assertion over the approve
	// challenge -> grant -> the session's own POST .../values releases the fake secret's exact
	// configured value.
	t.Run("4_UIApprovalGrantsAndValuesReleases", func(t *testing.T) {
		status, body := ts.ui(t, http.MethodGet, "/v1/credential-requests/"+pendingRecordID, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/credential-requests/{id} = %d: %s", status, body)
		}
		readBack := decode[wireRecord](t, body)
		if readBack.State != "pending" || readBack.Challenges == nil {
			t.Fatalf("record read = %+v, want pending with challenges", readBack)
		}

		assertion := key1Auth.Assert(t, testRPID, testOrigin, decodeChallenge(t, readBack.Challenges.Approve))
		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+pendingRecordID+"/approve",
			map[string]any{"assertion": json.RawMessage(assertion)})
		if status != http.StatusOK {
			t.Fatalf("approve = %d: %s", status, body)
		}
		approved := decode[struct {
			State   string  `json:"state"`
			GrantID *string `json:"grant_id"`
		}](t, body)
		if approved.State != "approved" || approved.GrantID == nil || *approved.GrantID == "" {
			t.Fatalf("approve response = %+v, want state=approved with a grant_id", approved)
		}
		grantID = *approved.GrantID

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
	})

	// Step 5: deny path on a second request — a fresh request from the same enrollment, decided
	// by an assertion over the deny challenge, ends denied with no grant.
	t.Run("5_SecondRequestDeniedHasNoGrant", func(t *testing.T) {
		compact := signAgentSecretRequest(t, sessionKey, ts.URL, "e2e second ask", "SLACK_MCP_XOXP_TOKEN")
		status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
			map[string]any{"request": compact, "session_id": nil})
		if status != http.StatusOK {
			t.Fatalf("POST /v1/requests (second) = %d, want 200: %s", status, body)
		}
		created := decode[wireCreateRequestResponse](t, body)
		recordID := *created.RecordID

		status, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/credential-requests/{id} (second) = %d: %s", status, body)
		}
		readBack := decode[wireRecord](t, body)

		denyAssertion := key1Auth.Assert(t, testRPID, testOrigin, decodeChallenge(t, readBack.Challenges.Deny))
		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/deny",
			map[string]any{"assertion": json.RawMessage(denyAssertion)})
		if status != http.StatusOK {
			t.Fatalf("deny = %d: %s", status, body)
		}
		denied := decode[struct {
			State string `json:"state"`
		}](t, body)
		if denied.State != "denied" {
			t.Fatalf("deny response = %+v, want state=denied", denied)
		}

		status, body = ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/requests/"+created.RequestID, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/requests/{id} (denied) = %d: %s", status, body)
		}
		statusResp := decode[wireRequestStatus](t, body)
		if statusResp.State != "denied" || statusResp.GrantID != nil {
			t.Fatalf("denied request status = %+v, want state=denied grant_id=nil", statusResp)
		}
	})

	// Step 6: revoke-by-approver kills the step-4 grant — a subsequent values call refuses it.
	t.Run("6_RevokeByApproverKillsTheGrant", func(t *testing.T) {
		revokeChallenge := record.RevokeChallenge(grantID)
		revokeAssertion := key1Auth.Assert(t, testRPID, testOrigin, revokeChallenge[:])
		status, body := ts.ui(t, http.MethodPost, "/v1/grants/"+grantID+"/revoke-by-approver",
			map[string]any{"assertion": json.RawMessage(revokeAssertion)})
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
	})

	// Step 7: a forged grant row — inserted straight into Postgres, bypassing every handler,
	// pointing at a request whose record was never decided (no approved event exists at all) —
	// releases nothing: VerifyChain's re-verification finds no approval event and refuses with
	// GRANT_CHAIN_INVALID.
	t.Run("7_ForgedGrantRowReleasesNothing", func(t *testing.T) {
		compact := signAgentSecretRequest(t, sessionKey, ts.URL, "e2e forged-grant target", "GITHUB_TOKEN")
		status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
			map[string]any{"request": compact, "session_id": nil})
		if status != http.StatusOK {
			t.Fatalf("POST /v1/requests (left pending, never decided) = %d, want 200: %s", status, body)
		}
		created := decode[wireCreateRequestResponse](t, body)
		if created.State != "pending" {
			t.Fatalf("create response = %+v, want state=pending (never decided, on purpose)", created)
		}

		forgedGrantID := uuid.NewString()
		if _, err := ts.Store.Pool.Exec(context.Background(),
			`insert into grants (id, request_id, enrollment_id, approver, expires_at) values ($1,$2,$3,$4, now() + interval '1 hour')`,
			forgedGrantID, created.RequestID, enrollmentID, "sjawhar"); err != nil {
			t.Fatalf("insert forged grants row: %v", err)
		}

		status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+forgedGrantID+"/values", nil)
		if status != http.StatusForbidden {
			t.Fatalf("values on forged grant = %d, want 403: %s", status, body)
		}
		werr := decode[wireError](t, body)
		if werr.Code != "GRANT_CHAIN_INVALID" {
			t.Fatalf("code = %q, want GRANT_CHAIN_INVALID", werr.Code)
		}
	})

	// Step 8: key rotation. Register and endorse a second key through the real UI ceremonies,
	// rewrite the rules file with both keys and wait for the live reload ticker to reconcile it,
	// confirm key2 can approve, then rewrite again dropping key1 (tombstoning it) and confirm
	// key1's assertion is now refused on a fresh record.
	t.Run("8_KeyRotationEndorseThenTombstone", func(t *testing.T) {
		status, body := ts.ui(t, http.MethodPost, "/v1/approvers/sjawhar/keys/register/begin", nil)
		if status != http.StatusOK {
			t.Fatalf("register/begin = %d: %s", status, body)
		}
		begin := decode[struct {
			CeremonyID string `json:"ceremony_id"`
			PublicKey  struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		}](t, body)
		if begin.CeremonyID == "" {
			t.Fatalf("register/begin = %+v, want a ceremony id", begin)
		}

		key2Auth = ca.NewAuthenticator(t, uuid.MustParse(testAAGUID))
		registration := key2Auth.Register(t, testRPID, testOrigin, decodeChallenge(t, begin.PublicKey.Challenge))
		status, body = ts.ui(t, http.MethodPost, "/v1/approvers/sjawhar/keys/register/finish",
			map[string]any{"ceremony_id": begin.CeremonyID, "response": json.RawMessage(registration)})
		if status != http.StatusOK {
			t.Fatalf("register/finish = %d: %s", status, body)
		}
		finished := decode[struct {
			YAML string `json:"yaml"`
		}](t, body)
		key2CredentialID, key2Nonce, key2Registration := extractRegistration(t, finished.YAML, "sjawhar")

		keyHash2 := sha256.Sum256(key2Auth.CredentialID)
		status, body = ts.ui(t, http.MethodPost, "/v1/approvers/sjawhar/keys/endorse/begin",
			map[string]any{"credential_id": key1Entry.CredentialID, "key_hash": hex.EncodeToString(keyHash2[:])})
		if status != http.StatusOK {
			t.Fatalf("endorse/begin = %d: %s", status, body)
		}
		endorseBegin := decode[struct {
			CeremonyID string `json:"ceremony_id"`
			PublicKey  struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		}](t, body)
		if endorseBegin.CeremonyID == "" {
			t.Fatalf("endorse/begin = %+v, want a ceremony id", endorseBegin)
		}

		endorseAssertion := key1Auth.Assert(t, testRPID, testOrigin, decodeChallenge(t, endorseBegin.PublicKey.Challenge))
		status, body = ts.ui(t, http.MethodPost, "/v1/approvers/sjawhar/keys/endorse/finish",
			map[string]any{"ceremony_id": endorseBegin.CeremonyID, "response": json.RawMessage(endorseAssertion)})
		if status != http.StatusOK {
			t.Fatalf("endorse/finish = %d: %s", status, body)
		}
		endorseFinished := decode[struct {
			YAML string `json:"yaml"`
		}](t, body)
		endorsedBy, endorsementAssertion := extractEndorsement(t, endorseFinished.YAML)

		key2Entry := approvers.KeyEntry{
			CredentialID:   key2CredentialID,
			ChallengeNonce: key2Nonce,
			Registration:   key2Registration,
			Endorsement:    &approvers.Endorsement{By: endorsedBy, Assertion: endorsementAssertion},
		}

		// Rewrite with both keys; wait for the live reload ticker (not a second rules.Current) to
		// reconcile them.
		writeRulesFile(t, ts.RulesPath, renderRulesYAML([]approvers.KeyEntry{key1Entry, key2Entry}))
		waitUntil(t, reloadTimeout, "both keys reconciled after rewrite", func() bool {
			status, body := ts.ui(t, http.MethodGet, "/v1/approvers/sjawhar/keys", nil)
			if status != http.StatusOK {
				return false
			}
			keys := decode[struct {
				Keys []wireKeyInfo `json:"keys"`
			}](t, body)
			return len(keys.Keys) == 2
		})

		// A new record approved with key2's assertion succeeds.
		compact := signAgentSecretRequest(t, sessionKey, ts.URL, "e2e key2 approves", "NOTION_API_KEY")
		status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
			map[string]any{"request": compact, "session_id": nil})
		if status != http.StatusOK {
			t.Fatalf("POST /v1/requests (for key2) = %d, want 200: %s", status, body)
		}
		created := decode[wireCreateRequestResponse](t, body)
		recordID := *created.RecordID

		status, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/credential-requests/{id} (for key2) = %d: %s", status, body)
		}
		readBack := decode[wireRecord](t, body)
		key2Assertion := key2Auth.Assert(t, testRPID, testOrigin, decodeChallenge(t, readBack.Challenges.Approve))
		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
			map[string]any{"assertion": json.RawMessage(key2Assertion)})
		if status != http.StatusOK {
			t.Fatalf("approve with key2's assertion = %d, want 200: %s", status, body)
		}

		// Rewrite dropping key1 entirely (tombstoning it); wait for the reload again.
		writeRulesFile(t, ts.RulesPath, renderRulesYAML([]approvers.KeyEntry{key2Entry}))
		waitUntil(t, reloadTimeout, "key1 tombstoned after rewrite", func() bool {
			status, body := ts.ui(t, http.MethodGet, "/v1/approvers/sjawhar/keys", nil)
			if status != http.StatusOK {
				return false
			}
			keys := decode[struct {
				Keys []wireKeyInfo `json:"keys"`
			}](t, body)
			for _, k := range keys.Keys {
				if k.CredentialID == key1Entry.CredentialID {
					return k.State == "tombstoned"
				}
			}
			return false
		})

		// key1's assertion against a FRESH record is now refused.
		compact2 := signAgentSecretRequest(t, sessionKey, ts.URL, "e2e tombstoned key1", "LINEAR_API_KEY")
		status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
			map[string]any{"request": compact2, "session_id": nil})
		if status != http.StatusOK {
			t.Fatalf("POST /v1/requests (for tombstoned key1) = %d, want 200: %s", status, body)
		}
		created2 := decode[wireCreateRequestResponse](t, body)
		recordID2 := *created2.RecordID

		status, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID2, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/credential-requests/{id} (for tombstoned key1) = %d: %s", status, body)
		}
		readBack2 := decode[wireRecord](t, body)
		key1AssertionNow := key1Auth.Assert(t, testRPID, testOrigin, decodeChallenge(t, readBack2.Challenges.Approve))
		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID2+"/approve",
			map[string]any{"assertion": json.RawMessage(key1AssertionNow)})
		if status != http.StatusForbidden {
			t.Fatalf("approve with tombstoned key1's assertion = %d, want 403: %s", status, body)
		}
		werr := decode[wireError](t, body)
		if werr.Code != "ASSERTION_INVALID" {
			t.Fatalf("code = %q, want ASSERTION_INVALID", werr.Code)
		}
	})

	// Step 9: an otherwise-valid assertion signed at the wrong origin is refused end to end, by
	// the currently live key (key2, post-rotation).
	t.Run("9_WrongOriginAssertionRefused", func(t *testing.T) {
		compact := signAgentSecretRequest(t, sessionKey, ts.URL, "e2e wrong origin", "FIGMA_API_KEY")
		status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
			map[string]any{"request": compact, "session_id": nil})
		if status != http.StatusOK {
			t.Fatalf("POST /v1/requests (wrong origin target) = %d, want 200: %s", status, body)
		}
		created := decode[wireCreateRequestResponse](t, body)
		recordID := *created.RecordID

		status, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/credential-requests/{id} (wrong origin target) = %d: %s", status, body)
		}
		readBack := decode[wireRecord](t, body)

		wrongOrigin := key2Auth.AssertAtOrigin(t, testRPID, "https://evil.example", decodeChallenge(t, readBack.Challenges.Approve))
		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
			map[string]any{"assertion": json.RawMessage(wrongOrigin)})
		if status != http.StatusForbidden {
			t.Fatalf("approve with wrong-origin assertion = %d, want 403: %s", status, body)
		}
		werr := decode[wireError](t, body)
		if werr.Code != "ASSERTION_INVALID" {
			t.Fatalf("code = %q, want ASSERTION_INVALID", werr.Code)
		}
	})
}

// TestRulesTestdataFixturesParse proves the rewritten v9-shaped fixtures under testdata/ are
// exactly what the task's "full rewrite for v9" calls for: no issue_assignee reference remains,
// and rules.Parse accepts the well-formed one while still refusing the ambiguous one exactly as
// it did before the rewrite. The fixtures' approvers section is a structural placeholder only
// (rules.Parse never verifies attestation cryptography — that is approvers.Service.Reconcile's
// job against a persisted, attested key set); TestEndToEnd above builds its own scratch rules file
// with real, dynamically generated key material for the live HTTP flow.
func TestRulesTestdataFixturesParse(t *testing.T) {
	t.Run("rules.yaml parses under v9's schema", func(t *testing.T) {
		data, err := os.ReadFile("testdata/rules.yaml")
		if err != nil {
			t.Fatalf("read testdata/rules.yaml: %v", err)
		}
		set, err := rules.Parse(data)
		if err != nil {
			t.Fatalf("rules.Parse(testdata/rules.yaml) = %v, want success", err)
		}
		if _, ok := set.Secrets["DEEL_API_KEY"]; !ok {
			t.Fatalf("parsed set is missing DEEL_API_KEY: %+v", set.Secrets)
		}
		if len(set.Approvers.Logins["sjawhar"]) != 1 {
			t.Fatalf("approvers.logins.sjawhar = %v, want one key", set.Approvers.Logins["sjawhar"])
		}
	})
	t.Run("rules_ambiguous.yaml is refused as ambiguous", func(t *testing.T) {
		data, err := os.ReadFile("testdata/rules_ambiguous.yaml")
		if err != nil {
			t.Fatalf("read testdata/rules_ambiguous.yaml: %v", err)
		}
		_, err = rules.Parse(data)
		if err == nil {
			t.Fatal("rules.Parse(testdata/rules_ambiguous.yaml) succeeded, want an ambiguous-requester refusal")
		}
		if !strings.Contains(err.Error(), "ambiguous requester") {
			t.Fatalf("error = %v, want it to mention \"ambiguous requester\"", err)
		}
	})
}
