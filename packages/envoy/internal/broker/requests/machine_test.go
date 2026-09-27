package requests

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/broker/approvers"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/broker/webauthntest"
)

const (
	testAudience = "broker"
	testOrigin   = "https://dispatch.test"
	testRPID     = "dispatch.test"
	testAAGUID   = "ee882879-721c-4913-9775-3dfcce97072a"
)

func str(s string) *string { return &s }

// ch flattens a domain-separated challenge's fixed-size digest to the []byte shape
// Authenticator.Assert wants.
func ch(c [32]byte) []byte { return c[:] }

// newApprover registers a real WebAuthn key (webauthntest packed attestation over a fresh test
// CA) for login and returns the CA (the trust root a fixture's approvers.Verifier must share), the
// authenticator (for later Assert calls), and the parsed KeyEntry a rules file's
// approvers.logins.<login>.keys needs to reference it — seeded, not endorsed, so Reconcile accepts
// it with no other live key on the login.
func newApproverKey(t *testing.T, ca *webauthntest.CA, login string) (*webauthntest.Authenticator, approvers.KeyEntry) {
	t.Helper()
	auth := ca.NewAuthenticator(t, uuid.MustParse(testAAGUID))
	nonce := strings.Repeat("a", 64)
	challenge := record.RegisterChallenge(login, nonce)
	entry := approvers.KeyEntry{
		CredentialID:   base64.RawURLEncoding.EncodeToString(auth.CredentialID),
		ChallengeNonce: nonce,
		Registration:   auth.Register(t, testRPID, testOrigin, challenge[:]),
		Seed:           true,
	}
	return auth, entry
}

func newApprover(t *testing.T, login string) (*webauthntest.CA, *webauthntest.Authenticator, approvers.KeyEntry) {
	t.Helper()
	ca := webauthntest.NewCA(t)
	auth, entry := newApproverKey(t, ca, login)
	return ca, auth, entry
}

// approversYAML renders the rules file's approvers: section for one login's key entry, in the
// same string-building shape rules_test.go's TestApproversSectionParses uses (YAML is a JSON
// superset, so the registration blob embeds verbatim).
func approversYAML(login string, entry approvers.KeyEntry) string {
	return "approvers:\n  origin: " + testOrigin + "\n  aaguids: [\"" + testAAGUID + "\"]\n  logins:\n    " + login + ":\n      keys:\n" +
		"        - credential_id: \"" + entry.CredentialID + "\"\n" +
		"          registration:\n            challenge_nonce: \"" + entry.ChallengeNonce + "\"\n            response: " + string(entry.Registration) + "\n" +
		"          seed: true\n"
}

// baseRulesYAML is the fixture's secrets section: DEEL_API_KEY needs sjawhar's approval for a box
// operated by sjawhar or for any pod, AUTO_TOKEN is automatic for a box operated by sjawhar,
// DENIED_KEY matches no requester and always denies.
const baseRulesYAML = `version: 1
secrets:
  DEEL_API_KEY:
    source: dev1/agent-secrets/DEEL_API_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
      - {kind: pod, decision: approval, approver: "login:sjawhar"}
  AUTO_TOKEN:
    source: dev1/agent-secrets/AUTO_TOKEN
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters: [{kind: box, operator: sjawhar, decision: automatic}]
  DENIED_KEY:
    source: dev1/agent-secrets/DENIED_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters: []
`

// newRulesCurrent points a fresh *rules.Current at yaml, written to a temp file (rules.Current has
// no in-memory loader, only FileLoader/S3Loader).
func newRulesCurrent(t *testing.T, yaml string) *rules.Current {
	t.Helper()
	path := t.TempDir() + "/rules.yaml"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cur, err := rules.NewCurrent(ctx, rules.FileLoader{Path: path}, time.Hour, func(error) {})
	if err != nil {
		t.Fatalf("rules.NewCurrent: %v", err)
	}
	return cur
}

// withRules re-points m at a rules file holding yaml — for a test that starts with the fixture's
// rules and then simulates a reload.
func withRules(t *testing.T, m *Machine, yaml string) {
	t.Helper()
	m.Rules = newRulesCurrent(t, yaml)
}

// replayer builds a Machine.Replay backed directly by the proof_jtis table, mirroring
// enroll.Service.Replay (which this fixture cannot use — see newEnrollment).
func replayer(st *store.Store) func(context.Context, string, time.Time) (bool, error) {
	return func(ctx context.Context, jti string, expires time.Time) (bool, error) {
		_, err := st.Pool.Exec(ctx, `insert into proof_jtis (jti, expires_at) values ($1,$2)`, jti, expires)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return false, nil
		}
		return err == nil, err
	}
}

