// packages/envoy/internal/broker/helper/contract_session_identity_test.go
//go:build linux

// A named session's grant surviving a reboot, against a REAL broker (brokertest.Rig), not the
// package's hand-rolled fake: the whole point of dispatch://INFRA-1167 is a property of the
// broker's own enrollment conflict path (enroll.recoverConflict) and of what endEnrollment
// cancels, and only an end-to-end run proves the helper's half lines up with it.
package helper

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

type contractCreatedRequest struct {
	RequestID string  `json:"request_id"`
	State     string  `json:"state"`
	GrantID   *string `json:"grant_id"`
	RecordID  *string `json:"record_id"`
}

type contractValues struct {
	Values map[string]string `json:"values"`
}

// sessionReq makes one broker call signed as the session: the proof a session's own key makes
// for that exact method and URL, which is how every session route authenticates.
func (cr *contractRig) sessionReq(t *testing.T, sess *Session, method, path string, body any) (int, []byte) {
	t.Helper()
	signed, err := proof.Sign(sess.identity().Key, sess.EnrollmentID(), method, cr.broker.URL+path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return cr.broker.Req(t, method, path, map[string]string{"Proof": signed}, body)
}

// TestContractARebootKeepsAGrantTheSessionWasGiven drives the whole case Sami hit on
// 2026-11-04: a session asks for a human-tier secret, he approves it, the machine reboots, and
// the session resumes. Before this change the resumed session enrolled afresh — a new pid, new
// start ticks, a new key — and the broker answered its release NOT_YOURS, because a grant is
// bound to the enrollment id alone. Now the session names itself, keeps its key in the records
// file, and the broker hands back the enrollment it already has, so the release still works and
// nobody is asked to approve anything twice.
func TestContractARebootKeepsAGrantTheSessionWasGiven(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cr := newContractRig(t)
	cr.login(t)
	sessionID := "11111111-2222-4333-8444-66666666cafe"

	// --- the session registers under its omp session id ---
	reg := cr.call(t, Request{Op: "register", WaitSeconds: 5, SessionID: &sessionID}) // under Call's own 10 s read deadline
	if !reg.OK || reg.State != "enrolled" || reg.EnrollmentID == "" {
		t.Fatalf("register --session-id: %+v", reg)
	}
	if reg.RuntimeID != SessionRuntimeID(sessionID) {
		t.Fatalf("runtime_id %q, want %s", reg.RuntimeID, SessionRuntimeID(sessionID))
	}
	before := cr.srv.Registry.Get(os.Getpid())
	if before == nil {
		t.Fatal("the registered session is not in the registry")
	}

	// --- it asks for a human-tier secret, and the request is pending on a person ---
	signed := cr.call(t, Request{Op: "sign-request", Secrets: []string{"TEST_SECRET"}, Reason: "the reboot case"})
	if !signed.OK || signed.RequestObject == "" {
		t.Fatalf("sign-request: %+v", signed)
	}
	status, body := cr.sessionReq(t, before, http.MethodPost, "/v1/requests",
		map[string]any{"request": signed.RequestObject, "session_id": sessionID})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d: %s", status, body)
	}
	created := contractDecode[contractCreatedRequest](t, body)
	if created.State != "pending" || created.RecordID == nil {
		t.Fatalf("a human-tier secret must wait on a person: %+v", created)
	}

	// --- the person approves it ---
	cr.broker.Decide(t, *created.RecordID, true)
	status, body = cr.sessionReq(t, before, http.MethodGet, "/v1/requests/"+created.RequestID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/requests/{id} = %d: %s", status, body)
	}
	granted := contractDecode[contractCreatedRequest](t, body)
	if granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("after the approval: %+v", granted)
	}

	// --- the machine reboots: the helper stops, and the recorded process is gone ---
	cr.cancel()
	rebootRecords(t, cr.sessionsPath)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	next := &Server{
		Registry: NewRegistry(cr.sessionsPath),
		Broker: &Broker{URL: cr.broker.URL, OperatorFile: cr.broker.OperatorFile, MachineFile: cr.machineFile,
			HTTP: http.DefaultClient, Log: log},
		Hostname: "contract-test-host", PeerOf: PeerOf, Log: log, MinRenew: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		for _, sess := range next.Registry.List() {
			if next.Registry.Remove(sess) && sess.peer != nil {
				sess.peer.Close()
			}
		}
		next.Registry.Close()
		cancel()
		next.Broker.Close()
	})
	if err := next.Broker.Start(ctx); err != nil { // resumes the machine login it kept
		t.Fatal(err)
	}
	waitForIssued(t, next.Broker)
	next.Recover(ctx)
	if _, ok := next.Registry.Dormant(sessionID); !ok {
		t.Fatal("after a reboot the session's identity must be kept for it, not revoked")
	}

	// --- the session resumes under the same id ---
	peer, err := PinPID(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	resumed := next.register(ctx, peer, os.Getpid(), sessionID, 20*time.Second)
	if !resumed.OK || resumed.EnrollmentID != reg.EnrollmentID {
		t.Fatalf("the resumed session got %+v, want the enrollment it had, %s", resumed, reg.EnrollmentID)
	}
	after := next.Registry.Get(os.Getpid())
	if after == nil || after.identity().Thumbprint != before.identity().Thumbprint {
		t.Fatal("the resumed session must present the key the broker knows")
	}

	// --- and the grant Sami approved before the reboot still releases its value ---
	status, body = cr.sessionReq(t, after, http.MethodPost, "/v1/grants/"+*granted.GrantID+"/values", nil)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/grants/{id}/values after the reboot = %d: %s; the grant did not survive", status, body)
	}
	if got := contractDecode[contractValues](t, body).Values["TEST_SECRET"]; got != "test-secret-v1" {
		t.Fatalf("the released value is %q, want test-secret-v1", got)
	}
}

