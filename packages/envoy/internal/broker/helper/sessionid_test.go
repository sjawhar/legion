// packages/envoy/internal/broker/helper/sessionid_test.go
//go:build linux

// The session identity a launcher names at register: what the helper records, what it sends the
// broker, what it keeps across a restart, and what it refuses. A session whose launcher names
// none is covered beside every case, since that is every session enrolled before this field
// existed and every session whose launcher does not know its id.
package helper

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testSessionID = "01a13dca-e979-7264-af32-f654ef57a6f5"

// recordedSessionID is the session id state's record for pid names ("" with no record, and ""
// for a record that names none).
func recordedSessionID(t *testing.T, state string, pid int) string {
	t.Helper()
	recs, err := LoadRecords(state)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(recs, func(rec Record) bool { return rec.PID == pid }); i >= 0 {
		return recs[i].SessionID
	}
	return ""
}

// TestRegisterWithASessionIDEnrollsUnderItAndRecordsIt is the whole path for a launcher that
// knows its session: the reply, the `sessions` list, the saved record and the enrollment the
// broker receives all name the one id, which is the field Dispatch's approval page reads as the
// session that enrolled.
func TestRegisterWithASessionIDEnrollsUnderItAndRecordsIt(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	r := startRig(t, state)
	resp := r.call(t, Request{Op: "register", WaitSeconds: 5, SessionID: new(testSessionID)})
	if !resp.OK || resp.State != "enrolled" || resp.SessionID != testSessionID {
		t.Fatalf("register: %+v", resp)
	}
	posts, _, _ := r.fake.snapshot()
	if len(posts) != 1 || posts[0]["session_id"] != testSessionID {
		t.Fatalf("the enrollment must carry the session id the launcher named: %+v", posts)
	}
	list := r.call(t, Request{Op: "sessions"})
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != testSessionID {
		t.Fatalf("sessions must list the session id: %+v", list.Sessions)
	}
	waitFor(t, func() bool { return recordedSessionID(t, state, os.Getpid()) == testSessionID })
	if got := recordedSessionID(t, state, os.Getpid()); got != testSessionID {
		t.Fatalf("the registry on disk must record the session id, so a running machine can be read without asking the broker: %q", got)
	}
}

// TestRegisterWithoutASessionIDStillEnrollsAndSigns: a launcher that names no session is the
// case every host session was before this field, and it must be untouched — the session
// registers, enrolls, and signs, and the enrollment names no session_id at all rather than some
// stand-in the page would show as the asking session.
func TestRegisterWithoutASessionIDStillEnrollsAndSigns(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	r := startRig(t, state)
	resp := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !resp.OK || resp.State != "enrolled" || resp.SessionID != "" {
		t.Fatalf("register: %+v", resp)
	}
	posts, _, _ := r.fake.snapshot()
	if len(posts) != 1 {
		t.Fatalf("one enrollment: %+v", posts)
	}
	if id, named := posts[0]["session_id"]; named && id != nil {
		t.Fatalf("a session whose launcher named none must send no session_id: %#v", id)
	}
	signed := r.call(t, Request{Op: "sign", Method: "POST", URL: "https://secrets.test/v1/requests"})
	if !signed.OK || signed.EnrollmentID != resp.EnrollmentID {
		t.Fatalf("a session with no id must still get its proofs: %+v", signed)
	}
	if got := recordedSessionID(t, state, os.Getpid()); got != "" {
		t.Fatalf("the record must say it has no session id: %q", got)
	}
}

// TestASessionKeepsTheIDItsOwnRegistrationNamed: nothing inside a session can set or change its
// id. A register from a process inside the session is answered with that session's reply, which
// carries the recorded id, and the asked-for one is only logged — the property that makes this
// field the launcher's statement rather than the session's own.
func TestASessionKeepsTheIDItsOwnRegistrationNamed(t *testing.T) {
	logs := &syncBuffer{}
	r := newLoggedRig(t, "", slog.New(slog.NewJSONHandler(logs, nil)))
	r.login(t)
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	reg := r.call(t, Request{Op: "register", WaitSeconds: 5, SessionID: new(testSessionID)})
	if !reg.OK || reg.SessionID != testSessionID {
		t.Fatalf("register child: %+v", reg)
	}
	// The test process is the child's parent, so a register from it makes a session of its own:
	// the one that must not be able to rename the child's. Register from the child again instead,
	// the inside-the-session case, naming another id.
	again := r.call(t, Request{Op: "register", SessionID: new("forged-by-the-session")})
	if !again.OK || again.SessionID != testSessionID || again.RuntimeID != reg.RuntimeID {
		t.Fatalf("a register inside a session must answer that session's own recorded id: %+v", again)
	}
	if sess := r.srv.Registry.Get(child.Process.Pid); sess == nil || sess.SessionID != testSessionID {
		t.Fatalf("the recorded id must not change: %+v", sess)
	}
	posts, _, _ := r.fake.snapshot()
	for _, post := range posts {
		if post["session_id"] == "forged-by-the-session" {
			t.Fatalf("no enrollment may carry an id a process inside the session named: %+v", posts)
		}
	}
	if !strings.Contains(logs.String(), "a register inside a session named another session id") {
		t.Fatalf("the refused id must be on the record: %s", logs.String())
	}
}

