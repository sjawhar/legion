// unchanged_paths_test.go pins that the Google VM path changes no other: a broker with no Google
// audience serves pods, Kubernetes service logins and grants as it did, and refuses kind vm; and on
// a broker that trusts both issuers, a VM's identity token proves nothing a pod's token proves.
package api_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestWithNoGoogleAudienceEveryExistingPathBehavesAsBefore: on a broker with no VM verifier, the
// Legion daemon logs in with its Kubernetes token, its credential enrolls a pod, the pod renews
// with its own projected token, gets the shared agent-tier secret at once and reads its value, and
// a machine session's renewal with no body still renews. Kind vm is 400 INVALID_KIND, as any
// kind the broker does not serve is.
func TestWithNoGoogleAudienceEveryExistingPathBehavesAsBefore(t *testing.T) {
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
		"kind": "vm", "runtime_id": "example-fleet@example-project.iam.gserviceaccount.com@example-project/example-zone-a/1",
		"thumbprint": thumbprintOf(t, newSigningKey(t)), "vm_token": "not-a-token",
	})
	if refusal := decode[wireError](t, body); status != http.StatusBadRequest || refusal.Code != "INVALID_KIND" || refusal.Error != `kind must be "session" or "pod"` {
		t.Fatalf("kind vm on a broker with no Google audience = %d %s, want 400 INVALID_KIND", status, body)
	}
}

// TestAVMTokenPresentedAsAPodTokenIsRefusedForWantOfAPodBinding (the spec's A4, third clause): on a
// broker that trusts both issuers, a fleet VM's identity token presented as a pod's projected token
// enrolls no pod and renews none: it names no pod, and it is not the cluster's, so both are 403
// POD_IDENTITY_MISMATCH.
func TestAVMTokenPresentedAsAPodTokenIsRefusedForWantOfAPodBinding(t *testing.T) {
	ts := newVMTestServer(t)
	credentialID, launcherKey := ts.mintServiceLauncherCredential(t)
	vmToken := ts.current(t, member("example-zone-a", "1001"))
	status, body := ts.launcher(t, launcherKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": uuid.NewString(), "thumbprint": thumbprintOf(t, newSigningKey(t)), "pod_token": vmToken,
	})
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "POD_IDENTITY_MISMATCH" {
		t.Fatalf("a pod enrollment carrying a VM's token = %d %s, want 403 POD_IDENTITY_MISMATCH", status, body)
	}
	podUID := uuid.NewString()
	podKey := newSigningKey(t)
	status, body = ts.launcher(t, launcherKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": podUID, "thumbprint": thumbprintOf(t, podKey), "pod_token": ts.podToken(t, workerAccount, podUID),
	})
	if status != http.StatusCreated {
		t.Fatalf("a pod enrollment with its own token = %d %s, want 201", status, body)
	}
	pod := decode[wireEnrolled](t, body).EnrollmentID
	if status, body := ts.session(t, podKey, pod, http.MethodPost, "/v1/enrollments/"+pod+"/renew", map[string]any{"pod_token": vmToken}); status != http.StatusForbidden || decode[wireError](t, body).Code != "POD_IDENTITY_MISMATCH" {
		t.Fatalf("a pod's renewal carrying a VM's token = %d %s, want 403 POD_IDENTITY_MISMATCH", status, body)
	}
	if status, body := ts.session(t, podKey, pod, http.MethodPost, "/v1/enrollments/"+pod+"/renew", map[string]any{"vm_token": vmToken}); status != http.StatusForbidden || decode[wireError](t, body).Code != "VM_IDENTITY_MISMATCH" {
		t.Fatalf("a pod's renewal carrying a VM token as vm_token = %d %s, want 403 VM_IDENTITY_MISMATCH", status, body)
	}
}
