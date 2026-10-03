package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const testAudience = "broker"

// newService opens a store on a fresh schema (params are extra connection parameters) and returns
// a Service with no pod verifier configured. Tests that need a pod verifier call withPodVerifier.
func newService(t *testing.T, params ...string) *Service {
	t.Helper()
	return &Service{Store: storetest.Open(t, params...), Lease: time.Hour}
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

// mintCredential mints a launcher credential directly through the package's own private mint
// path — a fresh key, a fresh thumbprint and JWK, no backing credential-request record — and
// returns the Credential these Create/Revoke tests exercise. These tests care about Create and
// Revoke given an already-minted credential, not about how a credential comes to exist (that is
// machine_test.go's job, including AuthenticateLauncher's own issuance-chain re-verification), so
// there is no record to back this credential and no need for one.
func mintCredential(t *testing.T, svc *Service, operator, service *string, host string) Credential {
	t.Helper()
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
	id, err := svc.mintLauncherCredential(context.Background(), svc.Store.Pool, operator, service, host, thumbprint, jwk, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("mintLauncherCredential: %v", err)
	}
	if operator != nil {
		operator = new(record.CanonicalLogin(*operator))
	}
	return Credential{ID: id, Operator: operator, Service: service, Host: host}
}

// TestAnOperatorCredentialEnrollsItsOwnOperator: a personal launcher credential's operator is the
// email of the person who approved its machine login, and every enrollment it makes records that
// person. A stated operator must name the same person, in any casing; one that names anyone else is
// refused; an enrollment that states none is that person's all the same.
func TestAnOperatorCredentialEnrollsItsOwnOperator(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")

	for i, tc := range []struct {
		name   string
		stated *string
	}{
		{"the credential's email", str("ada@example.com")},
		{"that email in other casing", str(" Ada@Example.COM ")},
		{"no operator", nil},
	} {
		runtimeID := fmt.Sprintf("host-ada-%d", i)
		enr, err := svc.Create(ctx, cred, Enrollment{Kind: "host", RuntimeID: runtimeID, Operator: tc.stated, Thumbprint: "tp-" + runtimeID})
		if err != nil {
			t.Fatalf("Create(stating %s): %v", tc.name, err)
		}
		if enr.ID == uuid.Nil || enr.Existing || enr.Operator == nil || *enr.Operator != "ada@example.com" {
			t.Fatalf("Create(stating %s) = %+v, want a fresh enrollment of ada@example.com", tc.name, enr)
		}
		stored, err := svc.Get(ctx, enr.ID.String())
		if err != nil || stored.Operator == nil || *stored.Operator != "ada@example.com" {
			t.Fatalf("Get(enrollment stating %s) = %+v %v, want operator ada@example.com", tc.name, stored, err)
		}
	}

	_, err := svc.Create(ctx, cred, Enrollment{
		Kind: "host", RuntimeID: "host-mallory", Operator: str("mallory@example.com"), Thumbprint: "tp-mallory",
	})
	if !errors.Is(err, ErrOperatorMismatch) {
		t.Fatalf("Create(another person's email) = %v, want ErrOperatorMismatch", err)
	}
}

// TestOperatorCredentialRefusesPodEnrollment is the regression for review finding 2: an operator
// credential enrols boxes and host sessions only, per Create's doc comment. Matching operator and
// an otherwise-valid pod token must still be refused for kind: pod.
func TestOperatorCredentialRefusesPodEnrollment(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	issuer, key := withPodVerifier(t, svc)
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")

	_, err := svc.Create(ctx, cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-op-1", Operator: str("ada@example.com"), Thumbprint: "tp-op-pod",
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
	cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	if cred.Operator != nil {
		t.Fatalf("service credential has operator %v, want nil", *cred.Operator)
	}

	// Refuses kind: box even with a nil operator.
	_, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-1", Thumbprint: "tp-1"})
	if !errors.Is(err, ErrOperatorMismatch) {
		t.Fatalf("Create(service cred, kind box) = %v, want ErrOperatorMismatch", err)
	}

	// Refuses any non-nil operator, even for kind: pod.
	_, err = svc.Create(ctx, cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-uid-1", Operator: str("ada@example.com"),
		Thumbprint: "tp-1", PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-uid-1"),
	})
	if !errors.Is(err, ErrOperatorMismatch) {
		t.Fatalf("Create(service cred, kind pod, operator set) = %v, want ErrOperatorMismatch", err)
	}

	// Accepts kind: pod with operator nil and a token whose pod UID matches RuntimeID.
	enr, err := svc.Create(ctx, cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-uid-2", Thumbprint: "tp-2",
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
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")

	first, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-idem-1", Operator: str("ada@example.com"), Thumbprint: "tp-same"})
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}

	again, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-idem-1", Operator: str("ada@example.com"), Thumbprint: "tp-same"})
	if err != nil {
		t.Fatalf("Create(retry, same thumbprint): %v", err)
	}
	if !again.Existing || again.ID != first.ID {
		t.Fatalf("Create(retry) = %+v, want Existing=true and ID=%s", again, first.ID)
	}

	_, err = svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-idem-1", Operator: str("ada@example.com"), Thumbprint: "tp-different"})
	if !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("Create(retry, different thumbprint) = %v, want ErrAlreadyEnrolled", err)
	}
}

