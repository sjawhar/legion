// vm_login_test.go drives a Google Compute Engine VM through the broker on its own identity token,
// on the same real broker api_test.go mounts, with a local issuer standing in for Google's: its
// login at POST /v1/launcher-credentials/token, its enrollment of kind vm, its renewals, and the
// secret its service owns.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The VM fleet newVMTestServer binds: fleetAccount runs fleetService's VMs, in fleetProject, named
// by fleetInstances; FLEET_KEY is an agent-tier secret fleetService owns. strangerAccount is a VM
// account the broker does not list.
const (
	fleetAudience   = "example-vm-audience"
	fleetAccount    = "example-fleet@example-project.iam.gserviceaccount.com"
	fleetService    = "example-fleet"
	fleetProject    = "example-project"
	fleetInstances  = "example-vm-*"
	strangerAccount = "example-stranger@example-project.iam.gserviceaccount.com"
)

// vmServer is a testServer that also trusts the VM issuer, with the issuer and its key, and the
// broker's live BROKER_VM_PROJECTS and BROKER_VM_INSTANCES, which a test edits to narrow the fleet
// as an operator changing them and redeploying does.
type vmServer struct {
	*testServer
	vmIssuer            *oidctest.Issuer
	vmKey               *oidctest.Key
	projects, instances map[string]string
}

// newVMTestServer is newTestServer as cmd/broker/main.go wires it with BROKER_GOOGLE_OIDC_AUDIENCE
// set: a VM verifier against a local issuer in Google's place, fleetAccount bound to fleetService,
// fleetProject and fleetInstances, and FLEET_KEY, which fleetService owns.
func newVMTestServer(t *testing.T) *vmServer {
	t.Helper()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "vm-signing-key")
	verifier, err := oidc.New(context.Background(), issuer.URL(), fleetAudience)
	if err != nil {
		t.Fatalf("oidc.New(the VM issuer): %v", err)
	}
	var projects, instances map[string]string
	ts := newTestServerWith(t, func(d *api.Deps) {
		local := d.Machine.Secrets.(secrets.AWS).Client.(*secrets.Local)
		local.Put(policytest.Secret("FLEET_KEY", fleetService, policy.TierAgent, "fleet-v1"))
		cur := policytest.Current(t, local, fleetService)
		d.Policy, d.Machine.Policy, d.MachineLogin.Policy = cur, cur, cur
		d.Enroll.VM = enroll.GoogleVMVerifier{Verifier: verifier}
		d.Enroll.VMServicesByAccount = map[string]string{fleetAccount: fleetService}
		d.Enroll.VMProjects = map[string]string{fleetAccount: fleetProject}
		d.Enroll.VMInstancePatterns = map[string]string{fleetAccount: fleetInstances}
		d.Machine.VMServicesByAccount, d.Machine.VMProjects, d.Machine.VMInstancePatterns = d.Enroll.VMServicesByAccount, d.Enroll.VMProjects, d.Enroll.VMInstancePatterns
		projects, instances = d.Enroll.VMProjects, d.Enroll.VMInstancePatterns
	})
	return &vmServer{testServer: ts, vmIssuer: issuer, vmKey: key, projects: projects, instances: instances}
}

// fleetVM is one VM: an account, project, zone, instance name and id. Its token is the one its
// metadata server mints for the audience, verified, unexpired, unless a test says otherwise.
type fleetVM struct {
	account, project, zone, instance, id string
}

// member is the fleet's VM named example-vm-<id>, in zone.
func member(zone, id string) fleetVM {
	return fleetVM{account: fleetAccount, project: fleetProject, zone: zone, instance: "example-vm-" + id, id: id}
}

// runtimeID is vm's runtime id, as enroll.VMClaims.RuntimeID keys it.
func (vm fleetVM) runtimeID() string {
	return vm.account + "@" + vm.project + "/" + vm.zone + "/" + vm.id
}

// vmToken mints vm's identity token in Google's full format for audience, expiring lifetime from
// now, an hour after its issue, its email verified as verified says.
func (ts *vmServer) vmToken(t *testing.T, vm fleetVM, audience string, lifetime time.Duration, verified any) string {
	t.Helper()
	claims := ts.vmIssuer.Claims("100000000000000000001", audience)
	expires := time.Now().Add(lifetime)
	claims["iat"] = expires.Add(-time.Hour).Unix()
	claims["nbf"] = claims["iat"]
	claims["exp"] = expires.Unix()
	claims["email"] = vm.account
	claims["email_verified"] = verified
	claims["google"] = map[string]any{"compute_engine": map[string]any{
		"project_id": vm.project, "zone": vm.zone, "instance_name": vm.instance, "instance_id": vm.id,
	}}
	return ts.vmIssuer.Mint(t, ts.vmKey, claims)
}

// current is vm's current, valid identity token.
func (ts *vmServer) current(t *testing.T, vm fleetVM) string {
	t.Helper()
	return ts.vmToken(t, vm, fleetAudience, time.Hour, true)
}

