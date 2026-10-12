// machine_logins_test.go drives GET /v1/launcher-credentials and POST
// /v1/launcher-credentials/{id}/revoke-by-approver, the machine-login routes Dispatch's page relays
// to, on the same real broker api_test.go mounts.
package api_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// capturingHandler records every slog record it is asked to handle, so a test can assert what the
// broker logged (as broker/wake/envoy_test.go does).
type capturingHandler struct {
	records *[]slog.Record
}

func (h capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h capturingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}

func (h capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h capturingHandler) WithGroup(string) slog.Handler { return h }

// wireLauncherCredential is one machine login in GET /v1/launcher-credentials's answer.
type wireLauncherCredential struct {
	CredentialID string    `json:"credential_id"`
	Host         string    `json:"host"`
	Service      *string   `json:"service"`
	Subject      *string   `json:"subject"`
	ApprovedBy   string    `json:"approved_by"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Expired      bool      `json:"expired"`
}

// machineLogins reads GET /v1/launcher-credentials?approver=<approver>, as Dispatch's server does
// for its signed-in person.
func (ts *testServer) machineLogins(t *testing.T, approver string) []wireLauncherCredential {
	t.Helper()
	status, body := ts.ui(t, http.MethodGet, "/v1/launcher-credentials?approver="+url.QueryEscape(approver), nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/launcher-credentials?approver=%s = %d: %s", approver, status, body)
	}
	return decode[struct {
		Credentials []wireLauncherCredential `json:"credentials"`
	}](t, body).Credentials
}

// enrollBox enrolls a box under launcher credential credentialID, signing the launcher proof with
// machineKey, and returns the broker's status and body, the box's enrollment id (when enrolled)
// and its own key.
func (ts *testServer) enrollBox(t *testing.T, machineKey *ecdsa.PrivateKey, credentialID, runtimeID string) (int, []byte, string, *ecdsa.PrivateKey) {
	t.Helper()
	key := newSigningKey(t)
	status, body := ts.launcher(t, machineKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "session", "runtime_id": runtimeID, "thumbprint": thumbprintOf(t, key),
	})
	return status, body, decode[wireEnrolled](t, body).EnrollmentID, key
}

// enrolledBox is enrollBox for a box that must enroll: it fails t on anything but 201.
func (ts *testServer) enrolledBox(t *testing.T, machineKey *ecdsa.PrivateKey, credentialID, runtimeID string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	status, body, enrollmentID, key := ts.enrollBox(t, machineKey, credentialID, runtimeID)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/enrollments = %d, want 201: %s", status, body)
	}
	return enrollmentID, key
}

// TestAPersonRevokesTheirOwnMachineLogin drives the machine-login routes Dispatch's page relays to:
// the person's live machine login is listed with its machine and lifetime and no service, under
// their email in any case and padding, as Dispatch may send it; another person, an unknown id, a
// malformed id and a missing name are refused; and the revoke by the person who approved it ends
// it at once — its session's proofs, its launcher proofs, its grant and its pending request all
// go, and another person's machine login stays.
func TestAPersonRevokesTheirOwnMachineLogin(t *testing.T) {
	ts := newTestServer(t)
	credentialID, machineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	otherID, _ := ts.mintLauncherCredential(t, "bob@example.com", "example-host-laptop")
	enrollmentID, sessionKey := ts.enrolledBox(t, machineKey, credentialID, "box-"+t.Name())
	request := func(name string) wireCreateRequestResponse {
		t.Helper()
		status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
			map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "need it", name), "session_id": nil})
		if status != http.StatusOK {
			t.Fatalf("POST /v1/requests (%s) = %d: %s", name, status, body)
		}
		return decode[wireCreateRequestResponse](t, body)
	}
	if granted := request("WORKER_TOKEN"); granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("request WORKER_TOKEN = %+v, want granted at once", granted)
	}
	pending := request("DEEL_API_KEY")
	if pending.State != "pending" || pending.RecordID == nil {
		t.Fatalf("request DEEL_API_KEY = %+v, want pending on its owner", pending)
	}

	logins := ts.machineLogins(t, testApprover)
	if len(logins) != 1 || logins[0].CredentialID != credentialID || logins[0].Host != "example-host-devbox" || logins[0].Service != nil ||
		logins[0].Expired || logins[0].ExpiresAt.Sub(logins[0].IssuedAt).Round(time.Minute) != 7*24*time.Hour {
		t.Fatalf("machine logins of %s = %+v, want the devbox's, unexpired, issued for a week", testApprover, logins)
	}
	if again := ts.machineLogins(t, " \t"+strings.ToUpper(testApprover)+" "); !slices.Equal(again, logins) {
		t.Fatalf("machine logins of %s in capitals and padding = %+v, want the same as %+v", testApprover, again, logins)
	}

	revokePath := "/v1/launcher-credentials/" + credentialID + "/revoke-by-approver"
	for _, tc := range []struct {
		name, path string
		body       any
		status     int
		code       string
	}{
		{"by another person", revokePath, map[string]any{"approver": "bob@example.com"}, http.StatusForbidden, "NOT_APPROVER"},
		{"naming no one", revokePath, map[string]any{}, http.StatusBadRequest, "APPROVER_REQUIRED"},
		{"of an unknown id", "/v1/launcher-credentials/" + uuid.NewString() + "/revoke-by-approver", map[string]any{"approver": testApprover}, http.StatusNotFound, "NOT_FOUND"},
		{"of a malformed id", "/v1/launcher-credentials/not-a-uuid/revoke-by-approver", map[string]any{"approver": testApprover}, http.StatusBadRequest, "CREDENTIAL_ID_INPUT"},
	} {
		status, body := ts.ui(t, http.MethodPost, tc.path, tc.body)
		if status != tc.status || decode[wireError](t, body).Code != tc.code {
			t.Fatalf("revoke %s = %d %s, want %d %s", tc.name, status, body, tc.status, tc.code)
		}
	}
	if status, body := ts.req(t, http.MethodPost, revokePath, nil, map[string]any{"approver": testApprover}); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "UI_INVALID" {
		t.Fatalf("revoke without the UI bearer = %d %s, want 401 UI_INVALID", status, body)
	}
	if status, body := ts.ui(t, http.MethodGet, "/v1/launcher-credentials", nil); status != http.StatusBadRequest || decode[wireError](t, body).Code != "APPROVER_REQUIRED" {
		t.Fatalf("list naming no one = %d %s, want 400 APPROVER_REQUIRED", status, body)
	}
	if status, body := ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/enrollments/self", nil); status != http.StatusOK {
		t.Fatalf("GET /v1/enrollments/self after the refused revokes = %d, want 200: %s", status, body)
	}

	status, body := ts.ui(t, http.MethodPost, revokePath, map[string]any{"approver": testApprover})
	if status != http.StatusOK || decode[stateBody](t, body).State != "revoked" {
		t.Fatalf("revoke by the person who approved it = %d %s, want 200 revoked", status, body)
	}

	if status, body := ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/enrollments/self", nil); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "PROOF_INVALID" {
		t.Fatalf("GET /v1/enrollments/self after the revoke = %d %s, want 401 PROOF_INVALID", status, body)
	}
	if status, body, _, _ := ts.enrollBox(t, machineKey, credentialID, "box-after-"+t.Name()); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("POST /v1/enrollments after the revoke = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	_, body = ts.ui(t, http.MethodGet, "/v1/pending?approver="+testApprover, nil)
	if left := decode[struct {
		Pending []wirePendingEntry `json:"pending"`
	}](t, body).Pending; len(left) != 0 {
		t.Fatalf("pending list after the revoke = %+v, want the session's request gone", left)
	}
	_, body = ts.ui(t, http.MethodGet, "/v1/grants?approver="+testApprover, nil)
	if live := decode[struct {
		Grants []wireApproverGrant `json:"grants"`
	}](t, body).Grants; len(live) != 0 {
		t.Fatalf("live grants after the revoke = %+v, want the session's grant gone", live)
	}
	if logins := ts.machineLogins(t, testApprover); len(logins) != 0 {
		t.Fatalf("machine logins of %s after the revoke = %+v, want none", testApprover, logins)
	}
	if logins := ts.machineLogins(t, "bob@example.com"); len(logins) != 1 || logins[0].CredentialID != otherID {
		t.Fatalf("machine logins of bob@example.com = %+v, want his own, untouched", logins)
	}
	if status, body := ts.ui(t, http.MethodPost, revokePath, map[string]any{"approver": testApprover}); status != http.StatusOK {
		t.Fatalf("revoking it again = %d %s, want 200", status, body)
	}
}

// TestAnyoneRevokesAServiceLogin: a service login a person approved before token logins, which has
// no operator and is still in its lifetime, keeps working: it is listed for every signed-in person,
// named by its service and by the person who approved it, and enrolls pods; the sentinel's revoke is
// refused and ends nothing; a second person's revoke, bob's, who approved nothing, ends it and every
// pod it enrolled: each pod's renew and grant values are refused, its launcher enrolls no pod more,
// it leaves everyone's list, and the audit row names bob, the service and both pods.
func TestAnyoneRevokesAServiceLogin(t *testing.T) {
	ts := newTestServer(t)
	credentialID, launcherKey := ts.mintOutstandingServiceCredential(t, testApprover)
	podUID := uuid.NewString()
	token := ts.podToken(t, workerAccount, podUID)
	enrollPod := func(slot string) (int, []byte, podSession) {
		t.Helper()
		key := newSigningKey(t)
		status, body := ts.launcher(t, launcherKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": "pod", "runtime_id": podUID, "slot": slot, "thumbprint": thumbprintOf(t, key), "pod_token": token,
		})
		return status, body, podSession{slot: slot, enrollmentID: decode[wireEnrolled](t, body).EnrollmentID, key: key}
	}
	pods := map[string]podSession{}
	grants := map[string]string{}
	for _, slot := range []string{"implementer-g1", "reviewer-g1"} {
		status, body, pod := enrollPod(slot)
		if status != http.StatusCreated {
			t.Fatalf("enroll pod slot %s = %d, want 201: %s", slot, status, body)
		}
		granted := pod.request(t, ts, "WORKER_TOKEN")
		if granted.State != "granted" || granted.GrantID == nil {
			t.Fatalf("pod %s's request = %+v, want WORKER_TOKEN granted at once", slot, granted)
		}
		pods[slot], grants[slot] = pod, *granted.GrantID
	}
	renew := func(pod podSession) (int, []byte) {
		t.Helper()
		return ts.session(t, pod.key, pod.enrollmentID, http.MethodPost, "/v1/enrollments/"+pod.enrollmentID+"/renew", map[string]any{"pod_token": token})
	}
	values := func(slot string) (int, []byte) {
		t.Helper()
		pod := pods[slot]
		return ts.session(t, pod.key, pod.enrollmentID, http.MethodPost, "/v1/grants/"+grants[slot]+"/values", nil)
	}

	for _, person := range []string{testApprover, "bob@example.com"} {
		logins := ts.machineLogins(t, person)
		if len(logins) != 1 || logins[0].CredentialID != credentialID || logins[0].Host != "cluster.example" ||
			logins[0].Service == nil || *logins[0].Service != "legion-daemon" || logins[0].ApprovedBy != testApprover {
			t.Fatalf("machine logins of %s = %+v, want legion-daemon's login on cluster.example, approved by %s", person, logins, testApprover)
		}
	}

	revokePath := "/v1/launcher-credentials/" + credentialID + "/revoke-by-approver"
	if status, body := ts.ui(t, http.MethodPost, revokePath, map[string]any{"approver": " Anyone "}); status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
		t.Fatalf("revoke by the sentinel = %d %s, want 403 NOT_APPROVER", status, body)
	}
	for slot, pod := range pods {
		if status, body := renew(pod); status != http.StatusOK {
			t.Fatalf("pod %s's renew after the refused revoke = %d %s, want 200", slot, status, body)
		}
		if status, body := values(slot); status != http.StatusOK {
			t.Fatalf("pod %s's values after the refused revoke = %d %s, want 200", slot, status, body)
		}
	}

	if status, body := ts.ui(t, http.MethodPost, revokePath, map[string]any{"approver": "bob@example.com"}); status != http.StatusOK || decode[stateBody](t, body).State != "revoked" {
		t.Fatalf("revoke by bob, who approved nothing = %d %s, want 200 revoked", status, body)
	}
	for slot, pod := range pods {
		if status, body := renew(pod); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "ENROLLMENT_NOT_LIVE" {
			t.Fatalf("pod %s's renew after the revoke = %d %s, want 401 ENROLLMENT_NOT_LIVE", slot, status, body)
		}
		if status, body := values(slot); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "PROOF_INVALID" {
			t.Fatalf("pod %s's values after the revoke = %d %s, want 401 PROOF_INVALID", slot, status, body)
		}
	}
	if status, body, _ := enrollPod("tester-g1"); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("enroll a pod after the revoke = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	for _, person := range []string{testApprover, "bob@example.com"} {
		if logins := ts.machineLogins(t, person); len(logins) != 0 {
			t.Fatalf("machine logins of %s after the revoke = %+v, want none", person, logins)
		}
	}
	var actor, service string
	var ended int
	if err := ts.Store.Pool.QueryRow(context.Background(), `select actor, detail->>'service', jsonb_array_length(detail->'enrollments') from audit
		where kind='launcher_credential.revoked' and detail->>'credential_id'=$1`, credentialID).Scan(&actor, &service, &ended); err != nil {
		t.Fatalf("read the launcher_credential.revoked audit row: %v", err)
	}
	if actor != "human:bob@example.com" || service != "legion-daemon" || ended != 2 {
		t.Fatalf("audit row: actor %s, service %s, %d enrollments; want human:bob@example.com, legion-daemon, 2", actor, service, ended)
	}
}

// TestOnlyTheLoginThatEnrolledAPodDeletesItsEnrollment: DELETE /v1/enrollments/{id} on a pod
// enrollment by a second service login, another daemon's token login, is 403
// NOT_ENROLLING_CREDENTIAL and ends nothing (the pod still renews); the login that enrolled the pod
// ends it (204), and its renew is refused from then on.
func TestOnlyTheLoginThatEnrolledAPodDeletesItsEnrollment(t *testing.T) {
	ts := newTestServer(t)
	enrolling, enrollingKey := ts.mintServiceLauncherCredential(t)
	other, otherKey := ts.mintServiceLauncherCredentialAs(t, otherDaemonAccount)
	podUID := uuid.NewString()
	key := newSigningKey(t)
	status, body := ts.launcher(t, enrollingKey, enrolling, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": podUID, "slot": "implementer-g1", "thumbprint": thumbprintOf(t, key),
		"pod_token": ts.podToken(t, workerAccount, podUID),
	})
	if status != http.StatusCreated {
		t.Fatalf("enroll the pod = %d %s, want 201", status, body)
	}
	id := decode[wireEnrolled](t, body).EnrollmentID
	renew := func() int {
		t.Helper()
		status, _ := ts.session(t, key, id, http.MethodPost, "/v1/enrollments/"+id+"/renew", map[string]any{"pod_token": ts.podToken(t, workerAccount, podUID)})
		return status
	}

	status, body = ts.launcher(t, otherKey, other, http.MethodDelete, "/v1/enrollments/"+id, nil)
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_ENROLLING_CREDENTIAL" {
		t.Fatalf("DELETE by another service login = %d %s, want 403 NOT_ENROLLING_CREDENTIAL", status, body)
	}
	if status := renew(); status != http.StatusOK {
		t.Fatalf("the pod's renew after the refused DELETE = %d, want 200", status)
	}
	if status, body := ts.launcher(t, enrollingKey, enrolling, http.MethodDelete, "/v1/enrollments/"+id, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE by the enrolling login = %d %s, want 204", status, body)
	}
	if status := renew(); status != http.StatusUnauthorized {
		t.Fatalf("the pod's renew after its enrolling login's DELETE = %d, want 401", status)
	}
}

// TestAPodRenewsCarryingItsCurrentToken: POST /v1/enrollments/{id}/renew on a pod enrollment with
// a body naming another pod's or account's projected token is 403 POD_IDENTITY_MISMATCH, and with
// the pod's own token past its expiry 403 POD_TOKEN_EXPIRED. A new pod enrollment renews with no
// body, and the broker logs each such renewal, naming it, until it first renews with its own token
// (200); from then on a renewal with no body is 403 POD_TOKEN_REQUIRED, and so is one from a second
// login's enrollment of the same pod UID. A machine session sending a pod token is 403
// POD_IDENTITY_MISMATCH.
func TestAPodRenewsCarryingItsCurrentToken(t *testing.T) {
	ts := newTestServer(t)
	cred, credKey := ts.mintServiceLauncherCredential(t)
	enrollPod := func(t *testing.T) (string, string, *ecdsa.PrivateKey) {
		t.Helper()
		podUID, key := uuid.NewString(), newSigningKey(t)
		status, body := ts.launcher(t, credKey, cred, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": "pod", "runtime_id": podUID, "slot": "implementer-g1", "thumbprint": thumbprintOf(t, key),
			"pod_token": ts.podToken(t, workerAccount, podUID),
		})
		if status != http.StatusCreated {
			t.Fatalf("enroll the pod = %d %s, want 201", status, body)
		}
		return decode[wireEnrolled](t, body).EnrollmentID, podUID, key
	}
	renew := func(t *testing.T, key *ecdsa.PrivateKey, id string, body any) (int, string) {
		t.Helper()
		status, raw := ts.session(t, key, id, http.MethodPost, "/v1/enrollments/"+id+"/renew", body)
		if status == http.StatusOK {
			return status, ""
		}
		return status, decode[wireError](t, raw).Code
	}

	t.Run("a new pod renews without a token, logged, until it renews with its own token", func(t *testing.T) {
		id, podUID, key := enrollPod(t)
		var records []slog.Record
		previous := slog.Default()
		slog.SetDefault(slog.New(capturingHandler{records: &records}))
		t.Cleanup(func() { slog.SetDefault(previous) })
		for range 2 {
			if status, code := renew(t, key, id, nil); status != http.StatusOK {
				t.Fatalf("renew with no body, a new pod enrollment = %d %s, want 200", status, code)
			}
		}
		slog.SetDefault(previous)
		warned := 0
		for _, r := range records {
			attrs := map[string]string{}
			r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
			if r.Level == slog.LevelWarn && r.Message == "broker: pod enrollment renewed without its projected token" &&
				attrs["enrollment"] == id && attrs["pod_uid"] == podUID && attrs["slot"] == "implementer-g1" {
				warned++
			}
		}
		if warned != 2 {
			t.Fatalf("WARN lines naming the enrollment, its pod and slot = %d of 2 token-less renewals (%d records)", warned, len(records))
		}
		for name, token := range map[string]string{
			"another pod's token":             ts.podToken(t, workerAccount, uuid.NewString()),
			"another service account's token": ts.podToken(t, "system:serviceaccount:legion:other", podUID),
		} {
			if status, code := renew(t, key, id, map[string]any{"pod_token": token}); status != http.StatusForbidden || code != "POD_IDENTITY_MISMATCH" {
				t.Fatalf("renew with %s = %d %s, want 403 POD_IDENTITY_MISMATCH", name, status, code)
			}
		}
		expiry := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
		claims := ts.podIssuer.Claims(workerAccount, podAudience)
		claims["iat"], claims["exp"] = expiry.Add(-10*time.Minute).Unix(), expiry.Unix()
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
		if status, code := renew(t, key, id, map[string]any{"pod_token": ts.podIssuer.Mint(t, ts.podKey, claims)}); status != http.StatusForbidden || code != "POD_TOKEN_EXPIRED" {
			t.Fatalf("renew with the pod's own token past its expiry = %d %s, want 403 POD_TOKEN_EXPIRED", status, code)
		}
		if status, code := renew(t, key, id, nil); status != http.StatusOK {
			t.Fatalf("renew with no body after refused tokens = %d %s, want 200 (a refused token sets nothing)", status, code)
		}
		if status, code := renew(t, key, id, map[string]any{"pod_token": ts.podToken(t, workerAccount, podUID)}); status != http.StatusOK {
			t.Fatalf("renew with the pod's own token = %d %s, want 200", status, code)
		}
		records = nil
		slog.SetDefault(slog.New(capturingHandler{records: &records}))
		if status, code := renew(t, key, id, nil); status != http.StatusForbidden || code != "POD_TOKEN_REQUIRED" {
			t.Fatalf("renew with no body after renewing with the token = %d %s, want 403 POD_TOKEN_REQUIRED", status, code)
		}
		slog.SetDefault(previous)
		refusedWarn := 0
		for _, r := range records {
			attrs := map[string]string{}
			r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
			if r.Level == slog.LevelWarn && r.Message == "broker: pod enrollment renewal refused: projected token required" && attrs["enrollment"] == id {
				refusedWarn++
			}
		}
		if refusedWarn != 1 {
			t.Fatalf("WARN lines naming the token-required refusal's enrollment = %d of 1 (%d records)", refusedWarn, len(records))
		}
	})

	t.Run("a second login's enrollment of a pod UID is refused token-less once the pod renewed with its token", func(t *testing.T) {
		id, podUID, key := enrollPod(t)
		if status, code := renew(t, key, id, map[string]any{"pod_token": ts.podToken(t, workerAccount, podUID)}); status != http.StatusOK {
			t.Fatalf("the pod's token renewal = %d %s, want 200", status, code)
		}
		other, otherKey := ts.mintServiceLauncherCredentialAs(t, otherDaemonAccount)
		copyKey := newSigningKey(t)
		status, body := ts.launcher(t, otherKey, other, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": "pod", "runtime_id": podUID, "slot": "implementer-g1", "thumbprint": thumbprintOf(t, copyKey),
			"pod_token": ts.podToken(t, workerAccount, podUID),
		})
		if status != http.StatusCreated {
			t.Fatalf("enroll the copy under a second login = %d %s, want 201", status, body)
		}
		copied := decode[wireEnrolled](t, body).EnrollmentID
		if status, code := renew(t, copyKey, copied, nil); status != http.StatusForbidden || code != "POD_TOKEN_REQUIRED" {
			t.Fatalf("the copy's token-less renewal = %d %s, want 403 POD_TOKEN_REQUIRED", status, code)
		}
	})

	t.Run("a machine session sending a pod token is refused", func(t *testing.T) {
		login, loginKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
		key := newSigningKey(t)
		status, body := ts.launcher(t, loginKey, login, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": "session", "runtime_id": "box-" + uuid.NewString(), "thumbprint": thumbprintOf(t, key),
		})
		if status != http.StatusCreated {
			t.Fatalf("enroll the machine session = %d %s, want 201", status, body)
		}
		id := decode[wireEnrolled](t, body).EnrollmentID
		if status, code := renew(t, key, id, map[string]any{"pod_token": ts.podToken(t, workerAccount, uuid.NewString())}); status != http.StatusForbidden || code != "POD_IDENTITY_MISMATCH" {
			t.Fatalf("a box's renew carrying a pod token = %d %s, want 403 POD_IDENTITY_MISMATCH", status, code)
		}
	})
}

// TestAPersonsPendingMachineLoginWaitsInTheirsAlone: a person's pending machine login is in their
// own pending list and no one else's, and a service's device-code login opens nothing to wait in
// anyone's.
func TestAPersonsPendingMachineLoginWaitsInTheirsAlone(t *testing.T) {
	ts := newTestServer(t)
	status, body := ts.req(t, http.MethodPost, "/v1/launcher-credentials", nil,
		map[string]any{"request": signMachineLoginRequest(t, newSigningKey(t), ts.URL, testApprover, "example-host-devbox", "")})
	if status != http.StatusAccepted {
		t.Fatalf("POST /v1/launcher-credentials = %d: %s", status, body)
	}
	code := decode[struct {
		Code string `json:"code"`
	}](t, body).Code
	_, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	personal := decode[wireRecord](t, body).RecordID
	if status, body := ts.req(t, http.MethodPost, "/v1/launcher-credentials", nil,
		map[string]any{"request": signMachineLoginRequest(t, newSigningKey(t), ts.URL, "", "cluster.example", "legion-daemon")}); status != http.StatusBadRequest {
		t.Fatalf("a service's device-code login = %d %s, want 400", status, body)
	}
	pendingOf := func(person string) []string {
		t.Helper()
		status, body := ts.ui(t, http.MethodGet, "/v1/pending?approver="+url.QueryEscape(person), nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/pending?approver=%s = %d: %s", person, status, body)
		}
		var ids []string
		for _, p := range decode[struct {
			Pending []wirePendingEntry `json:"pending"`
		}](t, body).Pending {
			ids = append(ids, p.RecordID)
		}
		return ids
	}
	if got := pendingOf(testApprover); !slices.Equal(got, []string{personal}) {
		t.Fatalf("pending of %s = %v, want their own machine's login %s alone", testApprover, got, personal)
	}
	if got := pendingOf("bob@example.com"); len(got) != 0 {
		t.Fatalf("pending of bob@example.com = %v, want none", got)
	}
}

// mintOutstandingServiceCredential writes a service login a person approved on a broker before
// token logins — its launcher_credential record naming anyone, the credential its approval minted,
// and the approved event recording approvedBy — as one still in its lifetime when this broker
// deployed, and returns the credential's id and key.
func (ts *testServer) mintOutstandingServiceCredential(t *testing.T, approvedBy string) (credentialID string, key *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	key = newSigningKey(t)
	body := record.Body{
		Request:         signMachineLoginRequest(t, key, ts.URL, "", "cluster.example", "legion-daemon"),
		Approver:        record.AnyoneApprover,
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-"},
		LifetimeSeconds: new(int((7 * 24 * time.Hour).Seconds())),
		RulesVersion:    "pre-token-logins",
		ExpiresAt:       time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second),
		Code:            "ABCD-EFGH",
	}
	if _, err := ts.Store.Pool.Exec(ctx, `insert into credential_requests (id, body, kind, approver, code, expires_at) values ($1,$2,'launcher_credential',$3,$4,$5)`,
		body.ID(), body.Canonical(), body.Approver, body.Code, body.ExpiresAt); err != nil {
		t.Fatalf("insert the service login's record: %v", err)
	}
	id := uuid.New()
	if _, err := ts.Store.Pool.Exec(ctx, `insert into launcher_credentials (id, service, host, key_thumbprint, public_jwk, record_id, expires_at)
		values ($1,'legion-daemon','cluster.example',$2,'{}'::jsonb,$3, now() + interval '6 days')`, id, thumbprintOf(t, key), body.ID()); err != nil {
		t.Fatalf("insert the credential its approval minted: %v", err)
	}
	if _, err := ts.Store.Pool.Exec(ctx, `insert into credential_request_events (record_id, event, login, credential_id, actor) values ($1,'approved',$2,$3,$4)`,
		body.ID(), approvedBy, id.String(), record.HumanActor(approvedBy)); err != nil {
		t.Fatalf("record its approval: %v", err)
	}
	return id.String(), key
}

// TestAPendingServiceLoginFromBeforeTokenLoginsIsNeverApproved: a service's device-code login a
// broker opened before token logins, still pending, is refused 400 SERVICE_LOGS_IN_WITH_TOKEN to anyone who
// approves it, and mints nothing; anyone signed in may still deny it.
func TestAPendingServiceLoginFromBeforeTokenLoginsIsNeverApproved(t *testing.T) {
	ts := newTestServer(t)
	body := record.Body{
		Request:         signMachineLoginRequest(t, newSigningKey(t), ts.URL, "", "cluster.example", "legion-daemon"),
		Approver:        record.AnyoneApprover,
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-"},
		LifetimeSeconds: new(int((7 * 24 * time.Hour).Seconds())),
		RulesVersion:    "pre-token-logins",
		ExpiresAt:       time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second),
		Code:            "WXYZ-ABCD",
	}
	if _, err := ts.Store.Pool.Exec(context.Background(), `insert into credential_requests (id, body, kind, approver, code, expires_at) values ($1,$2,'launcher_credential',$3,$4,$5)`,
		body.ID(), body.Canonical(), body.Approver, body.Code, body.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	status, answer := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+body.ID()+"/approve", map[string]any{"approver": "bob@example.com", "code": body.Code})
	if status != http.StatusBadRequest || decode[wireError](t, answer).Code != "SERVICE_LOGS_IN_WITH_TOKEN" {
		t.Fatalf("approve a pending service login = %d %s, want 400 SERVICE_LOGS_IN_WITH_TOKEN", status, answer)
	}
	if got := ts.column(t, `select count(*)::text from launcher_credentials`); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("launcher credentials = %v, want none", got)
	}
	status, answer = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+body.ID()+"/deny", map[string]any{"approver": "bob@example.com", "code": body.Code})
	if status != http.StatusOK || decode[stateBody](t, answer).State != "denied" {
		t.Fatalf("deny it = %d %s, want 200 denied", status, answer)
	}
}

// stateBody is a {"state"} answer.
type stateBody struct {
	State string `json:"state"`
}

// TestAnEnrollmentRacingItsMachineLoginsRevokeIsLauncherInvalid: a launcher proof verified just
// before its machine login's revoke commits does not land an enrollment. The enrollment waits on
// the credential's row, which the revoke holds (here a transaction holding it as
// enroll.Service.RevokeCredential does, with revoked_at set), and is answered 401 LAUNCHER_INVALID
// once the revoke commits, the refusal the helper drops its credential on.
func TestAnEnrollmentRacingItsMachineLoginsRevokeIsLauncherInvalid(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	credentialID, machineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	tx, err := ts.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from launcher_credentials where id=$1 for no key update`, credentialID); err != nil {
		t.Fatalf("lock the credential: %v", err)
	}
	if _, err := tx.Exec(ctx, `update launcher_credentials set revoked_at=now() where id=$1`, credentialID); err != nil {
		t.Fatalf("revoke the credential: %v", err)
	}

	sessionKey := newSigningKey(t)
	thumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	launcherProof, err := proof.SignLauncher(machineKey, credentialID, http.MethodPost, ts.URL+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatalf("proof.SignLauncher: %v", err)
	}
	payload, err := json.Marshal(map[string]any{"kind": "session", "runtime_id": "session-" + t.Name(), "thumbprint": thumbprint})
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		request, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/enrollments", bytes.NewReader(payload))
		if err != nil {
			done <- answer{err: err}
			return
		}
		request.Header.Set("Proof", launcherProof)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			done <- answer{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		done <- answer{response.StatusCode, body, err}
	}()

	storetest.AwaitLockWaiters(t, ts.Store.Pool, tx, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the revoke: %v", err)
	}
	got := <-done
	if got.err != nil || got.status != http.StatusUnauthorized || decode[wireError](t, got.body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("POST /v1/enrollments racing the revoke = %d %s (%v), want 401 LAUNCHER_INVALID", got.status, got.body, got.err)
	}
	var n int
	if err := ts.Store.Pool.QueryRow(ctx, `select count(*) from enrollments where launcher_credential_id=$1`, credentialID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("enrollments under the revoked credential = %d (%v), want none", n, err)
	}
}

