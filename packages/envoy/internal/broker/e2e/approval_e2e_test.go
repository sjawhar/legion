// approval_e2e_test.go is the acceptance proof of the other half of the broker's approval story:
// a broker whose BROKER_APPROVAL_* group is set, where the listed caller can no longer approve
// anything and every approval goes through the broker's own page and the approving person's own
// sign-in. e2e_test.go drives the same broker with the group unset, which is what a deployment
// runs until it sets it.
//
// The issuer is the same local one the rest of this package uses, armed with a code flow for the
// broker's own confidential client: a test signs in there as a browser does, so what approves a
// record here is an ID token the broker verified, exactly as in production.
package e2e

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// The broker's own OAuth client at the approval issuer, and the Workspace domain every approving
// person must be in: testApprover and otherPerson are both in it, so what tells them apart is the
// email the sign-in returns and nothing else.
const (
	approvalClientID     = "broker-approval-client"
	approvalClientSecret = "broker-approval-secret"
	approvalDomain       = "example.com"
)

// browser is a person's: it keeps the binding cookie the start page sets and follows no redirect,
// so each hop is read here.
func newBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// openApproval opens an approval as the listed caller — the call Dispatch's server makes — and
// answers the page's URL.
func (s *e2eServer) openApproval(t *testing.T, recordID, approver string, code *string) string {
	t.Helper()
	body := map[string]any{"approver": approver}
	if code != nil {
		body["code"] = *code
	}
	status, answer := s.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approval", body)
	if status != http.StatusOK {
		t.Fatalf("open an approval of %s = %d: %s", recordID, status, answer)
	}
	opened := decode[struct {
		URL string `json:"url"`
	}](t, answer)
	if !strings.HasPrefix(opened.URL, s.URL+"/approve/") {
		t.Fatalf("open an approval answered %s, want the broker's own page", answer)
	}
	return opened.URL
}

// signIn drives the browser's half of an open approval as email: the page, the start that sets the
// cookie and redirects, the sign-in at the issuer, and the callback. It answers the callback's
// status and page.
func (s *e2eServer) signIn(t *testing.T, browser *http.Client, approvalURL, email string) (int, string) {
	t.Helper()
	page, err := browser.Get(approvalURL)
	if err != nil {
		t.Fatalf("GET %s: %v", approvalURL, err)
	}
	page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("the approval page = %d, want 200", page.StatusCode)
	}
	started, err := browser.Post(approvalURL+"/start", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST %s/start: %v", approvalURL, err)
	}
	started.Body.Close()
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("the sign-in's start = %d, want 303", started.StatusCode)
	}
	s.approvalIssuer.SignInAs(map[string]any{
		"sub": "google-subject-" + email, "email": email, "email_verified": true,
		"hd": approvalDomain, "auth_time": time.Now().Add(-time.Minute).Unix(),
	})
	code, state := s.approvalIssuer.Authorize(t, started.Header.Get("Location"))
	done, err := browser.Get(s.URL + "/approve/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatalf("GET the callback: %v", err)
	}
	defer done.Body.Close()
	answer := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := done.Body.Read(buf)
		answer = append(answer, buf[:n]...)
		if err != nil {
			break
		}
	}
	return done.StatusCode, string(answer)
}

// TestEndToEndWithTheApproversOwnSignIn drives the whole story on a broker that takes a person's
// sign-in: a machine login approved by its operator's own sign-in mints the credential that
// enrolls a session; that session's secret request is approved the same way and releases its
// value; the listed caller's own approve is refused outright, and another person's sign-in on an
// approval that is not theirs decides nothing.
func TestEndToEndWithTheApproversOwnSignIn(t *testing.T) {
	ts := newE2EServer(t, withApprovalSignIn())

	// 1. The machine login: its code goes in when the approval is opened, and the operator's own
	// sign-in mints the credential.
	machineKey := brokertest.NewSigningKey(t)
	code := ts.deviceLogin(t, machineKey, testApprover, "e2e-approval-agents").UserCode
	_, body := ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	machineRecord := decode[wireRecord](t, body).RecordID

	refused, answer := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+machineRecord+"/approve",
		map[string]any{"approver": testApprover, "code": code})
	if refused != http.StatusForbidden || decode[wireError](t, answer).Code != "APPROVAL_NEEDS_PERSON" {
		t.Fatalf("the listed caller's approve = %d %s, want 403 APPROVAL_NEEDS_PERSON", refused, answer)
	}

	approvalURL := ts.openApproval(t, machineRecord, testApprover, &code)
	if status, page := ts.signIn(t, newBrowser(t), approvalURL, testApprover); status != http.StatusOK || !strings.Contains(page, "Approved") {
		t.Fatalf("the operator's sign-in = %d: %s", status, page)
	}
	status, body := ts.ui(t, http.MethodGet, "/v1/credential-requests/"+machineRecord, nil)
	read := decode[struct {
		State   string `json:"state"`
		Decided *struct {
			CredentialID *string `json:"credential_id"`
		} `json:"decided"`
	}](t, body)
	if status != http.StatusOK || read.State != "approved" || read.Decided == nil || read.Decided.CredentialID == nil {
		t.Fatalf("the machine login after the sign-in = %d %s, want approved with a credential", status, body)
	}
	credentialID := *read.Decided.CredentialID

	// 2. The session that credential enrolls, and its own request for a secret the operator owns.
	sessionKey := brokertest.NewSigningKey(t)
	thumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	status, body = ts.launcher(t, machineKey, credentialID, http.MethodPost, "/v1/enrollments",
		map[string]any{"kind": "session", "runtime_id": "e2e-approval-session", "thumbprint": thumbprint})
	if status != http.StatusCreated {
		t.Fatalf("enroll the session = %d: %s", status, body)
	}
	enrollmentID := decode[struct {
		EnrollmentID string `json:"enrollment_id"`
	}](t, body).EnrollmentID

	requestID, recordID := ts.createPending(t, sessionKey, enrollmentID, "deploy the demo", "DEEL_API_KEY")

	// 3. Another person signs in on an approval that is not theirs: nothing is decided, and the
	// approval is still the approver's to spend.
	approvalURL = ts.openApproval(t, recordID, testApprover, nil)
	if status, page := ts.signIn(t, newBrowser(t), approvalURL, otherPerson); status != http.StatusForbidden {
		t.Fatalf("%s's sign-in on %s's approval = %d: %s", otherPerson, testApprover, status, page)
	}
	status, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID, nil)
	if decode[wireRecord](t, body).State != "pending" {
		t.Fatalf("the record after the wrong person's sign-in = %s, want it still pending", body)
	}

	// 4. The approver's own sign-in approves it, and the session reads the value.
	if status, page := ts.signIn(t, newBrowser(t), approvalURL, testApprover); status != http.StatusOK || !strings.Contains(page, "Approved") {
		t.Fatalf("the approver's sign-in = %d: %s", status, page)
	}
	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/requests/"+requestID, nil)
	granted := decode[wireRequestStatus](t, body)
	if status != http.StatusOK || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("the request after the approval = %d %s, want granted with a grant", status, body)
	}
	ts.awaitValue(t, sessionKey, enrollmentID, *granted.GrantID, "DEEL_API_KEY", "deel-v1")

	// 5. The approval is spent: its own page says so, and there is nothing left to sign in for.
	spent, err := newBrowser(t).Get(approvalURL)
	if err != nil {
		t.Fatalf("GET the spent approval: %v", err)
	}
	defer spent.Body.Close()
	if spent.StatusCode != http.StatusGone {
		t.Fatalf("the spent approval's page = %d, want 410", spent.StatusCode)
	}
}
