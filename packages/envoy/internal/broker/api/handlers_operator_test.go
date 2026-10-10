// handlers_operator_test.go drives the operator self-service routes (/v1/operator/*), which a
// person's machine login calls with its launcher proof, on the same real broker api_test.go
// mounts: the machine list answers the person's own machines' rows of Dispatch's machine-login
// page and the grant list what Dispatch's Live grants page answers, each revoke acts as the
// credential's operator and records the calling machine login as its actor, and a service's
// login, which has no operator, is refused every one of them.
package api_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
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

// column reads one text column of every row query selects, in the order Postgres returns them,
// with "<null>" for a null, failing t only when the query does: a caller compares the whole
// slice, so a row too many or too few fails there.
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

// machineRows splits a machine-login list body into its rows as the broker wrote them, keeping
// only a person's own machines' (service null) when own is set.
func machineRows(t *testing.T, body []byte, own bool) []string {
	t.Helper()
	var rows []string
	for _, raw := range decode[struct {
		Credentials []json.RawMessage `json:"credentials"`
	}](t, body).Credentials {
		if !own || decode[wireLauncherCredential](t, raw).Service == nil {
			rows = append(rows, string(raw))
		}
	}
	return rows
}

// TestOperatorListsAndRevokesTheirOwnMachines: a person's machine login lists their machine,
// row for row as Dispatch's machine-login page lists it, and ends its own login, after which the
// broker refuses its proofs; another person's machine login is refused it and keeps its own.
func TestOperatorListsAndRevokesTheirOwnMachines(t *testing.T) {
	ts := newTestServer(t)
	credID, key := ts.mintLauncherCredential(t, testApprover, "example-host-devbox-a")
	otherID, otherKey := ts.mintLauncherCredential(t, "mallory@example.com", "example-host-devbox-m")
	body := ts.operatorGet(t, key, credID, "/v1/operator/machines")
	list := decode[struct {
		Credentials []wireLauncherCredential `json:"credentials"`
	}](t, body)
	if len(list.Credentials) != 1 || list.Credentials[0].CredentialID != credID {
		t.Fatalf("machines = %+v, want exactly the operator's own", list.Credentials)
	}
	if got, want := machineRows(t, body, false), machineRows(t, ts.uiGet(t, "/v1/launcher-credentials", testApprover), true); !slices.Equal(got, want) {
		t.Fatalf("operator rows = %v, want Dispatch's page's rows for the operator's machines, %v", got, want)
	}
	// mallory's credential cannot revoke testApprover's machine
	if status, body := ts.launcher(t, otherKey, otherID, http.MethodPost, "/v1/operator/machines/"+credID+"/revoke", nil); status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
		t.Fatalf("cross-operator revoke: %d %s, want 403 NOT_APPROVER", status, body)
	}
	// revoking one's own ends its proofs
	if status, body := ts.launcher(t, key, credID, http.MethodPost, "/v1/operator/machines/"+credID+"/revoke", nil); status != http.StatusOK || decode[stateBody](t, body).State != "revoked" {
		t.Fatalf("self revoke: %d %s, want 200 revoked", status, body)
	}
	if status, body := ts.launcher(t, key, credID, http.MethodGet, "/v1/operator/machines", nil); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("after revoke: %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	if logins := ts.machineLogins(t, "mallory@example.com"); len(logins) != 1 || logins[0].CredentialID != otherID {
		t.Fatalf("mallory's machine logins = %+v, want her own, untouched", logins)
	}
}

