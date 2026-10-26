// approval_test.go drives the whole of how an approval carries the approving person's own
// sign-in: the listed caller opens one, a browser reads the broker's own page, signs in at the
// issuer and comes back, and only then is the record decided. The issuer is a local oidctest
// issuer standing in for Google — the broker's own confidential client of it, which nothing else
// in the deployment holds — so every refusal below is the broker refusing a token it verified,
// not a fake refusing on its behalf.
package api_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/approval"
	"github.com/sjawhar/envoy/internal/broker/ratelimit"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The broker's own OAuth client at the approval issuer, as BROKER_APPROVAL_CLIENT_ID,
// BROKER_APPROVAL_CLIENT_SECRET and BROKER_APPROVAL_DOMAIN name it. testApprover is in that
// domain, which is what makes their sign-in the one these records' approvals take.
const (
	approvalClientID     = "broker-approval-client"
	approvalClientSecret = "broker-approval-secret"
	approvalDomain       = "example.com"
)

// approvalServer is a test server whose approval group is set: the pages are mounted, approve is
// refused, and a sign-in at issuer is what decides a record.
type approvalServer struct {
	*testServer
	issuer *oidctest.Issuer
	// browser is the person's: it keeps the binding cookie the start page sets and follows no
	// redirect, so a test reads each hop itself.
	browser *http.Client
}

func newApprovalServer(t *testing.T, options ...func(*api.Deps)) *approvalServer {
	t.Helper()
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "approval-key"), approvalClientID, approvalClientSecret)
	ts := newTestServerWith(t, func(d *api.Deps) {
		flow, err := oidc.NewConfirmFlow(context.Background(), issuer.URL(), approvalClientID, approvalClientSecret, approvalDomain)
		if err != nil {
			t.Fatalf("oidc.NewConfirmFlow: %v", err)
		}
		d.Approval = &api.ApprovalSignIn{
			Challenges: &approval.Service{Store: d.Enroll.Store},
			Flow:       flow,
			Domain:     approvalDomain,
		}
		for _, option := range options {
			option(d)
		}
	})
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &approvalServer{
		testServer: ts,
		issuer:     issuer,
		browser: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

// pendingSecretRecord makes one pending agent_secret record for DEEL_API_KEY, which testApprover
// owns and so approves.
func (ts *approvalServer) pendingSecretRecord(t *testing.T) string {
	t.Helper()
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "need it for the demo", "DEEL_API_KEY"), "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	if created.State != "pending" || created.RecordID == nil {
		t.Fatalf("POST /v1/requests answered %s, want a pending record", body)
	}
	return *created.RecordID
}

// wireApproval is POST /v1/credential-requests/{record}/approval's answer.
type wireApproval struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// openApproval opens an approval as the listed caller, the call Dispatch's server makes.
func (ts *approvalServer) openApproval(t *testing.T, recordID, approver string, code *string) (int, []byte) {
	t.Helper()
	body := map[string]any{"approver": approver}
	if code != nil {
		body["code"] = *code
	}
	return ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approval", body)
}

// challengeID is the last segment of the page's URL: the approval's own id.
func challengeID(t *testing.T, pageURL string) string {
	t.Helper()
	id := pageURL[strings.LastIndexByte(pageURL, '/')+1:]
	if id == "" {
		t.Fatalf("approval URL %q names no challenge", pageURL)
	}
	return id
}

// get and post are the person's browser: no credential of any kind, the binding cookie carried by
// the jar.
func (ts *approvalServer) get(t *testing.T, path string) (int, string) {
	t.Helper()
	response, err := ts.browser.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	return response.StatusCode, readBody(t, response)
}

// post is the browser pressing the page's button: a form POST on the broker's own origin, which a
// browser marks with that Origin.
func (ts *approvalServer) post(t *testing.T, path string) *http.Response {
	t.Helper()
	return ts.postWith(t, path, map[string]string{"Origin": ts.URL})
}

// postWith is a POST through the browser's cookie jar carrying exactly headers, for a test of what
// the start route refuses.
func (ts *approvalServer) postWith(t *testing.T, path string, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := ts.browser.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	page := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := response.Body.Read(buf)
		page = append(page, buf[:n]...)
		if err != nil {
			break
		}
	}
	return string(page)
}

// workspaceClaims are the claims the issuer signs the next sign-in with: a Google account of the
// deployment's own Workspace domain, authenticated just now.
func workspaceClaims(email string) map[string]any {
	return map[string]any{
		"sub":            "google-subject-" + email,
		"email":          email,
		"email_verified": true,
		"hd":             approvalDomain,
		"auth_time":      time.Now().Add(-time.Minute).Unix(),
	}
}

