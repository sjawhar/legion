// packages/envoy/internal/broker/helper/lapse_test.go
//go:build linux

package helper

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// lapseRig is a logged-in rig whose one session (this test process) enrolls and then has its
// first renew refused (the fake's refuseRenewNext: 401 LEASE_EXPIRED, whenever that renew comes).
// Nothing here races a lease's wall clock. The first enrollment's lease is short (the fake's
// nextLease) only so that its renew comes soon (at MinRenew, 50 ms), and every later enrollment
// gets the fake's 900 s default, so the re-enrollment a test waits for can never lapse again
// underneath it. The fake then answers failRevokes revokes of the lapsed id with a 503.
//
// lapseRig returns once the first of those revokes has been answered. It returns the rig, the
// enrollment id the session had before the lapse, and the state path its records are saved to.
func lapseRig(t *testing.T, failRevokes int) (*rig, string, string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "sessions.json")
	r := startRig(t, state)
	r.fake.mu.Lock()
	r.fake.nextLease = 150 * time.Millisecond
	r.fake.refuseRenewNext = 1
	r.fake.revokeFailFirst = failRevokes
	r.fake.mu.Unlock()
	reg := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register: %+v", reg)
	}
	waitFor(t, func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.revokeAttempts >= 1
	})
	return r, reg.EnrollmentID, state
}

func revoked(r *rig, id string) bool {
	_, deletes, _ := r.fake.snapshot()
	return slices.Contains(deletes, id)
}

// endSessionThenReleaseRevokes ends sess while every revoke is failing (reg.Remove, as retire and
// unregister do), waits for the next revoke attempt after that, then clears the fake's revoke
// fault. The fake fails an attempt in the same critical section that counts it, so the attempt the
// helper sees has already failed. That attempt is the handed-off revoke's first, or at most one
// last try of the session's own loop, which then returns at once. In that second case the
// handed-off revoke's first try follows at once, and the 200 ms sleep before the fault is cleared
// makes that try fail too. Either way the id lands on the handed-off revoke's next try, 2 s later,
// although no test asserts which try lands it. The helper fails the test when no attempt comes
// within 5 s: nothing handed the id on.
func endSessionThenReleaseRevokes(t *testing.T, r *rig, reg *Registry, sess *Session) {
	t.Helper()
	r.fake.mu.Lock()
	before := r.fake.revokeAttempts
	r.fake.mu.Unlock()
	reg.Remove(sess)
	if sess.peer != nil {
		sess.peer.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.fake.mu.Lock()
		n := r.fake.revokeAttempts
		r.fake.mu.Unlock()
		if n > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no revoke was attempted after the session ended; nothing handed its id on")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	r.fake.mu.Lock()
	r.fake.revokeFailFirst = 0
	r.fake.mu.Unlock()
}

// testGate returns a gate for the fake's enrollGate or renewGate and the func that opens it, once
// however often it is called. Call it after the rig starts: the open it registers for cleanup then
// runs before the fake's own Close, which waits for any request still held at the gate.
func testGate(t *testing.T) (chan struct{}, func()) {
	gate := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)
	return gate, open
}

// lapseReason is the last error a lapse records for the fake's refused renew (LEASE_EXPIRED).
const lapseReason = "the broker refused this session's renew (LEASE_EXPIRED); enrolling again"

