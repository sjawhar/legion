package machine

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

const (
	testAudience = "https://secrets.test"
	testURL      = "https://secrets.test/v1/requests"
	// testApprover is every machine login's login_hint below, and so the record's approver.
	testApprover = "sjawhar"
)

// codePattern is the confirmation code's own shape: eight symbols from confirmationAlphabet
// (a 32-symbol subset of A-Z2-9 with the easily-confused characters removed) as XXXX-XXXX.
var codePattern = regexp.MustCompile(`^[A-Z2-9]{4}-[A-Z2-9]{4}$`)

// newFixture wires a Service against a fresh Postgres schema: a real enroll.Service wired with
// its ChainVerifier (so AuthenticateLauncher's own issuance-chain re-verification is the genuine
// thing, not a stub), and rules.Current loaded from a minimal valid rules file — a machine login's
// own decision never consults the rules (it always requires approval), so the fixture needs only
// a valid, versioned Set.
func newFixture(t *testing.T) *Service {
	t.Helper()
	st := storetest.Open(t)

	enr := &enroll.Service{Store: st, Lease: time.Hour}
	enr.Chain = enroll.NewChainVerifier(st, testAudience, time.Minute)

	rulesPath := t.TempDir() + "/rules.yaml"
	if err := os.WriteFile(rulesPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cur, err := rules.NewCurrent(context.Background(), rules.FileLoader{Path: rulesPath}, time.Hour, func(error) {})
	if err != nil {
		t.Fatalf("rules.NewCurrent: %v", err)
	}

	return &Service{
		Store: st, Enroll: enr, Rules: cur,
		Audience: testAudience, Skew: time.Minute, PendingTTL: 10 * time.Minute, CredentialLifetime: 24 * time.Hour,
		Replay: enr.Replay,
	}
}

// signMachineLogin builds a machine's own credential-request object for a launcher_credential
// login: identifier is the host, service (optional) distinguishes a service credential from a
// personal one exactly as record.AuthorizationDetail's own doc describes.
func signMachineLogin(t *testing.T, loginHint, identifier, service string) (compact string) {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	compact, err = record.Sign(key, testAudience, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: identifier, Service: service},
	}, "", loginHint, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return compact
}

// approvedCredential logs a fresh machine in and approves it as testApprover, returning the
// record id and the minted credential's id.
func approvedCredential(t *testing.T, svc *Service) (recordID, credentialID string) {
	t.Helper()
	ctx := context.Background()
	_, code, err := svc.Login(ctx, signMachineLogin(t, testApprover, "example-host-devbox", ""))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatalf("LookupByCode: %v", err)
	}
	_, credentialID, err = svc.ApplyDecision(ctx, view.RecordID, true, testApprover, code)
	if err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	return view.RecordID, credentialID
}