// signIn runs the whole browser half of one approval: the page, the start that sets the cookie and
// redirects, the issuer's sign-in with claims, and the callback. It answers the callback's status
// and page.
func (ts *approvalServer) signIn(t *testing.T, id string, claims map[string]any) (int, string) {
	t.Helper()
	started := ts.post(t, "/approve/"+id+"/start")
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /approve/%s/start = %d, want 303", id, started.StatusCode)
	}
	ts.issuer.SignInAs(claims)
	code, state := ts.issuer.Authorize(t, started.Header.Get("Location"))
	if state != id {
		t.Fatalf("the issuer came back with state %q, want the challenge id %q", state, id)
	}
	return ts.get(t, "/approve/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state))
}

// recordState is what GET /v1/credential-requests/{id} says the record is now.
func (ts *approvalServer) recordState(t *testing.T, recordID string) wireRecord {
	t.Helper()
	status, body := ts.ui(t, http.MethodGet, "/v1/credential-requests/"+recordID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET the record = %d: %s", status, body)
	}
	return decode[wireRecord](t, body)
}

// TestAnApprovalIsAppliedOnlyByTheApproversOwnSignIn is the whole happy path of a secret request:
// opening the approval decides nothing, the page shows what the broker's own record holds, the
// sign-in's ID token is what approves, and the challenge is spent once — the second callback
// decides nothing more.
func TestAnApprovalIsAppliedOnlyByTheApproversOwnSignIn(t *testing.T) {
	ts := newApprovalServer(t)
	recordID := ts.pendingSecretRecord(t)

	status, body := ts.openApproval(t, recordID, testApprover, nil)
	if status != http.StatusOK {
		t.Fatalf("open an approval = %d: %s", status, body)
	}
	opened := decode[wireApproval](t, body)
	if !strings.HasPrefix(opened.URL, ts.URL+"/approve/") || !opened.ExpiresAt.After(time.Now()) {
		t.Fatalf("open an approval answered %+v, want the broker's own page and a future expiry", opened)
	}
	if read := ts.recordState(t, recordID); read.State != "pending" {
		t.Fatalf("the record is %q once an approval is open, want it still pending", read.State)
	}
	id := challengeID(t, opened.URL)

	pageResponse, err := ts.browser.Get(ts.URL + "/approve/" + id)
	if err != nil {
		t.Fatalf("GET the approval page: %v", err)
	}
	page := readBody(t, pageResponse)
	pageResponse.Body.Close()
	if pageResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET the approval page = %d: %s", pageResponse.StatusCode, page)
	}
	for _, want := range []string{"DEEL_API_KEY", testApprover, "need it for the demo", "Sign in to approve", approvalDomain} {
		if !strings.Contains(page, want) {
			t.Errorf("the approval page does not name %q: %s", want, page)
		}
	}
	// Its form posts here and its answer redirects to the issuer's authorization endpoint, which
	// Chromium checks against form-action too: the policy names both origins, and only those.
	if policy := pageResponse.Header.Get("Content-Security-Policy"); !strings.Contains(policy, "form-action 'self' "+ts.issuer.URL()+";") ||
		!strings.HasPrefix(policy, "default-src 'none';") || !strings.Contains(policy, "frame-ancestors 'none'") {
		t.Errorf("the approval page's Content-Security-Policy is %q, want form-action 'self' and the issuer's origin %s", policy, ts.issuer.URL())
	}
	// Every other page allows this origin alone.
	if response, err := ts.browser.Get(ts.URL + "/approve/" + strings.Repeat("z", 43)); err != nil {
		t.Fatal(err)
	} else {
		response.Body.Close()
		if policy := response.Header.Get("Content-Security-Policy"); !strings.Contains(policy, "form-action 'self';") {
			t.Errorf("a page with no form has Content-Security-Policy %q, want form-action 'self' alone", policy)
		}
	}

	// The start page's redirect: the issuer's authorization endpoint, asking for this person's
	// account through the account chooser, for auth_time, and for nothing to keep.
	started := ts.post(t, "/approve/"+id+"/start")
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /approve/%s/start = %d, want 303", id, started.StatusCode)
	}
	target, err := url.Parse(started.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse the start page's redirect: %v", err)
	}
	query := target.Query()
	if !strings.HasPrefix(target.String(), ts.issuer.URL()) {
		t.Fatalf("the start page redirects to %s, want the approval issuer", target)
	}
	for field, want := range map[string]string{
		"state": id, "login_hint": testApprover, "prompt": "select_account",
		"claims": `{"id_token":{"auth_time":{"essential":true}}}`,
		"hd":     approvalDomain, "client_id": approvalClientID, "response_type": "code",
	} {
		if query.Get(field) != want {
			t.Errorf("the sign-in's %s is %q, want %q", field, query.Get(field), want)
		}
	}
	if query.Get("nonce") == "" {
		t.Error("the sign-in carries no nonce")
	}
	if query.Has("max_age") {
		t.Errorf("the sign-in asks for max_age=%q, which Google ignores", query.Get("max_age"))
	}
	if query.Get("access_type") != "" || query.Get("scope") == "" {
		t.Errorf("the sign-in asks for access_type=%q, want none at all (no refresh token)", query.Get("access_type"))
	}
	var binding *http.Cookie
	for _, cookie := range started.Cookies() {
		if strings.HasPrefix(cookie.Name, "approval_binding_") {
			binding = cookie
		}
	}
	switch {
	case binding == nil:
		t.Fatalf("the start page set no binding cookie: %v", started.Cookies())
	case binding.Name != approval.CookieName(id, false):
		t.Fatalf("the binding cookie is named %q, want this approval's own %q", binding.Name, approval.CookieName(id, false))
	case !binding.HttpOnly || binding.SameSite != http.SameSiteLaxMode || binding.Path != "/approve/" || binding.Value == "":
		t.Fatalf("the binding cookie is %+v, want an HttpOnly SameSite=Lax cookie of /approve/ on this http broker", binding)
	case strings.Contains(query.Get("nonce"), binding.Value):
		t.Fatal("the sign-in's nonce carries the cookie's own value; it must be a hash of it")
	}

	ts.issuer.SignInAs(workspaceClaims(testApprover))
	code, state := ts.issuer.Authorize(t, started.Header.Get("Location"))
	status, page = ts.get(t, "/approve/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state))
	if status != http.StatusOK || !strings.Contains(page, "Approved") {
		t.Fatalf("the callback = %d: %s", status, page)
	}

	read := ts.recordState(t, recordID)
	if read.State != "approved" || read.Decided == nil || read.Decided.Event != "approved" {
		t.Fatalf("the record is %+v, want it approved by the sign-in", read)
	}

	// The broker's own record of whose sign-in decided it.
	spent := ts.spentChallenge(t, id)
	if spent.usedAt == nil || spent.issuer != ts.issuer.URL() || spent.subject != "google-subject-"+testApprover || spent.authTime == nil {
		t.Fatalf("the spent approval is %+v, want the issuer, the token's own sub and its auth_time", spent)
	}

	// Spent once: the same callback again decides nothing more.
	status, page = ts.get(t, "/approve/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state))
	if status != http.StatusGone || !strings.Contains(page, "Already used") {
		t.Fatalf("replaying the callback = %d: %s", status, page)
	}
}

// TestASignInTheBrokerWillNotTakeApprovesNothing is the refusal matrix of the callback: every one
// of these is a token the broker verified and would not act on, and every one leaves the record
// pending and the approval unspent, so the person can start again with the right account.
func TestASignInTheBrokerWillNotTakeApprovesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		claims map[string]any
		want   string
	}{
		"another person's account": {
			claims: workspaceClaims("mallory@example.com"),
			want:   "not the one this approval is for",
		},
		"an account of another domain": {
			claims: func() map[string]any {
				claims := workspaceClaims(testApprover)
				claims["hd"] = "other.example"
				return claims
			}(),
			// html/template escapes the page's own apostrophe, so the match stops before it.
			want: "not in this deployment",
		},
		"an unverified email": {
			claims: func() map[string]any {
				claims := workspaceClaims(testApprover)
				claims["email_verified"] = false
				return claims
			}(),
			want: "no verified email address",
		},
		"a sign-in an hour in the future": {
			claims: func() map[string]any {
				claims := workspaceClaims(testApprover)
				claims["auth_time"] = time.Now().Add(time.Hour).Unix()
				return claims
			}(),
			want: "has not come yet",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ts := newApprovalServer(t)
			recordID := ts.pendingSecretRecord(t)
			_, body := ts.openApproval(t, recordID, testApprover, nil)
			id := challengeID(t, decode[wireApproval](t, body).URL)

			status, page := ts.signIn(t, id, tc.claims)
			if status != http.StatusForbidden || !strings.Contains(page, tc.want) {
				t.Fatalf("the callback = %d, want 403 naming %q: %s", status, tc.want, page)
			}
			if read := ts.recordState(t, recordID); read.State != "pending" {
				t.Fatalf("the record is %q, want it still pending", read.State)
			}
			// Unspent: the right account still approves it.
			if status, page := ts.signIn(t, id, workspaceClaims(testApprover)); status != http.StatusOK {
				t.Fatalf("the approver's own sign-in after a refusal = %d: %s", status, page)
			}
			if read := ts.recordState(t, recordID); read.State != "approved" {
				t.Fatalf("the record is %q after the approver signed in, want approved", read.State)
			}
		})
	}
}

