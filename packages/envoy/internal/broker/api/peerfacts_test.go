package api_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// TestAPodsPeerClaimIsRefused: a peer claim is a machine session helper's reading through the
// pidfd it pinned on its own unix socket. A pod has no helper and no such socket, and its key
// lives in the pod, so a pod's claim can only be a value something in the pod chose — and the
// approval page renders a peer claim as the kernel's answer about the asking process. The broker
// refuses the request instead of storing it.
func TestAPodsPeerClaimIsRefused(t *testing.T) {
	ts := newTestServer(t)
	cred, credKey := ts.mintServiceLauncherCredential(t)
	podUID, podKey := uuid.NewString(), newSigningKey(t)
	status, body := ts.launcher(t, credKey, cred, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": podUID, "slot": "implementer-g1", "thumbprint": thumbprintOf(t, podKey),
		"pod_token": ts.podToken(t, workerAccount, podUID),
	})
	if status != http.StatusCreated {
		t.Fatalf("enroll the pod = %d %s, want 201", status, body)
	}
	pod := decode[wireEnrolled](t, body).EnrollmentID

	compact, err := record.Sign(podKey, ts.URL, []record.AuthorizationDetail{
		{Type: record.KindAgentSecret, Identifier: "DEEL_API_KEY", Actions: []string{"inject"}},
	}, "a pod naming facts no helper read", "", nil, time.Now(), record.WithPeerFacts(record.PeerFacts{
		PID:        3224579,
		Executable: "/opt/legion/bin/omp",
		WorkingDir: "/legion/workspace",
		Cgroup:     "0::/kubepods/burstable/pod-example/container",
		StartedAt:  1793402648,
		ReadAt:     1793402651,
	}))
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	status, body = ts.session(t, podKey, pod, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusBadRequest || decode[wireError](t, body).Code != "REQUEST_INVALID" {
		t.Fatalf("a pod's request naming peer facts = %d %s, want 400 REQUEST_INVALID", status, body)
	}
	if got := ts.column(t, `select count(*)::text from credential_requests`); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("credential_requests = %v, want no record written", got)
	}
}
