package api_test

import (
	"crypto/ecdsa"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

const (
	workerAccount = "system:serviceaccount:legion:worker"
	otherAccount  = "system:serviceaccount:legion:other"
)

// podToken mints a projected service-account token for account, bound to podUID, signed by the
// local OIDC issuer the test server's pod verifier trusts.
func (ts *testServer) podToken(t *testing.T, account, podUID string) string {
	t.Helper()
	claims := ts.podIssuer.Claims(account, podAudience)
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	return ts.podIssuer.Mint(t, ts.podKey, claims)
}

// mintServiceLauncherCredential is mintLauncherCredential for a service login (a launcher
// credential detail naming service legion-daemon): the credential has no operator and enrolls pods
// only, the shape the Legion daemon logs in with.
func (ts *testServer) mintServiceLauncherCredential(t *testing.T) (credentialID string, key *ecdsa.PrivateKey) {
	t.Helper()
	key = newSigningKey(t)
	compact, err := record.Sign(key, ts.URL, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: "cluster.example", Service: "legion-daemon"},
	}, "", testApprover, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	return ts.approveMachineLogin(t, compact, testApprover), key
}

type wireEnrolled struct {
	EnrollmentID string  `json:"enrollment_id"`
	Slot         *string `json:"slot"`
}

// podSession is one slot of a pod, enrolled through POST /v1/enrollments with its own key.
type podSession struct {
	slot, enrollmentID string
	key                *ecdsa.PrivateKey
}

func thumbprintOf(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	tp, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

// request signs an agent_secret request object for names and posts it as this session.
func (s podSession) request(t *testing.T, ts *testServer, names ...string) wireCreateRequestResponse {
	t.Helper()
	status, body := ts.session(t, s.key, s.enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, s.key, ts.URL, "role "+s.slot, names...), "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests as %s = %d: %s", s.slot, status, body)
	}
	return decode[wireCreateRequestResponse](t, body)
}