// rebootRecords rewrites the records file as a reboot leaves it: every recorded process is gone,
// so each record names a pid whose start ticks no live process can match. Everything else — the
// session id, the runtime id, the key and the enrollment — is exactly what the helper wrote,
// because that file is all a reboot leaves behind.
func rebootRecords(t *testing.T, path string) {
	t.Helper()
	recs, err := LoadRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatalf("%s holds no session records to reboot", path)
	}
	for i := range recs {
		recs[i].StartTicks = ^uint64(0)
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestContractASessionThatDoesNotComeBackLosesItsGrant is the other half: the same reboot, and
// the session never resumes. Its enrollment is revoked once the grace passes, which is what
// ends its grants — so a machine that reboots does not leave approved credentials live for
// sessions that are gone.
func TestContractASessionThatDoesNotComeBackLosesItsGrant(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cr := newContractRig(t)
	cr.login(t)
	sessionID := "11111111-2222-4333-8444-77777777beef"

	reg := cr.call(t, Request{Op: "register", WaitSeconds: 5, SessionID: &sessionID})
	if !reg.OK || reg.EnrollmentID == "" {
		t.Fatalf("register --session-id: %+v", reg)
	}
	sess := cr.srv.Registry.Get(os.Getpid())
	if sess == nil {
		t.Fatal("the registered session is not in the registry")
	}
	signed := cr.call(t, Request{Op: "sign-request", Secrets: []string{"TEST_AUTO_SECRET"}, Reason: "the reboot case"})
	if !signed.OK {
		t.Fatalf("sign-request: %+v", signed)
	}
	status, body := cr.sessionReq(t, sess, http.MethodPost, "/v1/requests",
		map[string]any{"request": signed.RequestObject, "session_id": sessionID})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d: %s", status, body)
	}
	created := contractDecode[contractCreatedRequest](t, body)
	if created.State != "granted" || created.GrantID == nil {
		t.Fatalf("an agent-tier secret is granted at once: %+v", created)
	}

	cr.cancel()
	rebootRecords(t, cr.sessionsPath)

	// The grace is the Server's, never a package variable: a test writing a global that the
	// Server's goroutines read is a data race, and was one.

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	next := &Server{
		Registry: NewRegistry(cr.sessionsPath),
		Broker: &Broker{URL: cr.broker.URL, OperatorFile: cr.broker.OperatorFile, MachineFile: cr.machineFile,
			HTTP: http.DefaultClient, Log: log},
		Hostname: "contract-test-host", PeerOf: PeerOf, Log: log, MinRenew: 50 * time.Millisecond,
	}
	next.SetDormantGrace(time.Nanosecond)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { next.Registry.Close(); cancel(); next.Broker.Close() })
	if err := next.Broker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitForIssued(t, next.Broker)
	next.Recover(ctx)
	next.SweepDormant(ctx)

	status, body = cr.sessionReq(t, sess, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil)
	if status == http.StatusOK {
		t.Fatalf("a session that never came back still released its grant: %d %s", status, body)
	}
	if !slices.Contains([]int{http.StatusForbidden, http.StatusUnauthorized, http.StatusNotFound}, status) {
		t.Fatalf("POST /v1/grants/{id}/values after the sweep = %d: %s", status, body)
	}
	if _, ok := next.Registry.Dormant(sessionID); ok {
		t.Fatal("the swept record must be forgotten")
	}
}

