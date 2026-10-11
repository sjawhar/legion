package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// TestEnrollSessionKeyEnrollsAndRevokesAnExternalKey drives the kind-free pair against a real
// broker: enroll-session-key enrolls a key the helper never held as kind session, under the
// operator's machine login, and that key's own proof is accepted; unenroll-session-key ends it, so
// the same proof is refused.
func TestEnrollSessionKeyEnrollsAndRevokesAnExternalKey(t *testing.T) {
	cr := newContractRig(t)
	b := cr.srv.Broker
	code, err := b.Login(context.Background(), cr.srv.Hostname)
	if err != nil {
		t.Fatalf("Broker.Login: %v", err)
	}
	cr.broker.DecideMachineLogin(t, code, true)
	waitForIssued(t, b)

	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	enrolled := cr.call(t, Request{Op: "enroll-session-key", RuntimeID: "container-" + t.Name(), Thumbprint: thumbprint})
	if !enrolled.OK || enrolled.EnrollmentID == "" || enrolled.LeaseExpires == "" {
		t.Fatalf("enroll-session-key: %+v", enrolled)
	}
	self := func() (int, []byte) {
		url := cr.broker.URL + "/v1/enrollments/self"
		p, err := proof.Sign(key, enrolled.EnrollmentID, http.MethodGet, url, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return cr.broker.Req(t, http.MethodGet, "/v1/enrollments/self", map[string]string{"Proof": p}, nil)
	}
	status, body := self()
	if got := contractDecode[contractSelfResponse](t, body); status != http.StatusOK || got.Kind != "session" || got.Operator != cr.broker.Operator {
		t.Fatalf("GET /v1/enrollments/self with the enrolled key = %d %s, want kind session under %s", status, body, cr.broker.Operator)
	}

	for range 2 {
		if un := cr.call(t, Request{Op: "unenroll-session-key", EnrollmentID: enrolled.EnrollmentID}); !un.OK {
			t.Fatalf("unenroll-session-key (twice, idempotent): %+v", un)
		}
	}
	if status, body := self(); status != http.StatusUnauthorized {
		t.Fatalf("GET /v1/enrollments/self after unenroll-session-key = %d %s, want 401", status, body)
	}
}

// TestAnOlderCLIsBoxOpsStillEnrollAndRevoke: an agent-secrets CLI from before the session key
// (agentbox's enroll and unenroll, run from whatever release the box host's PATH holds) asks
// enroll-box and unenroll-box, with the "kind" field older CLIs also sent. This helper answers
// both as the kind-free pair: the key enrolls as kind session, its proof is accepted, and
// unenroll-box ends it.
func TestAnOlderCLIsBoxOpsStillEnrollAndRevoke(t *testing.T) {
	cr := newContractRig(t)
	b := cr.srv.Broker
	code, err := b.Login(context.Background(), cr.srv.Hostname)
	if err != nil {
		t.Fatalf("Broker.Login: %v", err)
	}
	cr.broker.DecideMachineLogin(t, code, true)
	waitForIssued(t, b)

	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	// The line a CLI before this release writes: its Request named the op enroll-box and carried Kind.
	enrolled := rawCall(t, cr.sock, fmt.Sprintf(`{"op":"enroll-box","runtime_id":"box-%s","thumbprint":%q,"kind":"box"}`, t.Name(), thumbprint))
	if !enrolled.OK || enrolled.EnrollmentID == "" || enrolled.LeaseExpires == "" {
		t.Fatalf("enroll-box from an older CLI: %+v", enrolled)
	}
	self := func() (int, []byte) {
		url := cr.broker.URL + "/v1/enrollments/self"
		p, err := proof.Sign(key, enrolled.EnrollmentID, http.MethodGet, url, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return cr.broker.Req(t, http.MethodGet, "/v1/enrollments/self", map[string]string{"Proof": p}, nil)
	}
	status, body := self()
	if got := contractDecode[contractSelfResponse](t, body); status != http.StatusOK || got.Kind != "session" {
		t.Fatalf("GET /v1/enrollments/self with the key enroll-box enrolled = %d %s, want kind session", status, body)
	}
	if un := rawCall(t, cr.sock, fmt.Sprintf(`{"op":"unenroll-box","enrollment_id":%q}`, enrolled.EnrollmentID)); !un.OK {
		t.Fatalf("unenroll-box from an older CLI: %+v", un)
	}
	if status, body := self(); status != http.StatusUnauthorized {
		t.Fatalf("GET /v1/enrollments/self after unenroll-box = %d %s, want 401", status, body)
	}
	for _, line := range []string{`{"op":"enroll-box","kind":"box"}`, `{"op":"unenroll-box"}`} {
		if resp := rawCall(t, cr.sock, line); resp.OK || resp.Code != CodeBadRequest {
			t.Fatalf("%s: %+v, want %s for a missing field", line, resp, CodeBadRequest)
		}
	}
}

// rawCall writes line to the helper's socket as a client of another release would and reads its
// one reply, so a test pins the bytes an older client sends rather than this release's Request.
func rawCall(t *testing.T, sock, line string) Response {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(reply, &resp); err != nil {
		t.Fatalf("helper reply %q: %v", reply, err)
	}
	return resp
}

// TestSignRequestSignsTheLifetimeItIsAsked: sign-request with lifetime_seconds signs that lifetime
// into the request object, the claim the broker grants for, and without it signs none, so the
// grant lasts until the session ends.
func TestSignRequestSignsTheLifetimeItIsAsked(t *testing.T) {
	r := startRig(t, "")
	if reg := r.call(t, Request{Op: "register", WaitSeconds: 5}); !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register: %+v", reg)
	}
	for _, lifetime := range []*int{new(120), nil} {
		resp := r.call(t, Request{Op: "sign-request", Secrets: []string{"DEEL_API_KEY"}, Reason: "x", LifetimeSeconds: lifetime})
		if !resp.OK {
			t.Fatalf("sign-request lifetime %v: %+v", lifetime, resp)
		}
		ro, err := record.VerifyRequestObject(resp.RequestObject, r.srv.Broker.URL, time.Minute, time.Now())
		if err != nil || (ro.LifetimeSeconds == nil) != (lifetime == nil) || lifetime != nil && *ro.LifetimeSeconds != *lifetime {
			t.Fatalf("sign-request lifetime %v signed lifetime %v (%v)", lifetime, ro.LifetimeSeconds, err)
		}
	}
}

// TestABrokerBeforeTheSessionKindStillEnrollsEverySession: a broker that refuses kind session
// (400 INVALID_KIND, every release before it) still enrolls the helper's own sessions, as host, and
// a key enroll-session-key brings, as box, so no session loses the broker while the helper ships
// ahead of the broker.
func TestABrokerBeforeTheSessionKindStillEnrollsEverySession(t *testing.T) {
	r := startRig(t, "")
	r.fake.mu.Lock()
	r.fake.refuseKinds = map[string]bool{"session": true}
	r.fake.mu.Unlock()

	reg := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register against a broker before the session kind: %+v", reg)
	}
	key := r.call(t, Request{Op: "enroll-session-key", RuntimeID: "container-1", Thumbprint: "tp-container-1"})
	if !key.OK || key.EnrollmentID == "" {
		t.Fatalf("enroll-session-key against a broker before the session kind: %+v", key)
	}
	posts, _, _ := r.fake.snapshot()
	var kinds []any
	for _, p := range posts {
		kinds = append(kinds, p["kind"])
	}
	if len(kinds) != 4 || kinds[0] != "session" || kinds[1] != "host" || kinds[2] != "session" || kinds[3] != "box" {
		t.Fatalf("enroll kinds sent = %v, want session then host for the session, session then box for the key", kinds)
	}
}
