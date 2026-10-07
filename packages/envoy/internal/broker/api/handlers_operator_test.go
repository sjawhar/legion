// handlers_operator_test.go drives the operator self-service routes (/v1/operator/*), which a
// person's machine login calls with its launcher proof, on the same real broker api_test.go
// mounts: each list answers what Dispatch's own page answers for the credential's operator, each
// revoke acts as that operator and records the calling machine login as its actor, and a
// service's login, which has no operator, is refused every one of them.
package api_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// operatorGet reads an operator route's list under launcher credential credentialID, failing t on
// anything but 200, and returns the body as the broker sent it.
func (ts *testServer) operatorGet(t *testing.T, key *ecdsa.PrivateKey, credentialID, path string) []byte {
	t.Helper()
	status, body := ts.launcher(t, key, credentialID, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, status, body)
	}
	return body
}

// uiGet reads a UI route's list as Dispatch's server does for its signed-in person, failing t on
// anything but 200, and returns the body as the broker sent it.
func (ts *testServer) uiGet(t *testing.T, path, approver string) []byte {
	t.Helper()
	status, body := ts.ui(t, http.MethodGet, path+"?approver="+url.QueryEscape(approver), nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s?approver=%s = %d: %s", path, approver, status, body)
	}
	return body
}

// requestAs signs an agent_secret request object for names with an enrolled session's own key and
// posts it as that session.
func (ts *testServer) requestAs(t *testing.T, key *ecdsa.PrivateKey, enrollmentID string, names ...string) wireCreateRequestResponse {
	t.Helper()
	status, body := ts.session(t, key, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, key, ts.URL, "need it", names...), "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests %v = %d: %s", names, status, body)
	}
	return decode[wireCreateRequestResponse](t, body)
}

// approveAs approves a pending agent_secret request's record through the UI route as approver and
// returns the grant it made.
func (ts *testServer) approveAs(t *testing.T, req wireCreateRequestResponse, approver string) string {
	t.Helper()
	if req.State != "pending" || req.RecordID == nil {
		t.Fatalf("request = %+v, want pending on a record", req)
	}
	status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+*req.RecordID+"/approve", map[string]any{"approver": approver})
	approved := decode[wireCreateRequestResponse](t, body)
	if status != http.StatusOK || approved.GrantID == nil {
		t.Fatalf("approve %s as %s = %d %s, want a grant", *req.RecordID, approver, status, body)
	}
	return *approved.GrantID
}

