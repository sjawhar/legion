// e2e_test.go is the acceptance proof for the secrets broker: the whole credential-request story
// driven as real HTTP against the mux built by api.Register, on a real Postgres store and a fake
// Secrets Manager. Every step below drives HTTP; none calls a service method directly
// (Machine.ApplyDecision and friends are exercised only through the routes that wrap them). A
// human decision is a UI-route call naming the approver's login, the body Dispatch's server sends
// on the human's behalf.
//
// Unlike internal/broker/api/api_test.go's per-route unit coverage, this test's wiring mirrors
// cmd/broker/main.go exactly: policy.NewCurrent runs a real policy.Loader over the fake Secrets
// Manager and reloads it on a real ticker, so step 8 edits a secret's tags in that store and waits
// for the live ticker to pick the edit up rather than standing up a second policy.Current to fake
// "a reload happened".
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
//  8. a secret whose owner tag is edited to no person is refused by the
//     live reload, logged by name, and served again once fixed          -> "8_..."
//  9. anyone signed in approves a shared human-tier secret              -> "9_..."
//  10. another person's session asking for the operator's agent-tier
//     secret waits for the operator's approval                          -> "10_..."
package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

const (
	testUIToken = "test-ui-token-e2e-0123456789abcdef"
	// testApprover operates the box every request below comes from, owns every secret but the
	// shared one, and so is every approval's approver but the shared one's; and the machine
	// login's login_hint.
	testApprover = "sami@example.com"
	// otherPerson signs in to Dispatch too: they approve the shared secret, and their own machine
	// asks for testApprover's agent-tier secret.
	otherPerson = "ben@example.com"

	// reloadInterval is policy.NewCurrent's own ticker period for this test: short enough that
	// step 8's bounded wait (reloadTimeout) sees the real reload path run several times without
	// slowing the suite down.
	reloadInterval = 40 * time.Millisecond
	reloadTimeout  = 5 * time.Second
)

// --- harness ---

// e2eServer is a live broker HTTP server (real handlers, real Postgres) wired exactly like
// cmd/broker/main.go: a real policy.Loader over Secrets, reloading on a real ticker. Log holds
// everything the broker logged.
type e2eServer struct {
	URL     string
	Store   *store.Store
	Secrets *secrets.Local
	Log     fmt.Stringer
}