// TestLoginIssuesAKeyBoundCredentialOnTypedCodeApproval drives the whole machine-login flow end
// to end: a machine signs its own request object, Login opens a pending record and mints a
// typed code, LookupByCode shows it to the approving human, ApplyDecision mints the credential
// once the record's approver decides it, Read reports it issued — with no token anywhere in any
// response — and the resulting credential authenticates a proof.SignLauncher proof by the very
// key the machine signed its login with, never by another.
func TestLoginIssuesAKeyBoundCredentialOnTypedCodeApproval(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()

	machineKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	compact, err := record.Sign(machineKey, testAudience, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: "example-host-devbox"},
	}, "", testApprover, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	pendingID, code, err := svc.Login(ctx, compact)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !codePattern.MatchString(code) {
		t.Fatalf("code = %q, want the shape XXXX-XXXX", code)
	}

	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatalf("LookupByCode: %v", err)
	}
	if view.Host != "example-host-devbox" || view.Approver != testApprover || view.State != "pending" {
		t.Fatalf("LookupByCode = %+v, want host example-host-devbox, approver sjawhar, state pending", view)
	}

	state, credentialID, err := svc.ApplyDecision(ctx, view.RecordID, true, testApprover, code)
	if err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	if state != "issued" || credentialID == "" {
		t.Fatalf("ApplyDecision = state=%q credentialID=%q, want issued and a credential id", state, credentialID)
	}

	readState, readCredentialID, err := svc.Read(ctx, pendingID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if readState != "issued" || readCredentialID != credentialID {
		t.Fatalf("Read = %q %q, want issued %q", readState, readCredentialID, credentialID)
	}

	// The key-bound credential authenticates a proof.SignLauncher proof by the same machine key
	// through AuthenticateLauncher's own issuance-chain re-verification, and refuses one by
	// another key.
	verifier := &proof.Verifier{
		Skew:           time.Minute,
		LookupLauncher: svc.Enroll.AuthenticateLauncher,
		Replay:         func(context.Context, string, time.Time) (bool, error) { return true, nil },
	}
	sameKeyProof, err := proof.SignLauncher(machineKey, credentialID, "POST", testURL, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(ctx, sameKeyProof, "POST", testURL, time.Now()); err != nil {
		t.Fatalf("proof by the enrolled key: %v", err)
	}

	otherKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	otherKeyProof, err := proof.SignLauncher(otherKey, credentialID, "POST", testURL, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(ctx, otherKeyProof, "POST", testURL, time.Now()); !errors.Is(err, proof.ErrInvalid) {
		t.Fatalf("proof by another key must fail, got %v", err)
	}
}

// TestApplyDecisionTakesTheCodeThenOnlyTheApproversLogin pins the machine login's two decision
// checks, in order: a mismatched code is refused before the login is even looked at, and with the
// right code any login but the record's approver (its login_hint) is refused on both approve and
// deny, leaving the login pending for its approver to decide.
func TestApplyDecisionTakesTheCodeThenOnlyTheApproversLogin(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()

	pendingID, code, err := svc.Login(ctx, signMachineLogin(t, testApprover, "example-host-devbox", ""))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatalf("LookupByCode: %v", err)
	}

	if _, _, err := svc.ApplyDecision(ctx, view.RecordID, true, testApprover, "WRONG-CODE"); !errors.Is(err, ErrCodeMismatch) {
		t.Fatalf("ApplyDecision(approver, wrong code) = %v, want ErrCodeMismatch", err)
	}
	for _, approve := range []bool{true, false} {
		if _, _, err := svc.ApplyDecision(ctx, view.RecordID, approve, "mallory", code); !errors.Is(err, record.ErrNotApprover) {
			t.Fatalf("ApplyDecision(approve=%v, mallory, right code) = %v, want record.ErrNotApprover", approve, err)
		}
	}
	if state, _, err := svc.Read(ctx, pendingID); err != nil || state != "pending" {
		t.Fatalf("Read after refused decisions = %q, %v, want pending", state, err)
	}

	if state, _, err := svc.ApplyDecision(ctx, view.RecordID, true, "  SJawhar ", code); err != nil || state != "issued" {
		t.Fatalf("ApplyDecision(approver in another casing) = %q, %v, want issued", state, err)
	}
	var login, actor string
	if err := svc.Store.Pool.QueryRow(ctx, `select login, actor from credential_request_events where record_id=$1 and event='approved'`, view.RecordID).
		Scan(&login, &actor); err != nil {
		t.Fatalf("read approved event: %v", err)
	}
	if login != testApprover || actor != "human:"+testApprover {
		t.Fatalf("approved event login=%q actor=%q, want the canonical login %q", login, actor, testApprover)
	}
}

// TestLoginRefusesAReplayedRequestObject pins that a captured signed machine-login request
// object cannot be resubmitted: Login checks the object's own jti through the same Replay seam
// requests.Machine.Create uses (proof_jtis), so a second submission of the identical request
// object within its freshness window is refused rather than minting a second pending record and
// confirmation code each time — a confirmation-fatigue/notification-spam vector against the
// named operator, since the record's code differs every call even with a byte-identical request.
func TestLoginRefusesAReplayedRequestObject(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()

	compact := signMachineLogin(t, testApprover, "example-host-devbox", "")
	if _, _, err := svc.Login(ctx, compact); err != nil {
		t.Fatalf("first Login: %v", err)
	}
	if _, _, err := svc.Login(ctx, compact); !errors.Is(err, record.ErrRequestInvalid) {
		t.Fatalf("second Login with the same request object = %v, want record.ErrRequestInvalid (replayed jti)", err)
	}
}

// TestServiceCredentialEnrollsOnlyPods pins that a login whose launcher_credential detail names a
// service mints a credential with a nil operator (never the approving human's own login), and
// that this new-flow-minted credential still respects enroll's own authorized() trust boundary:
// a service credential enrols pods only.
func TestServiceCredentialEnrollsOnlyPods(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()

	compact := signMachineLogin(t, testApprover, "cluster", "legion-daemon")
	_, code, err := svc.Login(ctx, compact)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatalf("LookupByCode: %v", err)
	}
	if view.Service != "legion-daemon" {
		t.Fatalf("view.Service = %q, want legion-daemon", view.Service)
	}

	state, credentialID, err := svc.ApplyDecision(ctx, view.RecordID, true, testApprover, code)
	if err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	if state != "issued" {
		t.Fatalf("state = %q, want issued", state)
	}

	var operator *string
	var service string
	if err := svc.Store.Pool.QueryRow(ctx, `select operator, service from launcher_credentials where id=$1`, credentialID).Scan(&operator, &service); err != nil {
		t.Fatalf("read launcher_credentials: %v", err)
	}
	if operator != nil {
		t.Fatalf("operator = %v, want nil for a service credential", *operator)
	}
	if service != "legion-daemon" {
		t.Fatalf("service = %q, want legion-daemon", service)
	}

	cred := enroll.Credential{ID: uuid.MustParse(credentialID), Service: &service, Host: "cluster"}
	if _, err := svc.Enroll.Create(ctx, cred, enroll.Enrollment{Kind: "box", RuntimeID: "box-1", Operator: new(testApprover), Thumbprint: "tp-1"}); !errors.Is(err, enroll.ErrOperatorMismatch) {
		t.Fatalf("Create(service cred, kind box) = %v, want ErrOperatorMismatch", err)
	}
}

