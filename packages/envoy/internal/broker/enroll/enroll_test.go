package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const testAudience = "broker"

func testDatabaseURL(t *testing.T) string {
	url := os.Getenv("BROKER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("BROKER_TEST_DATABASE_URL must be set to run Postgres enroll tests")
	}
	return url
}

// newService opens a migrated store and returns a Service with no pod verifier configured. Tests
// that need a pod verifier call withPodVerifier instead.
func newService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Pool.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &Service{Store: s, Lease: time.Hour}
}

// withPodVerifier wires svc to a real K8sPodVerifier backed by a fresh local OIDC issuer, and
// returns the issuer and its published signing key so a test can mint pod tokens.
func withPodVerifier(t *testing.T, svc *Service) (*oidctest.Issuer, *oidctest.Key) {
	t.Helper()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "signing-key")
	verifier, err := oidc.New(context.Background(), issuer.URL(), testAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	svc.Pod = K8sPodVerifier{Verifier: verifier}
	return issuer, key
}

// mintPodToken mints a projected service-account token bound to podUID, the shape
// K8sPodVerifier.Verify reads.
func mintPodToken(t *testing.T, issuer *oidctest.Issuer, key *oidctest.Key, subject, podUID string) string {
	t.Helper()
	claims := issuer.Claims(subject, testAudience)
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(raw, &merged); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	merged["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	return issuer.Mint(t, key, merged)
}

func str(s string) *string { return &s }

func TestAuthenticateLauncherSucceedsAndFailsOnWrongToken(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	id, token, err := svc.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-1")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}

	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher(right token): %v", err)
	}
	if cred.ID != id || cred.Operator == nil || *cred.Operator != "sjawhar" || cred.Host != "devbox" {
		t.Fatalf("credential = %+v, want operator sjawhar id %s", cred, id)
	}

	if _, err := svc.AuthenticateLauncher(ctx, token+"x"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("AuthenticateLauncher(wrong token) = %v, want ErrUnauthenticated", err)
	}
}

func TestCreateSucceedsForMatchingOperatorAndRejectsMismatch(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	_, token, err := svc.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-1")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}

	enr, err := svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-sjawhar-1", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-match",
	})
	if err != nil {
		t.Fatalf("Create(matching operator): %v", err)
	}
	if enr.ID == uuid.Nil || enr.Existing {
		t.Fatalf("Create(matching operator) = %+v, want a fresh enrollment", enr)
	}

	_, err = svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-mallory-1", Operator: str("mallory"),
		ApproverKind: "operator", Thumbprint: "tp-mallory",
	})
	if !errors.Is(err, ErrOperatorMismatch) {
		t.Fatalf("Create(operator mismatch) = %v, want ErrOperatorMismatch", err)
	}
}

// TestOperatorCredentialRefusesPodEnrollment is the regression for review finding 2: an operator
// credential enrols boxes and host sessions only, per Create's doc comment. Matching operator and
// an otherwise-valid pod token must still be refused for kind: pod.
func TestOperatorCredentialRefusesPodEnrollment(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	issuer, key := withPodVerifier(t, svc)
	_, token, err := svc.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-op-pod")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}

	_, err = svc.Create(ctx, cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-op-1", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-op-pod",
		PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-op-1"),
	})
	if !errors.Is(err, ErrOperatorMismatch) {
		t.Fatalf("Create(operator credential, kind pod, matching operator and valid pod token) = %v, want ErrOperatorMismatch", err)
	}
}