// vmLogin logs vm in with token, its request object naming fleetService and signed by key.
func (ts *vmServer) vmLogin(t *testing.T, vm fleetVM, token string, key *ecdsa.PrivateKey) (int, []byte) {
	t.Helper()
	return ts.tokenLogin(t, token, signMachineLoginRequest(t, key, ts.URL, "", vm.instance, fleetService))
}

// loggedIn logs vm in with its current token and a fresh key, failing t on anything but 200, and
// returns the credential's id and key.
func (ts *vmServer) loggedIn(t *testing.T, vm fleetVM) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key := newSigningKey(t)
	status, body := ts.vmLogin(t, vm, ts.current(t, vm), key)
	if status != http.StatusOK {
		t.Fatalf("VM %s's token login = %d %s, want 200", vm.runtimeID(), status, body)
	}
	return decode[wireTokenLogin](t, body).CredentialID, key
}

// enrolVM posts vm's enrollment under the credential, with token and the session key's thumbprint.
func (ts *vmServer) enrolVM(t *testing.T, credentialID string, launcherKey *ecdsa.PrivateKey, vm fleetVM, token string, sessionKey *ecdsa.PrivateKey) (int, []byte) {
	t.Helper()
	return ts.launcher(t, launcherKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "vm", "runtime_id": vm.runtimeID(), "thumbprint": thumbprintOf(t, sessionKey), "vm_token": token,
	})
}

// enrolled logs vm in and enrolls it with a fresh session key, failing t on anything but 201, and
// returns the credential, its key, the enrollment and the session key.
func (ts *vmServer) enrolled(t *testing.T, vm fleetVM) (credentialID string, launcherKey *ecdsa.PrivateKey, enrollmentID string, sessionKey *ecdsa.PrivateKey) {
	t.Helper()
	credentialID, launcherKey = ts.loggedIn(t, vm)
	sessionKey = newSigningKey(t)
	status, body := ts.enrolVM(t, credentialID, launcherKey, vm, ts.current(t, vm), sessionKey)
	if status != http.StatusCreated {
		t.Fatalf("VM %s's enrollment = %d %s, want 201", vm.runtimeID(), status, body)
	}
	return credentialID, launcherKey, decode[wireEnrolled](t, body).EnrollmentID, sessionKey
}

// renewVM renews the enrollment as its session, carrying body.
func (ts *vmServer) renewVM(t *testing.T, key *ecdsa.PrivateKey, enrollmentID string, body any) (int, []byte) {
	t.Helper()
	return ts.session(t, key, enrollmentID, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/renew", body)
}

// TestAVMLogsInWithItsOwnTokenAndNoOneApproves: a fleet VM's identity token and a request object
// its new key signed issue a launcher credential at once, with no machine-login record: no
// operator, its service the fleet's, its host the VM's project, zone and instance name, its
// subject the VM's runtime id, expiring with the token. Its key enrolls the VM as kind vm, whose
// session reads its own enrollment back as a vm with no operator.
func TestAVMLogsInWithItsOwnTokenAndNoOneApproves(t *testing.T) {
	ts := newVMTestServer(t)
	vm := member("example-zone-a", "1001")
	before := time.Now()
	credentialID, launcherKey := ts.loggedIn(t, vm)
	got := ts.column(t, `select coalesce(operator,'-') || ' ' || service || ' ' || host || ' ' || subject from launcher_credentials where id=$1`, credentialID)
	if want := "- " + fleetService + " example-project/example-zone-a/example-vm-1001 " + vm.runtimeID(); !slices.Equal(got, []string{want}) {
		t.Fatalf("launcher credential = %v, want %q", got, want)
	}
	expires := ts.column(t, `select (expires_at between $2::timestamptz and $3::timestamptz)::text from launcher_credentials where id=$1`,
		credentialID, before.Add(time.Hour-2*time.Second), time.Now().Add(time.Hour+time.Second))
	if !slices.Equal(expires, []string{"true"}) {
		t.Fatalf("launcher credential expires within the token's hour = %v, want true", expires)
	}
	if got := ts.column(t, `select count(*)::text from credential_requests`); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("credential-request records = %v, want none: no person approves a VM's login", got)
	}
	sessionKey := newSigningKey(t)
	status, body := ts.enrolVM(t, credentialID, launcherKey, vm, ts.current(t, vm), sessionKey)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/enrollments kind vm = %d %s, want 201", status, body)
	}
	enrollmentID := decode[wireEnrolled](t, body).EnrollmentID
	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/enrollments/self", nil)
	self := decode[struct {
		Kind     string  `json:"kind"`
		Operator *string `json:"operator"`
	}](t, body)
	if status != http.StatusOK || self.Kind != "vm" || self.Operator != nil {
		t.Fatalf("GET /v1/enrollments/self of a VM = %d %s, want kind vm with no operator", status, body)
	}
}

