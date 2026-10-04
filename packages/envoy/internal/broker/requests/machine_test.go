package requests

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

const testAudience = "broker"

const (
	// fixtureOperator is the person whose machine the fixture's session runs on.
	fixtureOperator = "sami@example.com"
	// otherPerson owns ALICE_KEY.
	otherPerson = "alice@example.com"
	// fixtureService is the one registered service, which owns SERVICE_KEY.
	fixtureService = "legion"
)

// fixtureSecrets are the fixture's namespace secrets: DEEL_API_KEY, the operator's and human-tier,
// needs the operator's approval whoever asks; ALICE_KEY, another person's agent-tier secret, needs
// alice's approval for the operator's session; AUTO_TOKEN, the operator's agent-tier secret, is
// automatic for the operator's session; SHARED_KEY, shared and human-tier, needs anyone's approval;
// SERVICE_KEY, the registered service's, is denied to every session, since none is the service.
func fixtureSecrets() []secrets.LocalSecret {
	return []secrets.LocalSecret{
		policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1"),
		policytest.Secret("ALICE_KEY", otherPerson, policy.TierAgent, "alice-v1"),
		policytest.Secret("AUTO_TOKEN", fixtureOperator, policy.TierAgent, "auto-v1"),
		policytest.Secret("SHARED_KEY", policy.OwnerShared, policy.TierHuman, "shared-v1"),
		policytest.Secret("SERVICE_KEY", fixtureService, policy.TierAgent, "service-v1"),
	}
}

// fixtureStore is the secrets.Local m's policy and values come from.
func fixtureStore(m *Machine) *secrets.Local {
	return m.Secrets.(secrets.AWS).Client.(*secrets.Local)
}

// retag puts changed into the fixture's store, replacing each secret of the same name, and reloads
// the policy, as the broker's next refresh does after someone edits a secret in Secrets Manager.
func retag(t *testing.T, m *Machine, changed ...secrets.LocalSecret) {
	t.Helper()
	for _, s := range changed {
		fixtureStore(m).Put(s)
	}
	if err := m.Policy.Refresh(context.Background()); err != nil {
		t.Fatalf("policy refresh: %v", err)
	}
}