// newEnrollment inserts a live enrollment directly, rather than through enroll.Service.Create:
// that service still writes the enrollments.approver_kind/approver_issue columns migration 0005
// already dropped (Task 7's own fix, tracked in this task's report, not this file's job), so it
// cannot be used to build fixtures on this schema yet. Returns the enrollment id and the
// requester's own signing key, whose RFC 7638 thumbprint the row carries (and Create checks a
// request object's iss against).
func newEnrollment(t *testing.T, st *store.Store, kind, runtimeID string, operator, subject *string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate enrollment key: %v", err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	credentialID := uuid.New()
	if _, err := st.Pool.Exec(ctx, `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,'devbox-test',$3,'{}'::jsonb, now() + interval '30 days')`, credentialID, operator, uuid.NewString()); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	id := uuid.NewString()
	if _, err := st.Pool.Exec(ctx, `insert into enrollments (id, kind, runtime_id, operator, thumbprint, subject, launcher_credential_id, lease_expires_at)
		values ($1,$2,$3,$4,$5,$6,$7, now() + interval '1 hour')`, id, kind, runtimeID, operator, thumbprint, subject, credentialID); err != nil {
		t.Fatalf("insert enrollments: %v", err)
	}
	return id, key
}

// newFixture opens a store on a fresh schema, registers and reconciles a real "sjawhar" approver
// key, builds a *rules.Current from baseRulesYAML plus that key's approvers section, and enrolls
// one live box/sjawhar enrollment. Returns the Machine, that enrollment's id, its own signing key
// (for record.Sign), and the approver's authenticator (for Assert/AssertWithCounter/RevokeKey).
func newFixture(t *testing.T) (m *Machine, enrollmentID string, requesterKey *ecdsa.PrivateKey, approver *webauthntest.Authenticator) {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t)

	ca, auth, entry := newApprover(t, "sjawhar")
	if _, err := st.Pool.Exec(ctx, `insert into approver_key_seeds (login, credential_id) values ($1,$2)`, "sjawhar", entry.CredentialID); err != nil {
		t.Fatalf("insert approver_key_seeds: %v", err)
	}
	approversSvc := &approvers.Service{Store: st, Verifier: &approvers.Verifier{
		Roots: ca.Pool(), Origin: testOrigin, AAGUIDs: map[uuid.UUID]bool{uuid.MustParse(testAAGUID): true},
	}}
	if err := approversSvc.Reconcile(ctx, map[string][]approvers.KeyEntry{"sjawhar": {entry}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cur := newRulesCurrent(t, baseRulesYAML+approversYAML("sjawhar", entry))
	enrollmentID, requesterKey = newEnrollment(t, st, "box", "box-a-"+t.Name(), str("sjawhar"), nil)

	m = &Machine{
		Store: st,
		Rules: cur,
		Secrets: secrets.Fake{
			"dev1/agent-secrets/DEEL_API_KEY": "deel-v1",
			"dev1/agent-secrets/AUTO_TOKEN":   "auto-v1",
		},
		Approvers:  approversSvc,
		MaxGrant:   time.Hour,
		PendingTTL: 12 * time.Hour,
		Audience:   testAudience,
		Skew:       time.Minute,
		Replay:     replayer(st),
	}
	return m, enrollmentID, requesterKey, auth
}

// signRequest signs an agent_secret request object over names, ready for Machine.Create.
func signRequest(t *testing.T, m *Machine, key *ecdsa.PrivateKey, reason string, names ...string) string {
	t.Helper()
	details := make([]record.AuthorizationDetail, len(names))
	for i, n := range names {
		details[i] = record.AuthorizationDetail{Type: "agent_secret", Identifier: n, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(key, m.Audience, details, reason, "", time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	return compact
}

func TestCreatePendingWritesTheRecordAndApproveMintsTheGrant(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	compact, err := record.Sign(key, m.Audience, []record.AuthorizationDetail{{Type: "agent_secret", Identifier: "DEEL_API_KEY", Actions: []string{"inject"}}}, "why", "", time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	req, err := m.Create(ctx, enr, compact, "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("%+v %v", req, err)
	}
	var body string
	if err := m.Store.Pool.QueryRow(ctx, `select body from credential_requests where id=$1`, *req.RecordID).Scan(&body); err != nil {
		t.Fatalf("read record body: %v", err)
	}
	parsed, err := record.ParseBody(body)
	if err != nil || parsed.Approver != "sjawhar" || parsed.ID() != *req.RecordID {
		t.Fatalf("record body %q: %+v %v", body, parsed, err)
	}
	assertion := approver.Assert(t, testRPID, testOrigin, ch(record.ApproveChallenge(*req.RecordID)))
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, assertion)
	if err != nil || dec.GrantID == "" {
		t.Fatal(err)
	}
	// second decision changes nothing
	if _, err := m.ApplyDecision(ctx, *req.RecordID, false, assertion); !errors.Is(err, ErrTerminal) {
		t.Fatalf("late deny: %v", err)
	}
}

// TestValuesSucceedsTwiceOnALiveApprovedGrant pins that VerifyChain's re-verification of the
// stored approval assertion is idempotent: the deciding transaction already bumped sign_count to
// exactly that assertion's own counter, so a re-verification of the same historical assertion
// always finds counter<=sign_count (approvers.ErrCounterReplay) and must treat that as expected,
// not as chain failure — otherwise a grant would work exactly once and never again.
func TestValuesSucceedsTwiceOnALiveApprovedGrant(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "why", "DEEL_API_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create: %+v %v", req, err)
	}
	assertion := approver.Assert(t, testRPID, testOrigin, ch(record.ApproveChallenge(*req.RecordID)))
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, assertion)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision: %+v %v", dec, err)
	}
	if _, _, _, err := m.Values(ctx, dec.GrantID, enr); err != nil {
		t.Fatalf("Values(first release) = %v, want success", err)
	}
	if _, _, _, err := m.Values(ctx, dec.GrantID, enr); err != nil {
		t.Fatalf("Values(second release, same never-revoked grant) = %v, want success", err)
	}
}

