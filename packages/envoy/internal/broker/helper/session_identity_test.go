// packages/envoy/internal/broker/helper/session_identity_test.go
//go:build linux

// A session's broker identity outliving its process (dispatch://INFRA-1167).
//
// A credential request and a grant are bound to the enrollment id alone, and ending an
// enrollment cancels every request still pending under it and revokes its grants (the broker's
// enroll.endEnrollment). Until this change a host session's runtime id was
// <hostname>:<pid>:<start ticks since boot> and its key lived only in the helper's memory, so a
// reboot — both halves of that id are boot-unique, and the recorded process is gone — could only
// ever enrol the session afresh, and the grant a human had approved before the reboot answered
// NOT_YOURS. These tests pin the three parts of the fix: a session named by its launcher, a key
// and an enrollment that survive the helper, and revocation that still happens for a session
// that does not come back.
package helper

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

const testSessionID = "11111111-2222-4333-8444-555555555555"

// seedEnrollment tells the fake broker it already holds id for this key under runtimeID, as the
// real broker does for an enrollment made before the helper restarted. Without it the fake has
// no memory of an enrollment this process did not make, and the idempotent-enroll answer the
// test is about could not happen.
func seedEnrollment(f *fakeBroker, id, thumbprint, runtimeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrolled[id], f.byTP[thumbprint], f.runtimeIDToID[runtimeID] = thumbprint, id, id
	f.leaseExpiry[id] = time.Now().Add(15 * time.Minute)
}

