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
	signed, err := proof.Sign(sess.Key, sess.EnrollmentID(), method, cr.broker.URL+path, time.Now())
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
	reg := cr.call(t, Request{Op: "register", WaitSeconds: 20, SessionID: &sessionID})
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
	if after == nil || after.Thumbprint != before.Thumbprint {
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

	reg := cr.call(t, Request{Op: "register", WaitSeconds: 20, SessionID: &sessionID})
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

	restore := DormantGrace
	DormantGrace = 0
	defer func() { DormantGrace = restore }()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	next := &Server{
		Registry: NewRegistry(cr.sessionsPath),
		Broker: &Broker{URL: cr.broker.URL, OperatorFile: cr.broker.OperatorFile, MachineFile: cr.machineFile,
			HTTP: http.DefaultClient, Log: log},
		Hostname: "contract-test-host", PeerOf: PeerOf, Log: log, MinRenew: 50 * time.Millisecond,
	}
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