func TestADenyAssertionCannotApprove(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "why", "DEEL_API_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("%+v %v", req, err)
	}
	denyAssertion := approver.Assert(t, testRPID, testOrigin, ch(record.DenyChallenge(*req.RecordID)))
	if _, err := m.ApplyDecision(ctx, *req.RecordID, true, denyAssertion); err == nil {
		t.Fatal("approve with an assertion signed over the deny challenge succeeded, want it refused")
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil || got.State != "pending" {
		t.Fatalf("Get = %+v, %v, want still pending", got, err)
	}
}

func TestValuesRefusesAGrantWhoseRowWasForged(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "why", "DEEL_API_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("%+v %v", req, err)
	}
	// Simulate a database writer other than the broker: a grant row pointing at a request whose
	// record has never received an approval event.
	grantID := uuid.NewString()
	if _, err := m.Store.Pool.Exec(ctx, `insert into grants (id, request_id, enrollment_id, approver, expires_at) values ($1,$2,$3,'sjawhar', now() + interval '1 hour')`,
		grantID, req.ID, enr); err != nil {
		t.Fatalf("insert forged grant: %v", err)
	}
	if _, _, _, err := m.Values(ctx, grantID, enr); !errors.Is(err, ErrGrantChainInvalid) {
		t.Fatalf("Values(forged grant) = %v, want ErrGrantChainInvalid", err)
	}
}

func TestRevokingTheApprovingKeyKillsTheGrantAtNextRelease(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "why", "DEEL_API_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("%+v %v", req, err)
	}
	assertion := approver.Assert(t, testRPID, testOrigin, ch(record.ApproveChallenge(*req.RecordID)))
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, assertion)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision: %+v %v", dec, err)
	}
	if _, _, _, err := m.Values(ctx, dec.GrantID, enr); err != nil {
		t.Fatalf("Values before revoke: %v", err)
	}

	credentialID := base64.RawURLEncoding.EncodeToString(approver.CredentialID)
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := m.Approvers.RevokeKey(ctx, tx, credentialID, "human:sjawhar"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, _, _, err := m.Values(ctx, dec.GrantID, enr); !errors.Is(err, ErrGrantChainInvalid) {
		t.Fatalf("Values after the approving key was revoked = %v, want ErrGrantChainInvalid", err)
	}
}

func TestCreateRefusesLoginHintAndForeignThumbprint(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()

	withHint, err := record.Sign(key, m.Audience, []record.AuthorizationDetail{{Type: "agent_secret", Identifier: "DEEL_API_KEY", Actions: []string{"inject"}}}, "why", "sjawhar", time.Now())
	if err != nil {
		t.Fatalf("record.Sign(login_hint): %v", err)
	}
	if _, err := m.Create(ctx, enr, withHint, ""); !errors.Is(err, record.ErrRequestInvalid) {
		t.Fatalf("Create(login_hint) = %v, want ErrRequestInvalid", err)
	}

	foreignKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate foreign key: %v", err)
	}
	foreign := signRequest(t, m, foreignKey, "why", "DEEL_API_KEY")
	if _, err := m.Create(ctx, enr, foreign, ""); !errors.Is(err, record.ErrRequestInvalid) {
		t.Fatalf("Create(foreign thumbprint) = %v, want ErrRequestInvalid", err)
	}
}

func TestAutomaticAndDeniedEvaluationsWriteNoRecordRow(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()

	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || auto.State != "granted" || auto.RecordID != nil || auto.GrantID == nil {
		t.Fatalf("Create(automatic) = %+v, %v, want granted with a grant and no record", auto, err)
	}
	denied, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DENIED_KEY"), "")
	if err != nil || denied.State != "denied" || denied.RecordID != nil {
		t.Fatalf("Create(denied) = %+v, %v, want denied with no record", denied, err)
	}
	var count int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from credential_requests`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("credential_requests rows = %d, %v, want 0", count, err)
	}
}

func TestDenyOpensNoRecord(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY", "DENIED_KEY"), "")
	if err != nil || req.State != "denied" || req.RecordID != nil {
		t.Fatalf("Create = %+v, %v, want denied with no record (any denied name refuses the whole request)", req, err)
	}
}

func TestCoalescesIdenticalPendingAndReturnsTheFirstRecordID(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	first, err := m.Create(ctx, enr, signRequest(t, m, key, "first", "DEEL_API_KEY"), "")
	if err != nil || first.RecordID == nil {
		t.Fatalf("Create(first) = %+v, %v", first, err)
	}
	second, err := m.Create(ctx, enr, signRequest(t, m, key, "second", "DEEL_API_KEY"), "")
	if err != nil {
		t.Fatalf("Create(second): %v", err)
	}
	if !second.Coalesced || second.ID != first.ID || second.RecordID == nil || *second.RecordID != *first.RecordID {
		t.Fatalf("second = %+v, want coalesced onto request %s / record %s", second, first.ID, *first.RecordID)
	}
	var count int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from credential_requests`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("credential_requests rows = %d, %v, want 1 (a coalesced request never gets its own record)", count, err)
	}
}