// TestASignInOfAnyAgeApprovesAndItsAuthTimeIsRecorded: Google authenticates nobody again on
// request, so how long ago the person signed in to Google decides nothing. The broker records the
// token's auth_time as the issuer stated it, and records none for a token that carries none.
func TestASignInOfAnyAgeApprovesAndItsAuthTimeIsRecorded(t *testing.T) {
	weekAgo := time.Now().Add(-7 * 24 * time.Hour).Truncate(time.Second)
	for name, tc := range map[string]struct {
		authTime any
		want     *time.Time
	}{
		"a sign-in a week old":              {authTime: weekAgo.Unix(), want: &weekAgo},
		"a token that carries no auth_time": {authTime: nil, want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			ts := newApprovalServer(t)
			recordID := ts.pendingSecretRecord(t)
			_, body := ts.openApproval(t, recordID, testApprover, nil)
			id := challengeID(t, decode[wireApproval](t, body).URL)
			claims := workspaceClaims(testApprover)
			if tc.authTime == nil {
				delete(claims, "auth_time")
			} else {
				claims["auth_time"] = tc.authTime
			}
			if status, page := ts.signIn(t, id, claims); status != http.StatusOK || !strings.Contains(page, "Approved") {
				t.Fatalf("the callback = %d: %s", status, page)
			}
			spent := ts.spentChallenge(t, id)
			switch {
			case tc.want == nil && spent.authTime != nil:
				t.Fatalf("the spent approval records auth_time %v for a token that carried none", *spent.authTime)
			case tc.want != nil && (spent.authTime == nil || !spent.authTime.Equal(*tc.want)):
				t.Fatalf("the spent approval records auth_time %v, want %v", spent.authTime, *tc.want)
			}
		})
	}
}

