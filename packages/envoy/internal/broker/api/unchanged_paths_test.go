// unchanged_paths_test.go pins the paths a machine's own grant changes nothing about: the Legion
// daemon's Kubernetes service login, the pod it enrols, that pod's renewal and its grant, a
// machine session's bodyless renewal, and the refusal of a kind the broker does not serve.
package api_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestEveryExistingEnrollmentPathBehavesAsBefore: the Legion daemon logs in with its Kubernetes
// token, its credential enrolls a pod, the pod renews with its own projected token, gets the
// shared agent-tier secret at once and reads its value, and a machine session's renewal with no
// body still renews. A kind the broker does not serve is 400 INVALID_KIND.
func TestEveryExistingEnrollmentPathBehavesAsBefore(t *testing.T) {
	ts := newTestServer(t)
	credentialID, launcherKey := ts.mintServiceLauncherCredential(t)
	podUID := uuid.NewString()
	podKey := newSigningKey(t)
	status, body := ts.launcher(t, launcherKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": podUID, "thumbprint": thumbprintOf(t, podKey), "pod_token": ts.podToken(t, workerAccount, podUID),
	})
	if status != http.StatusCreated {
		t.Fatalf("the daemon's pod enrollment = %d %s, want 201", status, body)
	}
	pod := decode[wireEnrolled](t, body).EnrollmentID
	if status, body := ts.session(t, podKey, pod, http.MethodPost, "/v1/enrollments/"+pod+"/renew", map[string]any{"pod_token": ts.podToken(t, workerAccount, podUID)}); status != http.StatusOK {
		t.Fatalf("the pod's renewal with its token = %d %s, want 200", status, body)
	}
	status, body = ts.session(t, podKey, pod, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, podKey, ts.URL, "need it", "WORKER_TOKEN"), "session_id": nil})
	granted := decode[wireCreateRequestResponse](t, body)
	if status != http.StatusOK || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("the pod's request for WORKER_TOKEN = %d %s, want granted", status, body)
	}
	status, body = ts.session(t, podKey, pod, http.MethodPost, "/v1/grants/"+*granted.GrantID+"/values", nil)
	values := decode[struct {
		Values map[string]string `json:"values"`
	}](t, body)
	if status != http.StatusOK || values.Values["WORKER_TOKEN"] != "worker-v1" {
		t.Fatalf("the pod's grant values = %d %s, want WORKER_TOKEN=worker-v1", status, body)
	}
	box, boxKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	if status, body := ts.session(t, boxKey, box, http.MethodPost, "/v1/enrollments/"+box+"/renew", nil); status != http.StatusOK {
		t.Fatalf("a machine session's renewal = %d %s, want 200", status, body)
	}
	status, body = ts.launcher(t, launcherKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "container", "runtime_id": uuid.NewString(), "thumbprint": thumbprintOf(t, newSigningKey(t)),
	})
	if refusal := decode[wireError](t, body); status != http.StatusBadRequest || refusal.Code != "INVALID_KIND" || refusal.Error != `kind must be "session" or "pod"` {
		t.Fatalf("a kind the broker does not serve = %d %s, want 400 INVALID_KIND", status, body)
	}
}