// TestAVMReadsItsServicesSecretAtOnce: an enrolled fleet VM asking for FLEET_KEY, which its service
// owns, is granted at once with no record, and the grant releases the value; a machine session's
// request for it is denied. The release re-checks the VM's service (requests.Machine.Values), so
// this is the whole path a VM's credential helper takes.
func TestAVMReadsItsServicesSecretAtOnce(t *testing.T) {
	ts := newVMTestServer(t)
	_, _, enrollmentID, sessionKey := ts.enrolled(t, member("example-zone-a", "1001"))
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "git fetch", "FLEET_KEY"), "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	if status != http.StatusOK || created.State != "granted" || created.GrantID == nil || created.RecordID != nil {
		t.Fatalf("POST /v1/requests FLEET_KEY as the VM = %d %s, want granted at once with no record", status, body)
	}
	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil)
	values := decode[struct {
		Values map[string]string `json:"values"`
	}](t, body)
	if status != http.StatusOK || values.Values["FLEET_KEY"] != "fleet-v1" {
		t.Fatalf("POST /v1/grants/{id}/values as the VM = %d %s, want FLEET_KEY=fleet-v1", status, body)
	}
	box, boxKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	status, body = ts.session(t, boxKey, box, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, boxKey, ts.URL, "", "FLEET_KEY"), "session_id": nil})
	if denied := decode[wireCreateRequestResponse](t, body); status != http.StatusOK || denied.State != "denied" {
		t.Fatalf("POST /v1/requests FLEET_KEY as a machine session = %d %s, want denied", status, body)
	}
}

// TestTwoVMsOfOneAccountLogInInTurnAndEachEnrolsAndRevokes: the fleet shares one service account,
// and each VM's login holds a credential of its own (keyed by its runtime id), so the second VM's
// login moves nothing of the first's: the first VM's credential still enrolls afterwards. No VM's
// launcher credential ends a VM's enrollment, its own included (403 VM_ENDS_BY_LOGIN_REVOKE); a
// person revoking the first VM's machine login on Dispatch ends the first VM's enrollment alone, and
// the second's renews.
func TestTwoVMsOfOneAccountLogInInTurnAndEachEnrolsAndRevokes(t *testing.T) {
	ts := newVMTestServer(t)
	one, two := member("example-zone-a", "1001"), member("example-zone-a", "1002")
	oneCred, oneKey := ts.loggedIn(t, one)
	twoCred, twoKey, twoEnrollment, twoSession := ts.enrolled(t, two)
	if oneCred == twoCred {
		t.Fatalf("two VMs of one account share launcher credential %s", oneCred)
	}
	oneSession := newSigningKey(t)
	status, body := ts.enrolVM(t, oneCred, oneKey, one, ts.current(t, one), oneSession)
	if status != http.StatusCreated {
		t.Fatalf("the first VM's enrollment after the second VM's login = %d %s, want 201", status, body)
	}
	oneEnrollment := decode[wireEnrolled](t, body).EnrollmentID
	for _, tc := range []struct {
		name, credential, enrollment string
		key                          *ecdsa.PrivateKey
	}{
		{"the first VM revoking the second's enrollment", oneCred, twoEnrollment, oneKey},
		{"the first VM revoking its own enrollment", oneCred, oneEnrollment, oneKey},
		{"the second VM revoking its own enrollment", twoCred, twoEnrollment, twoKey},
	} {
		if status, body := ts.launcher(t, tc.key, tc.credential, http.MethodDelete, "/v1/enrollments/"+tc.enrollment, nil); status != http.StatusForbidden || decode[wireError](t, body).Code != "VM_ENDS_BY_LOGIN_REVOKE" {
			t.Fatalf("%s = %d %s, want 403 VM_ENDS_BY_LOGIN_REVOKE", tc.name, status, body)
		}
	}
	if status, body := ts.ui(t, http.MethodPost, "/v1/launcher-credentials/"+oneCred+"/revoke-by-approver", map[string]any{"approver": testApprover}); status != http.StatusOK {
		t.Fatalf("a person revoking the first VM's machine login = %d %s, want 200", status, body)
	}
	if status, body := ts.renewVM(t, oneSession, oneEnrollment, map[string]any{"vm_token": ts.current(t, one)}); status != http.StatusUnauthorized {
		t.Fatalf("the first VM's renewal after its login's revoke = %d %s, want 401", status, body)
	}
	if status, body := ts.renewVM(t, twoSession, twoEnrollment, map[string]any{"vm_token": ts.current(t, two)}); status != http.StatusOK {
		t.Fatalf("the second VM's renewal after the first's revoke = %d %s, want 200", status, body)
	}
}

// TestTwoVMsOfOneInstanceIdInDifferentZonesAreTwoCredentials: an instance id is unique only within a
// project and a zone, so two VMs of one id in two zones log in as two subjects under two
// credentials.
func TestTwoVMsOfOneInstanceIdInDifferentZonesAreTwoCredentials(t *testing.T) {
	ts := newVMTestServer(t)
	a, b := member("example-zone-a", "1001"), member("example-zone-b", "1001")
	aCred, _ := ts.loggedIn(t, a)
	bCred, _ := ts.loggedIn(t, b)
	if aCred == bCred {
		t.Fatalf("one instance id in two zones shares launcher credential %s", aCred)
	}
	got := ts.column(t, `select subject from launcher_credentials where subject is not null order by subject`)
	if want := []string{a.runtimeID(), b.runtimeID()}; !slices.Equal(got, want) {
		t.Fatalf("token-login subjects = %v, want %v", got, want)
	}
}

