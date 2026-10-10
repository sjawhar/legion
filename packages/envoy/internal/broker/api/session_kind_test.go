// session_kind_test.go pins POST /v1/enrollments's kinds on the real broker api_test.go mounts: a
// machine session enrolls as kind session, the kind an agent-secrets-helper before it sent (host
// for its own sessions, box for a key a launcher enrolled through it) still enrolls and is recorded
// as session, and no other kind does.
package api_test

import (
	"net/http"
	"testing"
)

func TestEveryMachineSessionIsOneKind(t *testing.T) {
	ts := newTestServer(t)
	credentialID, machineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	for _, kind := range []string{"session", "host", "box"} {
		key := newSigningKey(t)
		status, body := ts.launcher(t, machineKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": kind, "runtime_id": kind + "-" + t.Name(), "operator": testApprover, "thumbprint": thumbprintOf(t, key),
		})
		if status != http.StatusCreated {
			t.Fatalf("POST /v1/enrollments kind %s = %d, want 201: %s", kind, status, body)
		}
		enrollmentID := decode[wireEnrolled](t, body).EnrollmentID
		status, body = ts.session(t, key, enrollmentID, http.MethodGet, "/v1/enrollments/self", nil)
		self := decode[struct {
			Kind string `json:"kind"`
		}](t, body)
		if status != http.StatusOK || self.Kind != "session" {
			t.Fatalf("GET /v1/enrollments/self of a %s enrollment = %d %s, want kind session", kind, status, body)
		}
	}
	for _, kind := range []string{"", "container", "Session"} {
		status, body := ts.launcher(t, machineKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": kind, "runtime_id": "other-" + t.Name(), "operator": testApprover, "thumbprint": thumbprintOf(t, newSigningKey(t)),
		})
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "INVALID_KIND" {
			t.Fatalf("POST /v1/enrollments kind %q = %d %s, want 400 INVALID_KIND", kind, status, body)
		}
	}
}
