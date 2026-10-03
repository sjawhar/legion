// e2e_test.go is the acceptance proof for the secrets broker: the whole
// credential-request story driven as real HTTP against the mux built by api.Register, on a real
// Postgres store and a fake Secrets Manager. Every step below drives HTTP; none calls a service
// method directly (Machine.ApplyDecision and friends are exercised only through the routes that
// wrap them). A human decision is a UI-route call naming the approver's login, the body Dispatch's
// server sends on the human's behalf.
//
// Unlike internal/broker/api/api_test.go's per-route unit coverage, this test's wiring mirrors
// cmd/broker/main.go exactly: rules.NewCurrent reads a real file on disk through a real
// rules.FileLoader and reloads it on a real ticker, so the reload step (8) rewrites that file on
// disk and waits for the live ticker to pick it up rather than standing up a second
// rules.Current to fake "a reload happened".
//
// Step-to-subtest map:
//
//  1. machine login -> code -> lookup -> approve by the operator's login -> credential id;
//     enrollment routes accept lid proofs by the machine key            -> "1_..."
//  2. box enrollment, session request -> pending in GET /v1/pending     -> "2_..."
//  3. UI record read -> approve -> grant -> values releases             -> "3_..."
//  4. deny path on a second request                                    -> "4_..."
//  5. revoke-by-approver kills the grant                                -> "5_..."
//  6. forged grant row releases nothing                                -> "6_..."
//  7. another login, or no UI bearer, decides nothing                   -> "7_..."
//  8. a reload carrying the removed approvers: section is refused        -> "8_..."
package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
	testUIToken = "test-ui-token-e2e-0123456789abcdef"
	// testApprover operates the box every request below comes from, and so is every approval's
	// approver (approver: operator) and the machine login's login_hint.
	testApprover = "sjawhar"

	// reloadInterval is rules.NewCurrent's own ticker period for this test: short enough that the
	// reload step's bounded wait (reloadTimeout) sees the real reload path run several times
	// without slowing the suite down.
	reloadInterval = 40 * time.Millisecond
	reloadTimeout  = 5 * time.Second
)

// --- harness ---

// e2eServer is a live broker HTTP server (real handlers, real Postgres) wired exactly like
// cmd/broker/main.go: a real rules.FileLoader over RulesPath, reloading on a real ticker. alarms
// receives every reload the ticker refused, as main.go logs them.
type e2eServer struct {
	URL       string
	Store     *store.Store
	RulesPath string
	alarms    chan error
}

// newE2EServer writes the initial scratch rules file and wires the full service graph —
// enroll.Service (+ ChainVerifier), requests.Machine, machine.Service, rules.NewCurrent — the same
// way main.go does, minus the HTTP listener and signal handling (api.Register mounts on an
// httptest.Server instead of a real net/http.Server, as api_test.go's newTestServer does).
func newE2EServer(t *testing.T) *e2eServer {
	t.Helper()
	st := storetest.Open(t)

	rulesPath := t.TempDir() + "/rules.yaml"
	writeRulesFile(t, rulesPath, rulesYAML)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	enr := &enroll.Service{Store: st, Lease: time.Hour}
	enr.Chain = enroll.NewChainVerifier(st, srv.URL, time.Minute)

	alarms := make(chan error, 64)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	current, err := rules.NewCurrent(ctx, rules.FileLoader{Path: rulesPath}, reloadInterval, func(e error) {
		select {
		case alarms <- e:
		default:
		}
	})
	if err != nil {
		t.Fatalf("rules.NewCurrent: %v", err)
	}

	reqMachine := &requests.Machine{
		Store: st, Rules: current, Secrets: secrets.Fake{
			"example/agent-secrets/DEEL_API_KEY":   "deel-v1",
			"example/agent-secrets/NOTION_API_KEY": "notion-v1",
		},
		MaxGrant: time.Hour, PendingTTL: 12 * time.Hour,
		Audience: srv.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	reqMachine.Chain = requests.NewChainVerifier(st, srv.URL, time.Minute)
	mach := &machine.Service{
		Store: st, Enroll: enr, Rules: current,
		Audience: srv.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		Replay: enr.Replay,
	}

	api.Register(mux, api.Deps{
		PublicURL: srv.URL, UIToken: testUIToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach,
		Proof: &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
	})

	return &e2eServer{URL: srv.URL, Store: st, RulesPath: rulesPath, alarms: alarms}
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

// rulesYAML is the scratch rules file. Every agent_secret is approval-needing (approver:
// operator), and each approval-needing secret used across the steps gets its own distinct name:
// requests.Machine.Create's reuseLiveGrant check reuses any still-live grant for an exact
// name-set match against the same enrollment before ever looking at the rules, so two steps
// sharing one secret name would make a later "fresh pending request" silently resolve to an
// earlier step's already-decided grant instead of creating the new record the step means to test.
const rulesYAML = `version: 1
secrets:
  DEEL_API_KEY:
    source: example/agent-secrets/DEEL_API_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
  SLACK_MCP_XOXP_TOKEN:
    source: example/agent-secrets/SLACK_MCP_XOXP_TOKEN
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
  GITHUB_TOKEN:
    source: example/agent-secrets/GITHUB_TOKEN
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
  NOTION_API_KEY:
    source: example/agent-secrets/NOTION_API_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
  LINEAR_API_KEY:
    source: example/agent-secrets/LINEAR_API_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
`

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

// createPending opens a pending agent_secret request for name from the step-1 enrollment and
// returns its request and record ids.
func (s *e2eServer) createPending(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, reason, name string) (requestID, recordID string) {
	t.Helper()
	compact := signAgentSecretRequest(t, key, s.URL, reason, name)
	status, body := s.session(t, key, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests (%s) = %d, want 200: %s", name, status, body)
	}
	created := decode[wireCreateRequestResponse](t, body)
	if created.State != "pending" || created.RecordID == nil || *created.RecordID == "" {
		t.Fatalf("create response (%s) = %+v, want state=pending with a record_id", name, created)
	}
	return created.RequestID, *created.RecordID
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, body)
	}
	return v
}

func newSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// --- wire-shape mirrors of the shared broker contract, the fields this test actually reads ---

type wireError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

type wireRecord struct {
	RecordID string `json:"record_id"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Approver string `json:"approver"`
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

// --- the test ---

func TestEndToEnd(t *testing.T) {
	ts := newE2EServer(t)

	// Shared across steps: the box enrollment and its session key (step 1), and the step-3 grant
	// later revoked in step 5.
	var (
		enrollmentID string
		sessionKey   *ecdsa.PrivateKey
		grantID      string
	)

	// Step 1: machine login (request object -> code -> UI lookup by code -> approve by the
	// operator's login with the code -> credential id), then prove the enrollment routes accept
	// a launcher proof ("lid") signed by the machine's own key by creating the box enrollment the
	// later steps reuse.
	t.Run("1_MachineLoginMintsCredentialAcceptedByEnrollmentRoutes", func(t *testing.T) {
		machineKey := newSigningKey(t)
		compact := signMachineLoginRequest(t, machineKey, ts.URL, testApprover, "e2e-agents")
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
		if looked.Kind != "launcher_credential" || looked.State != "pending" || looked.Approver != testApprover {
			t.Fatalf("lookup = %+v, want kind=launcher_credential state=pending approver=%s", looked, testApprover)
		}

		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve",
			map[string]any{"approver": testApprover, "code": login.Code})
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
			"kind": "box", "runtime_id": "e2e-box", "operator": testApprover, "thumbprint": sessionThumbprint,
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

	// Step 2: box enrollment (from step 1), session request with a request object -> pending
	// record listed in GET /v1/pending?approver=sjawhar.
	t.Run("2_SessionRequestGoesPendingAndListsForApprover", func(t *testing.T) {
		_, pendingRecordID = ts.createPending(t, sessionKey, enrollmentID, "e2e needs it for the demo", "DEEL_API_KEY")

		status, body := ts.ui(t, http.MethodGet, "/v1/pending?approver="+testApprover, nil)
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

	// Step 3: UI record read -> approve by the record's approver -> grant -> the session's own
	// POST .../values releases the fake secret's exact configured value.
	t.Run("3_UIApprovalGrantsAndValuesReleases", func(t *testing.T) {
		status, body := ts.ui(t, http.MethodGet, "/v1/credential-requests/"+pendingRecordID, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/credential-requests/{id} = %d: %s", status, body)
		}
		readBack := decode[wireRecord](t, body)
		if readBack.State != "pending" || readBack.Approver != testApprover {
			t.Fatalf("record read = %+v, want pending with approver %s", readBack, testApprover)
		}

		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+pendingRecordID+"/approve",
			map[string]any{"approver": testApprover})
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

	// Step 4: deny path on a second request — a fresh request from the same enrollment, denied by
	// its approver, ends denied with no grant.
	t.Run("4_SecondRequestDeniedHasNoGrant", func(t *testing.T) {
		requestID, recordID := ts.createPending(t, sessionKey, enrollmentID, "e2e second ask", "SLACK_MCP_XOXP_TOKEN")

		status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/deny",
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

		status, body = ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/requests/"+requestID, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/requests/{id} (denied) = %d: %s", status, body)
		}
		statusResp := decode[wireRequestStatus](t, body)
		if statusResp.State != "denied" || statusResp.GrantID != nil {
			t.Fatalf("denied request status = %+v, want state=denied grant_id=nil", statusResp)
		}
	})

	// Step 5: revoke-by-approver kills the step-3 grant — a subsequent values call refuses it.
	t.Run("5_RevokeByApproverKillsTheGrant", func(t *testing.T) {
		status, body := ts.ui(t, http.MethodPost, "/v1/grants/"+grantID+"/revoke-by-approver",
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
	})

	// Step 6: a forged grant row — inserted straight into Postgres, bypassing every handler,
	// pointing at a request whose record was never decided (no approved event exists at all) —
	// releases nothing: VerifyChain's re-verification finds no approval event and refuses with
	// GRANT_CHAIN_INVALID.
	t.Run("6_ForgedGrantRowReleasesNothing", func(t *testing.T) {
		requestID, _ := ts.createPending(t, sessionKey, enrollmentID, "e2e forged-grant target", "GITHUB_TOKEN")

		forgedGrantID := uuid.NewString()
		if _, err := ts.Store.Pool.Exec(context.Background(),
			`insert into grants (id, request_id, enrollment_id, approver, expires_at) values ($1,$2,$3,$4, now() + interval '1 hour')`,
			forgedGrantID, requestID, enrollmentID, testApprover); err != nil {
			t.Fatalf("insert forged grants row: %v", err)
		}

		status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+forgedGrantID+"/values", nil)
		if status != http.StatusForbidden {
			t.Fatalf("values on forged grant = %d, want 403: %s", status, body)
		}
		werr := decode[wireError](t, body)
		if werr.Code != "GRANT_CHAIN_INVALID" {
			t.Fatalf("code = %q, want GRANT_CHAIN_INVALID", werr.Code)
		}
	})

	// Step 7: only the record's approver decides it, and only through the UI bearer: the approver's
	// own login sent without the bearer is 401, another login is 403 NOT_APPROVER, and the record
	// is still pending for its approver, whose approval then releases the value.
	t.Run("7_AnotherLoginOrNoBearerDecidesNothing", func(t *testing.T) {
		_, recordID := ts.createPending(t, sessionKey, enrollmentID, "e2e wrong login", "NOTION_API_KEY")

		status, body := ts.req(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve", nil,
			map[string]any{"approver": testApprover})
		if status != http.StatusUnauthorized || decode[wireError](t, body).Code != "UI_INVALID" {
			t.Fatalf("approve without the UI bearer = %d %s, want 401 UI_INVALID", status, body)
		}
		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
			map[string]any{"approver": "mallory"})
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
			t.Fatalf("approve as mallory = %d %s, want 403 NOT_APPROVER", status, body)
		}
		_, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID, nil)
		if read := decode[wireRecord](t, body); read.State != "pending" {
			t.Fatalf("record after refused decisions = %+v, want pending", read)
		}

		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
			map[string]any{"approver": testApprover})
		if status != http.StatusOK {
			t.Fatalf("approve by the approver = %d: %s", status, body)
		}
		granted := decode[struct {
			GrantID *string `json:"grant_id"`
		}](t, body)
		status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+*granted.GrantID+"/values", nil)
		if status != http.StatusOK || decode[struct {
			Values map[string]string `json:"values"`
		}](t, body).Values["NOTION_API_KEY"] != "notion-v1" {
			t.Fatalf("values after the approver's approval = %d %s, want NOTION_API_KEY=notion-v1", status, body)
		}
	})

	// Step 8: a rules file still carrying the removed approvers: section is refused by the live
	// reload ticker, naming the removal, and the previous rules stay in force. The refused file
	// also drops LINEAR_API_KEY, so a fresh LINEAR_API_KEY request going pending (rather than
	// 400 UNKNOWN_SECRET) shows the previous rules are still the live ones. The file is then put
	// back so later reloads are clean again.
	t.Run("8_ReloadWithApproversSectionIsRefused", func(t *testing.T) {
		withoutLinear, _, found := strings.Cut(rulesYAML, "  LINEAR_API_KEY:")
		if !found {
			t.Fatal("rulesYAML has no LINEAR_API_KEY entry to drop")
		}
		writeRulesFile(t, ts.RulesPath, withoutLinear+"approvers:\n  origin: https://dispatch.test\n  logins: {}\n")
		deadline := time.After(reloadTimeout)
	wait:
		for {
			select {
			case err := <-ts.alarms:
				if strings.Contains(err.Error(), "approvers: section is removed") {
					break wait
				}
			case <-deadline:
				t.Fatalf("no reload alarm naming the removed approvers: section within %s", reloadTimeout)
			}
		}
		ts.createPending(t, sessionKey, enrollmentID, "e2e rules kept", "LINEAR_API_KEY")
		writeRulesFile(t, ts.RulesPath, rulesYAML)
	})
}

// TestRulesTestdataFixturesParse proves the v9-shaped fixtures under testdata/ are what the
// broker accepts: no issue_assignee reference and no approvers: section remain, and rules.Parse
// accepts the well-formed one while still refusing the ambiguous one.
func TestRulesTestdataFixturesParse(t *testing.T) {
	t.Run("rules.yaml parses", func(t *testing.T) {
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