// TestAVMOfAnotherProjectOrInstanceNameIsRefused (review focus 3): a VM running as the fleet's
// account in another project, or under an instance name the fleet's glob does not match, is 403
// ACCOUNT_NOT_LISTED, each naming what did not match, and no credential is issued.
func TestAVMOfAnotherProjectOrInstanceNameIsRefused(t *testing.T) {
	ts := newVMTestServer(t)
	otherProject := member("example-zone-a", "1001")
	otherProject.project = "example-other-project"
	otherName := member("example-zone-a", "1002")
	otherName.instance = "other-vm-1002"
	for _, tc := range []struct {
		name string
		vm   fleetVM
		says string
	}{
		{"another project", otherProject, "project example-project only (BROKER_VM_PROJECTS), and this VM is in project example-other-project"},
		{"another instance name", otherName, `instance other-vm-1002 does not match ` + fleetAccount + `'s instance names "example-vm-*" (BROKER_VM_INSTANCES)`},
	} {
		status, body := ts.vmLogin(t, tc.vm, ts.current(t, tc.vm), newSigningKey(t))
		refusal := decode[wireError](t, body)
		if status != http.StatusForbidden || refusal.Code != "ACCOUNT_NOT_LISTED" || !strings.Contains(refusal.Error, tc.says) {
			t.Errorf("a VM in %s = %d %s, want 403 ACCOUNT_NOT_LISTED saying %q", tc.name, status, body, tc.says)
		}
	}
	if got := ts.column(t, `select count(*)::text from launcher_credentials`); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("launcher credentials = %v, want none", got)
	}
}

// TestAVMTokenThatDoesNotVerifyIsRefused: a fleet VM's token for another audience (review focus 1)
// and one whose email is not verified (review focus 2) are 401 TOKEN_INVALID at the login, naming
// why; an expired one is 401 TOKEN_EXPIRED; and a VM enrolling with a token for another audience
// under its own credential is 403 VM_IDENTITY_MISMATCH, and with an expired one 403
// VM_TOKEN_EXPIRED.
func TestAVMTokenThatDoesNotVerifyIsRefused(t *testing.T) {
	ts := newVMTestServer(t)
	vm := member("example-zone-a", "1001")
	for _, tc := range []struct {
		name, token, code, says string
	}{
		{"a token for another audience", ts.vmToken(t, vm, "tailnet-audience", time.Hour, true), "TOKEN_INVALID", "another audience"},
		{"a token whose email is not verified", ts.vmToken(t, vm, fleetAudience, time.Hour, false), "TOKEN_INVALID", "not verified"},
		{"an expired token", ts.vmToken(t, vm, fleetAudience, -time.Minute, true), "TOKEN_EXPIRED", "expired"},
	} {
		status, body := ts.vmLogin(t, vm, tc.token, newSigningKey(t))
		refusal := decode[wireError](t, body)
		if status != http.StatusUnauthorized || refusal.Code != tc.code || !strings.Contains(refusal.Error, tc.says) {
			t.Errorf("token login with %s = %d %s, want 401 %s saying %q", tc.name, status, body, tc.code, tc.says)
		}
	}
	credentialID, launcherKey := ts.loggedIn(t, vm)
	for _, tc := range []struct{ name, token, code string }{
		{"a token for another audience", ts.vmToken(t, vm, "tailnet-audience", time.Hour, true), "VM_IDENTITY_MISMATCH"},
		{"a token whose email is not verified", ts.vmToken(t, vm, fleetAudience, time.Hour, false), "VM_IDENTITY_MISMATCH"},
		{"another VM's token", ts.current(t, member("example-zone-a", "1002")), "VM_IDENTITY_MISMATCH"},
		{"an expired token", ts.vmToken(t, vm, fleetAudience, -time.Minute, true), "VM_TOKEN_EXPIRED"},
	} {
		status, body := ts.enrolVM(t, credentialID, launcherKey, vm, tc.token, newSigningKey(t))
		if status != http.StatusForbidden || decode[wireError](t, body).Code != tc.code {
			t.Errorf("enrollment with %s = %d %s, want 403 %s", tc.name, status, body, tc.code)
		}
	}
}

// TestAnUnlistedAccountIsRefusedOnABrokerThatListsAnother: a VM token of an account
// BROKER_VM_SERVICES does not list is 403 ACCOUNT_NOT_LISTED naming the account and the setting.
func TestAnUnlistedAccountIsRefusedOnABrokerThatListsAnother(t *testing.T) {
	ts := newVMTestServer(t)
	stranger := member("example-zone-a", "1001")
	stranger.account = strangerAccount
	status, body := ts.vmLogin(t, stranger, ts.current(t, stranger), newSigningKey(t))
	refusal := decode[wireError](t, body)
	if status != http.StatusForbidden || refusal.Code != "ACCOUNT_NOT_LISTED" || !strings.Contains(refusal.Error, strangerAccount+" is not in BROKER_VM_SERVICES") {
		t.Fatalf("an unlisted account's VM = %d %s, want 403 ACCOUNT_NOT_LISTED naming it", status, body)
	}
}