// column reads one text column of the row query selects, failing t when there is not exactly one.
func (ts *testServer) column(t *testing.T, query string, args ...any) []string {
	t.Helper()
	rows, err := ts.Store.Pool.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v *string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if v == nil {
			out = append(out, "<null>")
			continue
		}
		out = append(out, *v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

func TestOperatorListsAndRevokesTheirOwnMachines(t *testing.T) {
	ts := newTestServer(t)
	credID, key := ts.mintLauncherCredential(t, testApprover, "example-host-devbox-a")
	otherID, otherKey := ts.mintLauncherCredential(t, "mallory@example.com", "example-host-devbox-m")
	status, body := ts.launcher(t, key, credID, http.MethodGet, "/v1/operator/machines", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %s", status, body)
	}
	list := decode[struct {
		Credentials []wireLauncherCredential `json:"credentials"`
	}](t, body)
	if len(list.Credentials) != 1 || list.Credentials[0].CredentialID != credID {
		t.Fatalf("machines = %+v, want exactly the operator's own", list.Credentials)
	}
	if ui := ts.machineLogins(t, testApprover); len(ui) != len(list.Credentials) {
		t.Fatalf("operator list (%d) must match Dispatch's page (%d)", len(list.Credentials), len(ui))
	}
	// mallory's credential cannot revoke testApprover's machine
	if status, body = ts.launcher(t, otherKey, otherID, http.MethodPost, "/v1/operator/machines/"+credID+"/revoke", nil); status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
		t.Fatalf("cross-operator revoke: %d %s, want 403 NOT_APPROVER", status, body)
	}
	// revoking one's own ends its proofs
	if status, body = ts.launcher(t, key, credID, http.MethodPost, "/v1/operator/machines/"+credID+"/revoke", nil); status != http.StatusOK || decode[stateBody](t, body).State != "revoked" {
		t.Fatalf("self revoke: %d %s, want 200 revoked", status, body)
	}
	if status, body = ts.launcher(t, key, credID, http.MethodGet, "/v1/operator/machines", nil); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("after revoke: %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	if logins := ts.machineLogins(t, "mallory@example.com"); len(logins) != 1 || logins[0].CredentialID != otherID {
		t.Fatalf("mallory's machine logins = %+v, want her own, untouched", logins)
	}
}

// TestOperatorMachinesMatchTheApproversDispatchList: a person's machine login lists, byte for byte,
// what Dispatch's machine-logins page lists for them — both their machines and the service login
// they approved — and another person's lists only theirs; an unknown or malformed id is refused
// and ends nothing.
func TestOperatorMachinesMatchTheApproversDispatchList(t *testing.T) {
	ts := newTestServer(t)
	devbox, devboxKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	laptop, _ := ts.mintLauncherCredential(t, testApprover, "example-host-laptop")
	service, _ := ts.mintServiceLauncherCredential(t)
	mallory, malloryKey := ts.mintLauncherCredential(t, "mallory@example.com", "example-host-devbox-m")

	got := ts.operatorGet(t, devboxKey, devbox, "/v1/operator/machines")
	if want := ts.uiGet(t, "/v1/launcher-credentials", testApprover); !bytes.Equal(got, want) {
		t.Fatalf("GET /v1/operator/machines = %s\nwant Dispatch's page's list %s", got, want)
	}
	var ids []string
	for _, c := range decode[struct {
		Credentials []wireLauncherCredential `json:"credentials"`
	}](t, got).Credentials {
		ids = append(ids, c.CredentialID)
	}
	if want := []string{service, laptop, devbox}; !slices.Equal(ids, want) {
		t.Fatalf("operator machines = %v, want the service login, the laptop and the devbox, newest first (%v)", ids, want)
	}
	if got, want := ts.operatorGet(t, malloryKey, mallory, "/v1/operator/machines"), ts.uiGet(t, "/v1/launcher-credentials", "mallory@example.com"); !bytes.Equal(got, want) {
		t.Fatalf("mallory's GET /v1/operator/machines = %s\nwant her page's list %s", got, want)
	}

	for _, tc := range []struct {
		name, path string
		status     int
		code       string
	}{
		{"of an unknown id", "/v1/operator/machines/" + uuid.NewString() + "/revoke", http.StatusNotFound, "NOT_FOUND"},
		{"of a malformed id", "/v1/operator/machines/not-a-uuid/revoke", http.StatusBadRequest, "CREDENTIAL_ID_INPUT"},
	} {
		if status, body := ts.launcher(t, devboxKey, devbox, http.MethodPost, tc.path, nil); status != tc.status || decode[wireError](t, body).Code != tc.code {
			t.Fatalf("revoke %s = %d %s, want %d %s", tc.name, status, body, tc.status, tc.code)
		}
	}
	if logins := ts.machineLogins(t, testApprover); len(logins) != 3 {
		t.Fatalf("machine logins after the refused revokes = %+v, want all three", logins)
	}
}

// TestServiceCredentialIsRefusedOperatorRoutes: the Legion daemon's service login has no operator,
// so every operator route refuses it, its own revoke included, which ends nothing.
func TestServiceCredentialIsRefusedOperatorRoutes(t *testing.T) {
	ts := newTestServer(t)
	credID, key := ts.mintServiceLauncherCredential(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/operator/machines"},
		{http.MethodPost, "/v1/operator/machines/" + credID + "/revoke"},
		{http.MethodGet, "/v1/operator/grants"},
		{http.MethodPost, "/v1/operator/grants/" + uuid.NewString() + "/revoke"},
	} {
		status, body := ts.launcher(t, key, credID, tc.method, tc.path, nil)
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "SERVICE_CREDENTIAL" {
			t.Fatalf("service login %s %s: %d %s, want 403 SERVICE_CREDENTIAL", tc.method, tc.path, status, body)
		}
	}
	if logins := ts.machineLogins(t, testApprover); len(logins) != 1 || logins[0].CredentialID != credID {
		t.Fatalf("machine logins of %s = %+v, want the service login still live", testApprover, logins)
	}
}

// TestOperatorGrantsMatchTheApproversDispatchList: a person's machine login lists, byte for byte,
// what Dispatch's Live grants page lists for them — their own session's automatic and approved
// grants, and a grant they approved on another person's session — and that other person's machine
// login lists theirs.
func TestOperatorGrantsMatchTheApproversDispatchList(t *testing.T) {
	ts := newTestServer(t)
	credID, machineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	box, boxKey := ts.enrolledBox(t, machineKey, credID, "box-"+t.Name())
	automatic := ts.requestAs(t, boxKey, box, "WORKER_TOKEN")
	if automatic.State != "granted" || automatic.GrantID == nil {
		t.Fatalf("request WORKER_TOKEN = %+v, want granted at once", automatic)
	}
	approved := ts.approveAs(t, ts.requestAs(t, boxKey, box, "DEEL_API_KEY"), testApprover)
	mallory, malloryKey := ts.mintLauncherCredential(t, "mallory@example.com", "example-host-devbox-m")
	malloryBox, malloryBoxKey := ts.enrolledBox(t, malloryKey, mallory, "box-m-"+t.Name())
	onHers := ts.approveAs(t, ts.requestAs(t, malloryBoxKey, malloryBox, "DEEL_API_KEY"), testApprover)

	got := ts.operatorGet(t, machineKey, credID, "/v1/operator/grants")
	if want := ts.uiGet(t, "/v1/grants", testApprover); !bytes.Equal(got, want) {
		t.Fatalf("GET /v1/operator/grants = %s\nwant Dispatch's Live grants list %s", got, want)
	}
	var ids []string
	for _, g := range decode[struct {
		Grants []wireApproverGrant `json:"grants"`
	}](t, got).Grants {
		ids = append(ids, g.GrantID)
	}
	if want := []string{onHers, approved, *automatic.GrantID}; !slices.Equal(ids, want) {
		t.Fatalf("operator grants = %v, want the grant on mallory's session, the approved one and the automatic one, newest first (%v)", ids, want)
	}
	if got, want := ts.operatorGet(t, malloryKey, mallory, "/v1/operator/grants"), ts.uiGet(t, "/v1/grants", "mallory@example.com"); !bytes.Equal(got, want) {
		t.Fatalf("mallory's GET /v1/operator/grants = %s\nwant her Live grants list %s", got, want)
	}
}

// TestOperatorRevokesAGrant: another person's machine login is refused the operator's grant, a
// session's own proof is refused the operator routes, an unknown or malformed id is refused, and
// the operator's machine login ends the grant. Its revoke is the operator's, so it withholds the
// secret the grant got automatically from that session and ends the session's other grant that got
// it automatically; every row it writes names the calling machine login, not a person signed in to
// Dispatch.
func TestOperatorRevokesAGrant(t *testing.T) {
	ts := newTestServer(t)
	credID, machineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	box, boxKey := ts.enrolledBox(t, machineKey, credID, "box-"+t.Name())
	automatic := ts.requestAs(t, boxKey, box, "WORKER_TOKEN")
	if automatic.GrantID == nil {
		t.Fatalf("request WORKER_TOKEN = %+v, want granted at once", automatic)
	}
	grantID := *automatic.GrantID
	mixed := ts.approveAs(t, ts.requestAs(t, boxKey, box, "WORKER_TOKEN", "DEEL_API_KEY"), testApprover)
	otherID, otherKey := ts.mintLauncherCredential(t, "mallory@example.com", "example-host-devbox-m")
	revokePath := "/v1/operator/grants/" + grantID + "/revoke"

	if status, body := ts.launcher(t, otherKey, otherID, http.MethodPost, revokePath, nil); status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
		t.Fatalf("revoke by another person's machine login = %d %s, want 403 NOT_APPROVER", status, body)
	}
	for _, path := range []string{revokePath, "/v1/operator/grants"} {
		method := http.MethodPost
		if path == "/v1/operator/grants" {
			method = http.MethodGet
		}
		if status, body := ts.session(t, boxKey, box, method, path, nil); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
			t.Fatalf("%s %s with the session's own proof = %d %s, want 401 LAUNCHER_INVALID", method, path, status, body)
		}
	}
	for _, tc := range []struct {
		name, path string
		status     int
		code       string
	}{
		{"of an unknown id", "/v1/operator/grants/" + uuid.NewString() + "/revoke", http.StatusNotFound, "NOT_FOUND"},
		{"of a malformed id", "/v1/operator/grants/not-a-uuid/revoke", http.StatusBadRequest, "GRANT_ID_INPUT"},
	} {
		if status, body := ts.launcher(t, machineKey, credID, http.MethodPost, tc.path, nil); status != tc.status || decode[wireError](t, body).Code != tc.code {
			t.Fatalf("revoke %s = %d %s, want %d %s", tc.name, status, body, tc.status, tc.code)
		}
	}
	for _, g := range []string{grantID, mixed} {
		if status, body := ts.session(t, boxKey, box, http.MethodPost, "/v1/grants/"+g+"/values", nil); status != http.StatusOK {
			t.Fatalf("values of %s after the refused revokes = %d %s, want 200", g, status, body)
		}
	}

	status, body := ts.launcher(t, machineKey, credID, http.MethodPost, revokePath, nil)
	if status != http.StatusOK || decode[stateBody](t, body).State != "revoked" {
		t.Fatalf("revoke by the operator's machine login = %d %s, want 200 revoked", status, body)
	}
	for _, g := range []string{grantID, mixed} {
		if status, body := ts.session(t, boxKey, box, http.MethodPost, "/v1/grants/"+g+"/values", nil); status != http.StatusForbidden || decode[wireError](t, body).Code != "GRANT_NOT_LIVE" {
			t.Fatalf("values of %s after the revoke = %d %s, want 403 GRANT_NOT_LIVE", g, status, body)
		}
	}
	if again := ts.requestAs(t, boxKey, box, "WORKER_TOKEN"); again.State != "pending" {
		t.Fatalf("the session's next WORKER_TOKEN request = %+v, want it pending: the operator's revoke withheld it", again)
	}

	actor := "launcher:" + credID
	for _, g := range []string{grantID, mixed} {
		if got := ts.column(t, `select revoked_by from grants where id=$1`, g); !slices.Equal(got, []string{actor}) {
			t.Fatalf("grant %s revoked_by = %v, want [%s]", g, got, actor)
		}
		if got := ts.column(t, `select actor from audit where kind='grant.revoked' and grant_id=$1`, g); !slices.Equal(got, []string{actor}) {
			t.Fatalf("grant %s's grant.revoked audit actors = %v, want [%s]", g, got, actor)
		}
	}
	if got := ts.column(t, `select detail->>'withheld' from audit where kind='grant.revoked' and grant_id=$1`, grantID); !slices.Equal(got, []string{`["WORKER_TOKEN"]`}) {
		t.Fatalf("revoked grant's audit withheld = %v, want [WORKER_TOKEN]", got)
	}
}

