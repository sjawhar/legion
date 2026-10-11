package enroll

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// podRenewalRig is one live pod enrollment under the service credential, with its lease set a
// minute out so a renewal that extends it shows, and a reader of its stored lease.
type podRenewalRig struct {
	svc    *Service
	issuer *oidctest.Issuer
	key    *oidctest.Key
	id     string
}

const (
	renewPodUID  = "pod-uid-renew"
	renewAccount = "system:serviceaccount:legion:worker"
)

func newPodRenewalRig(t *testing.T) podRenewalRig {
	t.Helper()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	enr, err := svc.Create(context.Background(), cred, Enrollment{
		Kind: "pod", RuntimeID: renewPodUID, Thumbprint: "tp-renew", Slot: "implementer-g1",
		PodToken: mintPodToken(t, issuer, key, renewAccount, renewPodUID),
	})
	if err != nil {
		t.Fatalf("Create(pod): %v", err)
	}
	r := podRenewalRig{svc: svc, issuer: issuer, key: key, id: enr.ID.String()}
	r.setLease(t, time.Minute)
	return r
}

func (r podRenewalRig) setLease(t *testing.T, in time.Duration) {
	t.Helper()
	if _, err := r.svc.Store.Pool.Exec(context.Background(), `update enrollments set lease_expires_at=now()+$2::interval where id=$1`, r.id, in.String()); err != nil {
		t.Fatal(err)
	}
}

// tokenRenewedAt reads the enrollment's pod_token_renewed_at: nil until a renewal carries the token.
func (r podRenewalRig) tokenRenewedAt(t *testing.T) *time.Time {
	t.Helper()
	var at *time.Time
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select pod_token_renewed_at from enrollments where id=$1`, r.id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func (r podRenewalRig) lease(t *testing.T) time.Time {
	t.Helper()
	var lease time.Time
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select lease_expires_at from enrollments where id=$1`, r.id).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	return lease
}

// TestARenewalCarryingThePodsCurrentTokenExtendsTheLease: a pod enrollment's renewal that carries a
// projected token for the pod UID and service account the enrollment recorded extends the lease by
// a full Lease.
func TestARenewalCarryingThePodsCurrentTokenExtendsTheLease(t *testing.T) {
	r := newPodRenewalRig(t)
	renewal, err := r.svc.Renew(context.Background(), r.id, mintPodToken(t, r.issuer, r.key, renewAccount, renewPodUID))
	if err != nil {
		t.Fatalf("Renew(the pod's current token): %v", err)
	}
	if renewal.WithoutPodToken {
		t.Fatal("a renewal carrying the pod's token reports it carried none")
	}
	if lease := r.lease(t); lease.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("lease after the renewal = %v (answered %v), want about an hour out", lease, renewal.LeaseExpires)
	}
}

// TestARenewalCarryingAnotherPodsOrAccountsTokenIsRefused: a token for another pod UID, for another
// service account, one no trusted issuer signed, or one bound to no pod is ErrPodIdentity, and one
// past its expiry is ErrPodTokenExpired naming that expiry (as Create answers it), and the lease is
// left where it was: a renewal carrying a token never renews an enrollment the token does not name,
// and a token from another pod never renews this one.
func TestARenewalCarryingAnotherPodsOrAccountsTokenIsRefused(t *testing.T) {
	r := newPodRenewalRig(t)
	expiry := time.Now().Add(-time.Hour).Truncate(time.Second)
	expired := r.issuer.Claims(renewAccount, testAudience)
	expired["iat"], expired["exp"] = expiry.Add(-time.Hour).Unix(), expiry.Unix()
	expired["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": renewPodUID}}
	stranger := oidctest.New(t)
	strangerKey := stranger.PublishKey(t, "stranger-key")
	before := r.lease(t)
	for name, token := range map[string]string{
		"another pod's token":              mintPodToken(t, r.issuer, r.key, renewAccount, "pod-uid-other"),
		"another service account's token":  mintPodToken(t, r.issuer, r.key, "system:serviceaccount:legion:other", renewPodUID),
		"a token no trusted issuer signed": mintPodToken(t, stranger, strangerKey, renewAccount, renewPodUID),
		"a token bound to no pod":          r.issuer.Mint(t, r.key, r.issuer.Claims(renewAccount, testAudience)),
		"not a token at all":               "not-a-jwt",
	} {
		if _, err := r.svc.Renew(context.Background(), r.id, token); !errors.Is(err, ErrPodIdentity) {
			t.Fatalf("Renew(%s) = %v, want ErrPodIdentity", name, err)
		}
		if lease := r.lease(t); !lease.Equal(before) {
			t.Fatalf("lease after the refused renewal with %s = %v, want it left at %v", name, lease, before)
		}
	}
	_, err := r.svc.Renew(context.Background(), r.id, r.issuer.Mint(t, r.key, expired))
	if !errors.Is(err, ErrPodTokenExpired) || errors.Is(err, ErrPodIdentity) {
		t.Fatalf("Renew(a token past its expiry) = %v, want ErrPodTokenExpired and not ErrPodIdentity", err)
	}
	if want := expiry.UTC().Format(time.RFC3339); !strings.Contains(err.Error(), want) {
		t.Fatalf("Renew(a token past its expiry) = %q, want it to name the expiry %s", err, want)
	}
	if lease := r.lease(t); !lease.Equal(before) {
		t.Fatalf("lease after the refused renewal with an expired token = %v, want it left at %v", lease, before)
	}
}