// recordNamed writes the records file a helper would have left for a named session enrolled as
// id: pid and its start ticks (0 and an impossible pid for one whose process is gone), the
// session id, and the key, which is what makes the identity recoverable. It returns the key.
func recordNamed(t *testing.T, state string, pid int, ticks uint64, sessionID, id string) *ecdsa.PrivateKey {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	prior := NewRegistry(state)
	sess, err := newSession(pid, ticks, SessionRuntimeID(sessionID), sessionID, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.setEnrolled(id)
	prior.Add(sess)
	if err := prior.Save(); err != nil {
		t.Fatal(err)
	}
	return key
}

// writeRecords puts recs back at state, for a test that damages one.
func writeRecords(t *testing.T, state string, recs []Record) {
	t.Helper()
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func thumbprintOf(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	tp, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

func (r *rig) revokes(t *testing.T) []string {
	t.Helper()
	r.fake.mu.Lock()
	defer r.fake.mu.Unlock()
	return slices.Clone(r.fake.deletes)
}

// TestANamedSessionEnrolsUnderItsSessionID: `register --session-id` names the session, so the
// runtime id is the same string on any machine, at any pid, after any reboot — which is the one
// thing that lets the broker hand the same enrollment back.
func TestANamedSessionEnrolsUnderItsSessionID(t *testing.T) {
	r := startRig(t, "")
	id := testSessionID
	resp := r.call(t, Request{Op: "register", WaitSeconds: 10, SessionID: &id})
	if !resp.OK || resp.State != "enrolled" {
		t.Fatalf("register --session-id: %+v", resp)
	}
	if resp.RuntimeID != "session-"+testSessionID {
		t.Fatalf("runtime_id %q, want session-%s", resp.RuntimeID, testSessionID)
	}
	sess := r.srv.Registry.Get(os.Getpid())
	if sess == nil || sess.SessionID() != testSessionID {
		t.Fatalf("the registered session does not carry its session id: %+v", sess)
	}
}

// TestAnUnnamedSessionIsUnchanged: a launcher that names no session gets exactly what it got
// before — a pid-derived runtime id, and no key in the records file.
func TestAnUnnamedSessionIsUnchanged(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	r := startRig(t, state)
	resp := r.call(t, Request{Op: "register", WaitSeconds: 10})
	if !resp.OK || resp.State != "enrolled" {
		t.Fatalf("register: %+v", resp)
	}
	if got, want := resp.RuntimeID, "testhost:"+strconv.Itoa(os.Getpid())+":"; len(got) <= len(want) || got[:len(want)] != want {
		t.Fatalf("runtime_id %q, want %s<ticks>", got, want)
	}
	recs, err := LoadRecords(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Key != "" || rec.SessionID != "" {
			t.Fatalf("an unnamed session's record must hold neither a key nor a session id: %+v", rec)
		}
	}
}

// TestARestartKeepsANamedSessionsEnrollment: the helper restarts while the session's process
// lives. The session is re-pinned with the key its record holds, keeps the enrollment it had,
// and nothing is revoked — so the requests it has pending and the grants it has been given are
// still its own. Before this, Recover minted a fresh key and lapsed the old enrollment, which
// cancelled both.
func TestARestartKeepsANamedSessionsEnrollment(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	child := sleeper(t)
	pid := child.Process.Pid
	ticks, err := StartTicks(pid)
	if err != nil {
		t.Fatal(err)
	}
	key := recordNamed(t, state, pid, ticks, testSessionID, "enr-before")

	// Seeded between Recover and the login: the enroll loop cannot reach the broker before the
	// credential lands, so the fake knows this key's enrollment by the time it tries.
	r := newRig(t, state)
	seedEnrollment(r.fake, "enr-before", thumbprintOf(t, key), SessionRuntimeID(testSessionID))
	r.login(t)

	sess := r.srv.Registry.Get(pid)
	if sess == nil {
		t.Fatal("the live process must be re-pinned")
	}
	if sess.identity().Thumbprint != thumbprintOf(t, key) {
		t.Fatal("a re-pinned named session must keep the key its record holds, not a fresh one")
	}
	waitFor(t, func() bool { return sess.EnrollmentID() == "enr-before" })
	if got := r.revokes(t); len(got) != 0 {
		t.Fatalf("a restart revoked %v; a named session's enrollment is not the restart's to end", got)
	}
}

// TestARebootGivesTheResumedSessionItsIdentityBack: the recorded process is gone — every one of
// them is, after a reboot — so the record is parked, and the session resuming under the same id
// registers with the same key under the same runtime id, which is what makes the broker answer
// with the enrollment it already has. Nothing is revoked in between.
func TestARebootGivesTheResumedSessionItsIdentityBack(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	// A pid that cannot be live: recorded with start ticks no live process of it could match.
	gone := sleeper(t)
	gonePID := gone.Process.Pid
	_ = gone.Process.Kill()
	_, _ = gone.Process.Wait()
	key := recordNamed(t, state, gonePID, 1, testSessionID, "enr-before")

	r := startRig(t, state)
	seedEnrollment(r.fake, "enr-before", thumbprintOf(t, key), SessionRuntimeID(testSessionID))
	if _, ok := r.srv.Registry.Dormant(testSessionID); !ok {
		t.Fatal("a named session whose process is gone must be parked, not revoked")
	}
	if got := r.revokes(t); len(got) != 0 {
		t.Fatalf("the reboot revoked %v before the session had a chance to come back", got)
	}

	id := testSessionID
	resp := r.call(t, Request{Op: "register", WaitSeconds: 10, SessionID: &id})
	if !resp.OK || resp.EnrollmentID != "enr-before" {
		t.Fatalf("the resumed session got %+v, want the enrollment it had, enr-before", resp)
	}
	if resp.RuntimeID != SessionRuntimeID(testSessionID) {
		t.Fatalf("runtime_id %q, want %s", resp.RuntimeID, SessionRuntimeID(testSessionID))
	}
	sess := r.srv.Registry.Get(os.Getpid())
	if sess == nil || sess.identity().Thumbprint != thumbprintOf(t, key) {
		t.Fatal("the resumed session must sign with the key its own earlier incarnation used")
	}
	if got := r.revokes(t); len(got) != 0 {
		t.Fatalf("taking the identity back revoked %v", got)
	}
	if _, ok := r.srv.Registry.Dormant(testSessionID); ok {
		t.Fatal("the dormant record must be taken, not left for the sweep to revoke")
	}
}

// TestASessionThatDoesNotComeBackLosesItsGrants: the other half of the bargain. A parked
// identity is not kept indefinitely — DormantGrace after its process went away, its enrollment
// is revoked, which is what cancels its pending requests and ends its grants.
func TestASessionThatDoesNotComeBackLosesItsGrants(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	gone := sleeper(t)
	gonePID := gone.Process.Pid
	_ = gone.Process.Kill()
	_, _ = gone.Process.Wait()
	recordNamed(t, state, gonePID, 1, testSessionID, "enr-before")

	r := startRig(t, state)
	if _, ok := r.srv.Registry.Dormant(testSessionID); !ok {
		t.Fatal("the record must be parked first")
	}
	r.srv.SweepDormant(context.Background()) // inside the grace: nothing yet
	if got := r.revokes(t); len(got) != 0 {
		t.Fatalf("a session inside its grace was revoked: %v", got)
	}

	// The grace is the Server's field, not a package variable: a test writing a global that the
	// Server's own goroutines read is a data race, which `go test -race` caught here.
	r.srv.SetDormantGrace(time.Nanosecond)
	r.srv.SweepDormant(context.Background())
	if got := r.revokes(t); len(got) != 1 || got[0] != "enr-before" {
		t.Fatalf("the sweep revoked %v, want [enr-before]", got)
	}
	if _, ok := r.srv.Registry.Dormant(testSessionID); ok {
		t.Fatal("a swept record must be forgotten")
	}
	recs, err := LoadRecords(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("the swept record is still on disk: %+v", recs)
	}
}

// TestASecondProcessMayNotTakeALiveSessionsIdentity: the identity is handed back to a session
// that has come BACK, never shared with a second process while the first is signing with it.
func TestASecondProcessMayNotTakeALiveSessionsIdentity(t *testing.T) {
	r := startRig(t, "")
	// The session root is a child process, so this test process is NOT inside it: a register
	// from here is a second root asking for the same name, not the session itself asking again.
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	id := testSessionID
	first := r.call(t, Request{Op: "register", WaitSeconds: 10, SessionID: &id})
	r.callerPID.Store(0)
	if !first.OK {
		t.Fatalf("first register: %+v", first)
	}
	resp := r.call(t, Request{Op: "register", SessionID: &id})
	if resp.OK || resp.Code != CodeBadRequest {
		t.Fatalf("a second process taking a live session's identity: %+v, want BAD_REQUEST", resp)
	}
	// And the same refusal for a second root that registered unnamed and then asks for the name.
	unnamed := r.call(t, Request{Op: "register", WaitSeconds: 10})
	if !unnamed.OK {
		t.Fatalf("second root register: %+v", unnamed)
	}
	if resp := r.call(t, Request{Op: "register", SessionID: &id}); resp.OK || resp.Code != CodeBadRequest {
		t.Fatalf("a second root taking the name later: %+v, want BAD_REQUEST", resp)
	}
}

// TestASessionNamedAfterItStartedEnrolsUnderThatName: omp opens its transcript seconds after it
// starts, so a launcher can name only a RESUMED session before the exec. A fresh one registers
// unnamed and is named from inside, which must move it to the session-scoped runtime id — ending
// the pid-derived enrollment it held for those few seconds, before any grant exists to lose.
func TestASessionNamedAfterItStartedEnrolsUnderThatName(t *testing.T) {
	r := startRig(t, "")
	first := r.call(t, Request{Op: "register", WaitSeconds: 10})
	if !first.OK || first.EnrollmentID == "" {
		t.Fatalf("register: %+v", first)
	}
	id := testSessionID
	// No WaitSeconds: naming the session revokes its old enrollment before it enrols under the
	// new name, two broker round trips, and Call's own read deadline is 10 s — a register that
	// waits that long server-side races it and times out under load. Poll instead.
	named := r.call(t, Request{Op: "register", SessionID: &id})
	if !named.OK || named.RuntimeID != SessionRuntimeID(testSessionID) {
		t.Fatalf("naming the session: %+v", named)
	}
	sess := r.srv.Registry.Get(os.Getpid())
	if sess == nil || sess.SessionID() != testSessionID {
		t.Fatalf("the session did not take its name: %+v", sess)
	}
	// The enrollment it held for those few seconds is revoked, never carried over, and the
	// session enrols afresh under its name.
	waitFor(t, func() bool { return slices.Contains(r.revokes(t), first.EnrollmentID) })
	waitFor(t, func() bool {
		cur := sess.EnrollmentID()
		return cur != "" && cur != first.EnrollmentID
	})
	// And a session that is already named is not another caller's to rename.
	otherID := "99999999-2222-4333-8444-555555555555"
	if resp := r.call(t, Request{Op: "register", SessionID: &otherID}); resp.OK || resp.Code != CodeBadRequest {
		t.Fatalf("renaming a named session: %+v, want BAD_REQUEST", resp)
	}
}

// TestAnUnnamedSessionIsStillRevokedWhenItEnds: a session with no id cannot be claimed by
// anything, so its enrollment ends the moment its process does, exactly as before.
func TestAnUnnamedSessionIsStillRevokedWhenItEnds(t *testing.T) {
	r := startRig(t, "")
	child := sleeper(t)
	pid := child.Process.Pid
	r.callerPID.Store(int64(pid))
	resp := r.call(t, Request{Op: "register", WaitSeconds: 10})
	r.callerPID.Store(0)
	if !resp.OK || resp.EnrollmentID == "" {
		t.Fatalf("register: %+v", resp)
	}
	_ = child.Process.Kill()
	_, _ = child.Process.Wait()
	waitFor(t, func() bool { return slices.Contains(r.revokes(t), resp.EnrollmentID) })
}

// TestANamedSessionThatEndsIsKeptForTheGraceThenRevoked: the same end, for a named session, is
// parked — an omp restarted in its pane, a box move and a reboot all look exactly like this —
// and revoked only when the grace passes without it.
func TestANamedSessionThatEndsIsKeptForTheGraceThenRevoked(t *testing.T) {
	r := startRig(t, "")
	child := sleeper(t)
	pid := child.Process.Pid
	r.callerPID.Store(int64(pid))
	id := testSessionID
	resp := r.call(t, Request{Op: "register", WaitSeconds: 10, SessionID: &id})
	r.callerPID.Store(0)
	if !resp.OK || resp.EnrollmentID == "" {
		t.Fatalf("register: %+v", resp)
	}
	_ = child.Process.Kill()
	_, _ = child.Process.Wait()
	waitFor(t, func() bool { _, ok := r.srv.Registry.Dormant(testSessionID); return ok })
	if got := r.revokes(t); len(got) != 0 {
		t.Fatalf("a named session's exit revoked %v straight away", got)
	}
	r.srv.SetDormantGrace(time.Nanosecond)
	r.srv.SweepDormant(context.Background())
	if got := r.revokes(t); !slices.Contains(got, resp.EnrollmentID) {
		t.Fatalf("the sweep revoked %v, want %s among them", got, resp.EnrollmentID)
	}
}

// TestTheRecordsFileIsOwnerOnlyUnderAnOwnerOnlyDirectory: it holds a named session's signing
// key, so nothing but this user may read it or list the directory it is in.
func TestTheRecordsFileIsOwnerOnlyUnderAnOwnerOnlyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	state := filepath.Join(dir, "sessions.json")
	r := startRig(t, state)
	id := testSessionID
	if resp := r.call(t, Request{Op: "register", WaitSeconds: 10, SessionID: &id}); !resp.OK {
		t.Fatalf("register: %+v", resp)
	}
	waitFor(t, func() bool { _, err := os.Stat(state); return err == nil })
	file, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if perm := file.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("the records file is mode %04o; it holds a session key", perm)
	}
	parent, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := parent.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("the records directory is mode %04o; it holds a session key", perm)
	}
	recs, err := LoadRecords(state)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(recs, func(rec Record) bool { return rec.SessionID == testSessionID })
	if i < 0 || recs[i].Key == "" {
		t.Fatalf("a named session's record must carry its key: %+v", recs)
	}
	key, err := recs[i].PrivateKey()
	if err != nil || key == nil {
		t.Fatalf("the recorded key does not read back: %v", err)
	}
}

// TestARecordWhoseKeyWillNotReadStillGivesTheSessionAnIdentity: a records file edited or
// truncated must cost a session its continuity, never its ability to run.
func TestARecordWhoseKeyWillNotReadStillGivesTheSessionAnIdentity(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	gone := sleeper(t)
	gonePID := gone.Process.Pid
	_ = gone.Process.Kill()
	_, _ = gone.Process.Wait()
	recordNamed(t, state, gonePID, 1, testSessionID, "enr-before")
	recs, err := LoadRecords(state)
	if err != nil {
		t.Fatal(err)
	}
	recs[0].Key = "not base64 at all"
	writeRecords(t, state, recs)

	r := startRig(t, state)
	id := testSessionID
	resp := r.call(t, Request{Op: "register", WaitSeconds: 10, SessionID: &id})
	if !resp.OK || resp.State != "enrolled" || resp.EnrollmentID == "" {
		t.Fatalf("a session whose recorded key will not read must still enrol: %+v", resp)
	}
	if resp.EnrollmentID == "enr-before" {
		t.Fatal("it cannot have been handed the old enrollment: it does not hold that key")
	}
}