// TestRevokingEveryListedLoginEndsEverySessionTheMachineStarted: a machine login that reaches its
// expiry does not end the sessions it enrolled — a box renews itself with its own key and keeps
// reading its grant — and the machine then logs in again (weekly). The person who approved both
// logins opens the page for a lost or compromised machine and revokes every login it lists. Every
// session the machine started must end, the box enrolled under the expired login included: so the
// list shows that login, marked expired, while one of its sessions is live.
func TestRevokingEveryListedLoginEndsEverySessionTheMachineStarted(t *testing.T) {
	ts := newTestServer(t)
	oldLogin, oldMachineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	oldBox, oldBoxKey := ts.enrolledBox(t, oldMachineKey, oldLogin, "box-old-"+t.Name())
	status, body := ts.session(t, oldBoxKey, oldBox, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, oldBoxKey, ts.URL, "need it", "WORKER_TOKEN"), "session_id": nil})
	granted := decode[wireCreateRequestResponse](t, body)
	if status != http.StatusOK || granted.GrantID == nil {
		t.Fatalf("request = %d %s, want an automatic grant", status, body)
	}
	if _, err := ts.Store.Pool.Exec(context.Background(), `update launcher_credentials set expires_at = now() - interval '1 minute' where id=$1`, oldLogin); err != nil {
		t.Fatal(err)
	}
	if status, body := ts.session(t, oldBoxKey, oldBox, http.MethodPost, "/v1/enrollments/"+oldBox+"/renew", nil); status != http.StatusOK {
		t.Fatalf("the old box's renew after its login expired = %d %s; this test assumes it still renews", status, body)
	}
	newLogin, newMachineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	newBox, newBoxKey := ts.enrolledBox(t, newMachineKey, newLogin, "box-new-"+t.Name())

	logins := ts.machineLogins(t, testApprover)
	if len(logins) != 2 || logins[0].CredentialID != newLogin || logins[0].Expired || logins[1].CredentialID != oldLogin || !logins[1].Expired {
		t.Fatalf("machine logins of %s = %+v, want the new login, then the old one marked expired (old %s, new %s)", testApprover, logins, oldLogin, newLogin)
	}
	for _, l := range logins {
		if status, body := ts.ui(t, http.MethodPost, "/v1/launcher-credentials/"+l.CredentialID+"/revoke-by-approver", map[string]any{"approver": testApprover}); status != http.StatusOK {
			t.Fatalf("revoke listed login %s = %d %s", l.CredentialID, status, body)
		}
	}
	if status, _ := ts.session(t, newBoxKey, newBox, http.MethodPost, "/v1/enrollments/"+newBox+"/renew", nil); status != http.StatusUnauthorized {
		t.Fatalf("the new box's renew after revoking every listed login = %d, want 401", status)
	}
	if status, body := ts.session(t, oldBoxKey, oldBox, http.MethodPost, "/v1/enrollments/"+oldBox+"/renew", nil); status != http.StatusUnauthorized {
		t.Fatalf("the old box's renew after revoking every listed login = %d %s, want 401: the machine's session under its expired login is still live", status, body)
	}
	if status, body := ts.session(t, oldBoxKey, oldBox, http.MethodPost, "/v1/grants/"+*granted.GrantID+"/values", nil); status != http.StatusUnauthorized {
		t.Fatalf("the old box's grant values after revoking every listed login = %d %s, want 401", status, body)
	}
	if logins := ts.machineLogins(t, testApprover); len(logins) != 0 {
		t.Fatalf("machine logins of %s after revoking every listed one = %+v, want none", testApprover, logins)
	}
}