func TestExpirePendingWritesTheExpiredEventAndFlipsTheRequest(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("%+v %v", req, err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update requests set pending_expires_at = now() - interval '1 hour' where id=$1`, req.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	n, err := m.ExpirePending(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("ExpirePending = %d, %v, want 1", n, err)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil || got.State != "expired" {
		t.Fatalf("Get = %+v, %v, want expired", got, err)
	}
	var event, actor string
	if err := m.Store.Pool.QueryRow(ctx, `select event, actor from credential_request_events where record_id=$1`, *req.RecordID).Scan(&event, &actor); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if event != "expired" || actor != "broker" {
		t.Fatalf("event=%q actor=%q, want expired/broker", event, actor)
	}
}

func TestExpirePendingAuditsEveryExpiredRequest(t *testing.T) {
	m, enrA, keyA, _ := newFixture(t)
	ctx := context.Background()
	enrB, keyB := newEnrollment(t, m.Store, "box", "box-b-"+t.Name(), str("sjawhar"), nil)

	reqA, err := m.Create(ctx, enrA, signRequest(t, m, keyA, "need it", "DEEL_API_KEY"), "")
	if err != nil {
		t.Fatalf("Create(A): %v", err)
	}
	reqB, err := m.Create(ctx, enrB, signRequest(t, m, keyB, "need it too", "DEEL_API_KEY"), "")
	if err != nil {
		t.Fatalf("Create(B): %v", err)
	}
	for _, id := range []string{reqA.ID, reqB.ID} {
		if _, err := m.Store.Pool.Exec(ctx, `update requests set pending_expires_at = now() - interval '1 hour' where id=$1`, id); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}
	n, err := m.ExpirePending(ctx, time.Now())
	if err != nil || n != 2 {
		t.Fatalf("ExpirePending = %d, %v, want 2", n, err)
	}
	for _, id := range []string{reqA.ID, reqB.ID} {
		got, err := m.Get(ctx, id)
		if err != nil || got.State != "expired" {
			t.Fatalf("Get(%s) = %+v, %v, want expired", id, got, err)
		}
		var auditCount int
		if err := m.Store.Pool.QueryRow(ctx, `select count(*) from audit where kind='request.expired' and request_id=$1`, id).Scan(&auditCount); err != nil {
			t.Fatalf("count audit(%s): %v", id, err)
		}
		if auditCount != 1 {
			t.Fatalf("audit rows for %s = %d, want exactly 1", id, auditCount)
		}
	}
}

func TestOtherEnrollmentCannotUseGrant(t *testing.T) {
	m, enrA, keyA, _ := newFixture(t)
	ctx := context.Background()
	enrB, _ := newEnrollment(t, m.Store, "box", "box-b-"+t.Name(), str("sjawhar"), nil)

	granted, err := m.Create(ctx, enrA, signRequest(t, m, keyA, "need it", "AUTO_TOKEN"), "")
	if err != nil {
		t.Fatalf("Create(automatic): %v", err)
	}
	pending, err := m.Create(ctx, enrA, signRequest(t, m, keyA, "need it", "DEEL_API_KEY"), "")
	if err != nil {
		t.Fatalf("Create(pending): %v", err)
	}
	if _, _, _, err := m.Values(ctx, *granted.GrantID, enrB); !errors.Is(err, ErrNotYours) {
		t.Fatalf("Values(other enrollment) = %v, want ErrNotYours", err)
	}
	if err := m.Cancel(ctx, pending.ID, enrB); !errors.Is(err, ErrNotYours) {
		t.Fatalf("Cancel(other enrollment) = %v, want ErrNotYours", err)
	}
	if err := m.RevokeGrant(ctx, *granted.GrantID, enrB); !errors.Is(err, ErrNotYours) {
		t.Fatalf("RevokeGrant(other enrollment) = %v, want ErrNotYours", err)
	}
}

func TestRevokeStopsValues(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || req.GrantID == nil {
		t.Fatalf("Create = %+v, %v", req, err)
	}
	if err := m.RevokeGrant(ctx, *req.GrantID, enr); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	if _, _, _, err := m.Values(ctx, *req.GrantID, enr); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values(after revoke) = %v, want ErrGrantNotLive", err)
	}
}

func TestMachineSessionID(t *testing.T) {
	m, enrA, keyA, _ := newFixture(t)
	ctx := context.Background()
	enrB, keyB := newEnrollment(t, m.Store, "box", "box-b-"+t.Name(), str("sjawhar"), nil)

	compact, err := record.Sign(keyA, m.Audience, []record.AuthorizationDetail{{Type: "agent_secret", Identifier: "AUTO_TOKEN", Actions: []string{"inject"}}}, "need it", "", time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	withSession, err := m.Create(ctx, enrA, compact, "session-abc")
	if err != nil {
		t.Fatalf("Create(withSession): %v", err)
	}
	if got, err := m.SessionID(ctx, withSession.ID); err != nil || got != "session-abc" {
		t.Fatalf("SessionID(withSession) = %q, %v, want %q, nil", got, err, "session-abc")
	}

	noSession, err := m.Create(ctx, enrB, signRequest(t, m, keyB, "need it", "AUTO_TOKEN"), "")
	if err != nil {
		t.Fatalf("Create(noSession): %v", err)
	}
	if got, err := m.SessionID(ctx, noSession.ID); err != nil || got != "" {
		t.Fatalf("SessionID(noSession) = %q, %v, want empty, nil", got, err)
	}
	if got, err := m.SessionID(ctx, uuid.NewString()); err != nil || got != "" {
		t.Fatalf("SessionID(nonexistent) = %q, %v, want empty, nil", got, err)
	}
}

func TestCreateReusesLiveGrantForIdenticalNameSetWithoutNewRecord(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create: %+v %v", req, err)
	}
	assertion := approver.Assert(t, testRPID, testOrigin, ch(record.ApproveChallenge(*req.RecordID)))
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, assertion)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision: %+v %v", dec, err)
	}

	reused, err := m.Create(ctx, enr, signRequest(t, m, key, "need it again", "DEEL_API_KEY"), "")
	if err != nil {
		t.Fatalf("Create(again): %v", err)
	}
	if reused.ID != req.ID || reused.GrantID == nil || *reused.GrantID != dec.GrantID {
		t.Fatalf("reused = %+v, want the same request %q and grant %q", reused, req.ID, dec.GrantID)
	}
	var recordCount int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from credential_requests`).Scan(&recordCount); err != nil || recordCount != 1 {
		t.Fatalf("credential_requests rows = %d, %v, want 1 (reuse never writes a new record)", recordCount, err)
	}
}