// TestOperatorMachinesAreThePersonsOwnRowsOfDispatchsList: a person's machine login lists
// exactly their own machines' rows of what Dispatch's machine-login page lists for them, byte for
// byte and in its order, and never a service's login, which that page lists beside them; another
// person's lists only theirs. A service's login is no more revocable there than an unknown id
// (404 NOT_FOUND), and a malformed id is refused; none of those revokes ends anything.
func TestOperatorMachinesAreThePersonsOwnRowsOfDispatchsList(t *testing.T) {
	ts := newTestServer(t)
	devbox, devboxKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	laptop, _ := ts.mintLauncherCredential(t, testApprover, "example-host-laptop")
	service, _ := ts.mintServiceLauncherCredential(t)
	mallory, malloryKey := ts.mintLauncherCredential(t, "mallory@example.com", "example-host-devbox-m")

	page := ts.uiGet(t, "/v1/launcher-credentials", testApprover)
	if got := len(machineRows(t, page, false)); got != 3 {
		t.Fatalf("Dispatch's page = %s, want three rows, the service login among them", page)
	}
	got := ts.operatorGet(t, devboxKey, devbox, "/v1/operator/machines")
	if rows, want := machineRows(t, got, false), machineRows(t, page, true); !slices.Equal(rows, want) {
		t.Fatalf("GET /v1/operator/machines rows = %v\nwant the own machines' rows of Dispatch's page, %v", rows, want)
	}
	var ids []string
	for _, c := range decode[struct {
		Credentials []wireLauncherCredential `json:"credentials"`
	}](t, got).Credentials {
		ids = append(ids, c.CredentialID)
	}
	if want := []string{laptop, devbox}; !slices.Equal(ids, want) {
		t.Fatalf("operator machines = %v, want the laptop and the devbox, newest first, and not the service login (%v)", ids, want)
	}
	if got, want := ts.operatorGet(t, malloryKey, mallory, "/v1/operator/machines"), ts.uiGet(t, "/v1/launcher-credentials", "mallory@example.com"); !slices.Equal(machineRows(t, got, false), machineRows(t, want, true)) {
		t.Fatalf("mallory's GET /v1/operator/machines = %s\nwant her own machines' rows of her page %s", got, want)
	}

	for _, tc := range []struct {
		name, path string
		status     int
		code       string
	}{
		{"of a service's login", "/v1/operator/machines/" + service + "/revoke", http.StatusNotFound, "NOT_FOUND"},
		{"of an unknown id", "/v1/operator/machines/" + uuid.NewString() + "/revoke", http.StatusNotFound, "NOT_FOUND"},
		{"of a malformed id", "/v1/operator/machines/not-a-uuid/revoke", http.StatusBadRequest, "CREDENTIAL_ID_INPUT"},
	} {
		if status, body := ts.launcher(t, devboxKey, devbox, http.MethodPost, tc.path, nil); status != tc.status || decode[wireError](t, body).Code != tc.code {
			t.Fatalf("revoke %s = %d %s, want %d %s", tc.name, status, body, tc.status, tc.code)
		}
	}
	if got := ts.uiGet(t, "/v1/launcher-credentials", testApprover); !bytes.Equal(got, page) {
		t.Fatalf("Dispatch's page after the refused revokes = %s, want it unchanged: %s", got, page)
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
// Dispatch. On a grant its session already ended, the operator's revoke still withholds, which a
// grant.withheld audit row records, naming the calling machine login too.
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

	other, otherBoxKey := ts.enrolledBox(t, machineKey, credID, "box-2-"+t.Name())
	ended := ts.requestAs(t, otherBoxKey, other, "WORKER_TOKEN")
	if ended.GrantID == nil {
		t.Fatalf("the second box's WORKER_TOKEN request = %+v, want granted at once", ended)
	}
	if status, body := ts.session(t, otherBoxKey, other, http.MethodPost, "/v1/grants/"+*ended.GrantID+"/revoke", nil); status != http.StatusOK {
		t.Fatalf("the second box revoking its own grant = %d %s, want 200", status, body)
	}
	if status, body := ts.launcher(t, machineKey, credID, http.MethodPost, "/v1/operator/grants/"+*ended.GrantID+"/revoke", nil); status != http.StatusOK {
		t.Fatalf("the operator's revoke of a grant its session ended = %d %s, want 200", status, body)
	}
	if got := ts.column(t, `select actor || ' ' || (detail->>'withheld') from audit where kind='grant.withheld' and grant_id=$1`, *ended.GrantID); !slices.Equal(got, []string{actor + ` ["WORKER_TOKEN"]`}) {
		t.Fatalf("grant.withheld audit rows = %v, want [%s [\"WORKER_TOKEN\"]]", got, actor)
	}
}

// TestAnApproverWhoIsNotTheOperatorRevokesWithoutWithholding: a person who approved a grant on
// another person's session ends it through the operator route as its approver, not its operator,
// so nothing is withheld from that session: its other grant that got a secret automatically stays
// live, it gets that secret at once again, and the grant's audit row withholds nothing.
func TestAnApproverWhoIsNotTheOperatorRevokesWithoutWithholding(t *testing.T) {
	ts := newTestServer(t)
	credID, machineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	mallory, malloryKey := ts.mintLauncherCredential(t, "mallory@example.com", "example-host-devbox-m")
	box, boxKey := ts.enrolledBox(t, malloryKey, mallory, "box-m-"+t.Name())
	automatic := ts.requestAs(t, boxKey, box, "WORKER_TOKEN")
	if automatic.GrantID == nil {
		t.Fatalf("request WORKER_TOKEN = %+v, want granted at once", automatic)
	}
	mixed := ts.approveAs(t, ts.requestAs(t, boxKey, box, "WORKER_TOKEN", "DEEL_API_KEY"), testApprover)

	status, body := ts.launcher(t, machineKey, credID, http.MethodPost, "/v1/operator/grants/"+mixed+"/revoke", nil)
	if status != http.StatusOK || decode[stateBody](t, body).State != "revoked" {
		t.Fatalf("the approver's revoke through the operator route = %d %s, want 200 revoked", status, body)
	}
	if status, body := ts.session(t, boxKey, box, http.MethodPost, "/v1/grants/"+mixed+"/values", nil); status != http.StatusForbidden || decode[wireError](t, body).Code != "GRANT_NOT_LIVE" {
		t.Fatalf("values of the revoked grant = %d %s, want 403 GRANT_NOT_LIVE", status, body)
	}
	if status, body := ts.session(t, boxKey, box, http.MethodPost, "/v1/grants/"+*automatic.GrantID+"/values", nil); status != http.StatusOK {
		t.Fatalf("values of the session's automatic grant = %d %s, want 200: an approver's revoke withholds nothing", status, body)
	}
	if again := ts.requestAs(t, boxKey, box, "WORKER_TOKEN"); again.State != "granted" {
		t.Fatalf("the session's next WORKER_TOKEN request = %+v, want it granted at once", again)
	}
	if got := ts.column(t, `select name from withheld_secrets where enrollment_id=$1`, box); len(got) != 0 {
		t.Fatalf("withheld from mallory's session = %v, want nothing", got)
	}
	if got := ts.column(t, `select actor || ' ' || coalesce(detail->>'withheld', '-') from audit where kind in ('grant.revoked', 'grant.withheld') and grant_id=$1`, mixed); !slices.Equal(got, []string{"launcher:" + credID + " -"}) {
		t.Fatalf("the revoked grant's audit rows = %v, want one grant.revoked by launcher:%s withholding nothing", got, credID)
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
		{"cancelled request's request.cancelled audit actor", `select actor from audit where kind='request.cancelled' and request_id=$1`, pending.RequestID},
		{"cancelled request's record event actor", `select ev.actor from credential_request_events ev join requests r on r.record_id = ev.record_id
			where r.id=$1 and ev.event='cancelled'`, pending.RequestID},
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
