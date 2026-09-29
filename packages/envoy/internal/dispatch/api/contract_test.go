// contract_test.go is Task 2's contract layer (plan-overview.md v9's own lesson: "every fake had
// been built from the client's assumption"): it mounts the real broker handlers (brokerapi.
// Register with real services on BROKER_TEST_DATABASE_URL, seeded via webauthntest + a seeded
// approver key, exactly as Plan A's broker/api tests do) behind an httptest.Server, wires
// Dispatch's own routes to relay to it, and drives every UI action through DISPATCH's routes —
// proving the proxy round-trips a real broker, not a fake built from agentsecrets.Client's own
// assumptions. Skips without BROKER_TEST_DATABASE_URL (via brokerstoretest.Open) or
// DISPATCH_TEST_DATABASE_URL (via storetest.Open, already required by every other test in this
// package).
package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	"github.com/sjawhar/envoy/internal/broker/approvers"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	brokerstoretest "github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/broker/webauthntest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const (
	contractRPID     = "dispatch.contract.test"
	contractOrigin   = "https://dispatch.contract.test"
	contractAAGUID   = "ee882879-721c-4913-9775-3dfcce97072a"
	contractApprover = "sjawhar"
	contractUIToken  = "contract-ui-token"
)

// contractRig is the real broker (real Postgres, real WebAuthn software authenticator) behind
// Dispatch's own mounted routes. Every UI action a test drives goes through rig.Dispatch; rig's
// direct broker helpers exist only to seed fixtures Dispatch has no route to create (a pending
// request comes from an agent's session proof, a machine login from the machine's own key —
// neither is a Dispatch UI action).
type contractRig struct {
	Dispatch    http.Handler
	BrokerURL   string
	Approver    *webauthntest.Authenticator
	CA          *webauthntest.CA
	brokerStore *store.Store
}

func newContractRig(t *testing.T) *contractRig {
	t.Helper()
	brokerStore := brokerstoretest.Open(t)

	ca := webauthntest.NewCA(t)
	auth := ca.NewAuthenticator(t, uuid.MustParse(contractAAGUID))
	nonce := strings.Repeat("a", 64)
	challenge := record.RegisterChallenge(contractApprover, nonce)
	entry := approvers.KeyEntry{
		CredentialID:   base64.RawURLEncoding.EncodeToString(auth.CredentialID),
		ChallengeNonce: nonce,
		Registration:   auth.Register(t, contractRPID, contractOrigin, challenge[:]),
		Seed:           true,
	}
	if _, err := brokerStore.Pool.Exec(context.Background(),
		`insert into approver_key_seeds (login, credential_id) values ($1,$2)`, contractApprover, entry.CredentialID); err != nil {
		t.Fatalf("insert approver_key_seeds: %v", err)
	}
	approversSvc := &approvers.Service{Store: brokerStore, Verifier: &approvers.Verifier{
		Roots: ca.Pool(), Origin: contractOrigin, AAGUIDs: map[uuid.UUID]bool{uuid.MustParse(contractAAGUID): true},
	}}
	if err := approversSvc.Reconcile(context.Background(), map[string][]approvers.KeyEntry{contractApprover: {entry}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rulesYAML := `version: 1
secrets:
  DEEL_API_KEY:
    source: example/agent-secrets/DEEL_API_KEY
    owner: ` + contractApprover + `
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: ` + contractApprover + `, decision: approval, approver: operator}
approvers:
  origin: ` + contractOrigin + `
  aaguids: ["` + contractAAGUID + `"]
  logins:
    ` + contractApprover + `:
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

	brokerMux := http.NewServeMux()
	brokerServer := httptest.NewServer(brokerMux)
	t.Cleanup(brokerServer.Close)

	enr := &enroll.Service{Store: brokerStore, Lease: time.Hour}
	enr.Chain = enroll.NewChainVerifier(brokerStore, approversSvc, brokerServer.URL, time.Minute)
	reqMachine := &requests.Machine{
		Store: brokerStore, Rules: cur, Secrets: secrets.Fake{"example/agent-secrets/DEEL_API_KEY": "deel-v1"},
		Approvers: approversSvc, MaxGrant: time.Hour, PendingTTL: 12 * time.Hour,
		Audience: brokerServer.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	mach := &machine.Service{
		Store: brokerStore, Enroll: enr, Approvers: approversSvc, Rules: cur,
		Audience: brokerServer.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		Replay: enr.Replay,
	}
	brokerapi.Register(brokerMux, brokerapi.Deps{
		PublicURL: brokerServer.URL, UIOrigin: contractOrigin, UIToken: contractUIToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach, Approvers: approversSvc,
		Proof: &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
	})

	dispatchDB := storetest.Open(t)
	documentService := docs.New(docs.Deps{
		Store: dispatchDB, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond,
	})
	t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
	allowed := map[string]struct{}{contractApprover: {}}
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

	return &contractRig{
		Dispatch: dispatchMux, BrokerURL: brokerServer.URL, Approver: auth, CA: ca,
		brokerStore: brokerStore,
	}
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

// createPendingAgentSecretRecord signs a real request object with a fresh session enrollment's
// key and posts it straight to the broker's own session route — the only way a pending
// agent_secret record comes to exist — returning the record id Dispatch's routes then decide.
func (rig *contractRig) createPendingAgentSecretRecord(t *testing.T, reason string, names ...string) string {
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
	return *created.RecordID
}

func decodeChallenge(t *testing.T, b64 string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode challenge %q: %v", b64, err)
	}
	return raw
}

type contractChallenges struct {
	Approve string `json:"approve"`
	Deny    string `json:"deny"`
}

type contractRecord struct {
	RecordID   string              `json:"record_id"`
	Kind       string              `json:"kind"`
	State      string              `json:"state"`
	Challenges *contractChallenges `json:"challenges"`
}

// --- tests ---

// TestApproveRelaysTheAssertionAndTheBrokerDecides is the plan's own contract test, copied
// verbatim (plan-dispatch-credential-inbox.md Task 2): approving through Dispatch's route with a
// real assertion succeeds and the broker's answer passes straight through; replaying the same
// call is the broker's own 409, unchanged by Dispatch.
func TestApproveRelaysTheAssertionAndTheBrokerDecides(t *testing.T) {
	rig := newContractRig(t)
	recordID := rig.createPendingAgentSecretRecord(t, "need it for the demo", "DEEL_API_KEY")

	readResp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-requests/"+recordID, nil, contractApprover)
	var readRecord contractRecord
	if err := json.Unmarshal(readResp.Body.Bytes(), &readRecord); err != nil || readRecord.Challenges == nil {
		t.Fatalf("record read = %s (err %v), want challenges", readResp.Body.String(), err)
	}

	assertion := rig.Approver.Assert(t, contractRPID, contractOrigin, decodeChallenge(t, readRecord.Challenges.Approve))
	body := map[string]any{"assertion": json.RawMessage(assertion)}
	resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+recordID+"/approve", body, contractApprover)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"approved"`) {
		t.Fatalf("%d %s", resp.Code, resp.Body.String())
	}
	// Dispatch added nothing and decided nothing: replaying the same call is the broker's 409.
	resp = dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+recordID+"/approve", body, contractApprover)
	if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "RECORD_TERMINAL") {
		t.Fatalf("terminal forwarding: %d %s", resp.Code, resp.Body.String())
	}
}