func TestCreateDoesNotReuseLiveGrantForADifferentNameSet(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	first, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || first.RecordID == nil {
		t.Fatalf("Create: %+v %v", first, err)
	}
	assertion := approver.Assert(t, testRPID, testOrigin, ch(record.ApproveChallenge(*first.RecordID)))
	if _, err := m.ApplyDecision(ctx, *first.RecordID, true, assertion); err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}

	second, err := m.Create(ctx, enr, signRequest(t, m, key, "need more", "DEEL_API_KEY", "AUTO_TOKEN"), "")
	if err != nil {
		t.Fatalf("Create(superset): %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("Create(superset) reused the first request, want a fresh one")
	}
}

func TestCreateDoesNotReuseAnExpiredOrRevokedGrant(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	granted, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Create = %+v, %v, want a grant id", granted, err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update grants set expires_at = now() - interval '1 hour' where id=$1`, *granted.GrantID); err != nil {
		t.Fatalf("backdate grant: %v", err)
	}

	fresh, err := m.Create(ctx, enr, signRequest(t, m, key, "need it again", "AUTO_TOKEN"), "")
	if err != nil {
		t.Fatalf("Create(after expiry): %v", err)
	}
	if fresh.ID == granted.ID || fresh.GrantID == nil || *fresh.GrantID == *granted.GrantID {
		t.Fatalf("fresh = %+v, want a brand-new request+grant, not the expired one %+v", fresh, granted)
	}

	if err := m.RevokeGrant(ctx, *fresh.GrantID, enr); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	again, err := m.Create(ctx, enr, signRequest(t, m, key, "need it a third time", "AUTO_TOKEN"), "")
	if err != nil {
		t.Fatalf("Create(after revoke): %v", err)
	}
	if again.ID == fresh.ID || again.GrantID == nil || *again.GrantID == *fresh.GrantID {
		t.Fatalf("again = %+v, want a brand-new request+grant, not the revoked one %+v", again, fresh)
	}
}

// TestValuesHoldsNoPooledConnectionWhileReadingSecrets pins that Values reads a grant's names into
// memory before fetching the first value, so no cursor (and its pooled connection) stays open
// across a Secrets Manager read.
func TestValuesHoldsNoPooledConnectionWhileReadingSecrets(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || req.GrantID == nil {
		t.Fatalf("Create = %+v, %v, want an automatic grant", req, err)
	}
	reader := gatedReader{Fake: m.Secrets.(secrets.Fake), entered: make(chan struct{}, 1), gate: make(chan struct{})}
	m.Secrets = reader
	done := make(chan error, 1)
	go func() {
		_, _, _, err := m.Values(ctx, *req.GrantID, enr)
		done <- err
	}()
	awaitEntered(t, reader.entered, "the secret read")
	acquired := m.Store.Pool.Stat().AcquiredConns()
	close(reader.gate)
	if err := <-done; err != nil {
		t.Fatalf("Values: %v", err)
	}
	if acquired != 0 {
		t.Fatalf("%d pooled connections checked out during the secret read, want 0", acquired)
	}
}

// gatedReader is a secrets.Reader that announces each Read on entered and answers from its Fake
// only once gate is closed.
type gatedReader struct {
	secrets.Fake
	entered chan struct{}
	gate    chan struct{}
}

func (g gatedReader) Read(ctx context.Context, source string) (string, error) {
	g.entered <- struct{}{}
	<-g.gate
	return g.Fake.Read(ctx, source)
}

// awaitEntered waits for one announcement on entered, failing t after five seconds.
func awaitEntered(t *testing.T, entered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never started", what)
	}
}