// spentRow is what approval_challenges holds about one approval once it has been spent.
type spentRow struct {
	usedAt, authTime *time.Time
	issuer, subject  string
}

func (ts *approvalServer) spentChallenge(t *testing.T, id string) spentRow {
	t.Helper()
	var row spentRow
	if err := ts.Store.Pool.QueryRow(context.Background(),
		`select used_at, coalesce(signed_in_issuer,''), coalesce(signed_in_subject,''), signed_in_auth_time from approval_challenges where id=$1`, id).
		Scan(&row.usedAt, &row.issuer, &row.subject, &row.authTime); err != nil {
		t.Fatalf("read the spent approval: %v", err)
	}
	return row
}

// TestACallbackWithoutTheBrowsersOwnBindingApprovesNothing pins the cookie half of the binding: an
// ID token that does not carry the nonce this browser's cookie derives approves nothing, so a
// sign-in captured or started elsewhere cannot be redirected into someone else's approval.
func TestACallbackWithoutTheBrowsersOwnBindingApprovesNothing(t *testing.T) {
	ts := newApprovalServer(t)
	recordID := ts.pendingSecretRecord(t)
	_, body := ts.openApproval(t, recordID, testApprover, nil)
	id := challengeID(t, decode[wireApproval](t, body).URL)

	started := ts.post(t, "/approve/"+id+"/start")
	ts.issuer.SignInAs(workspaceClaims(testApprover))
	code, state := ts.issuer.Authorize(t, started.Header.Get("Location"))
	callback := "/approve/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)

	for name, cookie := range map[string]string{
		"no cookie at all":                       "",
		"another browser's for this approval":    approval.CookieName(id, false) + "=" + strings.Repeat("a", 43),
		"the old shared name, which is no one's": "approval_binding=" + strings.Repeat("a", 43),
	} {
		t.Run(name, func(t *testing.T) {
			headers := map[string]string{}
			if cookie != "" {
				headers["Cookie"] = cookie
			}
			status, page := ts.req(t, http.MethodGet, callback, headers, nil)
			if status != http.StatusForbidden {
				t.Fatalf("the callback (%s) = %d, want 403: %s", name, status, page)
			}
			if read := ts.recordState(t, recordID); read.State != "pending" {
				t.Fatalf("the record is %q, want it still pending", read.State)
			}
		})
	}
}

