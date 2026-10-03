// contract_test.go is Task 2's contract layer (the shared broker contract's own lesson,
// dispatch://AGENTC-393/artifact/plan-overview-md: "every fake had been built from the client's
// assumption"): it mounts the real broker handlers (brokerapi.Register with real services on
// BROKER_TEST_DATABASE_URL, exactly as the broker/api tests do)
// behind an httptest.Server, wires Dispatch's own routes to relay to it, and drives every UI
// action through DISPATCH's routes as a signed-in human — proving the proxy round-trips a real
// broker, which decides by the login Dispatch names, not a fake built from agentsecrets.Client's
// own assumptions. Skips without BROKER_TEST_DATABASE_URL (via brokerstoretest.Open) or
// DISPATCH_TEST_DATABASE_URL (via storetest.Open, already required by every other test in this
// package).
package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	brokerapi "github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	brokerstoretest "github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const (
	contractApprover = "sami@example.com"
	// contractOther is a second allowed Dispatch login, neither any record's approver nor any
	// enrollment's operator.
	contractOther   = "mallory@example.com"
	contractUIToken = "contract-ui-token"
)

// contractRig is the real broker (real Postgres) behind Dispatch's own mounted routes. Every UI
// action a test drives goes through rig.Dispatch; rig's direct broker helpers exist only to seed
// fixtures Dispatch has no route to create (a pending request comes from an agent's session
// proof, a machine login from the machine's own key — neither is a Dispatch UI action) and to
// read a grant's value back as the session that holds it.
type contractRig struct {
	Dispatch    http.Handler
	BrokerURL   string
	brokerStore *store.Store
}

func newContractRig(t *testing.T) *contractRig {
	t.Helper()
	brokerStore := brokerstoretest.Open(t)

	local := secrets.NewLocal(
		policytest.Secret("DEEL_API_KEY", contractApprover, policy.TierHuman, "deel-v1"),
		// contractApprover's own sessions get it without asking.
		policytest.Secret("AUTO_TOKEN", contractApprover, policy.TierAgent, "auto-v1"),
		// Any signed-in person approves a request for it.
		policytest.Secret("SHARED_KEY", policy.OwnerShared, policy.TierHuman, "shared-v1"),
	)
	cur := policytest.Current(t, local)

	brokerMux := http.NewServeMux()
	brokerServer := httptest.NewServer(brokerMux)
	t.Cleanup(brokerServer.Close)

	enr := &enroll.Service{Store: brokerStore, Lease: time.Hour}
	enr.Chain = enroll.NewChainVerifier(brokerStore, brokerServer.URL, time.Minute)
	reqMachine := &requests.Machine{
		Store: brokerStore, Policy: cur, Secrets: secrets.AWS{Client: local},
		MaxGrant: time.Hour, PendingTTL: 12 * time.Hour,
		Audience: brokerServer.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	reqMachine.Chain = requests.NewChainVerifier(brokerStore, brokerServer.URL, time.Minute)
	mach := &machine.Service{
		Store: brokerStore, Enroll: enr, Policy: cur,
		Audience: brokerServer.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		Replay: enr.Replay,
	}
	brokerapi.Register(brokerMux, brokerapi.Deps{
		PublicURL: brokerServer.URL, UIToken: contractUIToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach,
		Proof: &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
	})

	dispatchDB := storetest.Open(t)
	documentService := docs.New(docs.Deps{
		Store: dispatchDB, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond,
	})
	t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
	seedPeople(t, dispatchDB, contractApprover, contractOther)
	deps, err := NewDeps(DepsInput{
		Store: dispatchDB, Identity: headerIdentity(dispatchDB),
		ServerURL: "https://dispatch.example", Docs: documentService, Events: events.NewBroker(),
		AgentSecretsURL: brokerServer.URL, AgentSecretsToken: contractUIToken,
	})
	if err != nil {
		t.Fatalf("new dispatch API dependencies: %v", err)
	}
	dispatchMux := http.NewServeMux()
	Register(dispatchMux, deps)

	return &contractRig{Dispatch: dispatchMux, BrokerURL: brokerServer.URL, brokerStore: brokerStore}
}

// --- fixture seeding: direct broker access, mirroring what an agent or a machine would do ---

func contractSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// newSessionEnrollment inserts a live box enrollment directly (mirroring broker/api's own
// api_test.go newSessionEnrollment): a session route is out of Dispatch's UI scope entirely, so
// there is no proxy path that could create one.
func (rig *contractRig) newSessionEnrollment(t *testing.T, operator string) (enrollmentID string, key *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	key = contractSigningKey(t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	credentialID := uuid.New()
	if _, err := rig.brokerStore.Pool.Exec(ctx, `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,'test-host',$3,'{}'::jsonb, now() + interval '30 days')`, credentialID, operator, thumbprint); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	enrollmentID = uuid.NewString()
	if _, err := rig.brokerStore.Pool.Exec(ctx, `insert into enrollments (id, kind, runtime_id, operator, thumbprint, launcher_credential_id, lease_expires_at)
		values ($1,'box',$2,$3,$4,$5, now() + interval '1 hour')`, enrollmentID, "box-"+t.Name(), operator, thumbprint, credentialID); err != nil {
		t.Fatalf("insert enrollments: %v", err)
	}
	return enrollmentID, key
}

// brokerReq talks to the broker directly (never through Dispatch): only what an agent's session
// proof or a machine's own request could produce.
func (rig *contractRig) brokerReq(t *testing.T, method, path string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, rig.BrokerURL+path, nil)
	if body != nil {
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			t.Fatalf("marshal request body: %v", marshalErr)
		}
		request, err = http.NewRequest(method, rig.BrokerURL+path, strings.NewReader(string(encoded)))
		if err == nil {
			request.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	respBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, path, err)
	}
	return response.StatusCode, respBody
}