// approverKeyEntry reads back login's already-reconciled key from Postgres as a KeyEntry, for a
// test that needs to embed the same key's approvers: section into a different rules file than the
// fixture's own — reconciling the same credential id again is a no-op once its registration
// matches byte-for-byte, so this never needs a second CA-signed registration.
func approverKeyEntry(t *testing.T, m *Machine, login string) approvers.KeyEntry {
	t.Helper()
	var entry approvers.KeyEntry
	if err := m.Store.Pool.QueryRow(context.Background(), `select credential_id, challenge_nonce, registration from approver_keys where login=$1 limit 1`, login).
		Scan(&entry.CredentialID, &entry.ChallengeNonce, &entry.Registration); err != nil {
		t.Fatalf("read %s's persisted key: %v", login, err)
	}
	entry.Seed = true
	return entry
}

// TestRevokeByApproverIsLimitedToTheApproverOrOperator pins that a human may end only a grant they
// approved or one whose enrollment they operate: any other key is refused and the grant stays
// live. Needs a second login (mallory) under the same trust root, so it builds its own
// environment from the shared low-level helpers rather than newFixture.
func TestRevokeByApproverIsLimitedToTheApproverOrOperator(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	ca := webauthntest.NewCA(t)
	sjawharAuth, sjawharEntry := newApproverKey(t, ca, "sjawhar")
	malloryAuth, malloryEntry := newApproverKey(t, ca, "mallory")
	for login, entry := range map[string]approvers.KeyEntry{"sjawhar": sjawharEntry, "mallory": malloryEntry} {
		if _, err := st.Pool.Exec(ctx, `insert into approver_key_seeds (login, credential_id) values ($1,$2)`, login, entry.CredentialID); err != nil {
			t.Fatalf("insert approver_key_seeds(%s): %v", login, err)
		}
	}
	approversSvc := &approvers.Service{Store: st, Verifier: &approvers.Verifier{
		Roots: ca.Pool(), Origin: testOrigin, AAGUIDs: map[uuid.UUID]bool{uuid.MustParse(testAAGUID): true},
	}}
	if err := approversSvc.Reconcile(ctx, map[string][]approvers.KeyEntry{"sjawhar": {sjawharEntry}, "mallory": {malloryEntry}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	cur := newRulesCurrent(t, baseRulesYAML+approversYAML("sjawhar", sjawharEntry))
	enr, key := newEnrollment(t, st, "box", "box-a-"+t.Name(), str("sjawhar"), nil)
	m := &Machine{
		Store: st, Rules: cur,
		Secrets:    secrets.Fake{"dev1/agent-secrets/DEEL_API_KEY": "deel-v1", "dev1/agent-secrets/AUTO_TOKEN": "auto-v1"},
		Approvers:  approversSvc,
		MaxGrant:   time.Hour,
		PendingTTL: 12 * time.Hour,
		Audience:   testAudience,
		Skew:       time.Minute,
		Replay:     replayer(st),
	}

	// The operator's own key may revoke an automatic grant it never personally approved; mallory,
	// neither its approver nor its operator, may not.
	automatic, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || automatic.GrantID == nil {
		t.Fatalf("Create(automatic) = %+v, %v", automatic, err)
	}
	revokeAssertion := func(auth *webauthntest.Authenticator, grantID string) json.RawMessage {
		return auth.Assert(t, testRPID, testOrigin, ch(record.RevokeChallenge(grantID)))
	}
	if err := m.RevokeByApprover(ctx, *automatic.GrantID, revokeAssertion(malloryAuth, *automatic.GrantID)); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("RevokeByApprover(mallory, not the operator) = %v, want ErrNotApprover", err)
	}
	if _, _, _, err := m.Values(ctx, *automatic.GrantID, enr); err != nil {
		t.Fatalf("Values after a refused revoke = %v, want the grant still live", err)
	}
	if err := m.RevokeByApprover(ctx, *automatic.GrantID, revokeAssertion(sjawharAuth, *automatic.GrantID)); err != nil {
		t.Fatalf("RevokeByApprover(sjawhar, the operator) = %v", err)
	}

	// A pending, then approved, request: only its approver (or the enrollment's operator, tested
	// above) may revoke it.
	pending, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(pending) = %+v, %v", pending, err)
	}
	approveAssertion := sjawharAuth.Assert(t, testRPID, testOrigin, ch(record.ApproveChallenge(*pending.RecordID)))
	dec, err := m.ApplyDecision(ctx, *pending.RecordID, true, approveAssertion)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision: %+v %v", dec, err)
	}
	if err := m.RevokeByApprover(ctx, dec.GrantID, revokeAssertion(malloryAuth, dec.GrantID)); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("RevokeByApprover(a key that is neither approver nor operator) = %v, want ErrNotApprover", err)
	}
	if err := m.RevokeByApprover(ctx, dec.GrantID, revokeAssertion(sjawharAuth, dec.GrantID)); err != nil {
		t.Fatalf("RevokeByApprover(the approver) = %v", err)
	}
	var actor string
	if err := m.Store.Pool.QueryRow(ctx, `select actor from audit where kind='grant.revoked' and grant_id=$1`, dec.GrantID).Scan(&actor); err != nil || actor != "human:sjawhar" {
		t.Fatalf("grant.revoked actor = %q, %v, want human:sjawhar", actor, err)
	}
}