// TestAForgedCredentialRowAuthenticatesNothing pins the security property enroll.Service.Chain
// exists for: a launcher_credentials row with no backing credential-request record — however
// live, unrevoked, and unexpired it looks — never authenticates.
func TestAForgedCredentialRowAuthenticatesNothing(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()

	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	jwk, err := (jose.JSONWebKey{Key: &key.PublicKey}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := svc.Store.Pool.Exec(ctx, `insert into launcher_credentials (id, host, key_thumbprint, public_jwk, expires_at) values ($1,'forged-host',$2,$3, now() + interval '1 hour')`,
		id, thumbprint, []byte(jwk)); err != nil {
		t.Fatalf("insert forged launcher_credentials row: %v", err)
	}

	if _, live, err := svc.Enroll.AuthenticateLauncher(ctx, id.String()); err != nil || live {
		t.Fatalf("AuthenticateLauncher(forged row, no record) = live=%v err=%v, want live=false, err=nil", live, err)
	}
}

// TestExpiredCredentialRefuses pins that a launcher credential past its own expires_at no longer
// authenticates, even with a perfectly valid issuance chain behind it.
func TestExpiredCredentialRefuses(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()
	_, credentialID := approvedCredential(t, svc)

	if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() - interval '1 minute' where id=$1`, credentialID); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	if _, live, err := svc.Enroll.AuthenticateLauncher(ctx, credentialID); err != nil || live {
		t.Fatalf("AuthenticateLauncher(expired) = live=%v err=%v, want live=false, err=nil", live, err)
	}
}

// TestExpirePendingMarksOverdueLoginsExpired exercises ExpirePending's own contract: a record
// past its own expires_at with no decision gets one 'expired' event and reads back as such,
// while a decided record is left alone.
func TestExpirePendingMarksOverdueLoginsExpired(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()

	overdueCompact := signMachineLogin(t, testApprover, "overdue-host", "")
	overduePending, _, err := svc.Login(ctx, overdueCompact)
	if err != nil {
		t.Fatalf("Login(overdue): %v", err)
	}

	decidedCompact := signMachineLogin(t, testApprover, "decided-host", "")
	_, decidedCode, err := svc.Login(ctx, decidedCompact)
	if err != nil {
		t.Fatalf("Login(decided): %v", err)
	}
	decidedView, err := svc.LookupByCode(ctx, decidedCode)
	if err != nil {
		t.Fatalf("LookupByCode(decided): %v", err)
	}
	if _, _, err := svc.ApplyDecision(ctx, decidedView.RecordID, false, testApprover, decidedCode); err != nil {
		t.Fatalf("ApplyDecision(deny decided): %v", err)
	}

	if err := svc.ExpirePending(ctx, time.Now().Add(svc.PendingTTL+time.Minute)); err != nil {
		t.Fatalf("ExpirePending: %v", err)
	}

	if state, _, err := svc.Read(ctx, overduePending); err != nil || state != "expired" {
		t.Fatalf("Read(overdue) = %q, %v, want expired", state, err)
	}
	if state, _, err := svc.recordState(ctx, decidedView.RecordID); err != nil || state != "denied" {
		t.Fatalf("recordState(decided) = %q, %v, want denied (ExpirePending must not overwrite a real decision)", state, err)
	}
}

// TestApplyDecisionRefusesALoginPastItsExpiry pins that a machine login past its own expires_at is
// decided no more, before the sweeper has written its 'expired' event as well as after: approve and
// deny both answer ErrLoginExpired, whose message says the login expired rather than that it was
// decided, and neither mints a credential or records a decision — including after the sweep has
// actually run, which writes exactly the one 'expired' event and nothing a later decision adds to.
func TestApplyDecisionRefusesALoginPastItsExpiry(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()
	svc.PendingTTL = -time.Minute
	_, code, err := svc.Login(ctx, signMachineLogin(t, testApprover, "example-host-devbox", ""))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatalf("LookupByCode: %v", err)
	}
	for _, approve := range []bool{true, false} {
		_, _, err := svc.ApplyDecision(ctx, view.RecordID, approve, testApprover, code)
		if !errors.Is(err, ErrLoginExpired) {
			t.Fatalf("ApplyDecision(approve=%v) before the sweep = %v, want ErrLoginExpired", approve, err)
		}
	}
	if err := svc.ExpirePending(ctx, time.Now()); err != nil {
		t.Fatalf("ExpirePending: %v", err)
	}
	for _, approve := range []bool{true, false} {
		_, _, err := svc.ApplyDecision(ctx, view.RecordID, approve, testApprover, code)
		if !errors.Is(err, ErrLoginExpired) {
			t.Fatalf("ApplyDecision(approve=%v) after the sweep = %v, want ErrLoginExpired", approve, err)
		}
	}
	var events, credentials int
	if err := svc.Store.Pool.QueryRow(ctx, `select (select count(*) from credential_request_events where record_id=$1),
		(select count(*) from launcher_credentials where record_id=$1)`, view.RecordID).Scan(&events, &credentials); err != nil || events != 1 || credentials != 0 {
		t.Fatalf("after the refused decisions: %d events, %d credentials, %v; want 1 event (the sweep's own), 0 credentials", events, credentials, err)
	}
}

// TestASweepWhileADecisionHoldsItsRowLockCommits pins ApplyDecision's row-lock level, with no
// seam in the service. A second transaction holds launcher_credentials exclusively, so an
// approving decision takes the record's row lock, passes every check and waits at its mint insert.
// While it waits there, a third connection's own `for no key update nowait` on the same row
// proves the lock is actually held (55P03), not just that its level would be right if it existed.
// The sweeper's 'expired' insert checks its foreign key with `for key share` on that row, which
// `for no key update` leaves free: the sweep commits while the decision holds the lock, and once
// the table is released the decision's own event insert hits the sweeper's terminal event and
// answers ErrLoginExpired — never ErrAlreadyDecided, since this is the sweeper's write, not a
// second human decision — minting nothing. Under `for update` the sweep waits on the decision
// instead; the sweep's own lock_timeout makes that wait this test's failure (55P03), not a hang.
func TestASweepWhileADecisionHoldsItsRowLockCommits(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()
	_, code, err := svc.Login(ctx, signMachineLogin(t, testApprover, "example-host-devbox", ""))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatalf("LookupByCode: %v", err)
	}
	config := svc.Store.Pool.Config()
	config.ConnConfig.RuntimeParams["lock_timeout"] = "5000"
	sweepPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the sweep's pool: %v", err)
	}
	defer sweepPool.Close()
	sweeper := &Service{Store: &store.Store{Pool: sweepPool}}

	holder, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `lock table launcher_credentials in access exclusive mode`); err != nil {
		t.Fatalf("hold launcher_credentials: %v", err)
	}
	decided := make(chan error, 1)
	go func() {
		_, _, err := svc.ApplyDecision(ctx, view.RecordID, true, testApprover, code)
		decided <- err
	}()
	waitForLockWait(t, ctx, svc.Store, "insert into launcher_credentials%", decided)
	_, probeErr := svc.Store.Pool.Exec(ctx, `select 1 from credential_requests where id=$1 for no key update nowait`, view.RecordID)
	var pgErr *pgconn.PgError
	if !(errors.As(probeErr, &pgErr) && pgErr.Code == "55P03") {
		t.Fatalf("probe the record's row lock while the decision holds it = %v, want SQLSTATE 55P03 (lock not available)", probeErr)
	}
	swept := sweeper.ExpirePending(ctx, time.Now().Add(svc.PendingTTL+time.Minute))
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if swept != nil {
		t.Fatalf("ExpirePending while a decision holds the record's row lock = %v, want it to commit", swept)
	}
	if err := <-decided; !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("ApplyDecision once the sweep committed = %v, want ErrLoginExpired", err)
	}
	if state, _, err := svc.recordState(ctx, view.RecordID); err != nil || state != "expired" {
		t.Fatalf("recordState = %q, %v, want expired", state, err)
	}
	var credentials int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from launcher_credentials where record_id=$1`, view.RecordID).Scan(&credentials); err != nil || credentials != 0 {
		t.Fatalf("launcher credentials for the record = %d, %v, want 0", credentials, err)
	}
}

