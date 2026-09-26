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
	if enr.ID.String() == "" || enr.Existing {
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

	if err := svc.Revoke(ctx, enr.ID.String(), "sjawhar"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if _, live, err := svc.Lookup(ctx, enr.ID.String()); err != nil || live {
		t.Fatalf("Lookup(after revoke) = live=%v err=%v, want live=false, err=nil", live, err)
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