// TestAVMLoginNamingAnotherServiceIsRefused: a fleet VM's request object naming another service
// than its account's is 403 ACCOUNT_NOT_LISTED, as a Kubernetes account's is.
func TestAVMLoginNamingAnotherServiceIsRefused(t *testing.T) {
	ts := newVMTestServer(t)
	vm := member("example-zone-a", "1001")
	status, body := ts.tokenLogin(t, ts.current(t, vm), signMachineLoginRequest(t, newSigningKey(t), ts.URL, "", vm.instance, "legion-daemon"))
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "ACCOUNT_NOT_LISTED" {
		t.Fatalf("a VM's login naming another service = %d %s, want 403 ACCOUNT_NOT_LISTED", status, body)
	}
}

// TestAVMTokenIsRefusedWhenNoVMServiceIsConfigured: on a broker with no Google audience a VM's
// identity token is the Kubernetes verifier's to judge, which refuses it 401 TOKEN_INVALID as a
// token from another issuer, and kind vm is 400 INVALID_KIND.
func TestAVMTokenIsRefusedWhenNoVMServiceIsConfigured(t *testing.T) {
	vms := newVMTestServer(t)
	plain := newTestServer(t)
	vm := member("example-zone-a", "1001")
	status, body := plain.tokenLogin(t, vms.current(t, vm), signMachineLoginRequest(t, newSigningKey(t), plain.URL, "", vm.instance, fleetService))
	refusal := decode[wireError](t, body)
	if status != http.StatusUnauthorized || refusal.Code != "TOKEN_INVALID" || !strings.Contains(refusal.Error, "another issuer") {
		t.Fatalf("a VM's token on a broker with no Google audience = %d %s, want 401 TOKEN_INVALID from another issuer", status, body)
	}
	credentialID, launcherKey := plain.mintServiceLauncherCredential(t)
	status, body = plain.launcher(t, launcherKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "vm", "runtime_id": vm.runtimeID(), "thumbprint": thumbprintOf(t, newSigningKey(t)), "vm_token": vms.current(t, vm),
	})
	if refusal := decode[wireError](t, body); status != http.StatusBadRequest || refusal.Code != "INVALID_KIND" || refusal.Error != `kind must be "session" or "pod"` {
		t.Fatalf("kind vm on a broker with no Google audience = %d %s, want 400 INVALID_KIND naming session and pod", status, body)
	}
}

// TestTheKubernetesLoginIsUnchangedByTheVMPath: with a VM verifier beside the Kubernetes one, the
// Legion daemon's token logs in and its credential enrolls a pod as before, a Kubernetes service's
// credential cannot enroll a VM, and a token neither issuer issued, or the cluster's token for
// another audience, is refused with exactly the answer a broker with no VM verifier gives.
func TestTheKubernetesLoginIsUnchangedByTheVMPath(t *testing.T) {
	vms := newVMTestServer(t)
	plain := newTestServer(t)
	credentialID, key := vms.mintServiceLauncherCredential(t)
	if status, body := vms.enrollPodWith(t, key, credentialID); status != http.StatusCreated {
		t.Fatalf("the daemon's pod enrollment on a broker with a VM verifier = %d %s, want 201", status, body)
	}
	vm := member("example-zone-a", "1001")
	status, body := vms.launcher(t, key, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "vm", "runtime_id": vm.runtimeID(), "thumbprint": thumbprintOf(t, newSigningKey(t)), "vm_token": vms.current(t, vm),
	})
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "VM_IDENTITY_MISMATCH" {
		t.Fatalf("the daemon's credential enrolling a VM = %d %s, want 403 VM_IDENTITY_MISMATCH", status, body)
	}
	wrongAudience := func(ts *testServer) string {
		claims := ts.podIssuer.Claims(daemonAccount, "another-audience")
		claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
		return ts.podIssuer.Mint(t, ts.podKey, claims)
	}
	for _, tc := range []struct {
		name  string
		token func(*testServer) string
	}{
		{"not a token", func(*testServer) string { return "not-a-token" }},
		{"the cluster's token for another audience", wrongAudience},
		{"an expired cluster token", func(ts *testServer) string { return ts.accountToken(t, daemonAccount, "", -time.Minute) }},
	} {
		withVM, withVMBody := vms.tokenLogin(t, tc.token(vms.testServer), vms.daemonRequest(t, newSigningKey(t)))
		without, withoutBody := plain.tokenLogin(t, tc.token(plain), plain.daemonRequest(t, newSigningKey(t)))
		a, b := decode[wireError](t, withVMBody), decode[wireError](t, withoutBody)
		if withVM != without || a.Code != b.Code || stripTimes(a.Error) != stripTimes(b.Error) {
			t.Errorf("%s: with a VM verifier %d %s, without %d %s; want the same answer", tc.name, withVM, withVMBody, without, withoutBody)
		}
	}
}

// stripTimes drops what differs between two refusals of two tokens minted a moment apart: the
// expiry an expired token's refusal names.
func stripTimes(message string) string {
	if at := strings.Index(message, "expired at"); at >= 0 {
		return message[:at]
	}
	return message
}