// TestTheSignInStartsOnlyFromTheBrokersOwnPage: /start sends a browser to the issuer only for the
// button on the broker's own page, which a browser marks same-origin. Another site's form — or a
// request that says nothing about where it came from — would take the person to the account
// chooser without their having been shown what they are approving, so it is refused, sets no
// cookie and redirects nowhere.
func TestTheSignInStartsOnlyFromTheBrokersOwnPage(t *testing.T) {
	ts := newApprovalServer(t)
	recordID := ts.pendingSecretRecord(t)
	_, body := ts.openApproval(t, recordID, testApprover, nil)
	id := challengeID(t, decode[wireApproval](t, body).URL)
	start := "/approve/" + id + "/start"

	for name, headers := range map[string]map[string]string{
		"no Origin and no Sec-Fetch-Site":  {},
		"another site's Origin":            {"Origin": "https://attacker.example"},
		"a cross-site fetch":               {"Sec-Fetch-Site": "cross-site"},
		"a same-site sibling's fetch":      {"Sec-Fetch-Site": "same-site", "Origin": "https://dispatch.example"},
		"a null Origin":                    {"Origin": "null"},
		"an Origin with the broker's path": {"Origin": ts.URL + "/approve"},
	} {
		t.Run(name, func(t *testing.T) {
			refused := ts.postWith(t, start, headers)
			if refused.StatusCode != http.StatusForbidden || refused.Header.Get("Location") != "" || len(refused.Cookies()) != 0 {
				t.Fatalf("POST %s (%s) = %d, Location %q, cookies %v; want 403 with no redirect and no cookie",
					start, name, refused.StatusCode, refused.Header.Get("Location"), refused.Cookies())
			}
		})
	}
	for name, headers := range map[string]map[string]string{
		"Sec-Fetch-Site: same-origin": {"Sec-Fetch-Site": "same-origin"},
		"the broker's own Origin":     {"Origin": ts.URL},
	} {
		t.Run(name, func(t *testing.T) {
			if started := ts.postWith(t, start, headers); started.StatusCode != http.StatusSeeOther {
				t.Fatalf("POST %s (%s) = %d, want 303 to the issuer", start, name, started.StatusCode)
			}
		})
	}
}