func TestServiceCredentialEnrolsPodOnlyWithNoOperator(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	issuer, key := withPodVerifier(t, svc)
	_, token, err := svc.MintLauncherCredential(ctx, nil, str("legion-daemon"), "cluster", "ask-2")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}
	if cred.Operator != nil {
		t.Fatalf("service credential has operator %v, want nil", *cred.Operator)
	}

	// Refuses kind: box even with a nil operator.
	_, err = svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-1", ApproverKind: "operator", Thumbprint: "tp-1",
	})
	if !errors.Is(err, ErrOperatorMismatch) {
		t.Fatalf("Create(service cred, kind box) = %v, want ErrOperatorMismatch", err)
	}

	// Refuses any non-nil operator, even for kind: pod.
	_, err = svc.Create(ctx, cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-uid-1", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-1", PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-uid-1"),
	})
	if !errors.Is(err, ErrOperatorMismatch) {
		t.Fatalf("Create(service cred, kind pod, operator set) = %v, want ErrOperatorMismatch", err)
	}

	// Accepts kind: pod with operator nil and a token whose pod UID matches RuntimeID.
	enr, err := svc.Create(ctx, cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-uid-2", ApproverKind: "operator", Thumbprint: "tp-2",
		PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-uid-2"),
	})
	if err != nil {
		t.Fatalf("Create(service cred, kind pod, matching token): %v", err)
	}
	if enr.Kind != "pod" || enr.Operator != nil {
		t.Fatalf("enrollment = %+v, want kind pod and nil operator", enr)
	}
}

func TestCreateIsIdempotentAndRefusesConflictingThumbprint(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	_, token, err := svc.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-3")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}

	first, err := svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-idem-1", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-same",
	})
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}

	again, err := svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-idem-1", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-same",
	})
	if err != nil {
		t.Fatalf("Create(retry, same thumbprint): %v", err)
	}
	if !again.Existing || again.ID != first.ID {
		t.Fatalf("Create(retry) = %+v, want Existing=true and ID=%s", again, first.ID)
	}

	_, err = svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-idem-1", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-different",
	})
	if !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("Create(retry, different thumbprint) = %v, want ErrAlreadyEnrolled", err)
	}
}

func TestRevokeThenLookupReportsNotLive(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	_, token, err := svc.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-4")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}
	enr, err := svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-revoke-1", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-revoke",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, live, err := svc.Lookup(ctx, enr.ID.String()); err != nil || !live {
		t.Fatalf("Lookup(before revoke) = live=%v err=%v, want live=true", live, err)
	}

	if err := svc.Revoke(ctx, cred, enr.ID.String(), "sjawhar"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if _, live, err := svc.Lookup(ctx, enr.ID.String()); err != nil || live {
		t.Fatalf("Lookup(after revoke) = live=%v err=%v, want live=false, err=nil", live, err)
	}
}

// TestRevokeNonexistentEnrollmentReturnsErrNotLive is the regression for review finding 1a:
// revoking an id with no enrollment row must not proceed to write an audit row.
func TestRevokeNonexistentEnrollmentReturnsErrNotLive(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	id := uuid.New().String()

	if err := svc.Revoke(ctx, Credential{}, id, "sjawhar"); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Revoke(nonexistent) = %v, want ErrNotLive", err)
	}

	var n int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from audit where kind='enrollment.revoked' and enrollment_id=$1`, id).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("audit rows for nonexistent enrollment = %d, want 0", n)
	}
}

// TestRevokeTwiceReturnsErrNotLive is the regression for review finding 1b: revoking an
// already-revoked enrollment a second time must not write a second audit row.
func TestRevokeTwiceReturnsErrNotLive(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	_, token, err := svc.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-revoke-twice")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}
	enr, err := svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-revoke-twice", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-revoke-twice",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Revoke(ctx, cred, enr.ID.String(), "sjawhar"); err != nil {
		t.Fatalf("Revoke(first): %v", err)
	}
	if err := svc.Revoke(ctx, cred, enr.ID.String(), "sjawhar"); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Revoke(second) = %v, want ErrNotLive", err)
	}

	var n int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from audit where kind='enrollment.revoked' and enrollment_id=$1`, enr.ID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("audit rows for enrollment = %d, want exactly 1", n)
	}
}

