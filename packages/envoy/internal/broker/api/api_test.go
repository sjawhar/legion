// packages/envoy/internal/broker/api/api_test.go
package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/dispatch"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
)

func testDatabaseURL(t *testing.T) string {
	url := os.Getenv("BROKER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("BROKER_TEST_DATABASE_URL must be set to run Postgres api tests")
	}
	return url
}

func str(s string) *string { return &s }

// fakeOpener is the machine's askOpener for these tests: no test here answers an ask (that's
// requests.Machine's own test suite), only opens one so a "needs approval" secret reaches
// "pending" instead of failing on a nil Dispatch.
type fakeOpener struct{ next int }

func (f *fakeOpener) CreateAsk(_ context.Context, issue, question string, options []dispatch.Option, urgency string) (dispatch.Ask, error) {
	f.next++
	return dispatch.Ask{ID: fmt.Sprintf("ask-%d", f.next), State: "open"}, nil
}

type enrolledAgent struct {
	id  string
	key *ecdsa.PrivateKey
}

// signProof signs a proof binding enr to exactly this method and URL, the shape
// proof.Verifier.Verify checks (htm/htu).
func signProof(t *testing.T, enr enrolledAgent, method, url string) string {
	t.Helper()
	tok, err := proof.Sign(enr.key, enr.id, method, url, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	return tok
}

// fixture wires api.Register onto a real httptest.Server backed by a migrated Postgres store and
// enrolls two live sessions (A, B) with their own signing keys, so a test can sign proofs with
// proof.Sign and exercise the broker end to end over real HTTP. It returns the machine too, so a
// test can move a pending request straight to granted with ApplyAnswer without a real Dispatch.
func fixture(t *testing.T) (srv *httptest.Server, enrA, enrB enrolledAgent, launcherToken string, machine *requests.Machine) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Pool.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	enr := &enroll.Service{Store: st, Lease: time.Hour}
	_, launcherToken, err = enr.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-enroll")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := enr.AuthenticateLauncher(ctx, launcherToken)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}

	mkAgent := func(name string) enrolledAgent {
		t.Helper()
		key, err := proof.NewKey()
		if err != nil {
			t.Fatalf("proof.NewKey: %v", err)
		}
		thumb, err := proof.Thumbprint(&key.PublicKey)
		if err != nil {
			t.Fatalf("proof.Thumbprint: %v", err)
		}
		e, err := enr.Create(ctx, cred, enroll.Enrollment{
			Kind: "box", RuntimeID: "box-" + name + "-" + t.Name(), Operator: str("sjawhar"),
			ApproverKind: "operator", Thumbprint: thumb,
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		return enrolledAgent{id: e.ID.String(), key: key}
	}
	enrA = mkAgent("a")
	enrB = mkAgent("b")

	cctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cur, err := rules.NewCurrent(cctx, rules.FileLoader{Path: "testdata/rules.yaml"}, time.Hour, func(error) {})
	if err != nil {
		t.Fatalf("rules.NewCurrent: %v", err)
	}

	machine = &requests.Machine{
		Store: st,
		Rules: cur,
		Dispatch: &fakeOpener{},
		Secrets: secrets.Fake{
			"dev1/agent-secrets/DEEL_API_KEY": "deel-v1",
			"dev1/agent-secrets/AUTO_TOKEN":   "auto-v1",
		},
		MaxGrant:      time.Hour,
		PendingTTL:    12 * time.Hour,
		StandingIssue: func(context.Context, string) (string, error) { return "AGENTC-1", nil },
		IssueAssignee: func(context.Context, string) (string, error) { return "alice", nil },
	}

	mux := http.NewServeMux()
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	Register(mux, Deps{
		PublicURL: srv.URL,
		Enroll:    enr,
		Machine:   machine,
		Proof:     &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, Replay: enr.Replay},
		Dispatch:  dispatch.New("http://127.0.0.1:0", "unused-in-these-tests", http.DefaultClient),
	})
	return srv, enrA, enrB, launcherToken, machine
}

func decodeJSON(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

func createPendingRequest(t *testing.T, srv *httptest.Server, enr enrolledAgent) string {
	t.Helper()
	url := srv.URL + "/v1/requests"
	body := `{"secrets":["DEEL_API_KEY"],"reason":"need it for testing","issue":null,"session_id":null}`
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Proof", signProof(t, enr, http.MethodPost, url))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v1/requests: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/requests status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	decodeJSON(t, resp, &out)
	if out["state"] != "pending" {
		t.Fatalf("state = %v, want pending", out["state"])
	}
	id, _ := out["request_id"].(string)
	if id == "" {
		t.Fatalf("response %+v carries no request_id", out)
	}
	return id
}

// TestCreateRequestPendingWithValidProof is Step 1's first assertion: POST /v1/requests with A's
// proof -> 200 pending, with the contract's secrets/grant_id/ask shape.
func TestCreateRequestPendingWithValidProof(t *testing.T) {
	srv, enrA, _, _, _ := fixture(t)
	url := srv.URL + "/v1/requests"
	body := `{"secrets":["DEEL_API_KEY"],"reason":"need it","issue":null,"session_id":null}`
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	req.Header.Set("Proof", signProof(t, enrA, http.MethodPost, url))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	decodeJSON(t, resp, &out)
	if out["state"] != "pending" {
		t.Fatalf("state = %v, want pending", out["state"])
	}
	if out["grant_id"] != nil {
		t.Fatalf("grant_id = %v, want nil for a pending request", out["grant_id"])
	}
	if out["ask"] == nil {
		t.Fatalf("ask = nil, want a dispatch:// ref for a pending request")
	}
	secretsOut, ok := out["secrets"].([]any)
	if !ok || len(secretsOut) != 1 {
		t.Fatalf("secrets = %v, want one decision", out["secrets"])
	}
}

// TestCancelOtherSessionsRequestForbidden is Step 1's second assertion: B's proof cancelling A's
// request_id -> 403 NOT_YOURS.
func TestCancelOtherSessionsRequestForbidden(t *testing.T) {
	srv, enrA, enrB, _, _ := fixture(t)
	requestID := createPendingRequest(t, srv, enrA)

	url := srv.URL + "/v1/requests/" + requestID + "/cancel"
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	req.Header.Set("Proof", signProof(t, enrB, http.MethodPost, url))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var out map[string]string
	decodeJSON(t, resp, &out)
	if out["code"] != "NOT_YOURS" {
		t.Fatalf("code = %q, want NOT_YOURS", out["code"])
	}
}

// TestNoProofHeaderUnauthorized is Step 1's third assertion: a request with no Proof header ->
// 401 PROOF_INVALID.
func TestNoProofHeaderUnauthorized(t *testing.T) {
	srv, _, _, _, _ := fixture(t)
	resp, err := srv.Client().Get(srv.URL + "/v1/enrollments/self")
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var out map[string]string
	decodeJSON(t, resp, &out)
	if out["code"] != "PROOF_INVALID" {
		t.Fatalf("code = %q, want PROOF_INVALID", out["code"])
	}
}

// TestProofForDifferentURLUnauthorized is Step 1's fourth assertion: a proof signed for one URL
// used against another -> 401 (htm/htu mismatch).
func TestProofForDifferentURLUnauthorized(t *testing.T) {
	srv, enrA, _, _, _ := fixture(t)
	signedFor := srv.URL + "/v1/enrollments/self"
	usedOn := srv.URL + "/v1/requests"
	tok := signProof(t, enrA, http.MethodGet, signedFor)

	req, _ := http.NewRequest(http.MethodPost, usedOn, bytes.NewReader([]byte(`{"secrets":["DEEL_API_KEY"],"reason":"x"}`)))
	req.Header.Set("Proof", tok)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var out map[string]string
	decodeJSON(t, resp, &out)
	if out["code"] != "PROOF_INVALID" {
		t.Fatalf("code = %q, want PROOF_INVALID", out["code"])
	}
}

// TestDeleteEnrollmentThenProofUnauthorized is Step 1's fifth assertion: DELETE
// /v1/enrollments/{A} with A's launcher token -> 204, then A's proof -> 401 (enrollment not
// live).
func TestDeleteEnrollmentThenProofUnauthorized(t *testing.T) {
	srv, enrA, _, launcherToken, _ := fixture(t)

	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/enrollments/"+enrA.id, nil)
	delReq.Header.Set("Authorization", "Bearer "+launcherToken)
	delResp, err := srv.Client().Do(delReq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", delResp.StatusCode)
	}

	url := srv.URL + "/v1/enrollments/self"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Proof", signProof(t, enrA, http.MethodGet, url))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET self: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var out map[string]string
	decodeJSON(t, resp, &out)
	if out["code"] != "PROOF_INVALID" {
		t.Fatalf("code = %q, want PROOF_INVALID", out["code"])
	}

	// A second DELETE of the same, now-revoked enrollment must remain idempotent (204), not error.
	delReq2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/enrollments/"+enrA.id, nil)
	delReq2.Header.Set("Authorization", "Bearer "+launcherToken)
	delResp2, err := srv.Client().Do(delReq2)
	if err != nil {
		t.Fatalf("DELETE (repeat): %v", err)
	}
	if delResp2.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE (repeat) status = %d, want 204", delResp2.StatusCode)
	}
}

// TestCreateEnrollmentBadBearerUnauthorized is Step 1's sixth assertion: POST /v1/enrollments
// with a bad bearer -> 401 LAUNCHER_INVALID.
func TestCreateEnrollmentBadBearerUnauthorized(t *testing.T) {
	srv, _, _, _, _ := fixture(t)
	body := `{"kind":"box","runtime_id":"box-x","operator":"sjawhar","approver":{"kind":"operator"},"thumbprint":"tp-x"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/enrollments", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var out map[string]string
	decodeJSON(t, resp, &out)
	if out["code"] != "LAUNCHER_INVALID" {
		t.Fatalf("code = %q, want LAUNCHER_INVALID", out["code"])
	}
}

// TestCreateEnrollmentSucceedsAndIsIdempotent exercises createEnrollment's own body decoding and
// the 201-vs-200 Existing distinction, beyond what Step 1's launcher-auth-only test covers.
func TestCreateEnrollmentSucceedsAndIsIdempotent(t *testing.T) {
	srv, _, _, launcherToken, _ := fixture(t)
	body := `{"kind":"box","runtime_id":"box-fresh","operator":"sjawhar","approver":{"kind":"operator"},"thumbprint":"tp-fresh"}`

	post := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/enrollments", bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+launcherToken)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		return resp
	}

	resp1 := post()
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp1.StatusCode)
	}
	var out1 map[string]any
	decodeJSON(t, resp1, &out1)
	if out1["enrollment_id"] == nil || out1["lease_expires_at"] == nil {
		t.Fatalf("response %+v missing enrollment_id/lease_expires_at", out1)
	}

	resp2 := post()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("repeat create status = %d, want 200 (Existing)", resp2.StatusCode)
	}
	var out2 map[string]any
	decodeJSON(t, resp2, &out2)
	if out2["enrollment_id"] != out1["enrollment_id"] {
		t.Fatalf("repeat create enrollment_id = %v, want %v", out2["enrollment_id"], out1["enrollment_id"])
	}
}

// TestReadRequestShapeAndOwnership covers GET /v1/requests/{id}'s reshaped response (nested
// "decision" rather than top-level decided_by/detail) and its 403 ownership check.
func TestReadRequestShapeAndOwnership(t *testing.T) {
	srv, enrA, enrB, _, _ := fixture(t)
	requestID := createPendingRequest(t, srv, enrA)

	getURL := srv.URL + "/v1/requests/" + requestID
	req, _ := http.NewRequest(http.MethodGet, getURL, nil)
	req.Header.Set("Proof", signProof(t, enrA, http.MethodGet, getURL))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	decodeJSON(t, resp, &out)
	if out["state"] != "pending" {
		t.Fatalf("state = %v, want pending", out["state"])
	}
	if _, hasDecidedBy := out["decided_by"]; hasDecidedBy {
		t.Fatalf("response %+v carries top-level decided_by, contract wants it nested under decision", out)
	}
	if out["decision"] != nil {
		t.Fatalf("decision = %v, want nil for a still-pending request", out["decision"])
	}

	req2, _ := http.NewRequest(http.MethodGet, getURL, nil)
	req2.Header.Set("Proof", signProof(t, enrB, http.MethodGet, getURL))
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatalf("GET (other session): %v", err)
	}
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp2.StatusCode)
	}
}

// TestGrantValuesAndRevoke exercises the automatic-grant path end to end: create a request for an
// automatic secret (granted immediately, no ask), read its values, revoke the grant through the
// session's own proof, then confirm a second values read is refused as GRANT_NOT_LIVE.
func TestGrantValuesAndRevoke(t *testing.T) {
	srv, enrA, _, _, _ := fixture(t)
	createURL := srv.URL + "/v1/requests"
	body := `{"secrets":["AUTO_TOKEN"],"reason":"need it","issue":null,"session_id":null}`
	req, _ := http.NewRequest(http.MethodPost, createURL, bytes.NewReader([]byte(body)))
	req.Header.Set("Proof", signProof(t, enrA, http.MethodPost, createURL))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created map[string]any
	decodeJSON(t, resp, &created)
	if created["state"] != "granted" {
		t.Fatalf("state = %v, want granted", created["state"])
	}
	grantID, _ := created["grant_id"].(string)
	if grantID == "" {
		t.Fatalf("response %+v carries no grant_id", created)
	}

	valuesURL := srv.URL + "/v1/grants/" + grantID + "/values"
	vreq, _ := http.NewRequest(http.MethodPost, valuesURL, nil)
	vreq.Header.Set("Proof", signProof(t, enrA, http.MethodPost, valuesURL))
	vresp, err := srv.Client().Do(vreq)
	if err != nil {
		t.Fatalf("values: %v", err)
	}
	if vresp.StatusCode != http.StatusOK {
		t.Fatalf("values status = %d, want 200", vresp.StatusCode)
	}
	var vout map[string]any
	decodeJSON(t, vresp, &vout)
	values, _ := vout["values"].(map[string]any)
	if values["AUTO_TOKEN"] != "auto-v1" {
		t.Fatalf("values = %v, want AUTO_TOKEN=auto-v1", vout["values"])
	}

	revokeURL := srv.URL + "/v1/grants/" + grantID + "/revoke"
	rreq, _ := http.NewRequest(http.MethodPost, revokeURL, nil)
	rreq.Header.Set("Proof", signProof(t, enrA, http.MethodPost, revokeURL))
	rresp, err := srv.Client().Do(rreq)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if rresp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", rresp.StatusCode)
	}

	vreq2, _ := http.NewRequest(http.MethodPost, valuesURL, nil)
	vreq2.Header.Set("Proof", signProof(t, enrA, http.MethodPost, valuesURL))
	vresp2, err := srv.Client().Do(vreq2)
	if err != nil {
		t.Fatalf("values (post-revoke): %v", err)
	}
	if vresp2.StatusCode != http.StatusForbidden {
		t.Fatalf("values (post-revoke) status = %d, want 403", vresp2.StatusCode)
	}
	var vout2 map[string]string
	decodeJSON(t, vresp2, &vout2)
	if vout2["code"] != "GRANT_NOT_LIVE" {
		t.Fatalf("code = %q, want GRANT_NOT_LIVE", vout2["code"])
	}
}

// TestReadSelfListsGrantsAndRenewEnforcesOwnID exercises GET /v1/enrollments/self (kind/operator/
// lease/grants) and the renew route's own-id-only design: a proof for A used against B's URL is
// refused as NOT_YOURS, and A renewing itself extends its own lease.
func TestReadSelfListsGrantsAndRenewEnforcesOwnID(t *testing.T) {
	srv, enrA, enrB, _, _ := fixture(t)

	createURL := srv.URL + "/v1/requests"
	body := `{"secrets":["AUTO_TOKEN"],"reason":"need it","issue":null,"session_id":null}`
	req, _ := http.NewRequest(http.MethodPost, createURL, bytes.NewReader([]byte(body)))
	req.Header.Set("Proof", signProof(t, enrA, http.MethodPost, createURL))
	if _, err := srv.Client().Do(req); err != nil {
		t.Fatalf("create: %v", err)
	}

	selfURL := srv.URL + "/v1/enrollments/self"
	sreq, _ := http.NewRequest(http.MethodGet, selfURL, nil)
	sreq.Header.Set("Proof", signProof(t, enrA, http.MethodGet, selfURL))
	sresp, err := srv.Client().Do(sreq)
	if err != nil {
		t.Fatalf("self: %v", err)
	}
	if sresp.StatusCode != http.StatusOK {
		t.Fatalf("self status = %d, want 200", sresp.StatusCode)
	}
	var sout map[string]any
	decodeJSON(t, sresp, &sout)
	if sout["kind"] != "box" || sout["operator"] != "sjawhar" || sout["enrollment_id"] != enrA.id {
		t.Fatalf("self = %+v, want kind box, operator sjawhar, enrollment_id %s", sout, enrA.id)
	}
	grants, _ := sout["grants"].([]any)
	if len(grants) != 1 {
		t.Fatalf("grants = %v, want exactly one live grant", sout["grants"])
	}

	// A proof authenticated as A, presented at B's renew URL, must be refused: a session may only
	// renew itself, whatever the URL's own {id} segment claims.
	mismatchURL := srv.URL + "/v1/enrollments/" + enrB.id + "/renew"
	mreq, _ := http.NewRequest(http.MethodPost, mismatchURL, nil)
	mreq.Header.Set("Proof", signProof(t, enrA, http.MethodPost, mismatchURL))
	mresp, err := srv.Client().Do(mreq)
	if err != nil {
		t.Fatalf("renew (mismatch): %v", err)
	}
	if mresp.StatusCode != http.StatusForbidden {
		t.Fatalf("renew (mismatch) status = %d, want 403", mresp.StatusCode)
	}
	var mout map[string]string
	decodeJSON(t, mresp, &mout)
	if mout["code"] != "NOT_YOURS" {
		t.Fatalf("code = %q, want NOT_YOURS", mout["code"])
	}

	ownURL := srv.URL + "/v1/enrollments/" + enrA.id + "/renew"
	oreq, _ := http.NewRequest(http.MethodPost, ownURL, nil)
	oreq.Header.Set("Proof", signProof(t, enrA, http.MethodPost, ownURL))
	oresp, err := srv.Client().Do(oreq)
	if err != nil {
		t.Fatalf("renew (own): %v", err)
	}
	if oresp.StatusCode != http.StatusOK {
		t.Fatalf("renew (own) status = %d, want 200", oresp.StatusCode)
	}
	var oout map[string]any
	decodeJSON(t, oresp, &oout)
	if oout["lease_expires_at"] == nil {
		t.Fatalf("renew response %+v missing lease_expires_at", oout)
	}
}

// TestLauncherCredentialRoutesAnswerNotImplemented pins handlers_launcher.go's stub choice: both
// authNone routes are registered (routes_table.go names them) and answer a clear 501 rather than
// panicking, since launcher.Service (Task 12) is not wired into Deps by anything in this task.
func TestLauncherCredentialRoutesAnswerNotImplemented(t *testing.T) {
	srv, _, _, _, _ := fixture(t)

	presp, err := srv.Client().Post(srv.URL+"/v1/launcher-credentials", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST launcher-credentials: %v", err)
	}
	if presp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("POST status = %d, want 501", presp.StatusCode)
	}

	gresp, err := srv.Client().Get(srv.URL + "/v1/launcher-credentials/pending-1")
	if err != nil {
		t.Fatalf("GET launcher-credentials: %v", err)
	}
	if gresp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("GET status = %d, want 501", gresp.StatusCode)
	}
}
