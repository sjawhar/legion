// packages/envoy/internal/broker/helper/lapse_test.go
//go:build linux

package helper

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// lapseRig is a logged-in rig whose one session (this test process) enrolls on a 60 ms lease and
// then has its renew refused: three failed renews outlast the lease, so the next is refused
// because the lease lapsed (TestRenewRefusedRevokesTheLapsedEnrollmentBeforeReenrolling). The
// fake then answers failRevokes revokes of the lapsed id with a 503.
//
// lapseRig returns once the first of those revokes has been answered. It returns the rig, the
// enrollment id the session had before the lapse, and the state path its records are saved to.
func lapseRig(t *testing.T, failRevokes int) (*rig, string, string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "sessions.json")
	r := startRig(t, state)
	r.fake.mu.Lock()
	r.fake.lease = 60 * time.Millisecond
	r.fake.renewFail = 3
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

// TestALapsedEnrollmentStopsSigningWhileItsRevokeRetries: once the broker refuses a renew, the
// enrollment is dead, so the session must stop signing with it at once rather than when its
// revoke finally succeeds. While the revoke retries, sign answers NOT_ENROLLED (the helper still
// holds a credential, so the session is enrolling) and the session's record still names the
// lapsed id, so a helper restart meanwhile revokes it. Once the revoke lands the session enrolls
// afresh and signs again.
func TestALapsedEnrollmentStopsSigningWhileItsRevokeRetries(t *testing.T) {
	r, lapsed, state := lapseRig(t, failAlways)
	sign := Request{Op: "sign", Method: "GET", URL: "https://secrets.test/v1/enrollments/self"}
	if resp := r.call(t, sign); resp.OK || resp.Code != CodeNotEnrolled {
		t.Fatalf("sign while the lapsed enrollment's revoke retries: %+v; want NOT_ENROLLED", resp)
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
// id to the independent revoke rather than abandon it.
func TestASessionThatEndsWhileItsLapsedRevokeRetriesStillRevokesIt(t *testing.T) {
	r, lapsed, _ := lapseRig(t, 1)
	if revoked(r, lapsed) {
		t.Fatal("the lapsed enrollment was revoked already; this run never reached the retry window")
	}
	sess := r.srv.Registry.Get(os.Getpid())
	r.srv.Registry.Remove(sess)
	if sess.peer != nil {
		sess.peer.Close()
	}
	waitFor(t, func() bool { return revoked(r, lapsed) })
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