func TestCoalescingComparesWholeNames(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	rule := `
    source: dev1/agent-secrets/%s
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
`
	entry := approverKeyEntry(t, m, "sjawhar")
	withRules(t, m, "version: 1\nsecrets:\n  PAIR_A:"+fmt.Sprintf(rule, "a")+"  PAIR_B:"+fmt.Sprintf(rule, "b")+"  \"PAIR_A,PAIR_B\":"+fmt.Sprintf(rule, "ab")+approversYAML("sjawhar", entry))

	pair, err := m.Create(ctx, enr, signRequest(t, m, key, "need both", "PAIR_A", "PAIR_B"), "")
	if err != nil || pair.State != "pending" {
		t.Fatalf("Create(pair) = %+v, %v, want pending", pair, err)
	}
	joined, err := m.Create(ctx, enr, signRequest(t, m, key, "need the joined one", "PAIR_A,PAIR_B"), "")
	if err != nil || joined.State != "pending" {
		t.Fatalf("Create(joined) = %+v, %v, want pending", joined, err)
	}
	if joined.Coalesced || joined.ID == pair.ID {
		t.Fatalf("joined = %+v, want its own request, never the pair's", joined)
	}
}

func TestAuditSurvivesControlCharactersInSecretNames(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	name := "BELL\aNAME\vTAB"
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "odd name", name), "")
	if err != nil || req.State != "denied" {
		t.Fatalf("Create = %+v, %v, want denied", req, err)
	}
	var recorded []string
	if err := m.Store.Pool.QueryRow(ctx, `select array(select jsonb_array_elements_text(detail->'secrets')) from audit where kind='request.created' and request_id=$1`, req.ID).Scan(&recorded); err != nil {
		t.Fatalf("read request.created audit row: %v", err)
	}
	if len(recorded) != 1 || recorded[0] != name {
		t.Fatalf("audited secrets = %q, want [%q]", recorded, name)
	}
}

// TestApprovalAfterTheEnrollmentLapsedDenies pins that an approval landing after the requesting
// session's lease lapsed denies the request as withdrawn instead of granting to a dead session.
func TestApprovalAfterTheEnrollmentLapsedDenies(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create: %+v %v", req, err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set lease_expires_at = now() - interval '1 minute' where id=$1`, enr); err != nil {
		t.Fatalf("lapse lease: %v", err)
	}
	assertion := approver.Assert(t, testRPID, testOrigin, ch(record.ApproveChallenge(*req.RecordID)))
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, assertion)
	if err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	if dec.State != "denied" || dec.GrantID != "" {
		t.Fatalf("dec = %+v, want denied with no grant because the enrollment is no longer live", dec)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil || got.State != "denied" || got.Detail == nil || *got.Detail != "the requesting enrollment is no longer live" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}

const tightenedRules = `version: 1
secrets:
  AUTO_TOKEN:
    source: dev1/agent-secrets/AUTO_TOKEN
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters: %s
`

// TestTightenedRulesStopLiveGrants pins that a live grant is re-checked against rules that changed
// after it was decided: once the rules deny the requester, or want an approval the automatic
// grant never had, values are refused and the grant is no longer handed back by reuse.
func TestTightenedRulesStopLiveGrants(t *testing.T) {
	for name, requesters := range map[string]string{
		"denied":            "[]",
		"approval-now":      "[{kind: box, operator: sjawhar, decision: approval, approver: operator}]",
		"other-operator":    "[{kind: box, operator: mallory, decision: automatic}]",
		"other-kind":        "[{kind: host, operator: sjawhar, decision: automatic}]",
		"explicitly-denied": "[{kind: box, operator: sjawhar, decision: deny}]",
	} {
		t.Run(name, func(t *testing.T) {
			m, enr, key, _ := newFixture(t)
			ctx := context.Background()
			granted, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
			if err != nil || granted.GrantID == nil {
				t.Fatalf("Create = %+v, %v, want an automatic grant", granted, err)
			}
			withRules(t, m, fmt.Sprintf(tightenedRules, requesters)+approversYAML("sjawhar", approverKeyEntry(t, m, "sjawhar")))
			if _, _, _, err := m.Values(ctx, *granted.GrantID, enr); !errors.Is(err, ErrGrantNotLive) {
				t.Fatalf("Values under tightened rules = %v, want ErrGrantNotLive", err)
			}
			again, err := m.Create(ctx, enr, signRequest(t, m, key, "need it again", "AUTO_TOKEN"), "")
			if err != nil {
				t.Fatalf("Create(again): %v", err)
			}
			if again.ID == granted.ID {
				t.Fatalf("Create(again) reused the grant the tightened rules no longer allow")
			}
		})
	}
}

// TestUnchangedPermissionSurvivesARulesChange pins the other side: a rules change that still
// allows the grant (here, only the lifetime changes) keeps it usable and reusable.
func TestUnchangedPermissionSurvivesARulesChange(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	granted, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Create = %+v, %v", granted, err)
	}
	withRules(t, m, strings.Replace(fmt.Sprintf(tightenedRules, "[{kind: box, operator: sjawhar, decision: automatic}]"), "43200", "600", 1))
	if _, _, _, err := m.Values(ctx, *granted.GrantID, enr); err != nil {
		t.Fatalf("Values = %v, want the grant still usable", err)
	}
	if again, err := m.Create(ctx, enr, signRequest(t, m, key, "again", "AUTO_TOKEN"), ""); err != nil || again.ID != granted.ID {
		t.Fatalf("Create(again) = %+v, %v, want the live grant reused", again, err)
	}
}