// contractSession is an agent's enrolled session, which alone can ask for secrets and read a
// grant's value back, and RecordID, the pending record its last request made, if it made one.
type contractSession struct {
	RecordID     string
	EnrollmentID string
	Key          *ecdsa.PrivateKey
}

// contractRequested is POST /v1/requests's answer as these tests read it.
type contractRequested struct {
	State    string  `json:"state"`
	GrantID  *string `json:"grant_id"`
	RecordID *string `json:"record_id"`
}

// newSession enrolls a fresh session operator's machine runs.
func (rig *contractRig) newSession(t *testing.T, operator string) contractSession {
	t.Helper()
	enrollmentID, key := rig.newSessionEnrollment(t, operator)
	return contractSession{EnrollmentID: enrollmentID, Key: key}
}

// request signs a real request object with session's key and posts it straight to the broker's own
// session route — the only way a request, and a pending agent_secret record, comes to exist.
func (rig *contractRig) request(t *testing.T, session contractSession, reason string, names ...string) contractRequested {
	t.Helper()
	details := make([]record.AuthorizationDetail, len(names))
	for i, name := range names {
		details[i] = record.AuthorizationDetail{Type: "agent_secret", Identifier: name, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(session.Key, rig.BrokerURL, details, reason, "", time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	p, err := proof.Sign(session.Key, session.EnrollmentID, http.MethodPost, rig.BrokerURL+"/v1/requests", time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	status, body := rig.brokerReq(t, http.MethodPost, "/v1/requests", map[string]string{"Proof": p},
		map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d: %s", status, body)
	}
	var created contractRequested
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create-request response: %v (body: %s)", err, body)
	}
	return created
}

// createPendingAgentSecretRecord asks for names from a fresh session of contractApprover's,
// returning the pending record Dispatch's routes then decide.
func (rig *contractRig) createPendingAgentSecretRecord(t *testing.T, reason string, names ...string) contractSession {
	t.Helper()
	session := rig.newSession(t, contractApprover)
	created := rig.request(t, session, reason, names...)
	if created.State != "pending" || created.RecordID == nil {
		t.Fatalf("create-request = %+v, want pending with a record", created)
	}
	session.RecordID = *created.RecordID
	return session
}

// values reads a grant's released values as the session that holds it, straight from the broker.
func (rig *contractRig) values(t *testing.T, pending contractSession, grantID string) (int, map[string]string) {
	t.Helper()
	path := "/v1/grants/" + grantID + "/values"
	p, err := proof.Sign(pending.Key, pending.EnrollmentID, http.MethodPost, rig.BrokerURL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	status, body := rig.brokerReq(t, http.MethodPost, path, map[string]string{"Proof": p}, nil)
	var released struct {
		Values map[string]string `json:"values"`
	}
	_ = json.Unmarshal(body, &released)
	return status, released.Values
}

type contractRecord struct {
	RecordID string `json:"record_id"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Approver string `json:"approver"`
}

type contractError struct {
	Code string `json:"code"`
}

// --- tests ---

// TestApproveThroughDispatchReleasesTheValue drives the approval as a signed-in human end to
// end against the real broker: another Dispatch login is refused NOT_APPROVER even when its
// browser body names the approver, the approver's own click succeeds whatever login its browser
// body names, the session then releases the value, and replaying the approval is the broker's
// own 409, forwarded unchanged.
func TestApproveThroughDispatchReleasesTheValue(t *testing.T) {
	rig := newContractRig(t)
	pending := rig.createPendingAgentSecretRecord(t, "need it for the demo", "DEEL_API_KEY")
	approvePath := "/api/v1/credential-requests/" + pending.RecordID + "/approve"

	readResp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-requests/"+pending.RecordID, nil, contractApprover)
	if read := decodeBody[contractRecord](t, readResp); read.State != "pending" || read.Approver != contractApprover {
		t.Fatalf("record read = %s, want pending with approver %s", readResp.Body.String(), contractApprover)
	}

	resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, approvePath, map[string]any{"approver": contractApprover}, contractOther)
	if resp.Code != http.StatusForbidden || decodeBody[contractError](t, resp).Code != "NOT_APPROVER" {
		t.Fatalf("approve as %s = %d %s, want 403 NOT_APPROVER", contractOther, resp.Code, resp.Body.String())
	}

	resp = dispatchRequest(t, rig.Dispatch, http.MethodPost, approvePath, map[string]any{"approver": contractOther}, contractApprover)
	if resp.Code != http.StatusOK {
		t.Fatalf("approve as %s = %d %s", contractApprover, resp.Code, resp.Body.String())
	}
	approved := decodeBody[struct {
		State   string  `json:"state"`
		GrantID *string `json:"grant_id"`
	}](t, resp)
	if approved.State != "approved" || approved.GrantID == nil {
		t.Fatalf("approve response = %s, want state=approved with a grant_id", resp.Body.String())
	}
	var decidedBy string
	if err := rig.brokerStore.Pool.QueryRow(context.Background(),
		`select login from credential_request_events where record_id=$1 and event='approved'`, pending.RecordID).Scan(&decidedBy); err != nil || decidedBy != contractApprover {
		t.Fatalf("approved event login = %q (%v), want %s", decidedBy, err, contractApprover)
	}

	if status, released := rig.values(t, pending, *approved.GrantID); status != http.StatusOK || released["DEEL_API_KEY"] != "deel-v1" {
		t.Fatalf("values = %d %v, want DEEL_API_KEY=deel-v1", status, released)
	}

	resp = dispatchRequest(t, rig.Dispatch, http.MethodPost, approvePath, map[string]any{}, contractApprover)
	if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "RECORD_TERMINAL") {
		t.Fatalf("terminal forwarding: %d %s", resp.Code, resp.Body.String())
	}
}

// TestPendingListShowsCreatedRecordThroughDispatch drives the inbox's own query: a pending
// record shows in Dispatch's proxied ?approver=me list with its minimal facts.
func TestPendingListShowsCreatedRecordThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	recordID := rig.createPendingAgentSecretRecord(t, "", "DEEL_API_KEY").RecordID

	resp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-requests?approver=me", nil, contractApprover)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET pending = %d: %s", resp.Code, resp.Body.String())
	}
	pending := decodeBody[struct {
		Pending []struct {
			RecordID string `json:"record_id"`
			Kind     string `json:"kind"`
		} `json:"pending"`
	}](t, resp)
	found := false
	for _, p := range pending.Pending {
		if p.RecordID == recordID {
			found = true
			if p.Kind != "agent_secret" {
				t.Fatalf("pending entry kind = %q, want agent_secret", p.Kind)
			}
		}
	}
	if !found {
		t.Fatalf("pending list %+v does not include %s", pending.Pending, recordID)
	}
}

// TestMachineLoginLookupAndApproveThroughDispatch drives the typed-code machine flow: a machine's
// signed login request produces a code; Dispatch's lookup route resolves it to the record (ruling
// 13's one selector); approving through Dispatch with the same code mints a launcher credential,
// and without the code the broker refuses CODE_REQUIRED.
func TestMachineLoginLookupAndApproveThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	machineKey := contractSigningKey(t)
	compact, err := record.Sign(machineKey, rig.BrokerURL,
		[]record.AuthorizationDetail{{Type: "launcher_credential", Identifier: "contract-test-host"}}, "", contractApprover, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	status, body := rig.brokerReq(t, http.MethodPost, "/v1/launcher-credentials", nil, map[string]any{"request": compact})
	if status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("POST /v1/launcher-credentials = %d: %s", status, body)
	}
	created := struct {
		Code string `json:"code"`
	}{}
	if err := json.Unmarshal(body, &created); err != nil || created.Code == "" {
		t.Fatalf("decode machine login response: %v (body: %s)", err, body)
	}

	lookupResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/machine-lookup",
		map[string]any{"code": created.Code}, contractApprover)
	if lookupResp.Code != http.StatusOK {
		t.Fatalf("machine-lookup = %d: %s", lookupResp.Code, lookupResp.Body.String())
	}
	looked := decodeBody[contractRecord](t, lookupResp)
	if looked.Kind != "launcher_credential" || looked.State != "pending" {
		t.Fatalf("machine-lookup = %+v, want a pending launcher_credential record", looked)
	}

	approvePath := "/api/v1/credential-requests/" + looked.RecordID + "/approve"
	noCode := dispatchRequest(t, rig.Dispatch, http.MethodPost, approvePath, map[string]any{}, contractApprover)
	if noCode.Code != http.StatusBadRequest || decodeBody[contractError](t, noCode).Code != "CODE_REQUIRED" {
		t.Fatalf("machine approve without its code = %d %s, want 400 CODE_REQUIRED", noCode.Code, noCode.Body.String())
	}
	approveResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, approvePath, map[string]any{"code": created.Code}, contractApprover)
	if approveResp.Code != http.StatusOK {
		t.Fatalf("machine approve = %d: %s", approveResp.Code, approveResp.Body.String())
	}
	approved := decodeBody[struct {
		State        string  `json:"state"`
		CredentialID *string `json:"credential_id"`
	}](t, approveResp)
	if approved.State != "approved" || approved.CredentialID == nil || *approved.CredentialID == "" {
		t.Fatalf("machine approve response = %+v, want state=approved with a credential_id", approved)
	}
}

// TestGrantsListAndRevokeByApproverThroughDispatch approves a request to mint a grant, lists it
// through Dispatch's own grants route, then revokes it as the signed-in approver — refused for
// another login — the human path that ends a grant, distinct from a session ending its own.
func TestGrantsListAndRevokeByApproverThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	pending := rig.createPendingAgentSecretRecord(t, "", "DEEL_API_KEY")

	approveResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+pending.RecordID+"/approve",
		map[string]any{}, contractApprover)
	approved := decodeBody[struct {
		GrantID *string `json:"grant_id"`
	}](t, approveResp)
	if approveResp.Code != http.StatusOK || approved.GrantID == nil {
		t.Fatalf("approve = %d %s, want a grant_id", approveResp.Code, approveResp.Body.String())
	}
	grantID := *approved.GrantID

	grantsResp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-grants?approver=me", nil, contractApprover)
	grants := decodeBody[struct {
		Grants []struct {
			GrantID  string `json:"grant_id"`
			Approver string `json:"approver"`
		} `json:"grants"`
	}](t, grantsResp)
	found := false
	for _, g := range grants.Grants {
		if g.GrantID == grantID && g.Approver == contractApprover {
			found = true
		}
	}
	if !found {
		t.Fatalf("grants list %+v does not include %s approved by %s", grants.Grants, grantID, contractApprover)
	}

	revokePath := "/api/v1/credential-grants/" + grantID + "/revoke"
	otherResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{}, contractOther)
	if otherResp.Code != http.StatusForbidden || decodeBody[contractError](t, otherResp).Code != "NOT_APPROVER" {
		t.Fatalf("revoke as %s = %d %s, want 403 NOT_APPROVER", contractOther, otherResp.Code, otherResp.Body.String())
	}
	revokeResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{}, contractApprover)
	if revokeResp.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", revokeResp.Code, revokeResp.Body.String())
	}

	grantsAfterResp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-grants?approver=me", nil, contractApprover)
	grantsAfter := decodeBody[struct {
		Grants []struct {
			GrantID string `json:"grant_id"`
		} `json:"grants"`
	}](t, grantsAfterResp)
	for _, g := range grantsAfter.Grants {
		if g.GrantID == grantID {
			t.Fatalf("revoked grant %s still listed: %+v", grantID, grantsAfter.Grants)
		}
	}
}

// contractPending is one row of Dispatch's pending list.
type contractPending struct {
	RecordID    string   `json:"record_id"`
	Kind        string   `json:"kind"`
	Identifiers []string `json:"identifiers"`
}

// pendingFor reads login's credential inbox through Dispatch.
func (rig *contractRig) pendingFor(t *testing.T, login string) []contractPending {
	t.Helper()
	resp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-requests?approver=me", nil, login)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET pending as %s = %d: %s", login, resp.Code, resp.Body.String())
	}
	return decodeBody[struct {
		Pending []contractPending `json:"pending"`
	}](t, resp).Pending
}

// TestASharedSecretsRequestIsInEveryInboxAndAnyoneApprovesIt drives a request whose approver is
// anyone (a shared human-tier secret) through Dispatch: it is in both signed-in people's
// credential inboxes, the second person, who neither owns nor operates anything, approves it, the
// broker records the email Dispatch sent as the deciding login, the session gets the value, and
// the request leaves both inboxes.
func TestASharedSecretsRequestIsInEveryInboxAndAnyoneApprovesIt(t *testing.T) {
	rig := newContractRig(t)
	pending := rig.createPendingAgentSecretRecord(t, "deploy the preview", "SHARED_KEY")

	for _, login := range []string{contractApprover, contractOther} {
		rows := rig.pendingFor(t, login)
		if len(rows) != 1 || rows[0].RecordID != pending.RecordID || rows[0].Kind != "agent_secret" || len(rows[0].Identifiers) != 1 || rows[0].Identifiers[0] != "SHARED_KEY" {
			t.Fatalf("%s's inbox = %+v, want the SHARED_KEY request %s", login, rows, pending.RecordID)
		}
	}
	readResp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-requests/"+pending.RecordID, nil, contractOther)
	readBody := readResp.Body.String()
	if read := decodeBody[contractRecord](t, readResp); read.State != "pending" || read.Approver != record.AnyoneApprover {
		t.Fatalf("record read = %s, want pending with approver %s", readBody, record.AnyoneApprover)
	}

	resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+pending.RecordID+"/approve", map[string]any{}, contractOther)
	approveBody := resp.Body.String()
	approved := decodeBody[struct {
		State   string  `json:"state"`
		GrantID *string `json:"grant_id"`
	}](t, resp)
	if resp.Code != http.StatusOK || approved.State != "approved" || approved.GrantID == nil {
		t.Fatalf("approve as %s = %d %s, want approved with a grant", contractOther, resp.Code, approveBody)
	}
	var decidedBy string
	if err := rig.brokerStore.Pool.QueryRow(context.Background(),
		`select login from credential_request_events where record_id=$1 and event='approved'`, pending.RecordID).Scan(&decidedBy); err != nil || decidedBy != contractOther {
		t.Fatalf("approved event login = %q (%v), want %s", decidedBy, err, contractOther)
	}
	if status, released := rig.values(t, pending, *approved.GrantID); status != http.StatusOK || released["SHARED_KEY"] != "shared-v1" {
		t.Fatalf("values = %d %v, want SHARED_KEY=shared-v1", status, released)
	}
	for _, login := range []string{contractApprover, contractOther} {
		if rows := rig.pendingFor(t, login); len(rows) != 0 {
			t.Fatalf("%s's inbox after the approval = %+v, want empty", login, rows)
		}
	}
}

// contractGrant is one row of Dispatch's Live grants list.
type contractGrant struct {
	GrantID  string   `json:"grant_id"`
	Granted  string   `json:"granted"`
	RecordID *string  `json:"record_id"`
	Approver *string  `json:"approver"`
	Names    []string `json:"names"`
}

// grantsOf reads login's Live grants through Dispatch.
func (rig *contractRig) grantsOf(t *testing.T, login string) []contractGrant {
	t.Helper()
	resp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-grants?approver=me", nil, login)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET grants as %s = %d: %s", login, resp.Code, resp.Body.String())
	}
	return decodeBody[struct {
		Grants []contractGrant `json:"grants"`
	}](t, resp).Grants
}

// asJSON renders v for a failure message, so a pointer field reads as its value or null rather
// than as an address.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(data)
}

// TestAnAutomaticGrantIsListedRevokedAndThenAsksItsOwner drives an automatic grant through
// Dispatch's Live grants: the person whose session holds it sees it listed as automatic, with no
// approver and no record, and nobody else does; another person cannot revoke it and its operator
// can, through the same route as an approved grant; after that, the same session's next request for
// the secret is an approval request to its owner, while another of the person's sessions still
// gets it at once.
func TestAnAutomaticGrantIsListedRevokedAndThenAsksItsOwner(t *testing.T) {
	rig := newContractRig(t)
	session := rig.newSession(t, contractApprover)
	auto := rig.request(t, session, "", "AUTO_TOKEN")
	if auto.State != "granted" || auto.GrantID == nil || auto.RecordID != nil {
		t.Fatalf("request = %s, want an automatic grant", asJSON(t, auto))
	}
	grantID := *auto.GrantID

	listed := rig.grantsOf(t, contractApprover)
	if len(listed) != 1 || listed[0].GrantID != grantID || listed[0].Granted != "automatic" || listed[0].Approver != nil || listed[0].RecordID != nil ||
		len(listed[0].Names) != 1 || listed[0].Names[0] != "AUTO_TOKEN" {
		t.Fatalf("%s's grants = %s, want the AUTO_TOKEN grant %s listed as automatic", contractApprover, asJSON(t, listed), grantID)
	}
	if others := rig.grantsOf(t, contractOther); len(others) != 0 {
		t.Fatalf("%s's grants = %s, want none", contractOther, asJSON(t, others))
	}

	revokePath := "/api/v1/credential-grants/" + grantID + "/revoke"
	otherRevoke := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{}, contractOther)
	otherBody := otherRevoke.Body.String()
	if otherRevoke.Code != http.StatusForbidden || decodeBody[contractError](t, otherRevoke).Code != "NOT_APPROVER" {
		t.Fatalf("revoke as %s = %d %s, want 403 NOT_APPROVER", contractOther, otherRevoke.Code, otherBody)
	}
	if resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{}, contractApprover); resp.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", resp.Code, resp.Body.String())
	}
	if status, _ := rig.values(t, session, grantID); status != http.StatusForbidden {
		t.Fatalf("values after the revoke = %d, want 403", status)
	}
	if listed := rig.grantsOf(t, contractApprover); len(listed) != 0 {
		t.Fatalf("grants after the revoke = %s, want none", asJSON(t, listed))
	}

	again := rig.request(t, session, "need it again", "AUTO_TOKEN")
	if again.State != "pending" || again.RecordID == nil || again.GrantID != nil {
		t.Fatalf("the same session's next request = %s, want an approval request", asJSON(t, again))
	}
	readResp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-requests/"+*again.RecordID, nil, contractApprover)
	readBody := readResp.Body.String()
	if read := decodeBody[contractRecord](t, readResp); read.State != "pending" || read.Approver != contractApprover {
		t.Fatalf("record read = %s, want pending with approver %s, the owner", readBody, contractApprover)
	}
	if rows := rig.pendingFor(t, contractApprover); len(rows) != 1 || rows[0].RecordID != *again.RecordID {
		t.Fatalf("the owner's inbox = %+v, want the request %s", rows, *again.RecordID)
	}

	elsewhere := rig.request(t, rig.newSession(t, contractApprover), "", "AUTO_TOKEN")
	if elsewhere.State != "granted" || elsewhere.GrantID == nil {
		t.Fatalf("another session's request = %s, want an automatic grant", asJSON(t, elsewhere))
	}
}

// approveMachineLogin logs a machine in as a person does: the machine posts its signed login,
// naming approver to approve it, straight to the broker, and approver looks its code up and
// approves it through Dispatch. service names the service a service's login is for (the Legion
// daemon's is legion-daemon), "" for approver's own machine. It returns the launcher credential the
// approval minted and the machine's key.
func (rig *contractRig) approveMachineLogin(t *testing.T, approver, service, host string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	machineKey := contractSigningKey(t)
	compact, err := record.Sign(machineKey, rig.BrokerURL,
		[]record.AuthorizationDetail{{Type: "launcher_credential", Identifier: host, Service: service}}, "", approver, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	status, body := rig.brokerReq(t, http.MethodPost, "/v1/launcher-credentials", nil, map[string]any{"request": compact})
	var created struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &created); status != http.StatusAccepted || err != nil || created.Code == "" {
		t.Fatalf("POST /v1/launcher-credentials = %d %s (%v), want a code", status, body, err)
	}
	looked := decodeBody[contractRecord](t, dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/machine-lookup",
		map[string]any{"code": created.Code}, approver))
	approveResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+looked.RecordID+"/approve",
		map[string]any{"code": created.Code}, approver)
	approved := decodeBody[struct {
		CredentialID *string `json:"credential_id"`
	}](t, approveResp)
	if approveResp.Code != http.StatusOK || approved.CredentialID == nil {
		t.Fatalf("approve the machine login = %d %s, want a credential_id", approveResp.Code, approveResp.Body.String())
	}
	return *approved.CredentialID, machineKey
}

// enrollWithLauncher enrolls a box under credentialID straight on the broker, as the machine's
// launcher does with a launcher proof signed by machineKey, and answers the status, body and box.
func (rig *contractRig) enrollWithLauncher(t *testing.T, machineKey *ecdsa.PrivateKey, credentialID, runtimeID string) (int, []byte, contractSession) {
	t.Helper()
	key := contractSigningKey(t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	p, err := proof.SignLauncher(machineKey, credentialID, http.MethodPost, rig.BrokerURL+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatalf("proof.SignLauncher: %v", err)
	}
	status, body := rig.brokerReq(t, http.MethodPost, "/v1/enrollments", map[string]string{"Proof": p},
		map[string]any{"kind": "box", "runtime_id": runtimeID, "thumbprint": thumbprint})
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	_ = json.Unmarshal(body, &enrolled)
	return status, body, contractSession{EnrollmentID: enrolled.EnrollmentID, Key: key}
}

// contractMachineLogin is one row of Dispatch's machine-login list.
type contractMachineLogin struct {
	CredentialID string    `json:"credential_id"`
	Host         string    `json:"host"`
	Service      *string   `json:"service"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// machineLoginsOf reads login's machine logins through Dispatch.
func (rig *contractRig) machineLoginsOf(t *testing.T, login string) []contractMachineLogin {
	t.Helper()
	resp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/machine-logins", nil, login)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET machine logins as %s = %d: %s", login, resp.Code, resp.Body.String())
	}
	return decodeBody[struct {
		Credentials []contractMachineLogin `json:"credentials"`
	}](t, resp).Credentials
}

// TestAPersonRevokesTheirMachineLoginThroughDispatch drives the machine-login page's routes against
// the real broker: the person who approved their machine's login sees it listed, and nobody else
// does; another person's revoke is refused whatever their browser's body names; the person's own
// revoke ends it, and with it the session the machine enrolled, whose grant then releases nothing
// and whose machine enrolls no one more.
func TestAPersonRevokesTheirMachineLoginThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	credentialID, machineKey := rig.approveMachineLogin(t, contractApprover, "", "contract-test-host")
	status, body, session := rig.enrollWithLauncher(t, machineKey, credentialID, "box-"+t.Name())
	if status != http.StatusCreated {
		t.Fatalf("enroll under the machine login = %d: %s", status, body)
	}
	auto := rig.request(t, session, "", "AUTO_TOKEN")
	if auto.State != "granted" || auto.GrantID == nil {
		t.Fatalf("request = %+v, want an automatic grant", auto)
	}

	if logins := rig.machineLoginsOf(t, contractApprover); len(logins) != 1 || logins[0].CredentialID != credentialID || logins[0].Host != "contract-test-host" || logins[0].Service != nil {
		t.Fatalf("%s's machine logins = %+v, want the one approved for contract-test-host, no service", contractApprover, logins)
	}
	if logins := rig.machineLoginsOf(t, contractOther); len(logins) != 0 {
		t.Fatalf("%s's machine logins = %+v, want none", contractOther, logins)
	}

	revokePath := "/api/v1/machine-logins/" + credentialID + "/revoke"
	resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{"approver": contractApprover}, contractOther)
	if resp.Code != http.StatusForbidden || decodeBody[contractError](t, resp).Code != "NOT_APPROVER" {
		t.Fatalf("revoke as %s = %d %s, want 403 NOT_APPROVER", contractOther, resp.Code, resp.Body.String())
	}
	if status, released := rig.values(t, session, *auto.GrantID); status != http.StatusOK || released["AUTO_TOKEN"] != "auto-v1" {
		t.Fatalf("values after the refused revoke = %d %v, want AUTO_TOKEN released", status, released)
	}

	resp = dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{}, contractApprover)
	if resp.Code != http.StatusOK || decodeBody[struct {
		State string `json:"state"`
	}](t, resp).State != "revoked" {
		t.Fatalf("revoke as %s = %d %s, want 200 revoked", contractApprover, resp.Code, resp.Body.String())
	}
	if status, _ := rig.values(t, session, *auto.GrantID); status != http.StatusUnauthorized {
		t.Fatalf("values after the revoke = %d, want 401: the session ended", status)
	}
	if status, body, _ := rig.enrollWithLauncher(t, machineKey, credentialID, "box-after-"+t.Name()); status != http.StatusUnauthorized || !strings.Contains(string(body), "LAUNCHER_INVALID") {
		t.Fatalf("enroll after the revoke = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	if logins := rig.machineLoginsOf(t, contractApprover); len(logins) != 0 {
		t.Fatalf("%s's machine logins after the revoke = %+v, want none", contractApprover, logins)
	}
}

// TestAServicesLoginIsListedAndRevokedByTheApproverAloneThroughDispatch: a service's machine
// login (the Legion daemon's, which has no operator) is listed, named by its service, for the
// person who approved it through Dispatch and for no one else; another person's revoke is refused
// whatever their browser's body names, and leaves its launcher proofs working; the approver's revoke
// ends it, after which its launcher proofs authenticate nothing.
func TestAServicesLoginIsListedAndRevokedByTheApproverAloneThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	credentialID, machineKey := rig.approveMachineLogin(t, contractApprover, "legion-daemon", "cluster.example")

	logins := rig.machineLoginsOf(t, contractApprover)
	if len(logins) != 1 || logins[0].CredentialID != credentialID || logins[0].Host != "cluster.example" ||
		logins[0].Service == nil || *logins[0].Service != "legion-daemon" {
		t.Fatalf("%s's machine logins = %s, want legion-daemon's login on cluster.example", contractApprover, asJSON(t, logins))
	}
	if logins := rig.machineLoginsOf(t, contractOther); len(logins) != 0 {
		t.Fatalf("%s's machine logins = %s, want none", contractOther, asJSON(t, logins))
	}

	revokePath := "/api/v1/machine-logins/" + credentialID + "/revoke"
	resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{"approver": contractApprover}, contractOther)
	if resp.Code != http.StatusForbidden || decodeBody[contractError](t, resp).Code != "NOT_APPROVER" {
		t.Fatalf("revoke as %s = %d %s, want 403 NOT_APPROVER", contractOther, resp.Code, resp.Body.String())
	}
	// A service's credential enrolls pods only, so a box is refused; what matters is that its
	// launcher proof still authenticates.
	if status, body, _ := rig.enrollWithLauncher(t, machineKey, credentialID, "box-"+t.Name()); status != http.StatusForbidden || !strings.Contains(string(body), "OPERATOR_MISMATCH") {
		t.Fatalf("enroll after the refused revoke = %d %s, want 403 OPERATOR_MISMATCH from a launcher that still authenticates", status, body)
	}

	resp = dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{}, contractApprover)
	if resp.Code != http.StatusOK {
		t.Fatalf("revoke as %s = %d %s, want 200", contractApprover, resp.Code, resp.Body.String())
	}
	if status, body, _ := rig.enrollWithLauncher(t, machineKey, credentialID, "box-after-"+t.Name()); status != http.StatusUnauthorized || !strings.Contains(string(body), "LAUNCHER_INVALID") {
		t.Fatalf("enroll after the revoke = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	if logins := rig.machineLoginsOf(t, contractApprover); len(logins) != 0 {
		t.Fatalf("%s's machine logins after the revoke = %s, want none", contractApprover, asJSON(t, logins))
	}
}
