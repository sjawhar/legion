// pod_self_enroll_test.go drives the pods a service's token login may enrol: the pod whose own
// token made it, and the pods of an account BROKER_SERVICES binds to its service — the Legion
// daemon's workers — and nothing else. It also drives the per-pod machine login behind the first
// of those: every pod of an account may hold a login at the same time, each with its own key and
// its own pod.
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
	// podLoginService is the service every login below logs in as.
	podLoginService = "legion-daemon"
	// selfAccount is an account BROKER_SERVICE_LOGINS lists and BROKER_SERVICES binds for no
	// service's pods: the pods that run as it hold a login of their own and enrol themselves.
	selfAccount = "system:serviceaccount:legion:k8s-session"
	// launcherAccount is the account a launcher logs in as, and workerPodAccount the account the
	// pods it launches run as, which BROKER_SERVICES binds to the launcher's service.
	launcherAccount  = "system:serviceaccount:legion:legion-daemon"
	workerPodAccount = "system:serviceaccount:legion:legion-worker"
	// strangerPodAccount is an account no setting binds to anything: no login enrols its pods.
	strangerPodAccount = "system:serviceaccount:other:stranger"
)

// podRig is a Service with a pod verifier and the one BROKER_SERVICES binding of the deployment
// this file models, so a test mints projected tokens and the token logins that present them.
type podRig struct {
	svc    *Service
	issuer *oidctest.Issuer
	key    *oidctest.Key
}