// TestTwoApprovalsInOneBrowserKeepTheirOwnBindings: each approval's cookie is its own, so starting
// a second approval in the same browser leaves the first one's sign-in able to finish.
func TestTwoApprovalsInOneBrowserKeepTheirOwnBindings(t *testing.T) {
	ts := newApprovalServer(t)
	first := ts.pendingSecretRecord(t)
	_, body := ts.openApproval(t, first, testApprover, nil)
	firstID := challengeID(t, decode[wireApproval](t, body).URL)
	_, body = ts.openApproval(t, first, testApprover, nil)
	secondID := challengeID(t, decode[wireApproval](t, body).URL)

	firstStart := ts.post(t, "/approve/"+firstID+"/start")
	secondStart := ts.post(t, "/approve/"+secondID+"/start")
	if firstStart.StatusCode != http.StatusSeeOther || secondStart.StatusCode != http.StatusSeeOther {
		t.Fatalf("the two starts = %d and %d, want 303 each", firstStart.StatusCode, secondStart.StatusCode)
	}
	brokerURL, err := url.Parse(ts.URL + "/approve/callback")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, cookie := range ts.browser.Jar.Cookies(brokerURL) {
		names[cookie.Name] = true
	}
	if !names[approval.CookieName(firstID, false)] || !names[approval.CookieName(secondID, false)] {
		t.Fatalf("the browser holds cookies %v, want one for each approval", names)
	}

	// The first approval's sign-in finishes after the second was started.
	ts.issuer.SignInAs(workspaceClaims(testApprover))
	code, state := ts.issuer.Authorize(t, firstStart.Header.Get("Location"))
	status, page := ts.get(t, "/approve/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state))
	if status != http.StatusOK || !strings.Contains(page, "Approved") {
		t.Fatalf("the first approval's callback after the second started = %d: %s", status, page)
	}
}

// TestAnHTTPSBrokersBindingCookieIsAHostCookie: a deployment's broker is reached over https, and
// its binding cookie is a __Host- cookie — Secure, Path=/, no Domain — which a browser lets no
// other host of the broker's registrable domain set. The same broker's handlers serve it here,
// with BROKER_PUBLIC_URL https, over a second test listener.
func TestAnHTTPSBrokersBindingCookieIsAHostCookie(t *testing.T) {
	var deps api.Deps
	ts := newApprovalServer(t, func(d *api.Deps) { deps = *d })
	recordID := ts.pendingSecretRecord(t)
	_, body := ts.openApproval(t, recordID, testApprover, nil)
	id := challengeID(t, decode[wireApproval](t, body).URL)

	const publicURL = "https://broker.example"
	deps.PublicURL = publicURL
	mux := http.NewServeMux()
	api.Register(mux, deps)
	deployed := httptest.NewServer(mux)
	t.Cleanup(deployed.Close)

	request, err := http.NewRequest(http.MethodPost, deployed.URL+"/approve/"+id+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", publicURL)
	response, err := ts.browser.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /approve/%s/start = %d, want 303", id, response.StatusCode)
	}
	setCookie := response.Header.Values("Set-Cookie")
	if len(setCookie) != 1 {
		t.Fatalf("Set-Cookie = %q, want one binding cookie", setCookie)
	}
	header := setCookie[0]
	want := approval.CookieName(id, true) + "="
	if !strings.HasPrefix(header, "__Host-approval_binding_") || !strings.HasPrefix(header, want) {
		t.Fatalf("Set-Cookie = %q, want this approval's own __Host- cookie %s", header, want)
	}
	for _, attribute := range []string{"Path=/;", "Secure", "HttpOnly", "SameSite=Lax"} {
		if !strings.Contains(header+";", attribute) {
			t.Errorf("Set-Cookie = %q, want %s", header, attribute)
		}
	}
	if strings.Contains(header, "Domain=") || strings.Contains(header, "Path=/approve") {
		t.Errorf("Set-Cookie = %q, want no Domain and Path=/ alone, as __Host- requires", header)
	}
}

// TestAnApprovalThatIsUnknownOrExpiredShowsSoAndDecidesNothing: the two states a link can be in
// that are neither live nor spent.
func TestAnApprovalThatIsUnknownOrExpiredShowsSoAndDecidesNothing(t *testing.T) {
	ts := newApprovalServer(t)
	if status, page := ts.get(t, "/approve/"+strings.Repeat("z", 43)); status != http.StatusNotFound || !strings.Contains(page, "Nothing to approve") {
		t.Fatalf("an unknown approval's page = %d: %s", status, page)
	}

	recordID := ts.pendingSecretRecord(t)
	_, body := ts.openApproval(t, recordID, testApprover, nil)
	id := challengeID(t, decode[wireApproval](t, body).URL)
	if _, err := ts.Store.Pool.Exec(context.Background(),
		`update approval_challenges set expires_at=now() - interval '1 minute' where id=$1`, id); err != nil {
		t.Fatalf("expire the approval: %v", err)
	}
	if status, page := ts.get(t, "/approve/"+id); status != http.StatusGone || !strings.Contains(page, "Expired") {
		t.Fatalf("an expired approval's page = %d: %s", status, page)
	}
	if started := ts.post(t, "/approve/"+id+"/start"); started.StatusCode != http.StatusGone || started.Header.Get("Location") != "" {
		t.Fatalf("starting an expired approval = %d (Location %q), want 410 and no redirect", started.StatusCode, started.Header.Get("Location"))
	}
	if read := ts.recordState(t, recordID); read.State != "pending" {
		t.Fatalf("the record is %q, want it still pending", read.State)
	}
}

// TestAnApprovalIsNotOpenedForARecordPastItsExpiry: a record past its own expiry is refused when
// the approval is opened, as the decision would refuse it, whether or not the sweeper has
// recorded it expired yet — so nobody is sent to sign in for something that cannot be approved.
func TestAnApprovalIsNotOpenedForARecordPastItsExpiry(t *testing.T) {
	ts := newApprovalServer(t, func(d *api.Deps) { d.MachineLogin.PendingTTL = time.Second })

	recordID := ts.pendingSecretRecord(t)
	if _, err := ts.Store.Pool.Exec(context.Background(),
		`update requests set pending_expires_at=now() - interval '1 minute' where record_id=$1`, recordID); err != nil {
		t.Fatalf("expire the request: %v", err)
	}
	status, body := ts.openApproval(t, recordID, testApprover, nil)
	if status != http.StatusConflict || decode[wireError](t, body).Code != "RECORD_TERMINAL" ||
		!strings.Contains(decode[wireError](t, body).Error, "expired") {
		t.Fatalf("open an approval of an unswept expired request = %d %s, want 409 RECORD_TERMINAL naming the expiry", status, body)
	}

	machineKey := newSigningKey(t)
	code := ts.openMachineLogin(t, machineKey, testApprover, "example-host-devbox")
	_, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	machineRecord := decode[wireRecord](t, body).RecordID
	time.Sleep(1100 * time.Millisecond)
	status, body = ts.openApproval(t, machineRecord, testApprover, &code)
	if status != http.StatusConflict || decode[wireError](t, body).Code != "RECORD_TERMINAL" ||
		!strings.Contains(decode[wireError](t, body).Error, "expired") {
		t.Fatalf("open an approval of an unswept expired machine login = %d %s, want 409 RECORD_TERMINAL naming the expiry", status, body)
	}
}

// TestApproveIsRefusedWhileTheBrokerTakesSignIns: the account caller's own approve route is closed
// on a broker that can take a person's sign-in, for either kind of record, while deny — which can
// only take access away — is untouched.
func TestApproveIsRefusedWhileTheBrokerTakesSignIns(t *testing.T) {
	ts := newApprovalServer(t)
	recordID := ts.pendingSecretRecord(t)

	status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"approver": testApprover})
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "APPROVAL_NEEDS_PERSON" {
		t.Fatalf("approve as the listed caller = %d %s, want 403 APPROVAL_NEEDS_PERSON", status, body)
	}
	if read := ts.recordState(t, recordID); read.State != "pending" {
		t.Fatalf("the record is %q after a refused approve, want it still pending", read.State)
	}

	machineKey := newSigningKey(t)
	code := ts.openMachineLogin(t, machineKey, testApprover, "example-host-devbox")
	_, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	machineRecord := decode[wireRecord](t, body).RecordID
	status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+machineRecord+"/approve",
		map[string]any{"approver": testApprover, "code": code})
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "APPROVAL_NEEDS_PERSON" {
		t.Fatalf("approve a machine login as the listed caller = %d %s, want 403 APPROVAL_NEEDS_PERSON", status, body)
	}

	status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/deny",
		map[string]any{"approver": testApprover})
	if status != http.StatusOK {
		t.Fatalf("deny as the listed caller = %d %s, want the 200 it always answered", status, body)
	}
}