// waitForLockWait returns once a statement matching like waits on a lock, and fails at once with
// the waiting operation's result if it returns first (Dispatch's docs tests use the same probe).
func waitForLockWait(t *testing.T, ctx context.Context, st *store.Store, like string, returned <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-returned:
			t.Fatalf("the operation returned (%v) before any statement matching %s waited on a lock", err, like)
		default:
		}
		var waiting int
		if err := st.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query like $1`, like).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no statement waiting on a lock matching %s", like)
}

// TestChainVerificationRefusesADecisionTheBrokerDidNotWrite pins what AuthenticateLauncher's
// chain re-check proves on every call: a credential authenticates only while its record embeds a
// request object the machine really signed and carries exactly one terminal decision, an approval
// by the record's own approver. An approved event rewritten to name another login, a second
// terminal event beside the real approval (which credential_request_decision's unique index
// refuses, so the test drops it the way a direct writer could), a copy of the record whose
// request object's signature was altered, approved by the approver, and an approved copy recorded
// as an agent_secret record, each turn a credential that authenticates into one that does not.
func TestChainVerificationRefusesADecisionTheBrokerDidNotWrite(t *testing.T) {
	for name, tamper := range map[string]func(t testing.TB, st *store.Store, recordID string){
		"approved by another login": storetest.Exec(`update credential_request_events set login='mallory' where record_id=$1 and event='approved'`),
		"a second terminal event": storetest.Exec(
			`drop index credential_request_decision`,
			`insert into credential_request_events (record_id, event, login, actor) values ($1, 'denied', 'sjawhar', 'human:sjawhar')`,
		),
		"a request object whose signature was altered": func(t testing.TB, st *store.Store, recordID string) {
			forged := storetest.ForgeRequestSignature(t, st, recordID)
			tag, err := st.Pool.Exec(context.Background(), `update launcher_credentials set record_id=$2 where record_id=$1`, recordID, forged)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("point the credential at the forged record: %d rows, %v", tag.RowsAffected(), err)
			}
		},
		"a record of the other kind": func(t testing.TB, st *store.Store, recordID string) {
			copyID := storetest.CopyAsOtherKind(t, st, recordID)
			tag, err := st.Pool.Exec(context.Background(), `update launcher_credentials set record_id=$2 where record_id=$1`, recordID, copyID)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("point the credential at the other kind's record: %d rows, %v", tag.RowsAffected(), err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc := newFixture(t)
			ctx := context.Background()
			recordID, credentialID := approvedCredential(t, svc)

			// Sanity: the untouched credential authenticates — otherwise a broken fixture (or a
			// regression that always refuses) could make the refusal below pass for the wrong
			// reason.
			if _, live, err := svc.Enroll.AuthenticateLauncher(ctx, credentialID); err != nil || !live {
				t.Fatalf("AuthenticateLauncher(before tamper) = live=%v err=%v, want live=true", live, err)
			}
			tamper(t, svc.Store, recordID)
			if _, live, err := svc.Enroll.AuthenticateLauncher(ctx, credentialID); err != nil || live {
				t.Fatalf("AuthenticateLauncher(%s) = live=%v err=%v, want live=false, err=nil", name, live, err)
			}
		})
	}
}