// TestRevokeRevokesLiveGrantsUnderEnrollment is the regression for review finding 1c: Revoke's
// "every live grant under it" behavior had zero test coverage. It inserts a request and a live
// grant directly (grant issuance belongs to a later task's service, not this package) and checks
// Revoke sets revoked_at/revoked_by on the grant in the same call.
func TestRevokeRevokesLiveGrantsUnderEnrollment(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	_, token, err := svc.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-revoke-grant")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}
	enr, err := svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-revoke-grant", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-revoke-grant",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	requestID := uuid.New()
	if _, err := svc.Store.Pool.Exec(ctx, `insert into requests (id, enrollment_id, issue_key, reason, state, rules_version, lifetime_seconds)
		values ($1,$2,'AGENTC-1','test fixture','granted','v1',3600)`, requestID, enr.ID); err != nil {
		t.Fatalf("insert request fixture: %v", err)
	}
	grantID := uuid.New()
	if _, err := svc.Store.Pool.Exec(ctx, `insert into grants (id, request_id, enrollment_id, expires_at) values ($1,$2,$3, now() + interval '1 hour')`,
		grantID, requestID, enr.ID); err != nil {
		t.Fatalf("insert grant fixture: %v", err)
	}

	if err := svc.Revoke(ctx, cred, enr.ID.String(), "sjawhar"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	var revokedAt *time.Time
	var revokedBy *string
	if err := svc.Store.Pool.QueryRow(ctx, `select revoked_at, revoked_by from grants where id=$1`, grantID).Scan(&revokedAt, &revokedBy); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if revokedAt == nil || revokedBy == nil || *revokedBy != "sjawhar" {
		t.Fatalf("grant revoked_at=%v revoked_by=%v, want both set with revoked_by=sjawhar", revokedAt, revokedBy)
	}
}

// TestRevokeRefusesWrongOperator is the regression for the review's Critical finding: Revoke had
// no ownership check at all, so any live launcher credential could revoke any enrollment by
// guessing or knowing its id. An operator A credential must not be able to revoke operator B's
// enrollment, and the enrollment must remain untouched (still live) afterward.
func TestRevokeRefusesWrongOperator(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	_, tokenB, err := svc.MintLauncherCredential(ctx, str("bob"), nil, "bobs-box", "ask-b")
	if err != nil {
		t.Fatalf("MintLauncherCredential(bob): %v", err)
	}
	credB, err := svc.AuthenticateLauncher(ctx, tokenB)
	if err != nil {
		t.Fatalf("AuthenticateLauncher(bob): %v", err)
	}
	enrB, err := svc.Create(ctx, credB, Enrollment{
		Kind: "box", RuntimeID: "box-bob-1", Operator: str("bob"),
		ApproverKind: "operator", Thumbprint: "tp-bob",
	})
	if err != nil {
		t.Fatalf("Create(bob's enrollment): %v", err)
	}

	_, tokenA, err := svc.MintLauncherCredential(ctx, str("alice"), nil, "alices-box", "ask-a")
	if err != nil {
		t.Fatalf("MintLauncherCredential(alice): %v", err)
	}
	credA, err := svc.AuthenticateLauncher(ctx, tokenA)
	if err != nil {
		t.Fatalf("AuthenticateLauncher(alice): %v", err)
	}

	if err := svc.Revoke(ctx, credA, enrB.ID.String(), "alice"); !errors.Is(err, ErrOperatorMismatch) {
		t.Fatalf("Revoke(alice's credential, bob's enrollment) = %v, want ErrOperatorMismatch", err)
	}

	if _, live, err := svc.Lookup(ctx, enrB.ID.String()); err != nil || !live {
		t.Fatalf("Lookup(bob's enrollment after refused cross-operator revoke) = live=%v err=%v, want live=true", live, err)
	}
}

// TestRevokeAllowsServiceCredentialForAnyPodEnrollment mirrors Create's own trust boundary: a
// service credential (no operator) may revoke any pod enrollment, since pod enrollments never
// carry an operator to distinguish between service credentials.
func TestRevokeAllowsServiceCredentialForAnyPodEnrollment(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	issuer, key := withPodVerifier(t, svc)
	_, token, err := svc.MintLauncherCredential(ctx, nil, str("legion-daemon"), "cluster", "ask-pod-revoke")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}
	enr, err := svc.Create(ctx, cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-revoke-1", ApproverKind: "operator", Thumbprint: "tp-pod-revoke",
		PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-revoke-1"),
	})
	if err != nil {
		t.Fatalf("Create(pod): %v", err)
	}

	if err := svc.Revoke(ctx, cred, enr.ID.String(), "legion-daemon"); err != nil {
		t.Fatalf("Revoke(service credential, pod enrollment) = %v, want nil", err)
	}
	if _, live, err := svc.Lookup(ctx, enr.ID.String()); err != nil || live {
		t.Fatalf("Lookup(after revoke) = live=%v err=%v, want live=false", live, err)
	}
}

