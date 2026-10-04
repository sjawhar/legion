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
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// wireLauncherCredential is one machine login in GET /v1/launcher-credentials's answer.
type wireLauncherCredential struct {
	CredentialID string    `json:"credential_id"`
	Host         string    `json:"host"`
	Service      *string   `json:"service"`
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
		"kind": "box", "runtime_id": runtimeID, "thumbprint": thumbprintOf(t, key),
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

// TestAnApproverRevokesTheServiceLoginTheyApproved: the Legion daemon's service login, which has
// no operator, is listed for the person who approved it, named by its service, and for no one
// else; another person's revoke is refused and ends nothing; the approver's revoke ends it and
// every pod it enrolled: each pod's renew and grant values are refused, its launcher enrolls no
// pod more, it leaves the approver's list, and the audit row names the service and both pods.
func TestAnApproverRevokesTheServiceLoginTheyApproved(t *testing.T) {
	ts := newTestServer(t)
	credentialID, launcherKey := ts.mintServiceLauncherCredential(t)
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
		return ts.session(t, pod.key, pod.enrollmentID, http.MethodPost, "/v1/enrollments/"+pod.enrollmentID+"/renew", nil)
	}
	values := func(slot string) (int, []byte) {
		t.Helper()
		pod := pods[slot]
		return ts.session(t, pod.key, pod.enrollmentID, http.MethodPost, "/v1/grants/"+grants[slot]+"/values", nil)
	}

	logins := ts.machineLogins(t, testApprover)
	if len(logins) != 1 || logins[0].CredentialID != credentialID || logins[0].Host != "cluster.example" ||
		logins[0].Service == nil || *logins[0].Service != "legion-daemon" {
		t.Fatalf("machine logins of %s = %+v, want legion-daemon's login on cluster.example", testApprover, logins)
	}
	if logins := ts.machineLogins(t, "bob@example.com"); len(logins) != 0 {
		t.Fatalf("machine logins of bob@example.com = %+v, want none: he approved nothing", logins)
	}

	revokePath := "/v1/launcher-credentials/" + credentialID + "/revoke-by-approver"
	if status, body := ts.ui(t, http.MethodPost, revokePath, map[string]any{"approver": "bob@example.com"}); status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
		t.Fatalf("revoke by another person = %d %s, want 403 NOT_APPROVER", status, body)
	}
	for slot, pod := range pods {
		if status, body := renew(pod); status != http.StatusOK {
			t.Fatalf("pod %s's renew after the refused revoke = %d %s, want 200", slot, status, body)
		}
		if status, body := values(slot); status != http.StatusOK {
			t.Fatalf("pod %s's values after the refused revoke = %d %s, want 200", slot, status, body)
		}
	}

	if status, body := ts.ui(t, http.MethodPost, revokePath, map[string]any{"approver": testApprover}); status != http.StatusOK || decode[stateBody](t, body).State != "revoked" {
		t.Fatalf("revoke by the person who approved it = %d %s, want 200 revoked", status, body)
	}
	for slot, pod := range pods {
		if status, body := renew(pod); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "PROOF_INVALID" {
			t.Fatalf("pod %s's renew after the revoke = %d %s, want 401 PROOF_INVALID", slot, status, body)
		}
		if status, body := values(slot); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "PROOF_INVALID" {
			t.Fatalf("pod %s's values after the revoke = %d %s, want 401 PROOF_INVALID", slot, status, body)
		}
	}
	if status, body, _ := enrollPod("tester-g1"); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("enroll a pod after the revoke = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	if logins := ts.machineLogins(t, testApprover); len(logins) != 0 {
		t.Fatalf("machine logins of %s after the revoke = %+v, want none", testApprover, logins)
	}
	var actor, service string
	var ended int
	if err := ts.Store.Pool.QueryRow(context.Background(), `select actor, detail->>'service', jsonb_array_length(detail->'enrollments') from audit
		where kind='launcher_credential.revoked' and detail->>'credential_id'=$1`, credentialID).Scan(&actor, &service, &ended); err != nil {
		t.Fatalf("read the launcher_credential.revoked audit row: %v", err)
	}
	if actor != "human:"+testApprover || service != "legion-daemon" || ended != 2 {
		t.Fatalf("audit row: actor %s, service %s, %d enrollments; want human:%s, legion-daemon, 2", actor, service, ended, testApprover)
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
	payload, err := json.Marshal(map[string]any{"kind": "box", "runtime_id": "box-" + t.Name(), "thumbprint": thumbprint})
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
