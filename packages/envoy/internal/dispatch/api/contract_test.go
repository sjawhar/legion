// contract_test.go is Task 2's contract layer (plan-overview.md v9's own lesson: "every fake had
// been built from the client's assumption"): it mounts the real broker handlers (brokerapi.
// Register with real services on BROKER_TEST_DATABASE_URL, exactly as the broker/api tests do)
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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	brokerapi "github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	brokerstoretest "github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const (
	contractApprover = "sjawhar"
	// contractOther is a second allowed Dispatch login, neither any record's approver nor any
	// enrollment's operator.
	contractOther   = "mallory"
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

	rulesYAML := `version: 1
secrets:
  DEEL_API_KEY:
    source: example/agent-secrets/DEEL_API_KEY
    owner: ` + contractApprover + `
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: ` + contractApprover + `, decision: approval, approver: operator}
`
	rulesPath := t.TempDir() + "/rules.yaml"
	if err := os.WriteFile(rulesPath, []byte(rulesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cur, err := rules.NewCurrent(context.Background(), rules.FileLoader{Path: rulesPath}, time.Hour, func(error) {})
	if err != nil {
		t.Fatalf("rules.NewCurrent: %v", err)
	}

	brokerMux := http.NewServeMux()
	brokerServer := httptest.NewServer(brokerMux)
	t.Cleanup(brokerServer.Close)

	enr := &enroll.Service{Store: brokerStore, Lease: time.Hour}
	enr.Chain = enroll.NewChainVerifier(brokerStore, brokerServer.URL, time.Minute)
	reqMachine := &requests.Machine{
		Store: brokerStore, Rules: cur, Secrets: secrets.Fake{"example/agent-secrets/DEEL_API_KEY": "deel-v1"},
		MaxGrant: time.Hour, PendingTTL: 12 * time.Hour,
		Audience: brokerServer.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	reqMachine.Chain = requests.NewChainVerifier(brokerStore, brokerServer.URL, time.Minute)
	mach := &machine.Service{
		Store: brokerStore, Enroll: enr, Rules: cur,
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
	allowed := map[string]struct{}{contractApprover: {}, contractOther: {}}
	deps, err := NewDeps(DepsInput{
		Store: dispatchDB, Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: allowed},
		AllowedLogins: allowed, ServerURL: "https://dispatch.example", Docs: documentService, Events: events.NewBroker(),
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

// pendingAgentSecret is a pending agent_secret record and the session that asked for it, which
// alone can read a grant's value back.
type pendingAgentSecret struct {
	RecordID     string
	EnrollmentID string
	Key          *ecdsa.PrivateKey
}

// createPendingAgentSecretRecord signs a real request object with a fresh session enrollment's
// key and posts it straight to the broker's own session route — the only way a pending
// agent_secret record comes to exist — returning the record Dispatch's routes then decide.
func (rig *contractRig) createPendingAgentSecretRecord(t *testing.T, reason string, names ...string) pendingAgentSecret {
	t.Helper()
	enrollmentID, key := rig.newSessionEnrollment(t, contractApprover)
	details := make([]record.AuthorizationDetail, len(names))
	for i, name := range names {
		details[i] = record.AuthorizationDetail{Type: "agent_secret", Identifier: name, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(key, rig.BrokerURL, details, reason, "", time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	p, err := proof.Sign(key, enrollmentID, http.MethodPost, rig.BrokerURL+"/v1/requests", time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	status, body := rig.brokerReq(t, http.MethodPost, "/v1/requests", map[string]string{"Proof": p},
		map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d: %s", status, body)
	}
	created := struct {
		RecordID *string `json:"record_id"`
	}{}
	if err := json.Unmarshal(body, &created); err != nil || created.RecordID == nil {
		t.Fatalf("decode create-request response: %v (body: %s)", err, body)
	}
	return pendingAgentSecret{RecordID: *created.RecordID, EnrollmentID: enrollmentID, Key: key}
}

// values reads a grant's released values as the session that holds it, straight from the broker.
func (rig *contractRig) values(t *testing.T, pending pendingAgentSecret, grantID string) (int, map[string]string) {
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
			GrantID string `json:"grant_id"`
		} `json:"grants"`
	}](t, grantsResp)
	found := false
	for _, g := range grants.Grants {
		if g.GrantID == grantID {
			found = true
		}
	}
	if !found {
		t.Fatalf("grants list %+v does not include %s", grants.Grants, grantID)
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
