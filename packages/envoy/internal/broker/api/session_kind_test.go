// session_kind_test.go pins POST /v1/enrollments's kinds on the real broker api_test.go mounts: a
// machine session enrolls as kind session, and no other kind does, the kinds an
// agent-secrets-helper before legion-envoy 10 sent (host, box) among them.
package api_test

import (
	"net/http"
	"testing"
)

func TestEveryMachineSessionIsOneKind(t *testing.T) {
	ts := newTestServer(t)
	credentialID, machineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	key := newSigningKey(t)
	status, body := ts.launcher(t, machineKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "session", "runtime_id": "session-" + t.Name(), "operator": testApprover, "thumbprint": thumbprintOf(t, key),
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/enrollments kind session = %d, want 201: %s", status, body)
	}
	enrollmentID := decode[wireEnrolled](t, body).EnrollmentID
	status, body = ts.session(t, key, enrollmentID, http.MethodGet, "/v1/enrollments/self", nil)
	self := decode[struct {
		Kind string `json:"kind"`
	}](t, body)
	if status != http.StatusOK || self.Kind != "session" {
		t.Fatalf("GET /v1/enrollments/self of a session enrollment = %d %s, want kind session", status, body)
	}
	for _, kind := range []string{"", "container", "Session", "host", "box"} {
		status, body := ts.launcher(t, machineKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": kind, "runtime_id": "other-" + t.Name(), "operator": testApprover, "thumbprint": thumbprintOf(t, newSigningKey(t)),
		})
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "INVALID_KIND" {
			t.Fatalf("POST /v1/enrollments kind %q = %d %s, want 400 INVALID_KIND", kind, status, body)
		}
	}
}