// TestAPodEnrollmentRenewsWithoutItsTokenUntilItFirstRenewsWithIt is the ratchet: Create leaves
// pod_token_renewed_at null, so a new pod enrollment's renewal without a token extends the lease and
// reports it renewed without one (which the API logs), as does one migration 0012 found. Its first
// renewal carrying the pod's token sets the column, and from then on a renewal without one is
// ErrPodTokenRequired and leaves the lease where it was.
func TestAPodEnrollmentRenewsWithoutItsTokenUntilItFirstRenewsWithIt(t *testing.T) {
	r := newPodRenewalRig(t)
	ctx := context.Background()
	if at := r.tokenRenewedAt(t); at != nil {
		t.Fatalf("pod_token_renewed_at after Create = %v, want null", *at)
	}
	renewal, err := r.svc.Renew(ctx, r.id, "")
	if err != nil {
		t.Fatalf("Renew(no token, a new pod enrollment): %v", err)
	}
	if !renewal.WithoutPodToken || renewal.RuntimeID != renewPodUID || renewal.Slot != "implementer-g1" {
		t.Fatalf("renewal = %+v, want it to report a renewal without the pod's token, naming the pod and slot", renewal)
	}
	if lease := r.lease(t); lease.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("lease after the token-less renewal = %v, want about an hour out", lease)
	}
	if at := r.tokenRenewedAt(t); at != nil {
		t.Fatalf("pod_token_renewed_at after a token-less renewal = %v, want still null", *at)
	}

	if _, err := r.svc.Renew(ctx, r.id, mintPodToken(t, r.issuer, r.key, renewAccount, renewPodUID)); err != nil {
		t.Fatalf("Renew(the pod's token): %v", err)
	}
	if r.tokenRenewedAt(t) == nil {
		t.Fatal("pod_token_renewed_at after a renewal with the token is null, want it set")
	}
	r.setLease(t, time.Minute)
	before := r.lease(t)
	if _, err := r.svc.Renew(ctx, r.id, ""); !errors.Is(err, ErrPodTokenRequired) {
		t.Fatalf("Renew(no token, after a renewal with one) = %v, want ErrPodTokenRequired", err)
	}
	if lease := r.lease(t); !lease.Equal(before) {
		t.Fatalf("lease after the refused token-less renewal = %v, want it left at %v", lease, before)
	}
}

// TestARefusedTokenNeverSetsTheRatchet: a renewal whose token is refused (another pod's) leaves
// pod_token_renewed_at null, so only a token the broker accepted moves an enrollment onto the
// token-required path.
func TestARefusedTokenNeverSetsTheRatchet(t *testing.T) {
	r := newPodRenewalRig(t)
	if _, err := r.svc.Renew(context.Background(), r.id, mintPodToken(t, r.issuer, r.key, renewAccount, "pod-uid-other")); !errors.Is(err, ErrPodIdentity) {
		t.Fatalf("Renew(another pod's token) = %v, want ErrPodIdentity", err)
	}
	if at := r.tokenRenewedAt(t); at != nil {
		t.Fatalf("pod_token_renewed_at after a refused token = %v, want null", *at)
	}
	if _, err := r.svc.Renew(context.Background(), r.id, ""); err != nil {
		t.Fatalf("Renew(no token) after a refused token: %v, want the token-less renewal still accepted", err)
	}
}

// TestABoxRenewsWithoutAPodTokenAndRefusesOne: a box or host session renews on its key's proof
// alone, as before. A pod token on its renewal is POD_IDENTITY_MISMATCH rather than silently
// accepted: an invalid token, a token on a broker without a pod verifier, and a valid token for
// the wrong enrollment all have one refusal.
func TestABoxRenewsWithoutAPodTokenAndRefusesOne(t *testing.T) {
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	ctx := context.Background()
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	box, err := svc.Create(ctx, cred, Enrollment{Kind: "box", RuntimeID: "box-renew", Thumbprint: "tp-box-renew"})
	if err != nil {
		t.Fatal(err)
	}
	renewal, err := svc.Renew(ctx, box.ID.String(), "")
	if err != nil || renewal.WithoutPodToken {
		t.Fatalf("Renew(box) = %+v, %v; want a plain renewal", renewal, err)
	}
	if _, err := svc.Renew(ctx, box.ID.String(), mintPodToken(t, issuer, key, renewAccount, "box-renew")); !errors.Is(err, ErrPodIdentity) {
		t.Fatalf("Renew(box, with a pod token) = %v, want ErrPodIdentity", err)
	}
}
