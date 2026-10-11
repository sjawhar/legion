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

// TestAPodEnrollmentRenewsWithoutItsTokenUntilItFirstRenewsWithIt is the ratchet on one row: Create
// leaves pod_token_renewed_at null, so a new pod enrollment's renewal without a token extends the
// lease and reports it renewed without one (which the API logs), as does one migration 0014 found.
// Its first renewal carrying the pod's token sets the column, and from then on a renewal without
// one is ErrPodTokenRequired and leaves the lease where it was.
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

// TestAMachineSessionRenewsWithoutAPodTokenAndRefusesOne: a machine session renews on its key's
// proof alone, as before. A pod token on its renewal is POD_IDENTITY_MISMATCH rather than silently
// accepted: an invalid token, a token on a broker without a pod verifier, and a valid token for
// the wrong enrollment all have one refusal.
func TestAMachineSessionRenewsWithoutAPodTokenAndRefusesOne(t *testing.T) {
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	ctx := context.Background()
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	box, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "box-renew", Thumbprint: "tp-box-renew"})
	if err != nil {
		t.Fatal(err)
	}
	renewal, err := svc.Renew(ctx, box.ID.String(), "")
	if err != nil || renewal.WithoutPodToken {
		t.Fatalf("Renew(machine session) = %+v, %v; want a plain renewal", renewal, err)
	}
	if _, err := svc.Renew(ctx, box.ID.String(), mintPodToken(t, issuer, key, renewAccount, "box-renew")); !errors.Is(err, ErrPodIdentity) {
		t.Fatalf("Renew(machine session, with a pod token) = %v, want ErrPodIdentity", err)
	}
}

// podUIDRig enrolls pod rows under launcher credentials of their own, all against one verifier,
// so a test can put several rows on one pod UID: a pod's role slots under the daemon's login, or a
// second login's copy.
type podUIDRig struct {
	svc    *Service
	issuer *oidctest.Issuer
	key    *oidctest.Key
}

func newPodUIDRig(t *testing.T) podUIDRig {
	t.Helper()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	return podUIDRig{svc: svc, issuer: issuer, key: key}
}

// login mints a service launcher credential, as a second service login approved by anyone would.
func (r podUIDRig) login(t *testing.T, host string) Credential {
	t.Helper()
	return mintCredential(t, r.svc, nil, str("legion-daemon"), host)
}

// enroll makes one pod row naming podUID in slot, under cred, with the pod's token as Create needs.
func (r podUIDRig) enroll(t *testing.T, cred Credential, podUID, slot string) string {
	t.Helper()
	enr, err := r.svc.Create(context.Background(), cred, Enrollment{
		Kind: "pod", RuntimeID: podUID, Thumbprint: "tp-" + cred.Host + "-" + podUID + "-" + slot, Slot: slot,
		PodToken: r.token(t, podUID),
	})
	if err != nil {
		t.Fatalf("Create(pod %s slot %s): %v", podUID, slot, err)
	}
	return enr.ID.String()
}

func (r podUIDRig) token(t *testing.T, podUID string) string {
	t.Helper()
	return mintPodToken(t, r.issuer, r.key, renewAccount, podUID)
}

// TestEveryRoleOfANewImagePodKeepsRenewingOnceOneRoleMarksItsUID: an issue pod holds one enrollment
// per role slot, all naming its one pod UID. Once the first role renews with its token the UID is
// marked, and every role, each renewing with the same pod's token as a new-image renewer does,
// keeps renewing, while a token-less renewal of any of them is now refused.
func TestEveryRoleOfANewImagePodKeepsRenewingOnceOneRoleMarksItsUID(t *testing.T) {
	r := newPodUIDRig(t)
	ctx := context.Background()
	daemon := r.login(t, "cluster")
	const podUID = "pod-uid-issue"
	slots := []string{"architect-g1", "implementer-g1", "reviewer-g1"}
	ids := map[string]string{}
	for _, slot := range slots {
		ids[slot] = r.enroll(t, daemon, podUID, slot)
	}
	if _, err := r.svc.Renew(ctx, ids["architect-g1"], r.token(t, podUID)); err != nil {
		t.Fatalf("first role's token renewal: %v", err)
	}
	for round := range 2 {
		for _, slot := range slots {
			if _, err := r.svc.Renew(ctx, ids[slot], r.token(t, podUID)); err != nil {
				t.Fatalf("round %d: %s's token renewal after the UID was marked: %v", round, slot, err)
			}
		}
	}
	if _, err := r.svc.Renew(ctx, ids["reviewer-g1"], ""); !errors.Is(err, ErrPodTokenRequired) {
		t.Fatalf("a role's token-less renewal after the UID was marked = %v, want ErrPodTokenRequired", err)
	}
}

