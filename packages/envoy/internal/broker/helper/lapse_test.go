// packages/envoy/internal/broker/helper/lapse_test.go
//go:build linux

package helper

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// lapseRig is a logged-in rig whose one session (this test process) enrolls on a 60 ms lease and
// then has its renew refused: three failed renews outlast the lease, so the next is refused
// because the lease lapsed (TestRenewRefusedRevokesTheLapsedEnrollmentBeforeReenrolling). failRevokes
// holds that many revokes of the lapsed id at a 503. It returns once the first revoke has been
// answered, the enrollment id the session had, and the state path its records are saved to.
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
	r, lapsed, state := lapseRig(t, 1<<30)
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
	child := sleeper(t)
	pid := child.Process.Pid
	ticks, err := StartTicks(pid)
	if err != nil {
		t.Fatal(err)
	}
	prior := NewRegistry(state)
	old, _ := newSession(pid, ticks, fmt.Sprintf("testhost:%d:%d", pid, ticks), nil)
	old.setEnrolled("enr-other-operator")
	prior.Add(old)
	if err := prior.Save(); err != nil {
		t.Fatal(err)
	}

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