// newE2EServer holds e2eSecrets in a fake Secrets Manager and wires the full service graph —
// enroll.Service (+ ChainVerifier), requests.Machine, machine.Service, policy.NewCurrent — the
// same way main.go does, minus the HTTP listener and signal handling (api.Register mounts on an
// httptest.Server instead of a real net/http.Server, as api_test.go's newTestServer does). The
// broker logs through slog's default handler, as main.go's does, here into Log.
func newE2EServer(t *testing.T) *e2eServer {
	t.Helper()
	st := storetest.Open(t)
	logged := policytest.CaptureLog(t)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	enr := &enroll.Service{Store: st, Lease: time.Hour}
	enr.Chain = enroll.NewChainVerifier(st, srv.URL, time.Minute)

	local := secrets.NewLocal(e2eSecrets...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	current, err := policy.NewCurrent(ctx, policytest.Loader(local), reloadInterval)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}

	reqMachine := &requests.Machine{
		Store: st, Policy: current, Secrets: secrets.AWS{Client: local},
		MaxGrant: time.Hour, PendingTTL: 12 * time.Hour,
		Audience: srv.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	reqMachine.Chain = requests.NewChainVerifier(st, srv.URL, time.Minute)
	mach := &machine.Service{
		Store: st, Enroll: enr, Policy: current,
		Audience: srv.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		Replay: enr.Replay,
	}

	api.Register(mux, api.Deps{
		PublicURL: srv.URL, UIToken: testUIToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach,
		Proof: &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
	})

	return &e2eServer{URL: srv.URL, Store: st, Secrets: local, Log: logged}
}

// e2eSecrets are the fake Secrets Manager's secrets. Every one testApprover owns is human-tier but
// AUTO_READ_TOKEN, so it needs their approval, and each approval-needing secret used across the
// steps gets its own distinct name: requests.Machine.Create's reuseLiveGrant check reuses any
// still-live grant for an exact name-set match against the same enrollment before ever evaluating
// the policy, so two steps sharing one secret name would make a later "fresh pending request"
// silently resolve to an earlier step's already-decided grant instead of creating the new record
// the step means to test.
var e2eSecrets = []secrets.LocalSecret{
	policytest.Secret("DEEL_API_KEY", testApprover, policy.TierHuman, "deel-v1"),
	policytest.Secret("SLACK_MCP_XOXP_TOKEN", testApprover, policy.TierHuman, "slack-v1"),
	policytest.Secret("GITHUB_TOKEN", testApprover, policy.TierHuman, "github-v1"),
	policytest.Secret("NOTION_API_KEY", testApprover, policy.TierHuman, "notion-v1"),
	policytest.Secret("LINEAR_API_KEY", testApprover, policy.TierHuman, "linear-v1"),
	policytest.Secret("SHARED_DEPLOY_KEY", policy.OwnerShared, policy.TierHuman, "shared-deploy-v1"),
	policytest.Secret("AUTO_READ_TOKEN", testApprover, policy.TierAgent, "read-v1"),
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

	// Step 8: an owner edits LINEAR_API_KEY's owner tag to a GitHub login, which names no person.
	// The live reload refuses that one secret, logging its name and why in the line the
	// deployment's alarm filters on, and a fresh request for it answers 400 UNKNOWN_SECRET while
	// every other secret is still served; once the tag names a person again the next reload
	// serves it again.
	t.Run("8_MalformedOwnerTagIsRefusedByTheLiveReload", func(t *testing.T) {
		linear := policytest.Secret("LINEAR_API_KEY", "sjawhar", policy.TierHuman, "linear-v1")
		ts.Secrets.Put(linear)
		refused := "ERROR agent secret policy refused name=" + policytest.ID("LINEAR_API_KEY") + " reason=owner-tag-malformed\n"
		ts.awaitStatus(t, "LINEAR_API_KEY", http.StatusBadRequest, "UNKNOWN_SECRET")
		if !strings.Contains(ts.Log.String(), refused) {
			t.Fatalf("broker log has no line %q:\n%s", refused, ts.Log.String())
		}
		ts.createPending(t, sessionKey, enrollmentID, "e2e other secrets still served", "SLACK_MCP_XOXP_TOKEN")

		ts.Secrets.Put(policytest.Secret("LINEAR_API_KEY", testApprover, policy.TierHuman, "linear-v1"))
		ts.awaitStatus(t, "LINEAR_API_KEY", http.StatusOK, "")
	})

	// Step 9: a shared human-tier secret's request names "anyone" as its approver, reaches the
	// pending list of a person who is neither its requester's operator nor anyone the policy names,
	// is refused to the sentinel itself, and that person's approval releases its value.
	t.Run("9_AnyoneSignedInApprovesASharedHumanTierSecret", func(t *testing.T) {
		_, recordID := ts.createPending(t, sessionKey, enrollmentID, "e2e shared deploy", "SHARED_DEPLOY_KEY")
		_, body := ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID, nil)
		if read := decode[wireRecord](t, body); read.Approver != record.AnyoneApprover {
			t.Fatalf("shared record = %+v, want approver %q", read, record.AnyoneApprover)
		}
		ts.awaitPendingFor(t, otherPerson, recordID)

		status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
			map[string]any{"approver": record.AnyoneApprover})
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
			t.Fatalf("approve as %q = %d %s, want 403 NOT_APPROVER", record.AnyoneApprover, status, body)
		}
		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
			map[string]any{"approver": otherPerson})
		if status != http.StatusOK {
			t.Fatalf("approve as %s = %d: %s", otherPerson, status, body)
		}
		granted := decode[struct {
			GrantID *string `json:"grant_id"`
		}](t, body)
		ts.awaitValue(t, sessionKey, enrollmentID, *granted.GrantID, "SHARED_DEPLOY_KEY", "shared-deploy-v1")
	})

	// Step 10: otherPerson logs their own machine in and its session asks for testApprover's
	// agent-tier AUTO_READ_TOKEN, which testApprover's own session gets at once: the request is an
	// approval request to testApprover, the owner, never to otherPerson, and the owner's approval
	// releases it.
	t.Run("10_AnotherPersonsSessionAsksTheOwnerOfAnAgentTierSecret", func(t *testing.T) {
		status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
			map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "mine", "AUTO_READ_TOKEN"), "session_id": nil})
		if status != http.StatusOK || decode[wireCreateRequestResponse](t, body).State != "granted" {
			t.Fatalf("the owner's own session asking for AUTO_READ_TOKEN = %d %s, want granted at once", status, body)
		}

		otherEnrollment, otherKey := ts.loginAndEnroll(t, otherPerson, "e2e-ben")
		_, recordID := ts.createPending(t, otherKey, otherEnrollment, "ben's agent needs it", "AUTO_READ_TOKEN")
		_, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID, nil)
		if read := decode[wireRecord](t, body); read.Approver != testApprover {
			t.Fatalf("ben's request = %+v, want its approver to be the owner %s", read, testApprover)
		}
		ts.awaitPendingFor(t, testApprover, recordID)
		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
			map[string]any{"approver": otherPerson})
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
			t.Fatalf("approve as the requester's own operator = %d %s, want 403 NOT_APPROVER", status, body)
		}
		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
			map[string]any{"approver": testApprover})
		if status != http.StatusOK {
			t.Fatalf("approve as the owner = %d: %s", status, body)
		}
		granted := decode[struct {
			GrantID *string `json:"grant_id"`
		}](t, body)
		ts.awaitValue(t, otherKey, otherEnrollment, *granted.GrantID, "AUTO_READ_TOKEN", "read-v1")
	})
}