// TestOperatorRevokesAnotherOfTheirMachinesAsTheCallingLogin: from one machine a person ends their
// other machine's login; that machine's proofs and sessions end, the calling machine's go on, and
// every row the revoke writes names the calling machine login.
func TestOperatorRevokesAnotherOfTheirMachinesAsTheCallingLogin(t *testing.T) {
	ts := newTestServer(t)
	devbox, devboxKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	laptop, laptopKey := ts.mintLauncherCredential(t, testApprover, "example-host-laptop")
	box, boxKey := ts.enrolledBox(t, laptopKey, laptop, "box-"+t.Name())
	granted := ts.requestAs(t, boxKey, box, "WORKER_TOKEN")
	pending := ts.requestAs(t, boxKey, box, "DEEL_API_KEY")
	if granted.GrantID == nil || pending.State != "pending" {
		t.Fatalf("requests = %+v, %+v; want one granted and one pending", granted, pending)
	}

	status, body := ts.launcher(t, devboxKey, devbox, http.MethodPost, "/v1/operator/machines/"+laptop+"/revoke", nil)
	if status != http.StatusOK || decode[stateBody](t, body).State != "revoked" {
		t.Fatalf("revoke the laptop from the devbox = %d %s, want 200 revoked", status, body)
	}
	if status, body := ts.launcher(t, laptopKey, laptop, http.MethodGet, "/v1/operator/machines", nil); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("the laptop's launcher proof after the revoke = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	if status, body := ts.session(t, boxKey, box, http.MethodGet, "/v1/enrollments/self", nil); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "PROOF_INVALID" {
		t.Fatalf("the laptop's session after the revoke = %d %s, want 401 PROOF_INVALID", status, body)
	}
	var ids []string
	for _, c := range decode[struct {
		Credentials []wireLauncherCredential `json:"credentials"`
	}](t, ts.operatorGet(t, devboxKey, devbox, "/v1/operator/machines")).Credentials {
		ids = append(ids, c.CredentialID)
	}
	if !slices.Equal(ids, []string{devbox}) {
		t.Fatalf("the devbox's list after the revoke = %v, want the devbox alone", ids)
	}

	actor := "launcher:" + devbox
	for _, check := range []struct {
		what, query, arg string
	}{
		{"launcher_credential.revoked audit actor", `select actor from audit where kind='launcher_credential.revoked' and detail->>'credential_id'=$1`, laptop},
		{"enrollment.revoked audit actor", `select actor from audit where kind='enrollment.revoked' and enrollment_id=$1`, box},
		{"grant revoked_by", `select revoked_by from grants where id=$1`, *granted.GrantID},
		{"cancelled request's decided_by", `select decided_by from requests where id=$1`, pending.RequestID},
	} {
		if got := ts.column(t, check.query, check.arg); !slices.Equal(got, []string{actor}) {
			t.Fatalf("%s = %v, want [%s]", check.what, got, actor)
		}
	}
}