// TestWithoutTheApprovalGroupTheBrokerDecidesAsItAlwaysDid is the rollout's first half: no issuer,
// no pages, the approval route says what is unset, and approve goes on deciding on the caller's
// word.
func TestWithoutTheApprovalGroupTheBrokerDecidesAsItAlwaysDid(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "need it", "DEEL_API_KEY"), "session_id": nil})
	recordID := *decode[wireCreateRequestResponse](t, body).RecordID

	status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approval",
		map[string]any{"approver": testApprover})
	if status != http.StatusServiceUnavailable || decode[wireError](t, body).Code != "APPROVAL_SIGN_IN_OFF" {
		t.Fatalf("open an approval with no issuer = %d %s, want 503 APPROVAL_SIGN_IN_OFF", status, body)
	}
	if status, page := ts.req(t, http.MethodGet, "/approve/"+strings.Repeat("z", 43), nil, nil); status != http.StatusNotFound {
		t.Fatalf("the approval page with no issuer = %d, want 404 from the mux: %s", status, page)
	}
	status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"approver": testApprover})
	if status != http.StatusOK {
		t.Fatalf("approve with no issuer = %d %s, want the 200 it always answered", status, body)
	}
}

// TestOpeningAnApprovalRefusesWhatTheDecisionWould: every refusal the decision itself would make
// is made here, before anyone is sent off to sign in.
func TestOpeningAnApprovalRefusesWhatTheDecisionWould(t *testing.T) {
	ts := newApprovalServer(t)
	recordID := ts.pendingSecretRecord(t)

	for name, tc := range map[string]struct {
		record   string
		approver string
		code     *string
		status   int
		code2    string
	}{
		"another login":    {recordID, "mallory@example.com", nil, http.StatusForbidden, "NOT_APPROVER"},
		"no approver":      {recordID, "   ", nil, http.StatusBadRequest, "APPROVER_REQUIRED"},
		"unknown record":   {strings.Repeat("a", 64), testApprover, nil, http.StatusNotFound, "NOT_FOUND"},
		"malformed record": {"not-a-record-id", testApprover, nil, http.StatusBadRequest, "RECORD_ID_INPUT"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := ts.openApproval(t, tc.record, tc.approver, tc.code)
			if status != tc.status || decode[wireError](t, body).Code != tc.code2 {
				t.Fatalf("open an approval (%s) = %d %s, want %d %s", name, status, body, tc.status, tc.code2)
			}
		})
	}

	// A machine login's code is checked against the record now, not at the end of a sign-in.
	machineKey := newSigningKey(t)
	code := ts.openMachineLogin(t, machineKey, testApprover, "example-host-devbox")
	_, body := ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	machineRecord := decode[wireRecord](t, body).RecordID

	status, body := ts.openApproval(t, machineRecord, testApprover, nil)
	if status != http.StatusBadRequest || decode[wireError](t, body).Code != "CODE_REQUIRED" {
		t.Fatalf("open a machine login's approval with no code = %d %s, want 400 CODE_REQUIRED", status, body)
	}
	wrong := "ZZZZ-9999"
	status, body = ts.openApproval(t, machineRecord, testApprover, &wrong)
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "CODE_MISMATCH" {
		t.Fatalf("open a machine login's approval with another code = %d %s, want 403 CODE_MISMATCH", status, body)
	}

	// A decided record has nothing to approve.
	if status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/deny",
		map[string]any{"approver": testApprover}); status != http.StatusOK {
		t.Fatalf("deny = %d: %s", status, body)
	}
	status, body = ts.openApproval(t, recordID, testApprover, nil)
	if status != http.StatusConflict || decode[wireError](t, body).Code != "RECORD_TERMINAL" {
		t.Fatalf("open an approval of a decided record = %d %s, want 409 RECORD_TERMINAL", status, body)
	}
}