// TestRolesAndGenerationsOfOnePodEnrollRequestAndReleaseIndependently drives slot enrollment over
// real HTTP with a launcher proof by the daemon's service credential and real projected pod
// tokens: two roles of one pod UID and a second generation of one role are three enrollments;
// a same-slot retry is idempotent and a different key in that slot, a token for another pod, a
// malformed slot and a slot off a pod are refused; a session cannot enroll itself; each slot's
// record and grant name its slot; one slot's grant never releases to another; the pod rules still
// match on the service account alone; and revoking one role leaves the other's grant live.
func TestRolesAndGenerationsOfOnePodEnrollRequestAndReleaseIndependently(t *testing.T) {
	ts := newTestServer(t)
	credentialID, launcherKey := ts.mintServiceLauncherCredential(t)
	podUID := uuid.NewString()
	enroll := func(slot, runtimeID, token string, key *ecdsa.PrivateKey) (int, []byte) {
		t.Helper()
		return ts.launcher(t, launcherKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": "pod", "runtime_id": runtimeID, "slot": slot, "thumbprint": thumbprintOf(t, key), "pod_token": token,
		})
	}
	token := ts.podToken(t, workerAccount, podUID)
	sessions := map[string]podSession{}
	for _, slot := range []string{"implementer-g1", "reviewer-g1", "implementer-g2"} {
		key := newSigningKey(t)
		status, body := enroll(slot, podUID, token, key)
		if status != http.StatusCreated {
			t.Fatalf("enroll %s = %d, want 201: %s", slot, status, body)
		}
		got := decode[wireEnrolled](t, body)
		if got.Slot == nil || *got.Slot != slot {
			t.Fatalf("enroll %s answered slot %v", slot, got.Slot)
		}
		for other, s := range sessions {
			if s.enrollmentID == got.EnrollmentID {
				t.Fatalf("slots %s and %s share enrollment %s", slot, other, got.EnrollmentID)
			}
		}
		sessions[slot] = podSession{slot: slot, enrollmentID: got.EnrollmentID, key: key}
	}
	implementer, reviewer := sessions["implementer-g1"], sessions["reviewer-g1"]

	status, body := enroll("implementer-g1", podUID, token, implementer.key)
	if status != http.StatusOK || decode[wireEnrolled](t, body).EnrollmentID != implementer.enrollmentID {
		t.Fatalf("same-slot retry with its key = %d %s, want 200 and enrollment %s", status, body, implementer.enrollmentID)
	}
	status, body = enroll("implementer-g1", podUID, token, newSigningKey(t))
	if status != http.StatusConflict || decode[wireError](t, body).Code != "ALREADY_ENROLLED" {
		t.Fatalf("same slot, different key = %d %s, want 409 ALREADY_ENROLLED", status, body)
	}
	status, body = enroll("tester-g1", podUID, ts.podToken(t, workerAccount, uuid.NewString()), newSigningKey(t))
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "POD_IDENTITY_MISMATCH" {
		t.Fatalf("another pod's token = %d %s, want 403 POD_IDENTITY_MISMATCH", status, body)
	}
	status, body = enroll("tester-g1", podUID+"/tester", token, newSigningKey(t))
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "POD_IDENTITY_MISMATCH" {
		t.Fatalf("a runtime id composed with a role = %d %s, want 403 POD_IDENTITY_MISMATCH", status, body)
	}
	for _, slot := range []string{"Implementer-g1", "1-implementer", "implementer_g1"} {
		status, body = enroll(slot, podUID, token, newSigningKey(t))
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "INVALID_SLOT" {
			t.Fatalf("slot %q = %d %s, want 400 INVALID_SLOT", slot, status, body)
		}
	}
	operatorCredential, operatorKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	for _, kind := range []string{"box", "host"} {
		status, body = ts.launcher(t, operatorKey, operatorCredential, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": kind, "runtime_id": kind + "-" + t.Name(), "operator": testApprover, "slot": "implementer-g1",
			"thumbprint": thumbprintOf(t, newSigningKey(t)),
		})
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "INVALID_SLOT" {
			t.Fatalf("%s with a slot = %d %s, want 400 INVALID_SLOT", kind, status, body)
		}
	}
	status, body = ts.session(t, implementer.key, implementer.enrollmentID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": podUID, "slot": "merger-g1", "thumbprint": thumbprintOf(t, implementer.key), "pod_token": token,
	})
	if status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("a session enrolling a slot itself = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}

	for _, s := range []podSession{implementer, reviewer} {
		status, body := ts.session(t, s.key, s.enrollmentID, http.MethodGet, "/v1/enrollments/self", nil)
		self := decode[struct {
			EnrollmentID string  `json:"enrollment_id"`
			Kind         string  `json:"kind"`
			Slot         *string `json:"slot"`
		}](t, body)
		if status != http.StatusOK || self.EnrollmentID != s.enrollmentID || self.Kind != "pod" || self.Slot == nil || *self.Slot != s.slot {
			t.Fatalf("GET /v1/enrollments/self as %s = %d %s", s.slot, status, body)
		}
	}

	records := map[string]string{}
	for _, s := range []podSession{implementer, reviewer} {
		created := s.request(t, ts, "DEEL_API_KEY")
		if created.State != "pending" || created.RecordID == nil || created.Coalesced {
			t.Fatalf("DEEL_API_KEY as %s = %+v, want a pending record of its own", s.slot, created)
		}
		records[s.slot] = *created.RecordID
		status, body := ts.ui(t, http.MethodGet, "/v1/credential-requests/"+*created.RecordID, nil)
		read := decode[wireRecord](t, body)
		if status != http.StatusOK || read.Enrollment == nil || read.Enrollment.Kind != "pod" || read.Enrollment.RuntimeID != podUID ||
			read.Enrollment.Slot == nil || *read.Enrollment.Slot != s.slot {
			t.Fatalf("record of %s = %d %s, want pod %s in slot %s", s.slot, status, body, podUID, s.slot)
		}
	}
	if records["implementer-g1"] == records["reviewer-g1"] {
		t.Fatalf("both roles' requests share record %s", records["implementer-g1"])
	}
	grants := map[string]string{}
	for slot, recordID := range records {
		status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve", map[string]any{"approver": testApprover})
		approved := decode[struct {
			GrantID *string `json:"grant_id"`
		}](t, body)
		if status != http.StatusOK || approved.GrantID == nil {
			t.Fatalf("approve %s's record = %d %s", slot, status, body)
		}
		grants[slot] = *approved.GrantID
	}

	values := func(s podSession, grantID string) (int, []byte) {
		t.Helper()
		return ts.session(t, s.key, s.enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
	}
	if status, body := values(implementer, grants["implementer-g1"]); status != http.StatusOK ||
		decode[struct {
			Values map[string]string `json:"values"`
		}](t, body).Values["DEEL_API_KEY"] != "deel-v1" {
		t.Fatalf("implementer-g1 reading its own grant = %d %s", status, body)
	}
	if status, body := values(reviewer, grants["implementer-g1"]); status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_YOURS" {
		t.Fatalf("reviewer-g1 reading implementer-g1's grant = %d %s, want 403 NOT_YOURS", status, body)
	}

	status, body = ts.ui(t, http.MethodGet, "/v1/grants?approver="+testApprover, nil)
	listed := decode[struct {
		Grants []wireApproverGrant `json:"grants"`
	}](t, body)
	if status != http.StatusOK || len(listed.Grants) != 2 {
		t.Fatalf("GET /v1/grants = %d %s, want both roles' grants", status, body)
	}
	for _, g := range listed.Grants {
		slot := ""
		for s, id := range grants {
			if id == g.GrantID {
				slot = s
			}
		}
		if g.Enrollment.Slot == nil || *g.Enrollment.Slot != slot || g.Enrollment.RuntimeID != podUID {
			t.Fatalf("listed grant %s = %+v, want pod %s in slot %q", g.GrantID, g.Enrollment, podUID, slot)
		}
	}

	// The pod rules match the verified service account, never the slot: every slot of a worker
	// pod gets WORKER_TOKEN automatically, and the same slot of a pod of another account is denied.
	for _, s := range []podSession{implementer, reviewer, sessions["implementer-g2"]} {
		if created := s.request(t, ts, "WORKER_TOKEN"); created.State != "granted" {
			t.Fatalf("WORKER_TOKEN as worker %s = %+v, want granted", s.slot, created)
		}
	}
	otherUID := uuid.NewString()
	otherKey := newSigningKey(t)
	status, body = enroll("implementer-g1", otherUID, ts.podToken(t, otherAccount, otherUID), otherKey)
	if status != http.StatusCreated {
		t.Fatalf("enroll a pod of another account = %d %s", status, body)
	}
	other := podSession{slot: "implementer-g1", enrollmentID: decode[wireEnrolled](t, body).EnrollmentID, key: otherKey}
	for _, name := range []string{"WORKER_TOKEN", "DEEL_API_KEY"} {
		if created := other.request(t, ts, name); created.State != "denied" {
			t.Fatalf("%s as implementer-g1 of account legion:other = %+v, want denied", name, created)
		}
	}

	status, body = ts.launcher(t, launcherKey, credentialID, http.MethodDelete, "/v1/enrollments/"+implementer.enrollmentID, nil)
	if status != http.StatusNoContent {
		t.Fatalf("revoke implementer-g1 = %d %s", status, body)
	}
	if status, body := values(implementer, grants["implementer-g1"]); status != http.StatusUnauthorized {
		t.Fatalf("revoked implementer-g1 reading its grant = %d %s, want 401", status, body)
	}
	if status, body := values(reviewer, grants["reviewer-g1"]); status != http.StatusOK {
		t.Fatalf("reviewer-g1 reading its grant after implementer-g1's revoke = %d %s, want 200", status, body)
	}
}