// TestProxyGrantNeverWidensToInject pins that the delivery frozen at grant time is a ceiling: a
// name granted for proxy delivery stays proxy-only even after the rules switch it to inject.
func TestProxyGrantNeverWidensToInject(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	proxied := `version: 1
secrets:
  AUTO_TOKEN:
    source: dev1/agent-secrets/AUTO_TOKEN
    owner: sjawhar
    delivery: %s
    max_lifetime_seconds: 43200
    requesters: [{kind: box, operator: sjawhar, decision: automatic}]
%s`
	withRules(t, m, fmt.Sprintf(proxied, "proxy", "    proxy: {scheme: https, host: example.com, port: 443, path_prefix: /, methods: [GET], header: Authorization, header_format: 'Bearer {value}'}\n"))
	granted, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Create = %+v, %v", granted, err)
	}
	withRules(t, m, fmt.Sprintf(proxied, "inject", ""))
	values, proxyOnly, _, err := m.Values(ctx, *granted.GrantID, enr)
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if len(values) != 0 || len(proxyOnly) != 1 || proxyOnly[0] != "AUTO_TOKEN" {
		t.Fatalf("Values released %d values, proxy_only %v; want none released and AUTO_TOKEN proxy-only", len(values), proxyOnly)
	}
}

// TestValuesNamesASecretMissingFromTheStore pins that a rule whose source the secrets store does
// not hold is refused as ErrSecretNotInStore naming the secret, not an opaque failure.
func TestValuesNamesASecretMissingFromTheStore(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	withRules(t, m, `version: 1
secrets:
  GHOST_KEY:
    source: dev1/agent-secrets/GHOST_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 3600
    requesters: [{kind: box, operator: sjawhar, decision: automatic}]
`)
	granted, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "GHOST_KEY"), "")
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Create = %+v, %v", granted, err)
	}
	if _, _, _, err := m.Values(ctx, *granted.GrantID, enr); !errors.Is(err, ErrSecretNotInStore) || !strings.Contains(err.Error(), "GHOST_KEY") {
		t.Fatalf("Values = %v, want ErrSecretNotInStore naming GHOST_KEY", err)
	}
}

// TestRevokingARevokedGrantWritesNoSecondAuditRow pins that a repeated revoke succeeds without
// recording a revocation that never happened: one grant.revoked row, naming the first revoker.
func TestRevokingARevokedGrantWritesNoSecondAuditRow(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || req.GrantID == nil {
		t.Fatalf("Create = %+v, %v", req, err)
	}
	if err := m.RevokeGrant(ctx, *req.GrantID, enr); err != nil {
		t.Fatalf("RevokeGrant (first): %v", err)
	}
	if err := m.RevokeGrant(ctx, *req.GrantID, enr); err != nil {
		t.Fatalf("RevokeGrant (again) = %v, want success", err)
	}
	var rows int
	var actor, revokedBy string
	if err := m.Store.Pool.QueryRow(ctx, `select count(*), min(a.actor), min(g.revoked_by) from audit a join grants g on g.id=a.grant_id where a.kind='grant.revoked' and a.grant_id=$1`, *req.GrantID).Scan(&rows, &actor, &revokedBy); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	want := "session:" + enr
	if rows != 1 || actor != want || revokedBy != want {
		t.Fatalf("grant.revoked rows=%d actor=%s revoked_by=%s, want one row naming %s", rows, actor, revokedBy, want)
	}
}

// TestPodRulesMatchTheVerifiedServiceAccount pins that a pod entry naming a service account
// matches only pods whose enrollment carries that verified service-account subject.
func TestPodRulesMatchTheVerifiedServiceAccount(t *testing.T) {
	m, _, _, _ := newFixture(t)
	ctx := context.Background()
	withRules(t, m, `version: 1
secrets:
  WORKER_KEY:
    source: dev1/agent-secrets/AUTO_TOKEN
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 3600
    requesters: [{kind: pod, service_account: 'system:serviceaccount:legion:worker', decision: automatic}]
`)
	workerID, workerKey := newEnrollment(t, m.Store, "pod", "pod-worker", nil, str("system:serviceaccount:legion:worker"))
	if req, err := m.Create(ctx, workerID, signRequest(t, m, workerKey, "need it", "WORKER_KEY"), ""); err != nil || req.State != "granted" {
		t.Fatalf("Create(pod running as legion:worker) = %+v, %v, want granted", req, err)
	}
	strangerID, strangerKey := newEnrollment(t, m.Store, "pod", "pod-stranger", nil, str("system:serviceaccount:default:stranger"))
	if req, err := m.Create(ctx, strangerID, signRequest(t, m, strangerKey, "need it", "WORKER_KEY"), ""); err != nil || req.State != "denied" {
		t.Fatalf("Create(pod running as default:stranger) = %+v, %v, want denied", req, err)
	}
}