// TestALapsedEnrollmentStopsSigningWhileItsRevokeRetries: once the broker refuses a renew, the
// enrollment is dead, so the session must stop signing with it at once rather than when its
// revoke finally succeeds. While the revoke retries, sign answers NOT_ENROLLED (the helper still
// holds a credential, so the session is enrolling), naming the refused renew as the last attempt,
// and a register reply carries the same reason; the session's record still names the lapsed id,
// so a helper restart meanwhile revokes it. Once the revoke lands the session enrolls afresh and
// signs again.
func TestALapsedEnrollmentStopsSigningWhileItsRevokeRetries(t *testing.T) {
	r, lapsed, state := lapseRig(t, failAlways)
	sign := Request{Op: "sign", Method: "GET", URL: "https://secrets.test/v1/enrollments/self"}
	want := "this session is not enrolled with the broker yet; last attempt: " + lapseReason
	if resp := r.call(t, sign); resp.OK || resp.Code != CodeNotEnrolled || resp.Error != want {
		t.Fatalf("sign while the lapsed enrollment's revoke retries: %+v; want NOT_ENROLLED %q", resp, want)
	}
	if reg := r.call(t, Request{Op: "register"}); !reg.OK || reg.State != "enrolling" || reg.Error != lapseReason {
		t.Fatalf("register while the lapsed enrollment's revoke retries: %+v; want enrolling, %q", reg, lapseReason)
	}
	if revoked(r, lapsed) {
		t.Fatal("the lapsed enrollment was revoked already; this run never reached the retry window")
	}

	// Any save meanwhile (another session registering, one retiring) writes every record; this
	// one must still name the lapsed id.
	r.srv.save()
	recs, err := LoadRecords(state)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(recs, func(rec Record) bool { return rec.PID == os.Getpid() })
	if i < 0 || recs[i].EnrollmentID != lapsed {
		t.Fatalf("the record must keep the lapsed id until its revoke succeeds: %+v, want %q", recs, lapsed)
	}
	r.fake.mu.Lock()
	r.fake.revokeFailFirst = 0
	r.fake.mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if id := r.srv.Registry.Get(os.Getpid()).EnrollmentID(); id != "" && id != lapsed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("once the lapsed id is revoked the session must enroll afresh")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !revoked(r, lapsed) {
		t.Fatal("the lapsed enrollment must be revoked before the session enrolls again")
	}
	if resp := r.call(t, sign); !resp.OK {
		t.Fatalf("sign after the fresh enrollment: %+v", resp)
	}
}