// replayer builds a Machine.Replay backed directly by the proof_jtis table, mirroring
// enroll.Service.Replay (which this fixture cannot use — see newEnrollment).
func replayer(st *store.Store) func(context.Context, string, time.Time) (bool, error) {
	return func(ctx context.Context, jti string, expires time.Time) (bool, error) {
		_, err := st.Pool.Exec(ctx, `insert into proof_jtis (jti, expires_at) values ($1,$2)`, jti, expires)
		if store.IsUniqueViolation(err) {
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

// newFixture opens a store on a fresh schema, loads the policy of fixtureSecrets with
// fixtureService registered, and enrolls one live box session the operator runs. Returns the
// Machine, that enrollment's id, its own signing key (for record.Sign), and the login
// DEEL_API_KEY's records name as approver (its owner, the operator).
func newFixture(t *testing.T) (m *Machine, enrollmentID string, requesterKey *ecdsa.PrivateKey, approver string) {
	t.Helper()
	st := storetest.Open(t)
	local := secrets.NewLocal(fixtureSecrets()...)
	enrollmentID, requesterKey = newEnrollment(t, st, "box", "box-a-"+t.Name(), new(fixtureOperator), nil)

	m = &Machine{
		Store:      st,
		Policy:     policytest.Current(t, local, fixtureService),
		Secrets:    secrets.AWS{Client: local},
		MaxGrant:   time.Hour,
		PendingTTL: 12 * time.Hour,
		Audience:   testAudience,
		Skew:       time.Minute,
		Replay:     replayer(st),
	}
	m.Chain = NewChainVerifier(st, testAudience, time.Minute)
	return m, enrollmentID, requesterKey, fixtureOperator
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
	if err != nil || parsed.Approver != fixtureOperator || parsed.ID() != *req.RecordID {
		t.Fatalf("record body %q: %+v %v", body, parsed, err)
	}
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.GrantID == "" {
		t.Fatal(err)
	}
	// second decision changes nothing
	if _, err := m.ApplyDecision(ctx, *req.RecordID, false, approver); !errors.Is(err, ErrTerminal) {
		t.Fatalf("late deny: %v", err)
	}
}

// TestOnlyTheRecordsApproverDecides pins the decision rule: any login but the record's approver
// is refused on both approve and deny with record.ErrNotApprover and leaves the request pending,
// the approver decides whatever casing Dispatch sends, and the approved event records the
// canonical deciding login the chain later re-checks.
func TestOnlyTheRecordsApproverDecides(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "why", "DEEL_API_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("%+v %v", req, err)
	}
	for _, approve := range []bool{true, false} {
		if _, err := m.ApplyDecision(ctx, *req.RecordID, approve, "mallory"); !errors.Is(err, record.ErrNotApprover) {
			t.Fatalf("ApplyDecision(approve=%v, mallory) = %v, want record.ErrNotApprover", approve, err)
		}
	}
	if got, err := m.Get(ctx, req.ID); err != nil || got.State != "pending" {
		t.Fatalf("Get after refused decisions = %+v, %v, want still pending", got, err)
	}

	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, " Sami@Example.com ")
	if err != nil || dec.State != "granted" {
		t.Fatalf("ApplyDecision(the approver in another casing) = %+v, %v, want granted", dec, err)
	}
	var login string
	if err := m.Store.Pool.QueryRow(ctx, `select login from credential_request_events where record_id=$1 and event='approved'`, *req.RecordID).Scan(&login); err != nil {
		t.Fatalf("read approved event: %v", err)
	}
	if login != approver {
		t.Fatalf("approved event login = %q, want %q", login, approver)
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
	if _, err := m.Store.Pool.Exec(ctx, `insert into grants (id, request_id, enrollment_id, approver, expires_at) values ($1,$2,$3,'sami@example.com', now() + interval '1 hour')`,
		grantID, req.ID, enr); err != nil {
		t.Fatalf("insert forged grant: %v", err)
	}
	if _, _, err := m.Values(ctx, grantID, enr); !errors.Is(err, ErrGrantChainInvalid) {
		t.Fatalf("Values(forged grant) = %v, want ErrGrantChainInvalid", err)
	}
}

// TestValuesRefusesAChainTheBrokerDidNotWrite pins what "every release re-verifies the whole
// chain" means now that no approval signature exists: a grant releases only while its record is
// an agent_secret record, reproduces its own content-addressed id, embeds a request object its
// requester really signed, and carries exactly one terminal decision, an approval by the record's
// approver. Each subtest writes the kind of row only a writer other than the broker could — past
// the append-only trigger and the one-decision unique index where it must — and the grant that
// released before the write releases nothing after it. Nor does reuse hand it back: a new request
// for the same name opens a fresh pending request rather than returning the grant whose chain no
// longer verifies.
func TestValuesRefusesAChainTheBrokerDidNotWrite(t *testing.T) {
	for name, tamper := range map[string]func(t testing.TB, st *store.Store, recordID string){
		"a tampered body": storetest.Exec(
			`alter table credential_requests disable trigger credential_requests_no_update`,
			`update credential_requests set body = replace(body, 'lifetime_seconds: 3600', 'lifetime_seconds: 43200')
				where id=$1 and body like '%lifetime_seconds: 3600%'`,
		),
		"an approval by another login": storetest.Exec(
			`update credential_request_events set login='mallory' where record_id=$1 and event='approved'`,
		),
		"a second terminal event": storetest.Exec(
			`drop index credential_request_decision`,
			`insert into credential_request_events (record_id, event, login, actor) values ($1, 'denied', 'sami@example.com', 'human:sami@example.com')`,
		),
		"a request object whose signature was altered": func(t testing.TB, st *store.Store, recordID string) {
			forged := storetest.ForgeRequestSignature(t, st, recordID)
			tag, err := st.Pool.Exec(context.Background(), `update requests set record_id=$2 where record_id=$1`, recordID, forged)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("point the request at the forged record: %d rows, %v", tag.RowsAffected(), err)
			}
		},
		"a record of the other kind": func(t testing.TB, st *store.Store, recordID string) {
			copyID := storetest.CopyAsOtherKind(t, st, recordID)
			tag, err := st.Pool.Exec(context.Background(), `update requests set record_id=$2 where record_id=$1`, recordID, copyID)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("point the request at the other kind's record: %d rows, %v", tag.RowsAffected(), err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, enr, key, approver := newFixture(t)
			ctx := context.Background()
			req, err := m.Create(ctx, enr, signRequest(t, m, key, "why", "DEEL_API_KEY"), "")
			if err != nil || req.RecordID == nil {
				t.Fatalf("%+v %v", req, err)
			}
			dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
			if err != nil || dec.GrantID == "" {
				t.Fatalf("ApplyDecision: %+v %v", dec, err)
			}
			if _, _, err := m.Values(ctx, dec.GrantID, enr); err != nil {
				t.Fatalf("Values before the write: %v", err)
			}

			tamper(t, m.Store, *req.RecordID)

			if _, _, err := m.Values(ctx, dec.GrantID, enr); !errors.Is(err, ErrGrantChainInvalid) {
				t.Fatalf("Values after %s = %v, want ErrGrantChainInvalid", name, err)
			}
			again, err := m.Create(ctx, enr, signRequest(t, m, key, "why, again", "DEEL_API_KEY"), "")
			if err != nil {
				t.Fatalf("Create after %s: %v", name, err)
			}
			if again.ID == req.ID || again.State != "pending" || again.GrantID != nil {
				t.Fatalf("Create after %s = %+v, want a fresh pending request, not request %s's grant %s", name, again, req.ID, dec.GrantID)
			}
		})
	}
}

func TestCreateRefusesLoginHintAndForeignThumbprint(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()

	withHint, err := record.Sign(key, m.Audience, []record.AuthorizationDetail{{Type: "agent_secret", Identifier: "DEEL_API_KEY", Actions: []string{"inject"}}}, "why", fixtureOperator, time.Now())
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
	denied, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "SERVICE_KEY"), "")
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
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY", "SERVICE_KEY"), "")
	if err != nil || req.State != "denied" || req.RecordID != nil {
		t.Fatalf("Create = %+v, %v, want denied with no record (any denied name refuses the whole request)", req, err)
	}
}