// TestContractTheProductionRecordsPathSurvivesTheRuntimeDirectoryGoing is the defect the two
// tests above could not catch, because each hands the helper a records path of its own: in
// production the path comes from the launcher, and until dispatch://INFRA-1167 it was
// `$XDG_RUNTIME_DIR/agent-secrets/sessions.json`, beside the socket — tmpfs, gone at the very
// reboot the key must survive. This drives the DEFAULTS a real helper computes, removes the
// whole runtime directory as a reboot does, and shows the records, the key in them and the
// enrollment still there afterwards; it also shows a helper upgraded in place carrying the
// records it finds at the old path over to the new one.
func TestContractTheProductionRecordsPathSurvivesTheRuntimeDirectoryGoing(t *testing.T) {
	home := t.TempDir()
	runtime := t.TempDir()
	getenv := func(key string) string {
		switch key {
		case "HOME":
			return home
		case "XDG_RUNTIME_DIR":
			return runtime
		}
		return ""
	}
	legacy, durable := LegacyStatePath(getenv), DefaultStatePath(getenv)
	if filepath.Dir(legacy) != filepath.Join(runtime, "agent-secrets") {
		t.Fatalf("the old path is not under the runtime directory: %s", legacy)
	}
	if filepath.Dir(durable) != filepath.Join(home, ".config", "agent-secrets") {
		t.Fatalf("the records must live beside the machine login, not in tmpfs: %s", durable)
	}

	// A helper of the previous release left its records at the old path; the first start of
	// this one carries them over before Recover reads them.
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	key := recordNamed(t, legacy, 1, 1, testSessionID, "enr-before")
	if err := MigrateStateFile(legacy, durable); err != nil {
		t.Fatalf("migrating the records: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("the old records file must be gone once it is carried over: %v", err)
	}
	file, err := os.Stat(durable)
	if err != nil {
		t.Fatal(err)
	}
	if perm := file.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("the carried-over records are mode %04o; they hold a session key", perm)
	}

	// The reboot: the whole runtime directory goes, socket, old records and all.
	if err := os.RemoveAll(runtime); err != nil {
		t.Fatal(err)
	}

	recs, err := LoadRecords(durable)
	if err != nil {
		t.Fatalf("reading the records after the reboot: %v", err)
	}
	i := slices.IndexFunc(recs, func(rec Record) bool { return rec.SessionID == testSessionID })
	if i < 0 {
		t.Fatalf("the session's record did not survive the reboot: %+v", recs)
	}
	if recs[i].EnrollmentID != "enr-before" {
		t.Fatalf("the record lost its enrollment: %+v", recs[i])
	}
	back, err := recs[i].PrivateKey()
	if err != nil || back == nil {
		t.Fatalf("the record lost its key: %v", err)
	}
	if !back.Equal(key) {
		t.Fatal("the key that survived the reboot is not the one the session had, so the broker would not hand its enrollment back")
	}

	// And a second migration is a no-op: nothing overwrites the records that are now live.
	if err := MigrateStateFile(legacy, durable); err != nil {
		t.Fatalf("a second migration must do nothing: %v", err)
	}
	if again, err := LoadRecords(durable); err != nil || len(again) != len(recs) {
		t.Fatalf("a second migration changed the records: %v %+v", err, again)
	}
}