// TestASessionThatEndsWhileItsLapsedRevokeRetriesStillRevokesIt: retire revokes only a live
// enrollment, so a session that ends while its lapsed id's revoke is backing off must hand that
// id to the bounded independent revoke rather than abandon it. Every revoke fails until the
// session has ended and its own retry loop has stopped, so only the handed-off revoke can land
// the id: its second try, 2 s after the first.
func TestASessionThatEndsWhileItsLapsedRevokeRetriesStillRevokesIt(t *testing.T) {
	r, lapsed, _ := lapseRig(t, failAlways)
	sess := r.srv.Registry.Get(os.Getpid())
	endSessionThenReleaseRevokes(t, r, r.srv.Registry, sess)
	deadline := time.Now().Add(10 * time.Second)
	for !revoked(r, lapsed) {
		if time.Now().After(deadline) {
			t.Fatal("the handed-off revoke never revoked the lapsed id")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRegisterWaitWaitsForTheReEnrollmentAfterALapse: a lapse reopens the session's ready
// channel, so `register --wait` from inside a lapsed session waits for its re-enrollment instead
// of returning at once with the session still enrolling. The re-enrollment is held at the fake's
// enroll gate until the register call is blocked in registerReply's select on that channel, so the
// test cannot pass by arriving after the session enrolled again: a register that does not wait
// answers while the gate is still shut, and the test fails.
func TestRegisterWaitWaitsForTheReEnrollmentAfterALapse(t *testing.T) {
	r, lapsed, _ := lapseRig(t, failAlways)
	gate, open := testGate(t)
	r.fake.mu.Lock()
	r.fake.enrollGate = gate
	r.fake.revokeFailFirst = 0 // the lapsed id's next revoke lands; the re-enrollment then waits at the gate
	r.fake.mu.Unlock()
	replies := registerWaitBlocked(t, r)
	open()
	freshEnrollment(t, replies, lapsed)
}

// TestASecondLapseWaitsAgainAndRevokesBothLapsedIDs: a session whose re-enrollment lapses too goes
// through the whole cycle a second time. A lapse needs a refused renew, so an enrollment comes
// before it, and that enrollment closes the ready channel a `register --wait` from the first
// lapse holds: the second lapse can only follow that wait's answer. The fake holds the second
// enrollment's first renew at its renew gate until the answer is in, so the second lapse cannot
// race the reply, and holds the third enrollment at its enroll gate until a second `register
// --wait` is waiting. Each wait answers only once the session is enrolled again, each lapsed id is
// revoked before the next enrollment (the fake answers an unrevoked id's key with that same id),
// and the session ends under a third id with no lapsed id left on it or on its record.
func TestASecondLapseWaitsAgainAndRevokesBothLapsedIDs(t *testing.T) {
	r, first, state := lapseRig(t, failAlways)
	renewGate, releaseRenew := testGate(t)
	r.fake.mu.Lock()
	r.fake.nextLease = 150 * time.Millisecond // the second enrollment renews at MinRenew,
	r.fake.refuseRenewNext = 1                // and that renew is refused whenever the gate lets it through
	r.fake.renewGate = renewGate
	r.fake.mu.Unlock()
	replies := registerWaitBlocked(t, r)
	r.fake.mu.Lock()
	r.fake.revokeFailFirst = 0 // the first lapsed id's revoke lands, and the session enrolls again
	r.fake.mu.Unlock()
	second := freshEnrollment(t, replies, first)

	enrollGate, releaseEnroll := testGate(t)
	r.fake.mu.Lock()
	r.fake.enrollGate = enrollGate // the third enrollment gets the fake's 900 s default lease
	r.fake.mu.Unlock()
	releaseRenew() // the second lapse
	deadline := time.Now().Add(10 * time.Second)
	for !revoked(r, second) {
		if time.Now().After(deadline) {
			t.Fatal("the second lapsed id was never revoked")
		}
		time.Sleep(20 * time.Millisecond)
	}
	replies = registerWaitBlocked(t, r)
	releaseEnroll()
	third := freshEnrollment(t, replies, first, second)

	if !revoked(r, first) {
		t.Fatalf("the first lapsed id %q was never revoked", first)
	}
	r.fake.mu.Lock()
	live := slices.Collect(maps.Keys(r.fake.enrolled))
	r.fake.mu.Unlock()
	if !slices.Equal(live, []string{third}) {
		t.Fatalf("live enrollments at the fake: %q; want only the third, %q", live, third)
	}
	if sess := r.srv.Registry.Get(os.Getpid()); sess.EnrollmentID() != third || sess.lapsed() != "" {
		t.Fatalf("session enrollment %q, lapsed id %q; want %q and none", sess.EnrollmentID(), sess.lapsed(), third)
	}
	waitFor(t, func() bool { return recordedID(t, state, os.Getpid()) == third })
}

// TestARenewThatFailsTransientlyKeepsTheLease: a renew the broker cannot answer (503) is an outage,
// not a refusal, so the session keeps its enrollment and its lease and renews again: it never
// lapses, revokes or enrolls afresh. The fake holds the first renew at one gate until the test has
// moved the enrollment's stored expiry a minute out, past every deadline in the test, so no lease
// passes under the test however slow the host is, and holds the retry at a second gate, so the
// session is read after it has handled the 503 and before the retry succeeds.
func TestARenewThatFailsTransientlyKeepsTheLease(t *testing.T) {
	r := startRig(t, "")
	first, releaseFirst := testGate(t)
	r.fake.mu.Lock()
	r.fake.nextLease = 150 * time.Millisecond // the first renew comes at MinRenew
	r.fake.renewFail = 1
	r.fake.renewGate = first
	r.fake.mu.Unlock()
	reg := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register: %+v", reg)
	}
	id := reg.EnrollmentID
	renews := func() int {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.renewAttempts
	}
	waitFor(t, func() bool { return renews() == 1 }) // the first renew is waiting at its gate
	retry, releaseRetry := testGate(t)
	r.fake.mu.Lock()
	stored := time.Now().Add(time.Minute)
	r.fake.leaseExpiry[id] = stored
	r.fake.renewGate = retry
	r.fake.mu.Unlock()
	releaseFirst() // answered 503

	sess := r.srv.Registry.Get(os.Getpid())
	kept := func(when string) {
		t.Helper()
		posts, deletes, _ := r.fake.snapshot()
		enrolls := len(posts)
		if sess.State() != "enrolled" || sess.EnrollmentID() != id || sess.lapsed() != "" || len(deletes) != 0 || enrolls != 1 {
			t.Fatalf("%s: session %s under %q (lapsed id %q), revokes %q, %d enrolls; want enrolled under %q, no revoke, one enroll",
				when, sess.State(), sess.EnrollmentID(), sess.lapsed(), deletes, enrolls, id)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for renews() < 2 {
		kept("until the retry arrives")
		if time.Now().After(deadline) {
			t.Fatal("the session never renewed again after a renew that failed with 503")
		}
		time.Sleep(20 * time.Millisecond)
	}
	kept("with the retry waiting")
	if resp := r.call(t, Request{Op: "sign", Method: "GET", URL: "https://secrets.test/v1/enrollments/self"}); !resp.OK {
		t.Fatalf("sign while the retry waits: %+v", resp)
	}
	releaseRetry()
	waitFor(t, func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.renews == 1
	})
	kept("after the retry")
	r.fake.mu.Lock()
	renewed := r.fake.leaseExpiry[id]
	r.fake.mu.Unlock()
	if !renewed.After(stored) {
		t.Fatalf("the retry must renew the lease: expiry %v, want after %v", renewed, stored)
	}
}

// registerWaitBlocked starts a `register --wait` from this process, the rig's one session, and
// returns its answer's channel once the call is blocked in registerReply's select. The call
// answering first fails the test: a session that is not enrolled must not answer a register that
// waits.
func registerWaitBlocked(t *testing.T, r *rig) <-chan registerAnswer {
	t.Helper()
	replies := make(chan registerAnswer, 1)
	go func() {
		resp, err := Call(r.sock, Request{Op: "register", WaitSeconds: 10}, 20*time.Second)
		replies <- registerAnswer{resp, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !blockedInRegisterReply() {
		select {
		case got := <-replies:
			t.Fatalf("register --wait answered while the re-enrollment was still held: %+v (%v); it must wait for the lapsed session to enroll again", got.resp, got.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the register call never started waiting on the session's ready channel")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return replies
}

// registerAnswer is a register call's reply, or the error that ended the call.
type registerAnswer struct {
	resp Response
	err  error
}

// freshEnrollment waits for the answer registerWaitBlocked's call gives once the session enrolls
// again, requires it to name an enrollment none of stale holds, and returns that enrollment id.
func freshEnrollment(t *testing.T, replies <-chan registerAnswer, stale ...string) string {
	t.Helper()
	select {
	case got := <-replies:
		if got.err != nil || !got.resp.OK || got.resp.State != "enrolled" || got.resp.EnrollmentID == "" || slices.Contains(stale, got.resp.EnrollmentID) {
			t.Fatalf("register --wait in a lapsed session: %+v (%v); want a fresh enrollment, none of %q", got.resp, got.err, stale)
		}
		return got.resp.EnrollmentID
	case <-time.After(15 * time.Second):
		t.Fatal("register --wait never answered after the re-enrollment was released")
	}
	return ""
}

// blockedInRegisterReply reports whether some goroutine is blocked in a select inside
// Server.registerReply, which is where a register call waits for its session to enroll. It finds
// that goroutine by name in the goroutine dump, a "[select" header over a ").registerReply("
// frame, so a rename of registerReply must be made here too. A name that no longer matches never
// lets the test pass: TestRegisterWaitWaitsForTheReEnrollmentAfterALapse then fails after about
// 10 s.
func blockedInRegisterReply() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, _, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "[select") && strings.Contains(g, ").registerReply(") {
			return true
		}
	}
	return false
}

// TestNotEnrolledNamesTheLastAttemptOnlyWhenThereIsOne: a session enrolling with no failure yet
// (before its first attempt returns) is told only that it is not enrolled; one whose last attempt
// failed is told why.
func TestNotEnrolledNamesTheLastAttemptOnlyWhenThereIsOne(t *testing.T) {
	r := startRig(t, "")
	sess, _ := newSession(1, 1, "h:1:1", nil)
	if got := r.srv.notEnrolled(sess.snapshot()); got.Code != CodeNotEnrolled || got.Error != "this session is not enrolled with the broker yet" {
		t.Fatalf("no attempt yet: %+v", got)
	}
	sess.setError("broker 503 DATABASE: postgres unreachable")
	if got := r.srv.notEnrolled(sess.snapshot()); got.Error != "this session is not enrolled with the broker yet; last attempt: broker 503 DATABASE: postgres unreachable" {
		t.Fatalf("after a failed attempt: %+v", got)
	}
}

// TestAPriorEnrollmentOfAnotherOperatorDoesNotStrandTheSession is the merge queue's finding 3 on
// #1589: after a helper restart the operator logs back in under a different login, and every
// revoke of the re-pinned session's prior enrollment is refused 403 OPERATOR_MISMATCH. That
// refusal is final, so the helper must stop retrying it and enroll the session afresh; before,
// the session stayed NOT_ENROLLED until it was relaunched.
func TestAPriorEnrollmentOfAnotherOperatorDoesNotStrandTheSession(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	pid := recordPrior(t, state, "enr-other-operator")

	r := newRig(t, state) // Recover re-pins the child; its prior revoke waits for a credential
	r.fake.mu.Lock()
	r.fake.revokeForbidden = map[string]bool{"enr-other-operator": true}
	r.fake.mu.Unlock()
	r.login(t)
	waitFor(t, func() bool {
		sess := r.srv.Registry.Get(pid)
		return sess != nil && sess.EnrollmentID() != ""
	})
	r.fake.mu.Lock()
	attempts := r.fake.revokeAttempts
	r.fake.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("a revoke refused for another operator is final, not retried: %d attempts", attempts)
	}
}

// recordPrior saves a session record at state for a live child process, as a helper that
// enrolled it under id would have left it, and returns the child's pid.
func recordPrior(t *testing.T, state, id string) int {
	t.Helper()
	child := sleeper(t)
	pid := child.Process.Pid
	ticks, err := StartTicks(pid)
	if err != nil {
		t.Fatal(err)
	}
	prior := NewRegistry(state)
	old, _ := newSession(pid, ticks, fmt.Sprintf("testhost:%d:%d", pid, ticks), nil)
	old.setEnrolled(id)
	prior.Add(old)
	if err := prior.Save(); err != nil {
		t.Fatal(err)
	}
	return pid
}

// recordedID is the enrollment id state's record for pid names ("" with no record).
func recordedID(t *testing.T, state string, pid int) string {
	t.Helper()
	recs, err := LoadRecords(state)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(recs, func(rec Record) bool { return rec.PID == pid }); i >= 0 {
		return recs[i].EnrollmentID
	}
	return ""
}

// TestARestartWithNoLoginKeepsThePriorEnrollmentOnDisk: a restart discards the launcher
// credential, so a re-pinned session's prior enrollment cannot be revoked until the next login.
// Until then the saved record must still name it, as a lapsed id; before, Recover's save wrote
// the re-pinned session with no enrollment id at all.
func TestARestartWithNoLoginKeepsThePriorEnrollmentOnDisk(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	pid := recordPrior(t, state, "enr-prior")
	r := newRig(t, state) // Recover re-pins the child and saves; no login
	if r.srv.Registry.Get(pid) == nil {
		t.Fatal("the live process must be re-pinned")
	}
	if got := recordedID(t, state, pid); got != "enr-prior" {
		t.Fatalf("the saved record after a restart with no login names %q, want the prior enrollment enr-prior", got)
	}
}

// TestASecondRestartBeforeTheLoginStillRevokesThePriorEnrollment: the helper restarts, and
// restarts again before anyone logs in. The second start must still find the prior enrollment
// on disk and, once logged in, revoke it before enrolling the session afresh.
func TestASecondRestartBeforeTheLoginStillRevokesThePriorEnrollment(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	pid := recordPrior(t, state, "enr-prior")

	// The first restart: Recover re-pins and saves, and the helper exits before any login.
	first := &Server{Registry: NewRegistry(state), Broker: &Broker{URL: "http://127.0.0.1:1", OperatorFile: operatorFile(t, "sjawhar")},
		Hostname: "testhost", PeerOf: PeerOf, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)), MinRenew: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	first.Recover(ctx)
	cancel() // the helper process ends: its goroutines stop, and nothing revokes or saves again
	if sess := first.Registry.Get(pid); sess != nil {
		first.Registry.Remove(sess)
		if sess.peer != nil {
			sess.peer.Close()
		}
	}

	// The second restart, then the login.
	r := newRig(t, state)
	r.login(t)
	waitFor(t, func() bool {
		sess := r.srv.Registry.Get(pid)
		return sess != nil && sess.EnrollmentID() != ""
	})
	if !revoked(r, "enr-prior") {
		t.Fatal("the prior enrollment must be revoked before the session enrolls again")
	}
	waitFor(t, func() bool {
		got := recordedID(t, state, pid)
		return got != "" && got != "enr-prior"
	})
}