// TestCreateRefusesAnUnknownSecretNameWithNoRecordWritten pins the shared broker contract's
// "identifier must name a rule's secret (else 400 UNKNOWN_SECRET at record time)"
// (dispatch://AGENTC-393/artifact/plan-overview-md): a request naming a secret no
// policy serves at all aborts the whole Create call with policy.ErrUnknownSecret rather than
// folding silently into an ordinary "deny" decision, and writes no request row at all.
func TestCreateRefusesAnUnknownSecretNameWithNoRecordWritten(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	_, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "NOT_A_REAL_SECRET"), "")
	if !errors.Is(err, policy.ErrUnknownSecret) {
		t.Fatalf("Create(unknown secret) error = %v, want policy.ErrUnknownSecret", err)
	}
	var count int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from requests where enrollment_id=$1`, enr).Scan(&count); err != nil || count != 0 {
		t.Fatalf("requests rows for enrollment = %d, %v, want 0 (an unknown secret name aborts the whole Create call)", count, err)
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

// TestApplyDecisionRefusesARecordPastItsExpiry pins that a record past its expiry is decided no
// more, before the sweeper has expired it as well as after: approve and deny both answer
// ErrExpired, whose message says the record expired rather than that it was decided, and neither
// writes a decision or a grant — including after the sweep has actually run, which writes exactly
// the one 'expired' event and nothing a later decision adds to.
func TestApplyDecisionRefusesARecordPastItsExpiry(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	m.PendingTTL = -time.Minute
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v", req, err)
	}
	for _, approve := range []bool{true, false} {
		dec, err := m.ApplyDecision(ctx, *req.RecordID, approve, approver)
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("ApplyDecision(approve=%v) before the sweep = %+v, %v, want ErrExpired", approve, dec, err)
		}
	}
	if _, err := m.ExpirePending(ctx, time.Now()); err != nil {
		t.Fatalf("ExpirePending: %v", err)
	}
	for _, approve := range []bool{true, false} {
		dec, err := m.ApplyDecision(ctx, *req.RecordID, approve, approver)
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("ApplyDecision(approve=%v) after the sweep = %+v, %v, want ErrExpired", approve, dec, err)
		}
	}
	var events, grants int
	if err := m.Store.Pool.QueryRow(ctx, `select (select count(*) from credential_request_events where record_id=$1),
		(select count(*) from grants where request_id=$2)`, *req.RecordID, req.ID).Scan(&events, &grants); err != nil || events != 1 || grants != 0 {
		t.Fatalf("after the refused decisions: %d events, %d grants, %v; want 1 event (the sweep's own), 0 grants", events, grants, err)
	}
}

func TestExpirePendingAuditsEveryExpiredRequest(t *testing.T) {
	m, enrA, keyA, _ := newFixture(t)
	ctx := context.Background()
	enrB, keyB := newEnrollment(t, m.Store, "box", "box-b-"+t.Name(), new(fixtureOperator), nil)

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
	enrB, _ := newEnrollment(t, m.Store, "box", "box-b-"+t.Name(), new(fixtureOperator), nil)

	granted, err := m.Create(ctx, enrA, signRequest(t, m, keyA, "need it", "AUTO_TOKEN"), "")
	if err != nil {
		t.Fatalf("Create(automatic): %v", err)
	}
	pending, err := m.Create(ctx, enrA, signRequest(t, m, keyA, "need it", "DEEL_API_KEY"), "")
	if err != nil {
		t.Fatalf("Create(pending): %v", err)
	}
	if _, _, err := m.Values(ctx, *granted.GrantID, enrB); !errors.Is(err, ErrNotYours) {
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
	if _, _, err := m.Values(ctx, *req.GrantID, enr); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values(after revoke) = %v, want ErrGrantNotLive", err)
	}
}

func TestMachineSessionID(t *testing.T) {
	m, enrA, keyA, _ := newFixture(t)
	ctx := context.Background()
	enrB, keyB := newEnrollment(t, m.Store, "box", "box-b-"+t.Name(), new(fixtureOperator), nil)

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
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
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
	if _, err := m.ApplyDecision(ctx, *first.RecordID, true, approver); err != nil {
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
	reader := gatedReader{Reader: m.Secrets, entered: make(chan struct{}, 1), gate: make(chan struct{})}
	m.Secrets = reader
	done := make(chan error, 1)
	go func() {
		_, _, err := m.Values(ctx, *req.GrantID, enr)
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

// gatedReader is a secrets.Reader that announces each Read on entered and answers from the
// Reader it wraps only once gate is closed.
type gatedReader struct {
	secrets.Reader
	entered chan struct{}
	gate    chan struct{}
}

func (g gatedReader) Read(ctx context.Context, source string) (string, error) {
	g.entered <- struct{}{}
	<-g.gate
	return g.Reader.Read(ctx, source)
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

// TestRevokeByApproverIsLimitedToTheApproverOrOperator pins that a human may end only a grant they
// approved or one whose enrollment they operate: any other login is refused and the grant stays
// live. ALICE_KEY's approver (alice, its owner) is not its enrollment's operator, so each half of
// the rule is exercised on its own.
func TestRevokeByApproverIsLimitedToTheApproverOrOperator(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()

	// The operator may revoke an automatic grant nobody approved; mallory, neither its approver
	// nor its operator, may not.
	automatic, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || automatic.GrantID == nil {
		t.Fatalf("Create(automatic) = %+v, %v", automatic, err)
	}
	if err := m.RevokeByApprover(ctx, *automatic.GrantID, "mallory"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("RevokeByApprover(mallory, not the operator) = %v, want ErrNotApprover", err)
	}
	if _, _, err := m.Values(ctx, *automatic.GrantID, enr); err != nil {
		t.Fatalf("Values after a refused revoke = %v, want the grant still live", err)
	}
	if err := m.RevokeByApprover(ctx, *automatic.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover(the operator) = %v", err)
	}

	// A grant alice approved: alice, its approver though not its operator, may revoke it, and
	// mallory still may not.
	pending, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "ALICE_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(pending) = %+v, %v", pending, err)
	}
	dec, err := m.ApplyDecision(ctx, *pending.RecordID, true, otherPerson)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(alice): %+v %v", dec, err)
	}
	if err := m.RevokeByApprover(ctx, dec.GrantID, "mallory"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("RevokeByApprover(a login that is neither approver nor operator) = %v, want ErrNotApprover", err)
	}
	if _, _, err := m.Values(ctx, dec.GrantID, enr); err != nil {
		t.Fatalf("Values after a refused revoke = %v, want the grant still live", err)
	}
	if err := m.RevokeByApprover(ctx, dec.GrantID, "Alice@Example.com"); err != nil {
		t.Fatalf("RevokeByApprover(the approver) = %v", err)
	}
	var actor string
	if err := m.Store.Pool.QueryRow(ctx, `select actor from audit where kind='grant.revoked' and grant_id=$1`, dec.GrantID).Scan(&actor); err != nil || actor != "human:"+otherPerson {
		t.Fatalf("grant.revoked actor = %q, %v, want human:%s", actor, err, otherPerson)
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
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
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

// TestTightenedPolicyStopsLiveGrants pins that a live automatic grant is re-checked against tags
// edited after it was decided: once the policy denies the requester, wants an approval the
// automatic grant never had, or no longer serves the secret at all, values are refused and the
// grant is no longer handed back by reuse.
func TestTightenedPolicyStopsLiveGrants(t *testing.T) {
	autoToken := func(owner, tier string) secrets.LocalSecret {
		return policytest.Secret("AUTO_TOKEN", owner, tier, "auto-v1")
	}
	for name, tighten := range map[string]func(t *testing.T, m *Machine){
		"tier raised to human": func(t *testing.T, m *Machine) {
			retag(t, m, autoToken(fixtureOperator, policy.TierHuman))
		},
		"owner changed to another person": func(t *testing.T, m *Machine) {
			retag(t, m, autoToken(otherPerson, policy.TierAgent))
		},
		"owner changed to a service": func(t *testing.T, m *Machine) {
			retag(t, m, autoToken(fixtureService, policy.TierAgent))
		},
		"owner tag removed": func(t *testing.T, m *Machine) {
			untagged := autoToken(fixtureOperator, policy.TierAgent)
			delete(untagged.Tags, policy.TagOwner)
			retag(t, m, untagged)
		},
		"moved to another key": func(t *testing.T, m *Machine) {
			moved := autoToken(fixtureOperator, policy.TierAgent)
			moved.KmsKeyID = "arn:aws:kms:us-east-1:111122223333:key/another-key"
			retag(t, m, moved)
		},
		"deleted": func(t *testing.T, m *Machine) {
			fixtureStore(m).Delete(policytest.ID("AUTO_TOKEN"))
			retag(t, m)
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, enr, key, _ := newFixture(t)
			ctx := context.Background()
			granted, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
			if err != nil || granted.GrantID == nil {
				t.Fatalf("Create = %+v, %v, want an automatic grant", granted, err)
			}
			tighten(t, m)
			if _, _, err := m.Values(ctx, *granted.GrantID, enr); !errors.Is(err, ErrGrantNotLive) {
				t.Fatalf("Values under the tightened policy = %v, want ErrGrantNotLive", err)
			}
			again, err := m.Create(ctx, enr, signRequest(t, m, key, "need it again", "AUTO_TOKEN"), "")
			if err == nil && again.ID == granted.ID {
				t.Fatalf("Create(again) reused the grant the tightened policy no longer allows")
			}
		})
	}
}

// TestUnchangedPermissionSurvivesAPolicyChange pins the other side: a tag edit that still lets the
// requester have the secret at once keeps its grant usable and reusable, whether the edit is to
// another secret or to this one.
func TestUnchangedPermissionSurvivesAPolicyChange(t *testing.T) {
	for name, edit := range map[string]secrets.LocalSecret{
		"another secret retiered": policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierAgent, "deel-v1"),
		"this secret now shared":  policytest.Secret("AUTO_TOKEN", policy.OwnerShared, policy.TierAgent, "auto-v1"),
	} {
		t.Run(name, func(t *testing.T) {
			m, enr, key, _ := newFixture(t)
			ctx := context.Background()
			granted, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
			if err != nil || granted.GrantID == nil {
				t.Fatalf("Create = %+v, %v", granted, err)
			}
			before := m.Policy.Get().Version
			retag(t, m, edit)
			if m.Policy.Get().Version == before {
				t.Fatalf("the edit left the policy version %s unchanged, so it re-checks nothing", before)
			}
			if _, _, err := m.Values(ctx, *granted.GrantID, enr); err != nil {
				t.Fatalf("Values = %v, want the grant still usable", err)
			}
			if again, err := m.Create(ctx, enr, signRequest(t, m, key, "again", "AUTO_TOKEN"), ""); err != nil || again.ID != granted.ID {
				t.Fatalf("Create(again) = %+v, %v, want the live grant reused", again, err)
			}
		})
	}
}

// TestValuesNamesASecretDeletedBeforeTheNextRefresh pins that a granted secret deleted from Secrets
// Manager before the policy's next refresh is refused as ErrSecretNotInStore naming the secret, not
// an opaque failure.
func TestValuesNamesASecretDeletedBeforeTheNextRefresh(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	granted, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Create = %+v, %v", granted, err)
	}
	fixtureStore(m).Delete(policytest.ID("AUTO_TOKEN"))
	if _, _, err := m.Values(ctx, *granted.GrantID, enr); !errors.Is(err, ErrSecretNotInStore) || !strings.Contains(err.Error(), "AUTO_TOKEN") {
		t.Fatalf("Values = %v, want ErrSecretNotInStore naming AUTO_TOKEN", err)
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

// TestEveryRequesterIsDecidedByOwnerAndTier drives the policy's whole table through Create: the
// owner's own session, another person's session and a pod each ask for a person's agent-tier and
// human-tier secret, a shared agent-tier and human-tier secret, and a service's secret. A request
// that needs approval writes a record naming the approver and reaches that approver's pending
// list: another person's session or a pod asking for a person's agent-tier secret is an approval
// request to its owner.
func TestEveryRequesterIsDecidedByOwnerAndTier(t *testing.T) {
	const otherOperator = "bob@example.com"
	type outcome struct{ state, approver string }
	granted, denied := outcome{state: "granted"}, outcome{state: "denied"}
	approval := func(approver string) outcome { return outcome{state: "pending", approver: approver} }
	table := map[string]map[string]outcome{
		"owner's session": {
			"AUTO_TOKEN": granted, "DEEL_API_KEY": approval(fixtureOperator),
			"SHARED_TOKEN": granted, "SHARED_KEY": approval(record.AnyoneApprover), "SERVICE_KEY": denied,
		},
		"another person's session": {
			"AUTO_TOKEN": approval(fixtureOperator), "DEEL_API_KEY": approval(fixtureOperator),
			"SHARED_TOKEN": granted, "SHARED_KEY": approval(record.AnyoneApprover), "SERVICE_KEY": denied,
		},
		"pod": {
			"AUTO_TOKEN": approval(fixtureOperator), "DEEL_API_KEY": approval(fixtureOperator),
			"SHARED_TOKEN": granted, "SHARED_KEY": approval(record.AnyoneApprover), "SERVICE_KEY": denied,
		},
	}
	m, ownerEnr, ownerKey, _ := newFixture(t)
	retag(t, m, policytest.Secret("SHARED_TOKEN", policy.OwnerShared, policy.TierAgent, "shared-token-v1"))
	ctx := context.Background()
	otherEnr, otherKey := newEnrollment(t, m.Store, "box", "box-other-"+t.Name(), new(otherOperator), nil)
	podEnr, podKey := newEnrollment(t, m.Store, "pod", "pod-"+t.Name(), nil, new("system:serviceaccount:legion:worker"))
	requesters := map[string]struct {
		enrollment string
		key        *ecdsa.PrivateKey
	}{
		"owner's session":          {ownerEnr, ownerKey},
		"another person's session": {otherEnr, otherKey},
		"pod":                      {podEnr, podKey},
	}
	for who, row := range table {
		for name, want := range row {
			r := requesters[who]
			req, err := m.Create(ctx, r.enrollment, signRequest(t, m, r.key, "need it", name), "")
			if err != nil || req.State != want.state {
				t.Errorf("%s asking for %s = %+v, %v; want %s", who, name, req, err, want.state)
				continue
			}
			if want.state != "pending" {
				continue
			}
			var approver string
			if err := m.Store.Pool.QueryRow(ctx, `select approver from credential_requests where id=$1`, *req.RecordID).Scan(&approver); err != nil || approver != want.approver {
				t.Errorf("%s asking for %s: record approver %q, %v; want %q", who, name, approver, err, want.approver)
			}
			listedFor := want.approver
			if listedFor == record.AnyoneApprover {
				listedFor = "carol@example.com"
			}
			pending, err := m.PendingForApprover(ctx, listedFor)
			if err != nil || !slices.ContainsFunc(pending, func(p PendingSummary) bool { return p.RecordID == *req.RecordID }) {
				t.Errorf("%s asking for %s: %s's pending list %+v, %v; want the record listed", who, name, listedFor, pending, err)
			}
		}
	}
}

// TestAnyoneDecidesASharedHumanTierRequest pins the approver record.AnyoneApprover: a request for a
// shared human-tier secret reaches every person's pending list, any person's login decides it and
// is recorded as the decider, the sentinel itself, in any casing, and a blank login decide nothing,
// approve or deny, and the grant releases its value because its chain verifies under that approver.
func TestAnyoneDecidesASharedHumanTierRequest(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need the shared one", "SHARED_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v, want pending with a record", req, err)
	}
	for _, person := range []string{"bob@example.com", "carol@example.com", fixtureOperator} {
		pending, err := m.PendingForApprover(ctx, person)
		if err != nil || len(pending) != 1 || pending[0].RecordID != *req.RecordID {
			t.Fatalf("PendingForApprover(%s) = %+v, %v, want the shared record", person, pending, err)
		}
	}
	for _, approve := range []bool{true, false} {
		for _, login := range []string{record.AnyoneApprover, " ANYONE ", "", "  ", "\t"} {
			if _, err := m.ApplyDecision(ctx, *req.RecordID, approve, login); !errors.Is(err, record.ErrNotApprover) {
				t.Fatalf("ApplyDecision(approve=%v, %q) = %v, want record.ErrNotApprover", approve, login, err)
			}
		}
	}
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, "Bob@Example.com")
	if err != nil || dec.State != "granted" || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(bob) = %+v, %v, want granted", dec, err)
	}
	var login string
	if err := m.Store.Pool.QueryRow(ctx, `select login from credential_request_events where record_id=$1 and event='approved'`, *req.RecordID).Scan(&login); err != nil || login != "bob@example.com" {
		t.Fatalf("approved event login = %q, %v, want bob@example.com", login, err)
	}
	values, _, err := m.Values(ctx, dec.GrantID, enr)
	if err != nil || values["SHARED_KEY"] != "shared-v1" {
		t.Fatalf("Values = %v, %v, want SHARED_KEY released", values, err)
	}
	grants, err := m.GrantsForApprover(ctx, "bob@example.com")
	if err != nil || len(grants) != 1 || grants[0].GrantID != dec.GrantID || grants[0].Approver != "bob@example.com" {
		t.Fatalf("GrantsForApprover(bob) = %+v, %v, want the grant bob approved", grants, err)
	}
	if pending, err := m.PendingForApprover(ctx, "carol@example.com"); err != nil || len(pending) != 0 {
		t.Fatalf("PendingForApprover(carol) after bob decided = %+v, %v, want nothing", pending, err)
	}
}