// TestRevokeRefusesOperatorCredentialForPodEnrollment mirrors Create's own refusal
// (TestOperatorCredentialRefusesPodEnrollment): an operator credential may not revoke a pod
// enrollment, symmetrically with being unable to create one.
func TestRevokeRefusesOperatorCredentialForPodEnrollment(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	issuer, key := withPodVerifier(t, svc)
	_, serviceToken, err := svc.MintLauncherCredential(ctx, nil, str("legion-daemon"), "cluster", "ask-pod-2")
	if err != nil {
		t.Fatalf("MintLauncherCredential(service): %v", err)
	}
	serviceCred, err := svc.AuthenticateLauncher(ctx, serviceToken)
	if err != nil {
		t.Fatalf("AuthenticateLauncher(service): %v", err)
	}
	enr, err := svc.Create(ctx, serviceCred, Enrollment{
		Kind: "pod", RuntimeID: "pod-revoke-2", ApproverKind: "operator", Thumbprint: "tp-pod-revoke-2",
		PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-revoke-2"),
	})
	if err != nil {
		t.Fatalf("Create(pod): %v", err)
	}

	_, opToken, err := svc.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-pod-3")
	if err != nil {
		t.Fatalf("MintLauncherCredential(operator): %v", err)
	}
	opCred, err := svc.AuthenticateLauncher(ctx, opToken)
	if err != nil {
		t.Fatalf("AuthenticateLauncher(operator): %v", err)
	}

	if err := svc.Revoke(ctx, opCred, enr.ID.String(), "sjawhar"); !errors.Is(err, ErrOperatorMismatch) {
		t.Fatalf("Revoke(operator credential, pod enrollment) = %v, want ErrOperatorMismatch", err)
	}
	if _, live, err := svc.Lookup(ctx, enr.ID.String()); err != nil || !live {
		t.Fatalf("Lookup(pod enrollment after refused revoke) = live=%v err=%v, want live=true", live, err)
	}
}

func TestReplayReportsFreshOnceThenSeen(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	expires := time.Now().Add(time.Hour)
	jti := uuid.New().String()

	fresh, err := svc.Replay(ctx, jti, expires)
	if err != nil || !fresh {
		t.Fatalf("Replay(first) = fresh=%v err=%v, want fresh=true", fresh, err)
	}

	fresh, err = svc.Replay(ctx, jti, expires)
	if err != nil || fresh {
		t.Fatalf("Replay(second, same jti) = fresh=%v err=%v, want fresh=false", fresh, err)
	}
}

func TestPodEnrollmentRejectsBadOrMismatchedToken(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	issuer, key := withPodVerifier(t, svc)
	_, token, err := svc.MintLauncherCredential(ctx, nil, str("legion-daemon"), "cluster", "ask-5")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}

	// A token that isn't even a valid JWT.
	_, err = svc.Create(ctx, cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-bad-1", ApproverKind: "operator", Thumbprint: "tp-bad",
		PodToken: "not-a-jwt",
	})
	if !errors.Is(err, ErrPodIdentity) {
		t.Fatalf("Create(garbage pod token) = %v, want ErrPodIdentity", err)
	}

	// A validly-signed token whose bound pod UID does not match the claimed RuntimeID.
	mismatched := mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-actual-uid")
	_, err = svc.Create(ctx, cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-claimed-uid", ApproverKind: "operator", Thumbprint: "tp-mismatch",
		PodToken: mismatched,
	})
	if !errors.Is(err, ErrPodIdentity) {
		t.Fatalf("Create(mismatched pod token) = %v, want ErrPodIdentity", err)
	}
}