// TestAVMRenewsOnlyWithItsCurrentToken: POST /v1/enrollments/{id}/renew for a VM carrying its
// current token renews it; with no body it is 403 VM_TOKEN_REQUIRED, with another VM's token 403
// VM_IDENTITY_MISMATCH, with an expired one 403 VM_TOKEN_EXPIRED, and with both a pod and a VM token
// 400 INVALID_RENEWAL. A machine session carrying a VM token is 403 VM_IDENTITY_MISMATCH.
func TestAVMRenewsOnlyWithItsCurrentToken(t *testing.T) {
	ts := newVMTestServer(t)
	vm := member("example-zone-a", "1001")
	_, _, enrollmentID, sessionKey := ts.enrolled(t, vm)
	for _, tc := range []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"no body", nil, http.StatusForbidden, "VM_TOKEN_REQUIRED"},
		{"another VM's token", map[string]any{"vm_token": ts.current(t, member("example-zone-a", "1002"))}, http.StatusForbidden, "VM_IDENTITY_MISMATCH"},
		{"an expired token", map[string]any{"vm_token": ts.vmToken(t, vm, fleetAudience, -time.Minute, true)}, http.StatusForbidden, "VM_TOKEN_EXPIRED"},
		{"a pod and a VM token", map[string]any{"vm_token": ts.current(t, vm), "pod_token": "x"}, http.StatusBadRequest, "INVALID_RENEWAL"},
	} {
		status, body := ts.renewVM(t, sessionKey, enrollmentID, tc.body)
		if status != tc.status || decode[wireError](t, body).Code != tc.code {
			t.Errorf("a VM's renewal with %s = %d %s, want %d %s", tc.name, status, body, tc.status, tc.code)
		}
	}
	if status, body := ts.renewVM(t, sessionKey, enrollmentID, map[string]any{"vm_token": ts.current(t, vm)}); status != http.StatusOK {
		t.Fatalf("a VM's renewal with its current token = %d %s, want 200", status, body)
	}
	box, boxKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	if status, body := ts.renewVM(t, boxKey, box, map[string]any{"vm_token": ts.current(t, vm)}); status != http.StatusForbidden || decode[wireError](t, body).Code != "VM_IDENTITY_MISMATCH" {
		t.Fatalf("a machine session's renewal carrying a VM token = %d %s, want 403 VM_IDENTITY_MISMATCH", status, body)
	}
}

// narrowings are the two ways an operator narrows a fleet past an enrolled VM: another project,
// and an instance-name glob its name no longer matches. Each edits the broker's live settings as
// changing them and redeploying does.
var narrowings = map[string]func(*vmServer){
	"another project":       func(ts *vmServer) { ts.projects[fleetAccount] = "example-other-project" },
	"another instance glob": func(ts *vmServer) { ts.instances[fleetAccount] = "other-vm-*" },
}

// askAs posts a request for names as the VM's session and answers the status and body.
func (ts *vmServer) askAs(t *testing.T, key *ecdsa.PrivateKey, enrollmentID string, names ...string) (int, []byte) {
	t.Helper()
	return ts.session(t, key, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, key, ts.URL, "git fetch", names...), "session_id": nil})
}

// TestANarrowedVMReleasesNoGrantItHolds: a fleet VM holds live grants of WORKER_TOKEN (shared,
// agent tier) and of FLEET_KEY (its service's); once the fleet is narrowed past it, releasing
// either is refused 401: the VM has no session identity left, whichever secret the grant holds.
func TestANarrowedVMReleasesNoGrantItHolds(t *testing.T) {
	for name, narrow := range narrowings {
		t.Run(name, func(t *testing.T) {
			ts := newVMTestServer(t)
			_, _, enrollmentID, key := ts.enrolled(t, member("example-zone-a", "1001"))
			var grants []string
			for _, secret := range []string{"WORKER_TOKEN", "FLEET_KEY"} {
				status, body := ts.askAs(t, key, enrollmentID, secret)
				granted := decode[wireCreateRequestResponse](t, body)
				if status != http.StatusOK || granted.State != "granted" || granted.GrantID == nil {
					t.Fatalf("the fleet's VM asking for %s = %d %s, want granted", secret, status, body)
				}
				grants = append(grants, *granted.GrantID)
			}
			narrow(ts)
			for _, grant := range grants {
				if status, body := ts.session(t, key, enrollmentID, http.MethodPost, "/v1/grants/"+grant+"/values", nil); status != http.StatusUnauthorized {
					t.Fatalf("releasing grant %s after the fleet was narrowed to %s = %d %s, want 401", grant, name, status, body)
				}
			}
		})
	}
}

