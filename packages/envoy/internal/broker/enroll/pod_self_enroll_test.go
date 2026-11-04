// pod_self_enroll_test.go drives the one narrowing a pod's own machine login brings: a credential
// a pod minted with its own projected token enrols that pod and no other pod of its account
// (ErrPodSelfEnrollOnly), while a launcher's login — the Legion daemon's — enrols the pods it
// launches exactly as before.
package enroll

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// selfAccount is an account BROKER_SERVICE_LOGINS lists and BROKER_SERVICES binds for nobody's
	// pods: the pods that run as it hold a login of their own and enrol themselves.
	selfAccount = "system:serviceaccount:legion:k8s-session"
	// launcherAccount is the account a launcher logs in as, and workerPodAccount the account the
	// pods it launches run as. config.Load refuses a configuration where the two are one account,
	// which is what keeps selfEnrolment false on every launcher's enrollment.
	launcherAccount  = "system:serviceaccount:legion:legion-daemon"
	workerPodAccount = "system:serviceaccount:legion:legion-worker"
)

// podRig is a Service with a pod verifier, so a test mints projected tokens and the token logins
// that present them.
type podRig struct {
	svc    *Service
	issuer *oidctest.Issuer
	key    *oidctest.Key
}

func newPodRig(t *testing.T) podRig {
	t.Helper()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	return podRig{svc: svc, issuer: issuer, key: key}
}

// token is account's projected token, bound to pod podUID ("" for a token bound to no pod).
func (r podRig) token(t *testing.T, account, podUID string) string {
	t.Helper()
	if podUID == "" {
		return r.issuer.Mint(t, r.key, r.issuer.Claims(account, testAudience))
	}
	return mintPodToken(t, r.issuer, r.key, account, podUID)
}

// login issues account's launcher credential as its token login does, from a token bound to pod
// podUID ("" for one bound to no pod), and reads it back as Create sees it.
func (r podRig) login(t *testing.T, account, podUID string) Credential {
	t.Helper()
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
	expires := time.Now().Add(time.Hour)
	id, err := r.svc.IssueTokenLogin(ctx, TokenLogin{
		Subject: account, Service: "legion-daemon", Host: "legion/" + account, PodUID: podUID,
		TokenExpires: expires, Thumbprint: thumbprint, JWK: jwk, Expires: expires,
	})
	if err != nil {
		t.Fatalf("IssueTokenLogin(%s, pod %q): %v", account, podUID, err)
	}
	cred, err := r.svc.Credential(ctx, id.String())
	if err != nil {
		t.Fatalf("Credential(%s): %v", id, err)
	}
	return cred
}

// enroll enrols podUID under cred with the token account presents for it.
func (r podRig) enroll(t *testing.T, cred Credential, account, podUID string) (Enrollment, error) {
	t.Helper()
	return r.svc.Create(context.Background(), cred, Enrollment{
		Kind: "pod", RuntimeID: podUID, Thumbprint: "tp-" + podUID,
		PodToken: r.token(t, account, podUID),
	})
}

// TestAPodsOwnLoginEnrolsItsOwnPodAlone: a pod that logs in with its own projected token holds a
// credential bound to that one pod, and it enrols that pod. Another pod UID of the same account,
// whose token it would have to have read out of that pod, is refused ErrPodSelfEnrollOnly — the
// narrowing kind vm already makes, without which one pod holding such a login enrols every pod
// whose token it can read.
func TestAPodsOwnLoginEnrolsItsOwnPodAlone(t *testing.T) {
	r := newPodRig(t)
	cred := r.login(t, selfAccount, "pod-uid-self")
	if cred.Subject == nil || *cred.Subject != selfAccount || cred.PodUID != "pod-uid-self" {
		t.Fatalf("the pod's credential = %+v, want subject %s bound to pod-uid-self", cred, selfAccount)
	}

	enr, err := r.enroll(t, cred, selfAccount, "pod-uid-self")
	if err != nil || enr.Operator != nil || enr.Subject == nil || *enr.Subject != selfAccount {
		t.Fatalf("Create(its own pod) = %+v, %v; want a new enrollment with no operator and subject %s", enr, err, selfAccount)
	}

	other, err := r.enroll(t, cred, selfAccount, "pod-uid-other")
	if !errors.Is(err, ErrPodSelfEnrollOnly) {
		t.Fatalf("Create(another pod of its own account) = %+v, %v; want ErrPodSelfEnrollOnly", other, err)
	}
	if got := r.rows(t, "pod-uid-other"); got != 0 {
		t.Fatalf("enrollments naming pod-uid-other = %d, want none", got)
	}
}

// TestALoginBoundToNoPodEnrolsNoPodOfItsOwnAccount: a token `kubectl create token` mints carries no
// pod, so the credential it issues belongs to no pod and enrols none of its account's pods — the
// hole a self-enrolling account would otherwise leave open to anyone who may mint its tokens.
func TestALoginBoundToNoPodEnrolsNoPodOfItsOwnAccount(t *testing.T) {
	r := newPodRig(t)
	cred := r.login(t, selfAccount, "")
	if cred.PodUID != "" {
		t.Fatalf("a login bound to no pod = %+v, want no pod uid", cred)
	}
	if enr, err := r.enroll(t, cred, selfAccount, "pod-uid-anyone"); !errors.Is(err, ErrPodSelfEnrollOnly) {
		t.Fatalf("Create(a pod of its account) = %+v, %v; want ErrPodSelfEnrollOnly", enr, err)
	}
}

// TestALaunchersLoginStillEnrolsThePodsItLaunches: the Legion daemon logs in as its own account
// from its own pod and enrols pods running as another account, several of them, none of them its
// own pod. BROKER_SERVICE_LOGINS may not name an account BROKER_SERVICES binds for a service's
// pods (config.Load), so a launcher's subject is never a pod token's subject and the narrowing
// above never reaches it.
func TestALaunchersLoginStillEnrolsThePodsItLaunches(t *testing.T) {
	r := newPodRig(t)
	cred := r.login(t, launcherAccount, "pod-uid-daemon")
	for _, podUID := range []string{"pod-uid-worker-1", "pod-uid-worker-2"} {
		enr, err := r.enroll(t, cred, workerPodAccount, podUID)
		if err != nil || enr.Subject == nil || *enr.Subject != workerPodAccount {
			t.Fatalf("Create(worker pod %s) = %+v, %v; want it enrolled with subject %s", podUID, enr, err, workerPodAccount)
		}
	}
}

// TestAServiceLoginAPersonApprovedEnrolsPodsAsBefore: a service's login a person approved through a
// machine-login code carries no subject at all, so it is no pod's own login and enrols the pods it
// launches, whatever account they run as.
func TestAServiceLoginAPersonApprovedEnrolsPodsAsBefore(t *testing.T) {
	r := newPodRig(t)
	cred := mintCredential(t, r.svc, nil, str("legion-daemon"), "legion/legion-daemon")
	for _, account := range []string{workerPodAccount, selfAccount} {
		if enr, err := r.enroll(t, cred, account, "pod-uid-approved-"+account); err != nil {
			t.Fatalf("Create(a %s pod under an approved service login) = %+v, %v; want it enrolled", account, enr, err)
		}
	}
}

// rows counts the enrollments naming podUID, live or not.
func (r podRig) rows(t *testing.T, podUID string) int {
	t.Helper()
	var n int
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select count(*) from enrollments where runtime_id=$1`, podUID).Scan(&n); err != nil {
		t.Fatalf("count enrollments of %s: %v", podUID, err)
	}
	return n
}