// TestAMachineLoginIsMintedByTheOperatorsOwnSignIn drives the machine-login half end to end: the
// code the person typed goes in when the approval is opened, the sign-in approves, and the
// credential is minted for that person.
func TestAMachineLoginIsMintedByTheOperatorsOwnSignIn(t *testing.T) {
	ts := newApprovalServer(t)
	machineKey := newSigningKey(t)
	code := ts.openMachineLogin(t, machineKey, testApprover, "example-host-devbox")
	_, body := ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	recordID := decode[wireRecord](t, body).RecordID

	status, body := ts.openApproval(t, recordID, testApprover, &code)
	if status != http.StatusOK {
		t.Fatalf("open a machine login's approval = %d: %s", status, body)
	}
	id := challengeID(t, decode[wireApproval](t, body).URL)

	status, page := ts.get(t, "/approve/"+id)
	if status != http.StatusOK || !strings.Contains(page, "example-host-devbox") {
		t.Fatalf("the machine login's approval page = %d: %s", status, page)
	}
	status, page = ts.signIn(t, id, workspaceClaims(testApprover))
	if status != http.StatusOK || !strings.Contains(page, "Approved") {
		t.Fatalf("the callback = %d: %s", status, page)
	}

	read := ts.recordState(t, recordID)
	if read.State != "approved" || read.Decided == nil || read.Decided.CredentialID == nil || *read.Decided.CredentialID == "" {
		t.Fatalf("the machine login is %+v, want it approved with a credential", read)
	}
	// The code never left the broker: the challenge kept its hash alone.
	var hash []byte
	if err := ts.Store.Pool.QueryRow(context.Background(), `select code_hash from approval_challenges where id=$1`, id).Scan(&hash); err != nil {
		t.Fatalf("read the approval's code hash: %v", err)
	}
	if len(hash) != 32 || strings.Contains(string(hash), code) {
		t.Fatalf("the approval holds %q, want the 32-byte hash of the code", hash)
	}
}

// TestTheApprovalPagesAreRateLimitedPerAddress: the pages take no credential, so their bucket is
// the only thing in front of them.
func TestTheApprovalPagesAreRateLimitedPerAddress(t *testing.T) {
	ts := newApprovalServer(t, func(d *api.Deps) {
		d.ApprovalLimit = &ratelimit.Limit{Every: time.Minute, Burst: 1}
	})
	recordID := ts.pendingSecretRecord(t)
	_, body := ts.openApproval(t, recordID, testApprover, nil)
	id := challengeID(t, decode[wireApproval](t, body).URL)

	if status, page := ts.get(t, "/approve/"+id); status != http.StatusOK {
		t.Fatalf("the first page = %d: %s", status, page)
	}
	response, err := ts.browser.Get(ts.URL + "/approve/" + id)
	if err != nil {
		t.Fatalf("the second page: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") == "" {
		t.Fatalf("the second page = %d (Retry-After %q), want 429 with a Retry-After",
			response.StatusCode, response.Header.Get("Retry-After"))
	}
}
