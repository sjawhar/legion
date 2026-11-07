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
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
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
	// otherLauncherAccount is a second service's launcher login.
	otherLauncherAccount = "system:serviceaccount:other:other-daemon"
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

// newPerPodRig is newPodRig on the schema the release after this one leaves: migration 0029
// keeps 0015's one-credential-per-subject index so a roll serves both broker generations, and
// until the follow-up migration drops it a subject holds one live credential whatever its pod
// (storetest.PerPodLoginSchema, IssueTokenLoginTx). Every row that drives the per-pod login
// takes its rig from here.
func newPerPodRig(t *testing.T) podRig {
	t.Helper()
	r := newPodRig(t)
	storetest.PerPodLoginSchema(t, r.svc.Store)
	return r
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

// serviceOf is the service BROKER_SERVICE_LOGINS gives account, as machine.Service.TokenLogin
// resolves it before issuing the credential.
func (r podRig) serviceOf(account string) string {
	return r.svc.ServiceLogins[account]
}

// loginWithKey is login proving key, so a test can log in twice with one key as a daemon does.
func (r podRig) loginWithKey(t *testing.T, account, podUID string, key loginKey) Credential {
	t.Helper()
	ctx := context.Background()
	expires := time.Now().Add(time.Hour)
	id, err := r.svc.IssueTokenLogin(ctx, TokenLogin{
		Subject: account, Service: r.serviceOf(account), Host: "legion/" + account, PodUID: podUID,
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
// On the schema of the release after this one, which is where the per-pod login starts
// (newPerPodRig).
func TestEveryPodOfAnAccountHoldsItsOwnLoginAtOnce(t *testing.T) {
	r := newPerPodRig(t)
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

// revoked reports whether the enrollment has ended.
func (r podRig) revoked(t *testing.T, enrollmentID string) bool {
	t.Helper()
	var ended bool
	if err := r.svc.Store.Pool.QueryRow(context.Background(),
		`select revoked_at is not null from enrollments where id=$1`, enrollmentID).Scan(&ended); err != nil {
		t.Fatalf("read enrollment %s: %v", enrollmentID, err)
	}
	return ended
}

// TestAReplacedLauncherPodEndsItsPredecessorsEnrollments: a machine login is per pod, so the pod
// that replaces a launcher holds a credential of its own, not its predecessor's. Without this it
// could end none of the enrollments the pods before it made, and the first boot after a deploy
// replaces every launcher at once, leaving every enrollment they made to run to its lease with
// nothing able to revoke it. The new credential ends them at once: same ServiceAccount, same
// service, and a pod of an account BROKER_SERVICES binds to that service.
// Each launcher pod holds a login of its own only on the schema of the release after this one
// (newPerPodRig); until then the replacement takes the account's row over and needs no rule.
func TestAReplacedLauncherPodEndsItsPredecessorsEnrollments(t *testing.T) {
	r := newPerPodRig(t)
	ctx := context.Background()
	first := r.login(t, launcherAccount, "pod-uid-daemon-one")
	worker, err := r.enroll(t, first, workerPodAccount, "pod-uid-worker")
	if err != nil {
		t.Fatalf("Create(a worker pod under the first launcher pod) = %v", err)
	}

	next := r.login(t, launcherAccount, "pod-uid-daemon-two")
	if next.ID == first.ID {
		t.Fatalf("the replacement launcher pod took credential %s over, want one of its own", first.ID)
	}
	if err := r.svc.Revoke(ctx, next, worker.ID.String(), "launcher:"+next.ID.String()); err != nil {
		t.Fatalf("the replacement ends its predecessor's enrollment: %v", err)
	}
	if !r.revoked(t, worker.ID.String()) {
		t.Fatalf("enrollment %s is still live, want it ended at once rather than at its lease", worker.ID)
	}
}

// TestALauncherStillEndsItsOwnEnrollments: the rule above adds to the enrolling credential's own
// revoke and replaces nothing — a launcher ends the pods it enrolled itself, as before.
func TestALauncherStillEndsItsOwnEnrollments(t *testing.T) {
	r := newPodRig(t)
	ctx := context.Background()
	cred := r.login(t, launcherAccount, "pod-uid-daemon")
	worker, err := r.enroll(t, cred, workerPodAccount, "pod-uid-worker-own")
	if err != nil {
		t.Fatalf("Create(a worker pod) = %v", err)
	}
	if err := r.svc.Revoke(ctx, cred, worker.ID.String(), "launcher:"+cred.ID.String()); err != nil {
		t.Fatalf("the launcher ends the pod it enrolled: %v", err)
	}
	if !r.revoked(t, worker.ID.String()) {
		t.Fatalf("enrollment %s is still live, want it ended", worker.ID)
	}
}

// TestASelfEnrolledPodEndsNoOtherPodsEnrollment: the service a self-enrolling account logs in as
// binds no pod account, so one session pod can never end another's enrollment, whichever of them
// logged in last. This is what keeps the replaced-launcher rule to launchers.
func TestASelfEnrolledPodEndsNoOtherPodsEnrollment(t *testing.T) {
	// Two session pods hold two logins at once only on the schema of the release after this
	// one (newPerPodRig).
	r := newPerPodRig(t)
	ctx := context.Background()
	first := r.login(t, selfAccount, "pod-uid-session-one")
	mine, err := r.enroll(t, first, selfAccount, "pod-uid-session-one")
	if err != nil {
		t.Fatalf("Create(the first session pod's own enrollment) = %v", err)
	}
	second := r.login(t, selfAccount, "pod-uid-session-two")
	if err := r.svc.Revoke(ctx, second, mine.ID.String(), "launcher:"+second.ID.String()); !errors.Is(err, ErrNotEnrollingCredential) {
		t.Fatalf("Revoke(another session pod's enrollment) = %v, want ErrNotEnrollingCredential", err)
	}
	if r.revoked(t, mine.ID.String()) {
		t.Fatalf("enrollment %s ended, want it untouched", mine.ID)
	}
}

// TestATokenLoginEndsNoEnrollmentOfAnotherServicesPods: the same ServiceAccount is not enough on
// its own — the pod's account must be one BROKER_SERVICES binds to the login's own service, so a
// login of one service ends nothing of another's pods.
func TestATokenLoginEndsNoEnrollmentOfAnotherServicesPods(t *testing.T) {
	r := newPodRig(t)
	ctx := context.Background()
	const otherService, otherPodAccount = "other-service", "system:serviceaccount:other:other-worker"
	r.svc.ServiceLogins[otherLauncherAccount] = otherService
	r.svc.ServicesByAccount[otherPodAccount] = otherService

	theirs := r.login(t, otherLauncherAccount, "pod-uid-other-daemon")
	pod, err := r.enroll(t, theirs, otherPodAccount, "pod-uid-other-worker")
	if err != nil {
		t.Fatalf("Create(the other service's pod) = %v", err)
	}
	ours := r.login(t, launcherAccount, "pod-uid-daemon")
	if err := r.svc.Revoke(ctx, ours, pod.ID.String(), "launcher:"+ours.ID.String()); !errors.Is(err, ErrNotEnrollingCredential) {
		t.Fatalf("Revoke(another service's pod) = %v, want ErrNotEnrollingCredential", err)
	}
	if r.revoked(t, pod.ID.String()) {
		t.Fatalf("enrollment %s ended, want it untouched", pod.ID)
	}
}

// TestThePreviousBrokersTokenLoginStillRunsOnTheMigratedSchema is the rolling upgrade. The
// broker runs more than one pod and a roll serves the previous generation beside this one
// against one database (docs/site/.../broker/operate.md, "Running more than one pod";
// cmd/broker/multiprocess_test.go), and the previous generation's own upsert names
// `on conflict (subject) where subject is not null and revoked_at is null`, which Postgres
// resolves against 0015's launcher_credentials_live_subject and against neither index migration
// 0029 adds. Dropping that index in 0029 would therefore fail every token login and every
// machine token exchange on the pods still serving, 42P10, for the length of every roll. So
// 0029 drops nothing and this runs the previous statement, word for word, against the schema
// 0029 leaves.
//
// Its counterpart is storetest.PerPodLoginSchema: once the release after this one drops that
// index the statement here stops resolving, which is exactly why the drop waits for the release
// whose pods no longer run it.
func TestThePreviousBrokersTokenLoginStillRunsOnTheMigratedSchema(t *testing.T) {
	r := newPodRig(t)
	key := newLoginKey(t)
	var id string
	// The statement as origin/main's IssueTokenLoginTx writes it, with that release's columns:
	// no pod_uid, and the conflict target it names.
	if err := r.svc.Store.Pool.QueryRow(context.Background(),
		`insert into launcher_credentials (id, operator, service, host, key_thumbprint, public_jwk, record_id, subject, expires_at)
		values ($1, null, $2, $3, $4, $5, null, $6, $7)
		on conflict (subject) where subject is not null and revoked_at is null
		do update set operator=excluded.operator, service=excluded.service, host=excluded.host,
			key_thumbprint=excluded.key_thumbprint, public_jwk=excluded.public_jwk,
			expires_at=excluded.expires_at
		returning id`,
		uuid.New(), podLoginService, "legion/"+selfAccount, key.thumbprint, key.jwk, selfAccount,
		time.Now().Add(time.Hour)).Scan(&id); err != nil {
		t.Fatalf("the previous broker's token-login upsert on the migrated schema: %v; it must still resolve its conflict target, or every roll of the broker fails that generation's logins with 42P10", err)
	}
	if id == "" {
		t.Fatal("the previous broker's upsert wrote no credential")
	}
}

// tokenLoginOf is one token login of account from pod podUID, as machine.Service.TokenLogin
// hands it to IssueTokenLoginTx.
func (r podRig) tokenLoginOf(account, podUID string, key loginKey) TokenLogin {
	expires := time.Now().Add(time.Hour)
	return TokenLogin{
		Subject: account, Service: r.serviceOf(account), Host: "legion/" + account, PodUID: podUID,
		TokenExpires: expires, Thumbprint: key.thumbprint, JWK: key.jwk, Expires: expires,
	}
}

// TestTheNextReleasesIndexDropWaitsForALoginInFlight: IssueTokenLoginTx reads which schema it is
// on and then names a conflict target for that schema, and the two reach the database as two
// statements. The release after this one drops launcher_credentials_live_subject while this one
// still serves, so without a lock of its own a login reads the schema, loses the index to that
// drop, and then names a conflict target the schema no longer has: 42P10, a refused login. The
// login takes ROW EXCLUSIVE on launcher_credentials before it reads and holds it to commit, so
// `drop index` — ACCESS EXCLUSIVE — waits for it.
//
// Winning that interleaving by chance is not practical, so this drives it through
// Service.testSchemaReadHook, the unexported seam that runs exactly where the drop would land
// (documented on the struct, as testConflictHook is). The hook starts the drop on a connection
// of its own and returns once that drop is either waiting on the login's lock or finished, so
// the login's own insert then runs with whatever schema the drop left. With the lock the drop
// waits and the login succeeds; without it the drop finishes first and the login is refused.
func TestTheNextReleasesIndexDropWaitsForALoginInFlight(t *testing.T) {
	r := newPodRig(t)
	ctx := context.Background()
	pool := r.svc.Store.Pool
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	dropped := make(chan error, 1)
	var waited bool
	r.svc.testSchemaReadHook = func() {
		go func() {
			_, err := pool.Exec(context.Background(), `drop index `+legacySubjectIndex)
			dropped <- err
		}()
		pid := int32(tx.Conn().PgConn().PID())
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			select {
			case err := <-dropped:
				dropped <- err // the drop did not wait; the login below meets the schema it left
				return
			default:
			}
			var waiters int
			if err := pool.QueryRow(context.Background(),
				`select count(*) from pg_stat_activity where wait_event_type = 'Lock' and $1 = any(pg_blocking_pids(pid))`,
				pid).Scan(&waiters); err != nil {
				t.Errorf("read the backends waiting behind the login: %v", err)
				return
			}
			if waiters > 0 {
				waited = true
				return
			}
		}
		t.Error("the drop neither waited for the login nor finished within 30s")
	}

	if _, err := r.svc.IssueTokenLoginTx(ctx, tx, r.tokenLoginOf(selfAccount, "pod-uid-in-flight", newLoginKey(t))); err != nil {
		t.Fatalf("the login whose schema read the next release's index drop raced: %v; the drop must wait for the login's lock, or the login names a conflict target the schema no longer has", err)
	}
	if !waited {
		t.Fatal("the drop ran while the login was in flight; it must wait on the login's lock")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the login: %v", err)
	}

	select {
	case err := <-dropped:
		if err != nil {
			t.Fatalf("the drop once the login committed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the drop did not finish within 30s of the login committing; it must wait for a login in flight, never for the next one")
	}
}

// TestTwoLoginsInFlightDoNotWaitForEachOther: the lock above is ROW EXCLUSIVE, which is the lock
// the upsert takes anyway and does not conflict with itself, so it costs ordinary logins
// nothing. Two logins of two subjects, each in a transaction of its own and neither committed,
// both finish; a lock that serialized them would leave the second waiting for the first.
func TestTwoLoginsInFlightDoNotWaitForEachOther(t *testing.T) {
	r := newPodRig(t)
	ctx := context.Background()
	first, err := r.svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	if _, err := r.svc.IssueTokenLoginTx(ctx, first, r.tokenLoginOf(selfAccount, "pod-uid-first", newLoginKey(t))); err != nil {
		t.Fatalf("the first login: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		second, err := r.svc.Store.Pool.Begin(context.Background())
		if err != nil {
			done <- err
			return
		}
		defer second.Rollback(context.Background())
		_, err = r.svc.IssueTokenLoginTx(context.Background(), second, r.tokenLoginOf(launcherAccount, "pod-uid-second", newLoginKey(t)))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the second login while the first is open: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the second login did not finish while the first was open: the lock serializes logins, which it must not")
	}
}