// TestPendingListShowsCreatedRecordThroughDispatch drives the inbox's own query: a pending
// record shows in Dispatch's proxied ?approver=me list with its minimal facts.
func TestPendingListShowsCreatedRecordThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	recordID := rig.createPendingAgentSecretRecord(t, "", "DEEL_API_KEY")

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
// signed login request produces a code; Dispatch's lookup route resolves it to the record and its
// challenges (ruling 13's one exception); approving through Dispatch mints a launcher credential.
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
	var looked contractRecord
	if err := json.Unmarshal(lookupResp.Body.Bytes(), &looked); err != nil || looked.Challenges == nil {
		t.Fatalf("machine-lookup body = %s (err %v), want challenges", lookupResp.Body.String(), err)
	}
	if looked.Kind != "launcher_credential" {
		t.Fatalf("machine-lookup kind = %q, want launcher_credential", looked.Kind)
	}

	assertion := rig.Approver.Assert(t, contractRPID, contractOrigin, decodeChallenge(t, looked.Challenges.Approve))
	approveResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+looked.RecordID+"/approve",
		map[string]any{"assertion": json.RawMessage(assertion), "code": created.Code}, contractApprover)
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

// TestApproverKeysListAndCeremoniesThroughDispatch drives the key pages' three proxied routes:
// the seeded key lists, a register ceremony returns yaml, and an endorse ceremony (signed by the
// seeded key over the new key's hash) returns yaml too.
func TestApproverKeysListAndCeremoniesThroughDispatch(t *testing.T) {
	rig := newContractRig(t)

	keysResp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-keys/"+contractApprover, nil, contractApprover)
	if keysResp.Code != http.StatusOK {
		t.Fatalf("GET keys = %d: %s", keysResp.Code, keysResp.Body.String())
	}
	keys := decodeBody[struct {
		Keys []struct {
			CredentialID string `json:"credential_id"`
			Seeded       bool   `json:"seeded"`
		} `json:"keys"`
	}](t, keysResp)
	if len(keys.Keys) != 1 || !keys.Keys[0].Seeded {
		t.Fatalf("keys = %+v, want one seeded key", keys.Keys)
	}
	seededCredentialID := keys.Keys[0].CredentialID

	beginResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-keys/"+contractApprover+"/register/begin", nil, contractApprover)
	if beginResp.Code != http.StatusOK {
		t.Fatalf("register/begin = %d: %s", beginResp.Code, beginResp.Body.String())
	}
	begin := decodeBody[struct {
		CeremonyID string `json:"ceremony_id"`
		PublicKey  struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}](t, beginResp)
	if begin.CeremonyID == "" {
		t.Fatalf("register/begin = %+v, want a ceremony id", begin)
	}

	newKeyAuth := rig.CA.NewAuthenticator(t, uuid.MustParse(contractAAGUID)) // must chain to the broker's pinned root
	registration := newKeyAuth.Register(t, contractRPID, contractOrigin, decodeChallenge(t, begin.PublicKey.Challenge))
	finishResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-keys/"+contractApprover+"/register/finish",
		map[string]any{"ceremony_id": begin.CeremonyID, "response": json.RawMessage(registration)}, contractApprover)
	if finishResp.Code != http.StatusOK {
		t.Fatalf("register/finish = %d: %s", finishResp.Code, finishResp.Body.String())
	}
	finished := decodeBody[struct {
		YAML string `json:"yaml"`
	}](t, finishResp)
	if !strings.Contains(finished.YAML, "credential_id") {
		t.Fatalf("register/finish yaml = %q, want it to mention credential_id", finished.YAML)
	}

	keyHash := sha256.Sum256(newKeyAuth.CredentialID)
	endorseBeginResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-keys/"+contractApprover+"/endorse/begin",
		map[string]any{"credential_id": seededCredentialID, "key_hash": hex.EncodeToString(keyHash[:])}, contractApprover)
	if endorseBeginResp.Code != http.StatusOK {
		t.Fatalf("endorse/begin = %d: %s", endorseBeginResp.Code, endorseBeginResp.Body.String())
	}
	endorseBegin := decodeBody[struct {
		CeremonyID string `json:"ceremony_id"`
		PublicKey  struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}](t, endorseBeginResp)
	if endorseBegin.CeremonyID == "" {
		t.Fatalf("endorse/begin = %+v, want a ceremony id", endorseBegin)
	}

	endorseAssertion := rig.Approver.Assert(t, contractRPID, contractOrigin, decodeChallenge(t, endorseBegin.PublicKey.Challenge))
	endorseFinishResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-keys/"+contractApprover+"/endorse/finish",
		map[string]any{"ceremony_id": endorseBegin.CeremonyID, "response": json.RawMessage(endorseAssertion)}, contractApprover)
	if endorseFinishResp.Code != http.StatusOK {
		t.Fatalf("endorse/finish = %d: %s", endorseFinishResp.Code, endorseFinishResp.Body.String())
	}
	endorseFinished := decodeBody[struct {
		YAML string `json:"yaml"`
	}](t, endorseFinishResp)
	if !strings.Contains(endorseFinished.YAML, "endorsement") {
		t.Fatalf("endorse/finish yaml = %q, want it to mention endorsement", endorseFinished.YAML)
	}
}

// TestGrantsListAndRevokeByApproverThroughDispatch approves a request to mint a grant, lists it
// through Dispatch's own grants route, then revokes it with an assertion over the revoke
// challenge — the human path that ends a grant, distinct from a session ending its own.
func TestGrantsListAndRevokeByApproverThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	recordID := rig.createPendingAgentSecretRecord(t, "", "DEEL_API_KEY")

	readResp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-requests/"+recordID, nil, contractApprover)
	var readRecord contractRecord
	if err := json.Unmarshal(readResp.Body.Bytes(), &readRecord); err != nil || readRecord.Challenges == nil {
		t.Fatalf("record read = %s (err %v), want challenges", readResp.Body.String(), err)
	}
	assertion := rig.Approver.Assert(t, contractRPID, contractOrigin, decodeChallenge(t, readRecord.Challenges.Approve))
	approveResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"assertion": json.RawMessage(assertion)}, contractApprover)
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

	revokeChallenge := record.RevokeChallenge(grantID)
	revokeAssertion := rig.Approver.Assert(t, contractRPID, contractOrigin, revokeChallenge[:])
	revokeResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-grants/"+grantID+"/revoke",
		map[string]any{"assertion": json.RawMessage(revokeAssertion)}, contractApprover)
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