// TestANarrowedVMOpensNoRequest: once the fleet is narrowed past an enrolled VM, a new request is
// refused 401 whatever it asks for: SHARED_KEY (shared, human tier, which would open a record
// anyone may approve), WORKER_TOKEN (shared, agent tier) and FLEET_KEY (its service's). No request
// row and no record is written.
func TestANarrowedVMOpensNoRequest(t *testing.T) {
	for name, narrow := range narrowings {
		t.Run(name, func(t *testing.T) {
			ts := newVMTestServer(t)
			_, _, enrollmentID, key := ts.enrolled(t, member("example-zone-a", "1001"))
			narrow(ts)
			for _, secret := range []string{"SHARED_KEY", "WORKER_TOKEN", "FLEET_KEY"} {
				if status, body := ts.askAs(t, key, enrollmentID, secret); status != http.StatusUnauthorized {
					t.Fatalf("the VM asking for %s after the fleet was narrowed to %s = %d %s, want 401", secret, name, status, body)
				}
			}
			if got := ts.column(t, `select count(*)::text from requests where enrollment_id = $1`, enrollmentID); !slices.Equal(got, []string{"0"}) {
				t.Fatalf("requests of the narrowed VM = %v, want none", got)
			}
		})
	}
}

// TestANarrowedVMsPendingRequestGrantsNothingWhenApproved: a fleet VM's request for DEEL_API_KEY
// (a person's, human tier) waits on that person; once the fleet is narrowed past the VM, the
// person's approval grants nothing: the request ends denied, naming why, with no grant, as a
// request of a session that ended does.
func TestANarrowedVMsPendingRequestGrantsNothingWhenApproved(t *testing.T) {
	for name, narrow := range narrowings {
		t.Run(name, func(t *testing.T) {
			ts := newVMTestServer(t)
			_, _, enrollmentID, key := ts.enrolled(t, member("example-zone-a", "1001"))
			status, body := ts.askAs(t, key, enrollmentID, "DEEL_API_KEY")
			pending := decode[wireCreateRequestResponse](t, body)
			if status != http.StatusOK || pending.State != "pending" || pending.RecordID == nil {
				t.Fatalf("the fleet's VM asking for DEEL_API_KEY = %d %s, want pending on a record", status, body)
			}
			narrow(ts)
			status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+*pending.RecordID+"/approve", map[string]any{"approver": testApprover})
			if approved := decode[wireCreateRequestResponse](t, body); approved.GrantID != nil || approved.State == "granted" {
				t.Fatalf("approving the narrowed VM's request (%s) = %d %s, want no grant", name, status, body)
			}
			if got := ts.column(t, `select state || ' ' || coalesce(decision_detail, '') from requests where id = $1`, pending.RequestID); !slices.Equal(got, []string{"denied the requesting VM is no longer one its fleet's settings allow"}) {
				t.Fatalf("the narrowed VM's request after the approval = %v, want denied naming the fleet", got)
			}
			if got := ts.column(t, `select count(*)::text from grants where enrollment_id = $1`, enrollmentID); !slices.Equal(got, []string{"0"}) {
				t.Fatalf("grants of the narrowed VM = %v, want none", got)
			}
		})
	}
}

// TestANarrowedVMNeitherRenewsNorEnrolls: once the fleet is narrowed past an enrolled VM, its
// renewal with its own current token is refused 401 ENROLLMENT_NOT_LIVE, on which `agent-secrets
// renew` stops, its launcher credential no longer enrolls (401), and its next login is 403
// ACCOUNT_NOT_LISTED.
func TestANarrowedVMNeitherRenewsNorEnrolls(t *testing.T) {
	for name, narrow := range narrowings {
		t.Run(name, func(t *testing.T) {
			ts := newVMTestServer(t)
			vm := member("example-zone-a", "1001")
			credentialID, launcherKey, enrollmentID, key := ts.enrolled(t, vm)
			narrow(ts)
			if status, body := ts.renewVM(t, key, enrollmentID, map[string]any{"vm_token": ts.current(t, vm)}); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "ENROLLMENT_NOT_LIVE" {
				t.Fatalf("the VM's renewal after the fleet was narrowed to %s = %d %s, want 401 ENROLLMENT_NOT_LIVE", name, status, body)
			}
			if status, body := ts.enrolVM(t, credentialID, launcherKey, vm, ts.current(t, vm), newSigningKey(t)); status != http.StatusUnauthorized {
				t.Fatalf("the VM's credential enrolling after the fleet was narrowed to %s = %d %s, want 401", name, status, body)
			}
			if status, body := ts.vmLogin(t, vm, ts.current(t, vm), newSigningKey(t)); status != http.StatusForbidden || decode[wireError](t, body).Code != "ACCOUNT_NOT_LISTED" {
				t.Fatalf("the VM's login after the fleet was narrowed to %s = %d %s, want 403 ACCOUNT_NOT_LISTED", name, status, body)
			}
		})
	}
}