// TestRegisterRefusesAnUnusableSessionID: the helper applies the broker's own rule for a session
// id (helper.ValidSessionID) at the socket, so a launcher learns at once rather than through an
// enrollment that keeps failing, and nothing is registered.
func TestRegisterRefusesAnUnusableSessionID(t *testing.T) {
	r := startRig(t, "")
	for _, id := range []string{"has space", "has\ttab", "has\nnewline", strings.Repeat("x", MaxSessionIDLength+1)} {
		resp := r.call(t, Request{Op: "register", SessionID: new(id)})
		if resp.OK || resp.Code != CodeBadRequest {
			t.Fatalf("session id %q must be refused BAD_REQUEST: %+v", id, resp)
		}
		if r.srv.Registry.Get(os.Getpid()) != nil {
			t.Fatalf("session id %q must register nothing", id)
		}
	}
	if posts, _, _ := r.fake.snapshot(); len(posts) != 0 {
		t.Fatalf("a refused register must reach the broker not at all: %+v", posts)
	}
}

// TestARecordFromBeforeTheSessionIDFieldStillRecovers is the backward-compatibility proof for
// every session already enrolled: a sessions.json written by a helper that had no session_id
// field at all (its records carry only pid, start_ticks, runtime_id and enrollment_id) re-pins,
// re-enrolls and keeps serving, with no session id and no refusal.
func TestARecordFromBeforeTheSessionIDFieldStillRecovers(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	child := sleeper(t)
	pid := child.Process.Pid
	ticks, err := StartTicks(pid)
	if err != nil {
		t.Fatal(err)
	}
	old := fmt.Sprintf(`[{"pid":%d,"start_ticks":%d,"runtime_id":"testhost:%d:%d","enrollment_id":"enr-prior"}]`, pid, ticks, pid, ticks)
	if err := os.WriteFile(state, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	r := startRig(t, state)
	sess := r.srv.Registry.Get(pid)
	if sess == nil || sess.SessionID != "" {
		t.Fatalf("a record from before the field must re-pin with no session id: %+v", sess)
	}
	waitFor(t, func() bool { return sess.EnrollmentID() != "" })
	if sess.EnrollmentID() == "" {
		t.Fatal("a re-pinned session from such a record must enroll again")
	}
	posts, _, _ := r.fake.snapshot()
	if len(posts) == 0 {
		t.Fatal("it must reach the broker")
	}
	if id, named := posts[len(posts)-1]["session_id"]; named && id != nil {
		t.Fatalf("and send no session_id: %#v", id)
	}
}

// TestARecordedSessionIDSurvivesARestart: a session the launcher named keeps that name through a
// helper restart, which re-pins the process with a fresh key — the record is the only place the
// id survives, since the launcher is long gone by then.
func TestARecordedSessionIDSurvivesARestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	child := sleeper(t)
	pid := child.Process.Pid
	ticks, err := StartTicks(pid)
	if err != nil {
		t.Fatal(err)
	}
	prior := NewRegistry(state)
	old, err := newSession(pid, ticks, fmt.Sprintf("testhost:%d:%d", pid, ticks), testSessionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	old.setEnrolled("enr-prior")
	prior.Add(old)
	if err := prior.Save(); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var recs []map[string]any
	if err := json.Unmarshal(saved, &recs); err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0]["session_id"] != testSessionID {
		t.Fatalf("Save must write the session id: %s", saved)
	}

	r := startRig(t, state)
	sess := r.srv.Registry.Get(pid)
	if sess == nil || sess.SessionID != testSessionID {
		t.Fatalf("the re-pinned session must keep its recorded id: %+v", sess)
	}
	waitFor(t, func() bool { return sess.EnrollmentID() != "" && sess.EnrollmentID() != "enr-prior" })
	posts, _, _ := r.fake.snapshot()
	if len(posts) == 0 || posts[len(posts)-1]["session_id"] != testSessionID {
		t.Fatalf("its fresh enrollment must carry the same id: %+v", posts)
	}
}