// TestIdempotentRetryNeedsOnlyOneConnection pins that the conflict-recovery path of Create never
// holds one pooled connection while asking for a second. On a one-connection pool, a retried
// enrollment must come back as Existing; holding the aborted insert's connection while the
// recovery lookup waits for another deadlocks, and enough concurrent retries deadlock any pool.
func TestIdempotentRetryNeedsOnlyOneConnection(t *testing.T) {
	svc := newService(t, "pool_max_conns=1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	in := Enrollment{Kind: "box", RuntimeID: "box-one-conn", Operator: str("ada@example.com"), Thumbprint: "tp-one-conn"}
	first, err := svc.Create(ctx, cred, in)
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	retryCtx, retryCancel := context.WithTimeout(ctx, 3*time.Second)
	defer retryCancel()
	again, err := svc.Create(retryCtx, cred, in)
	if err != nil {
		t.Fatalf("Create(retry) on a one-connection pool: %v", err)
	}
	if !again.Existing || again.ID != first.ID {
		t.Fatalf("Create(retry) = %+v, want Existing with ID %s", again, first.ID)
	}
}

func TestRevokeThenLookupReportsNotLive(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	enr, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-revoke-1", Operator: str("ada@example.com"), Thumbprint: "tp-revoke"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, live, err := svc.Lookup(ctx, enr.ID.String()); err != nil || !live {
		t.Fatalf("Lookup(before revoke) = live=%v err=%v, want live=true", live, err)
	}

	if err := svc.Revoke(ctx, cred, enr.ID.String(), "ada@example.com"); err != nil {
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

	if err := svc.Revoke(ctx, Credential{}, id, "ada@example.com"); !errors.Is(err, ErrNotLive) {
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
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	enr, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-revoke-twice", Operator: str("ada@example.com"), Thumbprint: "tp-revoke-twice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Revoke(ctx, cred, enr.ID.String(), "ada@example.com"); err != nil {
		t.Fatalf("Revoke(first): %v", err)
	}
	if err := svc.Revoke(ctx, cred, enr.ID.String(), "ada@example.com"); !errors.Is(err, ErrNotLive) {
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
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	enr, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-revoke-grant", Operator: str("ada@example.com"), Thumbprint: "tp-revoke-grant"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	requestID := uuid.New()
	if _, err := svc.Store.Pool.Exec(ctx, `insert into requests (id, enrollment_id, reason, state, rules_version, lifetime_seconds)
		values ($1,$2,'test fixture','granted','v1',3600)`, requestID, enr.ID); err != nil {
		t.Fatalf("insert request fixture: %v", err)
	}
	grantID := uuid.New()
	if _, err := svc.Store.Pool.Exec(ctx, `insert into grants (id, request_id, enrollment_id, expires_at) values ($1,$2,$3, now() + interval '1 hour')`,
		grantID, requestID, enr.ID); err != nil {
		t.Fatalf("insert grant fixture: %v", err)
	}

	if err := svc.Revoke(ctx, cred, enr.ID.String(), "ada@example.com"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	var revokedAt *time.Time
	var revokedBy *string
	if err := svc.Store.Pool.QueryRow(ctx, `select revoked_at, revoked_by from grants where id=$1`, grantID).Scan(&revokedAt, &revokedBy); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if revokedAt == nil || revokedBy == nil || *revokedBy != "ada@example.com" {
		t.Fatalf("grant revoked_at=%v revoked_by=%v, want both set with revoked_by=ada@example.com", revokedAt, revokedBy)
	}
}

// TestRevokeRefusesWrongOperator is the regression for the review's Critical finding: without an
// ownership check in Revoke, any live launcher credential could revoke any enrollment by guessing
// or knowing its id. An operator A credential must not be able to revoke operator B's enrollment,
// and the enrollment must remain untouched (still live) afterward.
func TestRevokeRefusesWrongOperator(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	credB := mintCredential(t, svc, str("bob@example.com"), nil, "bobs-box")
	enrB, err := svc.Create(ctx, credB, Enrollment{Kind: "box", RuntimeID: "box-bob-1", Operator: str("bob@example.com"), Thumbprint: "tp-bob"})
	if err != nil {
		t.Fatalf("Create(bob's enrollment): %v", err)
	}

	credA := mintCredential(t, svc, str("alice@example.com"), nil, "alices-box")

	if err := svc.Revoke(ctx, credA, enrB.ID.String(), "alice@example.com"); !errors.Is(err, ErrOperatorMismatch) {
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
	cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	enr, err := svc.Create(ctx, cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-revoke-1", Thumbprint: "tp-pod-revoke",
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
	serviceCred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	enr, err := svc.Create(ctx, serviceCred, Enrollment{
		Kind: "pod", RuntimeID: "pod-revoke-2", Thumbprint: "tp-pod-revoke-2",
		PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-revoke-2"),
	})
	if err != nil {
		t.Fatalf("Create(pod): %v", err)
	}

	opCred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")

	if err := svc.Revoke(ctx, opCred, enr.ID.String(), "ada@example.com"); !errors.Is(err, ErrOperatorMismatch) {
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
	cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")

	// A token that isn't even a valid JWT.
	_, err := svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: "pod-bad-1", Thumbprint: "tp-bad", PodToken: "not-a-jwt"})
	if !errors.Is(err, ErrPodIdentity) {
		t.Fatalf("Create(garbage pod token) = %v, want ErrPodIdentity", err)
	}

	// A validly-signed token whose bound pod UID does not match the claimed RuntimeID.
	mismatched := mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-actual-uid")
	_, err = svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: "pod-claimed-uid", Thumbprint: "tp-mismatch", PodToken: mismatched})
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
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")

	first, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-race-1", Operator: str("ada@example.com"), Thumbprint: "tp-race-first"})
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

	second, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-race-1", Operator: str("ada@example.com"), Thumbprint: "tp-race-second"})
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
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")

	withSession, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-with-session", Operator: str("ada@example.com"), Thumbprint: "tp-with-session", SessionID: str("session-123")})
	if err != nil {
		t.Fatalf("Create(withSession): %v", err)
	}
	if got, err := svc.SessionID(ctx, withSession.ID.String()); err != nil || got != "session-123" {
		t.Fatalf("SessionID(withSession) = %q, %v, want %q, nil", got, err, "session-123")
	}

	noSession, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-no-session", Operator: str("ada@example.com"), Thumbprint: "tp-no-session"})
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

// insertPendingRequest writes a pending request row under enrollmentID the way requests.Machine
// would, so these tests can watch what ending an enrollment does to it.
func insertPendingRequest(t *testing.T, svc *Service, enrollmentID uuid.UUID) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := svc.Store.Pool.Exec(context.Background(), `insert into requests (id, enrollment_id, reason, state, allowed_approver, rules_version, lifetime_seconds, pending_expires_at)
		values ($1,$2,'need it','pending','ada@example.com','v',3600, now() + interval '1 hour')`, id, enrollmentID); err != nil {
		t.Fatalf("insert pending request: %v", err)
	}
	return id
}

func requestState(t *testing.T, svc *Service, id string) (state, decidedBy string, audits int) {
	t.Helper()
	ctx := context.Background()
	if err := svc.Store.Pool.QueryRow(ctx, `select state, coalesce(decided_by, '') from requests where id=$1`, id).Scan(&state, &decidedBy); err != nil {
		t.Fatalf("read request %s: %v", id, err)
	}
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from audit where kind='request.cancelled' and request_id=$1`, id).Scan(&audits); err != nil {
		t.Fatalf("count request.cancelled audit rows: %v", err)
	}
	return state, decidedBy, audits
}

// TestRevokeCancelsPendingRequests pins that ending an enrollment withdraws what it was still
// waiting on: every pending request under it is cancelled, with its own audit row, in the same
// transaction as the revoke.
func TestRevokeCancelsPendingRequests(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	enr, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-cancel", Operator: str("ada@example.com"), Thumbprint: "tp-cancel"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	requestID := insertPendingRequest(t, svc, enr.ID)
	if err := svc.Revoke(ctx, cred, enr.ID.String(), "launcher:test"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if state, by, audits := requestState(t, svc, requestID); state != "cancelled" || by != "launcher:test" || audits != 1 {
		t.Fatalf("pending request after revoke: state=%s decided_by=%s audit rows=%d, want cancelled by launcher:test with one audit row", state, by, audits)
	}
}

// TestRevokeClosesPendingRecords pins that a pending request's credential-request record is closed
// with a cancelled event when its enrollment ends, so the approver's pending list (which lists
// records with no terminal event) stops offering a request nobody can approve any more.
func TestRevokeClosesPendingRecords(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	enr, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-record", Operator: str("ada@example.com"), Thumbprint: "tp-record"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	recordID := strings.Repeat("ab", 32)
	if _, err := svc.Store.Pool.Exec(ctx, `insert into credential_requests (id, body, kind, approver, enrollment_id, expires_at)
		values ($1,'body','agent_secret','ada@example.com',$2, now() + interval '1 hour')`, recordID, enr.ID); err != nil {
		t.Fatalf("insert record: %v", err)
	}
	requestID := insertPendingRequest(t, svc, enr.ID)
	if _, err := svc.Store.Pool.Exec(ctx, `update requests set record_id=$2 where id=$1`, requestID, recordID); err != nil {
		t.Fatalf("link record: %v", err)
	}
	if err := svc.Revoke(ctx, cred, enr.ID.String(), "launcher:test"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	var event, actor string
	if err := svc.Store.Pool.QueryRow(ctx, `select event, actor from credential_request_events where record_id=$1`, recordID).Scan(&event, &actor); err != nil {
		t.Fatalf("read the record's event: %v", err)
	}
	if event != "cancelled" || actor != "launcher:test" {
		t.Fatalf("record event = %s by %s, want cancelled by launcher:test", event, actor)
	}
}

// TestLapsedLeaseReleasesTheRuntimeID pins that an enrollment whose lease lapsed without a revoke
// is dead to Create: re-enrolling the same key mints a fresh enrollment (never the dead one back
// as Existing), re-enrolling a different key is not refused as already enrolled, and the lapsed
// enrollment is ended — revoked with an enrollment.expired audit row and its pending requests
// cancelled.
func TestLapsedLeaseReleasesTheRuntimeID(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	in := Enrollment{Kind: "box", RuntimeID: "box-lapse", Operator: str("ada@example.com"), Thumbprint: "tp-lapse"}
	lapse := func(id uuid.UUID) {
		t.Helper()
		if _, err := svc.Store.Pool.Exec(ctx, `update enrollments set lease_expires_at = now() - interval '1 minute' where id=$1`, id); err != nil {
			t.Fatalf("lapse lease: %v", err)
		}
	}

	first, err := svc.Create(ctx, cred, in)
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	pending := insertPendingRequest(t, svc, first.ID)
	lapse(first.ID)
	sameKey, err := svc.Create(ctx, cred, in)
	if err != nil {
		t.Fatalf("Create(same key after the lease lapsed): %v", err)
	}
	if sameKey.Existing || sameKey.ID == first.ID {
		t.Fatalf("Create(same key after lapse) = %+v, want a fresh enrollment, not the dead %s", sameKey, first.ID)
	}
	if _, err := svc.Renew(ctx, sameKey.ID.String()); err != nil {
		t.Fatalf("Renew(the fresh enrollment): %v", err)
	}
	var revoked bool
	var expiredAudits int
	if err := svc.Store.Pool.QueryRow(ctx, `select revoked_at is not null, (select count(*) from audit where kind='enrollment.expired' and enrollment_id=$1) from enrollments where id=$1`, first.ID).Scan(&revoked, &expiredAudits); err != nil {
		t.Fatalf("read the lapsed enrollment: %v", err)
	}
	if !revoked || expiredAudits != 1 {
		t.Fatalf("lapsed enrollment revoked=%v enrollment.expired audit rows=%d, want revoked with one", revoked, expiredAudits)
	}
	if state, by, audits := requestState(t, svc, pending); state != "cancelled" || by != "broker" || audits != 1 {
		t.Fatalf("pending request of the lapsed enrollment: state=%s decided_by=%s audit rows=%d, want cancelled by broker", state, by, audits)
	}

	lapse(sameKey.ID)
	otherKey := in
	otherKey.Thumbprint = "tp-lapse-rotated"
	rotated, err := svc.Create(ctx, cred, otherKey)
	if err != nil {
		t.Fatalf("Create(different key after the lease lapsed) = %v, want a fresh enrollment", err)
	}
	if rotated.Existing || rotated.ID == sameKey.ID {
		t.Fatalf("Create(different key after lapse) = %+v, want a fresh enrollment", rotated)
	}
}

// TestPodEnrollmentRecordsTheVerifiedSubject pins that a pod enrollment stores the service-account
// subject its projected token proved, and a box enrollment stores none.
func TestPodEnrollmentRecordsTheVerifiedSubject(t *testing.T) {
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	ctx := context.Background()
	cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	pod, err := svc.Create(ctx, cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-subject",
		Thumbprint: "tp-subject", Subject: str("system:serviceaccount:spoofed:caller"),
		PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-subject"),
	})
	if err != nil {
		t.Fatalf("Create(pod): %v", err)
	}
	var subject *string
	if err := svc.Store.Pool.QueryRow(ctx, `select subject from enrollments where id=$1`, pod.ID).Scan(&subject); err != nil {
		t.Fatalf("read subject: %v", err)
	}
	if subject == nil || *subject != "system:serviceaccount:legion:worker" {
		t.Fatalf("stored subject = %v, want the token's system:serviceaccount:legion:worker, never the caller's", subject)
	}
}

// TestEachSlotOfAPodIsAnEnrollmentOfItsOwn pins per-slot uniqueness: two roles in one pod, and two
// generations of one role, are distinct live enrollments under one launcher credential and one pod
// UID; a retry in a slot with its own key gets that slot's enrollment back, a different key in a
// live slot is refused, the slotless enrollment of the same pod is a third identity, and revoking
// one slot leaves the others live.
func TestEachSlotOfAPodIsAnEnrollmentOfItsOwn(t *testing.T) {
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	ctx := context.Background()
	cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	const podUID = "pod-uid-roles"
	pod := func(slot, thumbprint string) Enrollment {
		return Enrollment{
			Kind: "pod", RuntimeID: podUID, Slot: slot, Thumbprint: thumbprint,
			PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", podUID),
		}
	}
	ids := map[uuid.UUID]string{}
	for _, in := range []Enrollment{
		pod("implementer-g1", "tp-implementer-g1"),
		pod("reviewer-g1", "tp-reviewer-g1"),
		pod("implementer-g2", "tp-implementer-g2"),
		pod("", "tp-no-slot"),
	} {
		enr, err := svc.Create(ctx, cred, in)
		if err != nil || enr.Existing {
			t.Fatalf("Create(slot %q) = %+v, %v; want a fresh enrollment", in.Slot, enr, err)
		}
		ids[enr.ID] = in.Slot
		got, err := svc.Get(ctx, enr.ID.String())
		if err != nil || got.Slot != in.Slot || got.RuntimeID != podUID {
			t.Fatalf("Get(slot %q) = %+v, %v; want runtime %s in that slot", in.Slot, got, err, podUID)
		}
	}
	if len(ids) != 4 {
		t.Fatalf("enrollments %v, want four distinct ids", ids)
	}
	var implementerG1 uuid.UUID
	for id, slot := range ids {
		if slot == "implementer-g1" {
			implementerG1 = id
		}
	}

	again, err := svc.Create(ctx, cred, pod("implementer-g1", "tp-implementer-g1"))
	if err != nil || !again.Existing || again.ID != implementerG1 {
		t.Fatalf("Create(implementer-g1 retry, same key) = %+v, %v; want Existing %s", again, err, implementerG1)
	}
	if _, err := svc.Create(ctx, cred, pod("implementer-g1", "tp-copied")); !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("Create(implementer-g1, a different key) = %v, want ErrAlreadyEnrolled", err)
	}

	if err := svc.Revoke(ctx, cred, implementerG1.String(), "launcher:"+cred.ID.String()); err != nil {
		t.Fatalf("Revoke(implementer-g1): %v", err)
	}
	for id, slot := range ids {
		_, live, err := svc.Lookup(ctx, id.String())
		if err != nil || live != (id != implementerG1) {
			t.Fatalf("Lookup(slot %q) live = %v, %v after revoking implementer-g1 alone", slot, live, err)
		}
	}
}

// TestASlotIsRefusedOffAPodAndWhenMalformed pins the slot's validation: a slot on a box or host
// enrollment, or one that does not match record.ValidSlot, is ErrInvalidSlot and writes no row;
// and a pod's runtime id stays the pod UID its token proves, never a UID composed with a role.
func TestASlotIsRefusedOffAPodAndWhenMalformed(t *testing.T) {
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	ctx := context.Background()
	operatorCred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	serviceCred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")

	for _, kind := range []string{"box", "host"} {
		_, err := svc.Create(ctx, operatorCred, Enrollment{Kind: kind, RuntimeID: kind + "-slot", Operator: str("ada@example.com"), Thumbprint: "tp-" + kind, Slot: "implementer-g1"})
		if !errors.Is(err, ErrInvalidSlot) {
			t.Fatalf("Create(%s with a slot) = %v, want ErrInvalidSlot", kind, err)
		}
	}
	for _, slot := range []string{"Implementer-g1", "1mplementer", "-implementer", "implementer_g1", "a" + strings.Repeat("b", 63)} {
		_, err := svc.Create(ctx, serviceCred, Enrollment{
			Kind: "pod", RuntimeID: "pod-uid-bad-slot", Thumbprint: "tp-bad-slot", Slot: slot,
			PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-uid-bad-slot"),
		})
		if !errors.Is(err, ErrInvalidSlot) {
			t.Fatalf("Create(pod, slot %q) = %v, want ErrInvalidSlot", slot, err)
		}
	}
	_, err := svc.Create(ctx, serviceCred, Enrollment{
		Kind: "pod", RuntimeID: "pod-uid-composed/implementer", Thumbprint: "tp-composed", Slot: "implementer-g1",
		PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-uid-composed"),
	})
	if !errors.Is(err, ErrPodIdentity) {
		t.Fatalf("Create(pod, runtime id composed with a role) = %v, want ErrPodIdentity", err)
	}
	var n int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from enrollments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("enrollments written by refused creates = %d (%v), want none", n, err)
	}
}