// TestCreateRetriesWhenConflictingRowIsRevokedBeforeRecovery is the regression for review
// finding 3: the conflicting live row can be revoked between createAttempt's failed insert and
// its recovery lookup (a concurrent Revoke racing this Create), which otherwise makes that lookup
// return pgx.ErrNoRows and propagate as a raw internal error instead of retrying.
//
// Building a genuine goroutine race that lands inside that narrow window deterministically isn't
// practical — it depends on winning a real interleaving between a second connection's Revoke and
// this connection's SELECT. Service.testConflictHook is an unexported test-only seam (documented
// on the struct) that runs synchronously right where that race would land, so this test revokes
// the conflicting enrollment from inside the hook and gets the same code path deterministically.
func TestCreateRetriesWhenConflictingRowIsRevokedBeforeRecovery(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	_, token, err := svc.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-race")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}

	first, err := svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-race-1", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-race-first",
	})
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}

	var hookCalls int
	svc.testConflictHook = func() {
		hookCalls++
		if hookCalls > 1 {
			return // already revoked on the first call; nothing left to race against.
		}
		if err := svc.Revoke(ctx, cred, first.ID.String(), "race-test"); err != nil {
			t.Fatalf("Revoke inside hook: %v", err)
		}
	}

	second, err := svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-race-1", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-race-second",
	})
	if err != nil {
		t.Fatalf("Create(second, races a concurrent revoke): %v", err)
	}
	if hookCalls == 0 {
		t.Fatal("testConflictHook never ran; the insert didn't conflict, so this test didn't exercise the race path")
	}
	if second.Existing {
		t.Fatalf("Create(second) = %+v, want a fresh enrollment (the conflicting row was revoked, not idempotently matched)", second)
	}
	if second.ID == first.ID {
		t.Fatalf("Create(second).ID = %s, want a new id distinct from the revoked enrollment %s", second.ID, first.ID)
	}
	if second.Thumbprint != "tp-race-second" {
		t.Fatalf("Create(second).Thumbprint = %q, want tp-race-second", second.Thumbprint)
	}
}

// TestLookupRejectsNonUUIDWithoutError is the regression for the cross-task finding recorded
// against Task 3's proof.Verifier review: an attacker-controlled eid claim in a forged session
// proof must fail Lookup as "not live" (so proof.Verifier.Verify answers 401), never propagate a
// raw Postgres type-mismatch error (SQLSTATE 22P02 from the enrollments.id uuid column), which
// proof.Verifier.Verify would otherwise surface as a 500.
func TestLookupRejectsNonUUIDWithoutError(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	thumbprint, live, err := svc.Lookup(ctx, "'; drop table enrollments; --")
	if err != nil {
		t.Fatalf("Lookup(non-UUID) returned an error, want nil: %v", err)
	}
	if live {
		t.Fatalf("Lookup(non-UUID) live = true, want false")
	}
	if thumbprint != "" {
		t.Fatalf("Lookup(non-UUID) thumbprint = %q, want empty", thumbprint)
	}
}

// TestSessionID covers enroll.Service.SessionID's three answers: a live enrollment's own
// session_id, a live enrollment with none (null), and an id with no live row at all (revoked or
// never existed) — every case answers ("", nil) except the first.
func TestSessionID(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	_, token, err := svc.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-1")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}

	withSession, err := svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-with-session", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-with-session", SessionID: str("session-123"),
	})
	if err != nil {
		t.Fatalf("Create(withSession): %v", err)
	}
	if got, err := svc.SessionID(ctx, withSession.ID.String()); err != nil || got != "session-123" {
		t.Fatalf("SessionID(withSession) = %q, %v, want %q, nil", got, err, "session-123")
	}

	noSession, err := svc.Create(ctx, cred, Enrollment{
		Kind: "box", RuntimeID: "box-no-session", Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-no-session",
	})
	if err != nil {
		t.Fatalf("Create(noSession): %v", err)
	}
	if got, err := svc.SessionID(ctx, noSession.ID.String()); err != nil || got != "" {
		t.Fatalf("SessionID(noSession) = %q, %v, want empty, nil", got, err)
	}

	if err := svc.Revoke(ctx, cred, withSession.ID.String(), "launcher:"+cred.ID.String()); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got, err := svc.SessionID(ctx, withSession.ID.String()); err != nil || got != "" {
		t.Fatalf("SessionID(revoked) = %q, %v, want empty, nil", got, err)
	}

	if got, err := svc.SessionID(ctx, uuid.NewString()); err != nil || got != "" {
		t.Fatalf("SessionID(nonexistent) = %q, %v, want empty, nil", got, err)
	}
}
