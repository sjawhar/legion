package helper

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
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
	for _, op := range []string{"enroll-box", "unenroll-box"} {
		if resp := cr.call(t, Request{Op: op, RuntimeID: "x", Thumbprint: "y", EnrollmentID: enrolled.EnrollmentID}); resp.OK || resp.Code != CodeBadRequest {
			t.Fatalf("%s: %+v, want %s: the box ops are gone", op, resp, CodeBadRequest)
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