// TestUIRevokesRecordTheSignedInPerson: a revoke through Dispatch's UI routes records the person
// Dispatch names, canonicalized, as its actor, however Dispatch spelled the login: the grant's
// revoke and the machine login's revoke, with every row each writes.
func TestUIRevokesRecordTheSignedInPerson(t *testing.T) {
	ts := newTestServer(t)
	spelled := " \t" + "SAMI@Example.COM "
	actor := "human:" + testApprover
	credID, machineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	box, boxKey := ts.enrolledBox(t, machineKey, credID, "box-"+t.Name())
	first := ts.requestAs(t, boxKey, box, "WORKER_TOKEN")
	second := ts.approveAs(t, ts.requestAs(t, boxKey, box, "WORKER_TOKEN", "DEEL_API_KEY"), testApprover)
	if first.GrantID == nil {
		t.Fatalf("request WORKER_TOKEN = %+v, want granted at once", first)
	}

	if status, body := ts.ui(t, http.MethodPost, "/v1/grants/"+*first.GrantID+"/revoke-by-approver", map[string]any{"approver": spelled}); status != http.StatusOK {
		t.Fatalf("UI revoke of the grant = %d %s, want 200", status, body)
	}
	for _, g := range []string{*first.GrantID, second} {
		if got := ts.column(t, `select revoked_by from grants where id=$1`, g); !slices.Equal(got, []string{actor}) {
			t.Fatalf("grant %s revoked_by = %v, want [%s]", g, got, actor)
		}
		if got := ts.column(t, `select actor from audit where kind='grant.revoked' and grant_id=$1`, g); !slices.Equal(got, []string{actor}) {
			t.Fatalf("grant %s's grant.revoked audit actors = %v, want [%s]", g, got, actor)
		}
	}

	pending := ts.requestAs(t, boxKey, box, "DEEL_API_KEY")
	if status, body := ts.ui(t, http.MethodPost, "/v1/launcher-credentials/"+credID+"/revoke-by-approver", map[string]any{"approver": spelled}); status != http.StatusOK {
		t.Fatalf("UI revoke of the machine login = %d %s, want 200", status, body)
	}
	for _, check := range []struct {
		what, query, arg string
	}{
		{"launcher_credential.revoked audit actor", `select actor from audit where kind='launcher_credential.revoked' and detail->>'credential_id'=$1`, credID},
		{"enrollment.revoked audit actor", `select actor from audit where kind='enrollment.revoked' and enrollment_id=$1`, box},
		{"cancelled request's decided_by", `select decided_by from requests where id=$1`, pending.RequestID},
	} {
		if got := ts.column(t, check.query, check.arg); !slices.Equal(got, []string{actor}) {
			t.Fatalf("%s = %v, want [%s]", check.what, got, actor)
		}
	}
}
