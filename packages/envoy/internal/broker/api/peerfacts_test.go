package api_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// wirePeer is the peer object GET /v1/credential-requests/{id} answers: what the kernel said
// about the process that asked, as the helper read it through the pidfd it had pinned.
type wirePeer struct {
	PID        int       `json:"pid"`
	Executable string    `json:"executable"`
	WorkingDir string    `json:"working_dir"`
	Cgroup     string    `json:"cgroup"`
	StartedAt  time.Time `json:"started_at"`
	ReadAt     time.Time `json:"read_at"`
}

// TestARecordAnswersTheKernelsFactsBesideTheProcesssOwnClaim: a request object signed with the
// facts the helper read answers them on the record, unchanged and each with when it was read,
// while the session id the asking process stated for itself stays where it was — a separate
// field the broker takes on the caller's word.
func TestARecordAnswersTheKernelsFactsBesideTheProcesssOwnClaim(t *testing.T) {
	ts := newTestServer(t)
	credID, machineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	box, boxKey := ts.enrolledBox(t, machineKey, credID, "box-"+t.Name())

	facts := record.PeerFacts{
		PID:        3224579,
		Executable: "/home/sami/.mise/installs/github-sjawhar-oh-my-pi/18.13.2/bin/omp",
		WorkingDir: "/home/sami/src/oss-verifier",
		Cgroup:     "0::/user.slice/user-1000.slice/session-3.scope",
		StartedAt:  1793402648,
		ReadAt:     1793402651,
	}
	compact, err := record.Sign(boxKey, ts.URL, []record.AuthorizationDetail{
		{Type: record.KindAgentSecret, Identifier: "DEEL_API_KEY", Actions: []string{"inject"}},
	}, "deel sync for ACME-1", "", nil, time.Now(), record.WithPeerFacts(facts))
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	status, body := ts.session(t, boxKey, box, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": "the-session-this-process-claims"})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d: %s", status, body)
	}
	created := decode[wireCreateRequestResponse](t, body)
	if created.State != "pending" || created.RecordID == nil {
		t.Fatalf("request = %+v, want pending on a record", created)
	}

	status, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+*created.RecordID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/credential-requests/{id} = %d: %s", status, body)
	}
	read := decode[struct {
		Peer    *wirePeer   `json:"peer"`
		Session wireSession `json:"session"`
	}](t, body)
	if read.Peer == nil {
		t.Fatalf("the record answers no peer: %s", body)
	}
	want := wirePeer{
		PID:        3224579,
		Executable: "/home/sami/.mise/installs/github-sjawhar-oh-my-pi/18.13.2/bin/omp",
		WorkingDir: "/home/sami/src/oss-verifier",
		Cgroup:     "0::/user.slice/user-1000.slice/session-3.scope",
		StartedAt:  time.Unix(1793402648, 0).UTC(),
		ReadAt:     time.Unix(1793402651, 0).UTC(),
	}
	if read.Peer.PID != want.PID || read.Peer.Executable != want.Executable ||
		read.Peer.WorkingDir != want.WorkingDir || read.Peer.Cgroup != want.Cgroup ||
		!read.Peer.StartedAt.Equal(want.StartedAt) || !read.Peer.ReadAt.Equal(want.ReadAt) {
		t.Fatalf("peer = %+v, want %+v", *read.Peer, want)
	}
	if read.Session.Request == nil || *read.Session.Request != "the-session-this-process-claims" {
		t.Fatalf("session = %+v, want the id the process stated, kept apart from the kernel's facts", read.Session)
	}
}

// TestARecordWithoutTheKernelsFactsAnswersNone: every record whose request object names no peer —
// a pod's, a box's, one signed before the claim existed — answers a null peer, so a page renders
// nothing rather than an empty row. The enrollment here is a box: it holds its own key, signs its
// own requests, and no helper pins anything for it.
func TestARecordWithoutTheKernelsFactsAnswersNone(t *testing.T) {
	ts := newTestServer(t)
	credID, machineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	box, boxKey := ts.enrolledBox(t, machineKey, credID, "box-"+t.Name())
	created := ts.requestAs(t, boxKey, box, "DEEL_API_KEY")
	if created.RecordID == nil {
		t.Fatalf("request = %+v, want a record", created)
	}
	status, body := ts.ui(t, http.MethodGet, "/v1/credential-requests/"+*created.RecordID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/credential-requests/{id} = %d: %s", status, body)
	}
	read := decode[struct {
		Peer *wirePeer `json:"peer"`
	}](t, body)
	if read.Peer != nil {
		t.Fatalf("peer = %+v, want none", *read.Peer)
	}
}

// TestAPodsPeerClaimIsRefused: a peer claim is a machine session helper's reading through the
// pidfd it pinned on its own unix socket. A pod has no helper and no such socket, and its key
// lives in the pod, so a pod's claim can only be a value something in the pod chose — and the
// page would render it as the kernel's answer. The broker refuses the request instead of storing
// it.
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
		Executable: "/home/sami/.mise/installs/github-sjawhar-oh-my-pi/18.13.2/bin/omp",
		WorkingDir: "/home/sami/src/oss-verifier",
		Cgroup:     "0::/user.slice/user-1000.slice/session-3.scope",
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