// awaitStatus asks for name from a fresh session until the create route answers status (and, for
// an error, code), failing after reloadTimeout: the reload ticker applies a tag edit on its next
// tick.
func (s *e2eServer) awaitStatus(t *testing.T, name string, status int, code string) {
	t.Helper()
	enrollmentID, key := s.loginAndEnroll(t, testApprover, "e2e-reload-"+uuid.NewString()[:8])
	deadline := time.Now().Add(reloadTimeout)
	for {
		got, body := s.session(t, key, enrollmentID, http.MethodPost, "/v1/requests",
			map[string]any{"request": signAgentSecretRequest(t, key, s.URL, "reload", name), "session_id": nil})
		if got == status && (code == "" || decode[wireError](t, body).Code == code) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("POST /v1/requests (%s) = %d %s; want %d %s within %s", name, got, body, status, code, reloadTimeout)
		}
		time.Sleep(reloadInterval)
	}
}

// awaitPendingFor fails t unless GET /v1/pending?approver=<person> lists recordID.
func (s *e2eServer) awaitPendingFor(t *testing.T, person, recordID string) {
	t.Helper()
	status, body := s.ui(t, http.MethodGet, "/v1/pending?approver="+person, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/pending?approver=%s = %d: %s", person, status, body)
	}
	for _, p := range decode[struct {
		Pending []wirePendingEntry `json:"pending"`
	}](t, body).Pending {
		if p.RecordID == recordID {
			return
		}
	}
	t.Fatalf("GET /v1/pending?approver=%s = %s, want record %s listed", person, body, recordID)
}

// awaitValue fails t unless the session's grant releases name's value.
func (s *e2eServer) awaitValue(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, grantID, name, value string) {
	t.Helper()
	status, body := s.session(t, key, enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
	if status != http.StatusOK || decode[struct {
		Values map[string]string `json:"values"`
	}](t, body).Values[name] != value {
		t.Fatalf("values of grant %s = %d %s, want %s=%s", grantID, status, body, name, value)
	}
}

// loginAndEnroll logs a machine of person in (their own approval of their own machine login) and
// enrolls a box session under it, as step 1 does for testApprover.
func (s *e2eServer) loginAndEnroll(t *testing.T, person, host string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	machineKey := newSigningKey(t)
	status, body := s.req(t, http.MethodPost, "/v1/launcher-credentials", nil,
		map[string]any{"request": signMachineLoginRequest(t, machineKey, s.URL, person, host)})
	if status != http.StatusAccepted {
		t.Fatalf("POST /v1/launcher-credentials (%s) = %d: %s", person, status, body)
	}
	code := decode[struct {
		Code string `json:"code"`
	}](t, body).Code
	_, body = s.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	recordID := decode[wireRecord](t, body).RecordID
	status, body = s.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"approver": person, "code": code})
	if status != http.StatusOK {
		t.Fatalf("approve %s's machine login = %d: %s", person, status, body)
	}
	credentialID := *decode[struct {
		CredentialID *string `json:"credential_id"`
	}](t, body).CredentialID
	sessionKey := newSigningKey(t)
	thumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	status, body = s.launcher(t, machineKey, credentialID, http.MethodPost, "/v1/enrollments",
		map[string]any{"kind": "box", "runtime_id": host + "-box", "thumbprint": thumbprint})
	if status != http.StatusCreated {
		t.Fatalf("enroll %s's session = %d: %s", person, status, body)
	}
	return decode[struct {
		EnrollmentID string `json:"enrollment_id"`
	}](t, body).EnrollmentID, sessionKey
}