// TestACopiedTokenNeitherEndsNorTakesOverALiveVM: a VM's identity token copied off the VM and
// replayed after the VM enrolled logs in (the VM's one credential now holds the copier's key) but
// enrolls no key of its own (409 ALREADY_ENROLLED), and its credential cannot end the VM's
// enrollment even knowing its id (403 VM_ENDS_BY_LOGIN_REVOKE). The VM's enrollment and grant keep
// working, its next `enroll --vm` (a fresh login, then the same session key) gets its enrollment
// back, and a person revoking the VM's machine login still ends it.
func TestACopiedTokenNeitherEndsNorTakesOverALiveVM(t *testing.T) {
	ts := newVMTestServer(t)
	vm := member("example-zone-a", "1001")
	victimCred, _, enrollmentID, sessionKey := ts.enrolled(t, vm)
	status, body := ts.askAs(t, sessionKey, enrollmentID, "FLEET_KEY")
	granted := decode[wireCreateRequestResponse](t, body)
	if status != http.StatusOK || granted.GrantID == nil {
		t.Fatalf("the VM asking for FLEET_KEY = %d %s, want granted", status, body)
	}

	copied, copierKey := ts.current(t, vm), newSigningKey(t)
	status, body = ts.vmLogin(t, vm, copied, copierKey)
	if status != http.StatusOK || decode[wireTokenLogin](t, body).CredentialID != victimCred {
		t.Fatalf("the copied token's login = %d %s, want 200 on the VM's own credential", status, body)
	}
	if status, body := ts.enrolVM(t, victimCred, copierKey, vm, copied, newSigningKey(t)); status != http.StatusConflict || decode[wireError](t, body).Code != "ALREADY_ENROLLED" {
		t.Fatalf("the copier enrolling its own key = %d %s, want 409 ALREADY_ENROLLED", status, body)
	}
	if status, body := ts.launcher(t, copierKey, victimCred, http.MethodDelete, "/v1/enrollments/"+enrollmentID, nil); status != http.StatusForbidden || decode[wireError](t, body).Code != "VM_ENDS_BY_LOGIN_REVOKE" {
		t.Fatalf("the copier ending the VM's enrollment = %d %s, want 403 VM_ENDS_BY_LOGIN_REVOKE", status, body)
	}

	if status, body := ts.renewVM(t, sessionKey, enrollmentID, map[string]any{"vm_token": ts.current(t, vm)}); status != http.StatusOK {
		t.Fatalf("the VM's renewal after the copier's login = %d %s, want 200", status, body)
	}
	if status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+*granted.GrantID+"/values", nil); status != http.StatusOK {
		t.Fatalf("the VM's grant values after the copier's login = %d %s, want 200", status, body)
	}
	relogin := newSigningKey(t)
	status, body = ts.vmLogin(t, vm, ts.current(t, vm), relogin)
	if status != http.StatusOK {
		t.Fatalf("the VM's next login = %d %s, want 200", status, body)
	}
	if status, body := ts.enrolVM(t, victimCred, relogin, vm, ts.current(t, vm), sessionKey); status != http.StatusOK || decode[wireEnrolled](t, body).EnrollmentID != enrollmentID {
		t.Fatalf("the VM's next enrollment of its own key = %d %s, want 200 with enrollment %s", status, body, enrollmentID)
	}
	if status, body := ts.enrolVM(t, victimCred, copierKey, vm, copied, newSigningKey(t)); status != http.StatusUnauthorized {
		t.Fatalf("the copier's key after the VM logged in again = %d %s, want 401", status, body)
	}

	if status, body := ts.ui(t, http.MethodPost, "/v1/launcher-credentials/"+victimCred+"/revoke-by-approver", map[string]any{"approver": testApprover}); status != http.StatusOK {
		t.Fatalf("a person revoking the VM's machine login = %d %s, want 200", status, body)
	}
	if status, body := ts.renewVM(t, sessionKey, enrollmentID, map[string]any{"vm_token": ts.current(t, vm)}); status != http.StatusUnauthorized {
		t.Fatalf("the VM's renewal after its login's revoke = %d %s, want 401", status, body)
	}
}

// TestATokenGoesToItsOwnIssuersVerifierAlone: a VM's login never makes the broker read the
// cluster's signing keys, and the Legion daemon's never makes it read the VM issuer's, so a flood of
// one issuer's tokens costs the other issuer nothing.
func TestATokenGoesToItsOwnIssuersVerifierAlone(t *testing.T) {
	ts := newVMTestServer(t)
	cluster, google := ts.podIssuer.JWKSFetches(), ts.vmIssuer.JWKSFetches()
	ts.loggedIn(t, member("example-zone-a", "1001"))
	if got := ts.podIssuer.JWKSFetches(); got != cluster {
		t.Fatalf("the cluster issuer's key-set reads after a VM's login = %d, want %d", got, cluster)
	}
	if got := ts.vmIssuer.JWKSFetches(); got == google {
		t.Fatalf("the VM issuer's key-set reads after a VM's login = %d, want more than %d", got, google)
	}
	google = ts.vmIssuer.JWKSFetches()
	ts.mintServiceLauncherCredential(t)
	if got := ts.vmIssuer.JWKSFetches(); got != google {
		t.Fatalf("the VM issuer's key-set reads after the daemon's login = %d, want %d", got, google)
	}
	for _, token := range []string{"not-a-token", ts.accountToken(t, daemonAccount, "", -time.Minute)} {
		ts.tokenLogin(t, token, ts.daemonRequest(t, newSigningKey(t)))
	}
	if got := ts.vmIssuer.JWKSFetches(); got != google {
		t.Fatalf("the VM issuer's key-set reads after refused cluster tokens = %d, want %d", got, google)
	}
}