func newPodRig(t *testing.T) podRig {
	t.Helper()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	svc.ServiceLogins = map[string]string{selfAccount: podLoginService, launcherAccount: podLoginService}
	svc.ServicesByAccount = map[string]string{workerPodAccount: podLoginService}
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
// podUID ("" for one bound to no pod), with a fresh key, and reads it back as Create sees it.
func (r podRig) login(t *testing.T, account, podUID string) Credential {
	t.Helper()
	return r.loginWithKey(t, account, podUID, newLoginKey(t))
}

// loginKey is one machine login's key, as its request object proved it.
type loginKey struct {
	thumbprint string
	jwk        []byte
}

func newLoginKey(t *testing.T) loginKey {
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
	return loginKey{thumbprint: thumbprint, jwk: jwk}
}

// loginWithKey is login proving key, so a test can log in twice with one key as a daemon does.
func (r podRig) loginWithKey(t *testing.T, account, podUID string, key loginKey) Credential {
	t.Helper()
	ctx := context.Background()
	expires := time.Now().Add(time.Hour)
	id, err := r.svc.IssueTokenLogin(ctx, TokenLogin{
		Subject: account, Service: podLoginService, Host: "legion/" + account, PodUID: podUID,
		TokenExpires: expires, Thumbprint: key.thumbprint, JWK: key.jwk, Expires: expires,
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
		Kind: "pod", RuntimeID: podUID, Thumbprint: "tp-" + podUID + "-" + cred.ID.String(),
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

// TestEveryPodOfAnAccountHoldsItsOwnLoginAtOnce: several pods run as one self-enrolling account at
// the same time, so each holds a machine login of its own, with its own key and its own pod, and
// one pod's login never takes another's over. Each enrols its own pod and is refused the other's.
func TestEveryPodOfAnAccountHoldsItsOwnLoginAtOnce(t *testing.T) {
	r := newPodRig(t)
	first := r.login(t, selfAccount, "pod-uid-one")
	second := r.login(t, selfAccount, "pod-uid-two")
	if first.ID == second.ID {
		t.Fatalf("the second pod's login took the first's credential %s, want one credential per pod", first.ID)
	}
	if live := r.liveRows(t, selfAccount); live != 2 {
		t.Fatalf("live credentials of %s = %d, want one per pod", selfAccount, live)
	}
	// The first pod's credential still reads back as its own, key and pod unmoved.
	again, err := r.svc.Credential(context.Background(), first.ID.String())
	if err != nil || again.PodUID != "pod-uid-one" {
		t.Fatalf("the first pod's credential after the second logged in = %+v, %v; want it still pod-uid-one's", again, err)
	}
	for _, c := range []struct {
		cred Credential
		own  string
		them string
	}{{first, "pod-uid-one", "pod-uid-two"}, {second, "pod-uid-two", "pod-uid-one"}} {
		if enr, err := r.enroll(t, c.cred, selfAccount, c.own); err != nil {
			t.Fatalf("Create(%s under its own login) = %+v, %v; want it enrolled", c.own, enr, err)
		}
		if enr, err := r.enroll(t, c.cred, selfAccount, c.them); !errors.Is(err, ErrPodSelfEnrollOnly) {
			t.Fatalf("Create(%s under %s's login) = %+v, %v; want ErrPodSelfEnrollOnly", c.them, c.own, enr, err)
		}
	}
}

// TestALoginBoundToNoPodIsItsAccountsOne: a token `kubectl create token` mints carries no pod, so
// every such login of an account is the same credential, as a launcher's is, and it belongs to no
// pod, so it enrols none of its own account's pods.
func TestALoginBoundToNoPodIsItsAccountsOne(t *testing.T) {
	r := newPodRig(t)
	first := r.login(t, selfAccount, "")
	second := r.login(t, selfAccount, "")
	if first.ID != second.ID || second.PodUID != "" {
		t.Fatalf("a second pod-less login = %+v, want the account's one credential %s with no pod", second, first.ID)
	}
	if live := r.liveRows(t, selfAccount); live != 1 {
		t.Fatalf("live credentials of %s = %d, want its one pod-less login", selfAccount, live)
	}
	if enr, err := r.enroll(t, second, selfAccount, "pod-uid-anyone"); !errors.Is(err, ErrPodSelfEnrollOnly) {
		t.Fatalf("Create(a pod of its account) = %+v, %v; want ErrPodSelfEnrollOnly", enr, err)
	}
}

// TestALoginRelogsInFromItsOwnPodUnderTheSameCredential: a daemon that logs in again from the same
// pod, proving the key it already holds, keeps its credential id, so every pod it enrolled stays
// under it. The same holds when it mints a new key, as a restarted process does.
func TestALoginRelogsInFromItsOwnPodUnderTheSameCredential(t *testing.T) {
	r := newPodRig(t)
	key := newLoginKey(t)
	first := r.loginWithKey(t, launcherAccount, "pod-uid-daemon", key)
	if same := r.loginWithKey(t, launcherAccount, "pod-uid-daemon", key); same.ID != first.ID {
		t.Fatalf("the same key logging in again from the same pod = %s, want credential %s", same.ID, first.ID)
	}
	if fresh := r.login(t, launcherAccount, "pod-uid-daemon"); fresh.ID != first.ID {
		t.Fatalf("a new key logging in from the same pod = %s, want credential %s", fresh.ID, first.ID)
	}
	if live := r.liveRows(t, launcherAccount); live != 1 {
		t.Fatalf("live credentials of %s = %d, want the one its pod holds", launcherAccount, live)
	}
}

// TestALaunchersLoginEnrolsThePodsItsServiceLaunches: the Legion daemon logs in as its own account
// from its own pod and enrols pods running as the account BROKER_SERVICES binds to its service,
// several of them, none of them its own pod.
func TestALaunchersLoginEnrolsThePodsItsServiceLaunches(t *testing.T) {
	r := newPodRig(t)
	cred := r.login(t, launcherAccount, "pod-uid-daemon")
	for _, podUID := range []string{"pod-uid-worker-1", "pod-uid-worker-2"} {
		enr, err := r.enroll(t, cred, workerPodAccount, podUID)
		if err != nil || enr.Subject == nil || *enr.Subject != workerPodAccount {
			t.Fatalf("Create(worker pod %s) = %+v, %v; want it enrolled with subject %s", podUID, enr, err, workerPodAccount)
		}
	}
}

// TestATokenLoginEnrolsNoPodOfAnAccountItsServiceDoesNotLaunch: the whole authorization of a token
// login is the deployment's settings, so a pod token it was handed proves whose pod the token is
// and never who may enrol it. A pod of an account BROKER_SERVICES binds to no service, or to
// another service than the login's, is ErrPodNotLaunched — including under a pod's own login,
// which would otherwise reach every account but its own.
func TestATokenLoginEnrolsNoPodOfAnAccountItsServiceDoesNotLaunch(t *testing.T) {
	r := newPodRig(t)
	r.svc.ServicesByAccount["system:serviceaccount:other:another-service-pod"] = "another-service"
	for who, cred := range map[string]Credential{
		"a pod's own login":  r.login(t, selfAccount, "pod-uid-self"),
		"a launcher's login": r.login(t, launcherAccount, "pod-uid-daemon"),
	} {
		for _, account := range []string{strangerPodAccount, "system:serviceaccount:other:another-service-pod"} {
			enr, err := r.enroll(t, cred, account, "pod-uid-"+who+"-"+account)
			if !errors.Is(err, ErrPodNotLaunched) {
				t.Errorf("Create(a %s pod under %s) = %+v, %v; want ErrPodNotLaunched", account, who, enr, err)
			}
		}
	}
}

// TestAServiceLoginAPersonApprovedEnrolsPodsAsBefore: a service's login a person approved through a
// machine-login code carries no subject at all. A person's approval is its authorization, not the
// deployment's settings, so neither rule reaches it and it enrols the pods it launches, whatever
// account they run as.
func TestAServiceLoginAPersonApprovedEnrolsPodsAsBefore(t *testing.T) {
	r := newPodRig(t)
	cred := mintCredential(t, r.svc, nil, str(podLoginService), "legion/legion-daemon")
	for i, account := range []string{workerPodAccount, selfAccount, strangerPodAccount} {
		if enr, err := r.enroll(t, cred, account, "pod-uid-approved-"+string(rune('a'+i))); err != nil {
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

// liveRows counts account's live launcher credentials.
func (r podRig) liveRows(t *testing.T, account string) int {
	t.Helper()
	var n int
	if err := r.svc.Store.Pool.QueryRow(context.Background(),
		`select count(*) from launcher_credentials where subject=$1 and revoked_at is null`, account).Scan(&n); err != nil {
		t.Fatalf("count live credentials of %s: %v", account, err)
	}
	return n
}
