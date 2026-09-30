// packages/envoy/internal/broker/helper/registry_test.go
//go:build linux

package helper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootFindsSelfAndDescendantOnly(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "sessions.json"))
	self := os.Getpid()
	sess, err := newSession(self, 1, "h:1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	reg.Add(sess)
	child := sleeper(t)
	if got := reg.Root(self); got != sess {
		t.Fatal("the root itself belongs to its session")
	}
	if got := reg.Root(child.Process.Pid); got != sess {
		t.Fatal("a child of the root belongs to its session")
	}
	if got := reg.Root(1); got != nil {
		t.Fatal("pid 1 belongs to no session")
	}
	if !reg.Remove(sess) {
		t.Fatal("Remove reports the removal")
	}
	if got := reg.Root(child.Process.Pid); got != nil {
		t.Fatal("a removed session matches nothing")
	}
	select {
	case <-sess.stop:
	default:
		t.Fatal("Remove must close stop")
	}
	if reg.Remove(sess) {
		t.Fatal("a second Remove of the same session is a no-op")
	}
	other, _ := newSession(self, 2, "h:1:2", nil)
	reg.Add(other)
	if reg.Remove(sess) || reg.Get(self) != other {
		t.Fatal("Remove takes only the very session it is given, never a later one with the same pid")
	}
}

func TestSaveAndLoadRecordsCarryNoKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	reg := NewRegistry(path)
	sess, err := newSession(4242, 99, "h:4242:99", nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.setEnrolled("enr-1")
	reg.Add(sess)
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("sessions.json must be 0600: %v %v", info, err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	if len(data) == 0 || strings.Contains(text, "PRIVATE") || strings.Contains(text, "\"d\":") || strings.Contains(text, "key") {
		t.Fatalf("the record must not carry key material: %s", data)
	}
	recs, err := LoadRecords(path)
	if err != nil || len(recs) != 1 || recs[0] != (Record{PID: 4242, StartTicks: 99, RuntimeID: "h:4242:99", EnrollmentID: "enr-1"}) {
		t.Fatalf("records: %+v %v", recs, err)
	}
	if recs, err := LoadRecords(filepath.Join(t.TempDir(), "none.json")); err != nil || recs != nil {
		t.Fatalf("a missing file is no records: %+v %v", recs, err)
	}
}

func TestStateFollowsEnrollment(t *testing.T) {
	sess, err := newSession(1, 1, "h:1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.State() != "enrolling" || sess.EnrollmentID() != "" {
		t.Fatal("a fresh session is enrolling")
	}
	sess.setError("broker 503 DATABASE: postgres unreachable")
	if sess.LastError() == "" {
		t.Fatal("the last broker error is kept for NOT_ENROLLED replies")
	}
	sess.setEnrolled("enr-2")
	if sess.State() != "enrolled" || sess.LastError() != "" {
		t.Fatalf("enrolled: %s %q", sess.State(), sess.LastError())
	}
	select {
	case <-sess.ready:
	default:
		t.Fatal("setEnrolled must close ready")
	}
	if id := sess.markLapsed("the broker refused this session's renew (PROOF_INVALID); enrolling again"); id != "enr-2" {
		t.Fatalf("markLapsed returns the lapsed id: %q", id)
	}
	if sess.State() != "enrolling" || sess.EnrollmentID() != "" {
		t.Fatal("a refused renew puts the session back to enrolling at once")
	}
	if got := sess.LastError(); got != "the broker refused this session's renew (PROOF_INVALID); enrolling again" {
		t.Fatalf("a lapse records its reason as the last error: %q", got)
	}
	select {
	case <-sess.readyCh():
		t.Fatal("a lapse must reopen ready, so a register --wait waits for the re-enrollment")
	default:
	}
	if got := sess.recordedEnrollmentID(); got != "enr-2" {
		t.Fatalf("the record keeps the lapsed id until its revoke: %q", got)
	}
	sess.clearLapsed()
	if got := sess.recordedEnrollmentID(); got != "" {
		t.Fatalf("a revoked lapsed id leaves the record: %q", got)
	}
	sess.setEnrolled("enr-3")
	select {
	case <-sess.readyCh():
	default:
		t.Fatal("the re-enrollment must close the reopened ready")
	}
}