// TestACopiedTokensRowIsRefusedTokenlessOnceTheRealPodRenewsWithItsToken is the copied-token
// attack in both orders. An off-cluster key enrolled under another service login with a token
// copied out of the pod gets a row of its own (rows are unique per login, pod UID and slot). That
// row is refused token-less once the real pod has renewed with its token, whether the copy was
// enrolled after that renewal or before it; before it, the copy renews token-less, the window that
// stays open.
func TestACopiedTokensRowIsRefusedTokenlessOnceTheRealPodRenewsWithItsToken(t *testing.T) {
	t.Run("the pod renews with its token, then the copy enrolls", func(t *testing.T) {
		r := newPodUIDRig(t)
		ctx := context.Background()
		const podUID = "pod-uid-copied-after"
		real := r.enroll(t, r.login(t, "cluster"), podUID, "implementer-g1")
		if _, err := r.svc.Renew(ctx, real, r.token(t, podUID)); err != nil {
			t.Fatalf("the real pod's token renewal: %v", err)
		}
		copied := r.enroll(t, r.login(t, "attacker"), podUID, "implementer-g1")
		if _, err := r.svc.Renew(ctx, copied, ""); !errors.Is(err, ErrPodTokenRequired) {
			t.Fatalf("the copy's token-less renewal = %v, want ErrPodTokenRequired", err)
		}
	})
	t.Run("the copy enrolls, then the pod renews with its token", func(t *testing.T) {
		r := newPodUIDRig(t)
		ctx := context.Background()
		const podUID = "pod-uid-copied-before"
		real := r.enroll(t, r.login(t, "cluster"), podUID, "implementer-g1")
		copied := r.enroll(t, r.login(t, "attacker"), podUID, "implementer-g1")
		if renewal, err := r.svc.Renew(ctx, copied, ""); err != nil || !renewal.WithoutPodToken {
			t.Fatalf("the copy's token-less renewal before the pod's first token renewal = %+v, %v; want it renewed, reported token-less", renewal, err)
		}
		if _, err := r.svc.Renew(ctx, real, r.token(t, podUID)); err != nil {
			t.Fatalf("the real pod's token renewal: %v", err)
		}
		if _, err := r.svc.Renew(ctx, copied, ""); !errors.Is(err, ErrPodTokenRequired) {
			t.Fatalf("the copy's token-less renewal after the pod's token renewal = %v, want ErrPodTokenRequired", err)
		}
	})
}

// TestAnOldImagePodKeepsRenewingTokenless: a pod whose renewer never sends a token marks nothing,
// so its roles keep renewing token-less, each renewal reported so the API logs it.
func TestAnOldImagePodKeepsRenewingTokenless(t *testing.T) {
	r := newPodUIDRig(t)
	ctx := context.Background()
	daemon := r.login(t, "cluster")
	const podUID = "pod-uid-old-image"
	ids := []string{r.enroll(t, daemon, podUID, "architect-g1"), r.enroll(t, daemon, podUID, "implementer-g1")}
	for round := range 3 {
		for _, id := range ids {
			renewal, err := r.svc.Renew(ctx, id, "")
			if err != nil || !renewal.WithoutPodToken {
				t.Fatalf("round %d: old-image renewal of %s = %+v, %v; want it renewed, reported token-less", round, id, renewal, err)
			}
		}
	}
}

// TestAMarkedUIDOutlivesItsRowsButTouchesNoOtherPod is the ratchet's liveness. It reads any marked
// row naming the UID, live or ended. A pod UID is the API server's uid of one pod object and is
// never given to another pod, so ended rows of a gone pod refuse nothing of an unrelated later pod,
// which has a UID of its own. They do keep refusing a copied token's token-less row naming the gone
// pod's UID, whether the pod is between role generations (every row revoked, as the daemon revokes
// each generation it lets go) or gone for good (every row revoked or lapsed): reading only live rows
// would let that copy renew token-less in every such gap. A new generation of the same pod renews
// with its token, as a current-image renewer does, and is never refused.
func TestAMarkedUIDOutlivesItsRowsButTouchesNoOtherPod(t *testing.T) {
	for _, ends := range []string{"revoked", "lapsed"} {
		t.Run(ends, func(t *testing.T) {
			r := newPodUIDRig(t)
			ctx := context.Background()
			daemon := r.login(t, "cluster")
			podUID := "pod-uid-ended-" + ends
			first := r.enroll(t, daemon, podUID, "implementer-g1")
			if _, err := r.svc.Renew(ctx, first, r.token(t, podUID)); err != nil {
				t.Fatalf("the pod's token renewal: %v", err)
			}
			end := `update enrollments set revoked_at = now() where id = $1`
			if ends == "lapsed" {
				end = `update enrollments set lease_expires_at = now() - interval '1 second' where id = $1`
			}
			if _, err := r.svc.Store.Pool.Exec(ctx, end, first); err != nil {
				t.Fatal(err)
			}

			copied := r.enroll(t, r.login(t, "attacker"), podUID, "implementer-g1")
			if _, err := r.svc.Renew(ctx, copied, ""); !errors.Is(err, ErrPodTokenRequired) {
				t.Fatalf("a copy's token-less renewal once the marked row is %s = %v, want ErrPodTokenRequired", ends, err)
			}
			next := r.enroll(t, daemon, podUID, "implementer-g2")
			if renewal, err := r.svc.Renew(ctx, next, r.token(t, podUID)); err != nil || renewal.WithoutPodToken {
				t.Fatalf("the pod's next generation's token renewal = %+v, %v; want a plain renewal", renewal, err)
			}
			unrelated := r.enroll(t, daemon, "pod-uid-unrelated", "implementer-g1")
			if renewal, err := r.svc.Renew(ctx, unrelated, ""); err != nil || !renewal.WithoutPodToken {
				t.Fatalf("an unrelated pod's token-less renewal beside a marked UID = %+v, %v; want it renewed, reported token-less", renewal, err)
			}
		})
	}
}
