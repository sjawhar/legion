package approval_test

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/approval"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
)

// TestThePagePrintsTheRecordAfterTheBrokersOwnSignIn pins A1: a person with no session is sent to
// the provider as the broker's own client, returning to the broker's own host, with PKCE S256, a
// nonce, a state and the record's approver as login_hint, and no offline access or forced consent;
// signed in, the page prints the record's facts from the broker's own record, with Approve and
// Deny, every other pending request the person may decide, and "Approve all N shown". A machine
// login's record links Dispatch's page and offers no button; a record another person must decide
// names who and offers only the switch; an unknown record is a 404.
func TestThePagePrintsTheRecordAfterTheBrokersOwnSignIn(t *testing.T) {
	r := newRig(t, true)
	enrollmentID, key := r.enroll(t, "ben-box", "ben-box-runtime", "enrollment-session-1")
	lifetime := 3600
	first := r.request(t, enrollmentID, key, "deploy the billing service", nil, "claimed-session-2", "TEST_SECRET")
	second := r.request(t, enrollmentID, key, "rotate the second key", &lifetime, "", "SECOND_SECRET")

	b := r.browser(t)
	auth := b.StartSignIn(t, "/approve/"+first.recordID)
	issuerURL, err := url.Parse(r.issuer.URL())
	if err != nil {
		t.Fatal(err)
	}
	query := auth.Query()
	switch {
	case auth.Host != issuerURL.Host || auth.Path != "/authorize":
		t.Errorf("the sign-in goes to %s, want the provider's /authorize", auth)
	case query.Get("client_id") != clientID:
		t.Errorf("client_id = %q, want the broker's own %q", query.Get("client_id"), clientID)
	case query.Get("redirect_uri") != r.url+"/signin/callback":
		t.Errorf("redirect_uri = %q, want the broker's own %s/signin/callback", query.Get("redirect_uri"), r.url)
	case query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "":
		t.Errorf("PKCE = %q %q, want an S256 challenge", query.Get("code_challenge_method"), query.Get("code_challenge"))
	case query.Get("nonce") == "" || query.Get("state") == "":
		t.Errorf("nonce %q, state %q: want both", query.Get("nonce"), query.Get("state"))
	case query.Get("login_hint") != ada:
		t.Errorf("login_hint = %q, want the record's approver %s", query.Get("login_hint"), ada)
	case query.Get("hd") != domain:
		t.Errorf("hd = %q, want %s", query.Get("hd"), domain)
	case query.Has("access_type") || query.Has("prompt"):
		t.Errorf("the sign-in asks access_type=%q prompt=%q, want neither", query.Get("access_type"), query.Get("prompt"))
	}
	res := b.Get(t, b.Authorize(t, auth, person(ada)))
	if res.Status != http.StatusSeeOther || res.Header.Get("Location") != "/approve/"+first.recordID {
		t.Fatalf("callback = %d to %q, want 303 back to the record", res.Status, res.Header.Get("Location"))
	}

	res = b.Page(t, first.recordID)
	got := pageText(t, res.Body)
	for _, want := range []string{
		"Signed in as " + ada,
		"TEST_SECRET",
		"Requested by (verified by the broker) a session enrollment, operator " + ben + ", runtime ben-box-runtime",
		"Session (as the enrollment stated it) enrollment-session-1",
		"Session the request names (its own unsigned claim, not verified) claimed-session-2",
		"Reason deploy the billing service",
		"Lasts once approved until the session ends",
		"Approver " + ada,
		"State pending",
		"SECOND_SECRET",
		"Lasts once approved 1h0m0s",
		"Reason rotate the second key",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the page does not say %q:\n%s", want, got)
		}
	}
	detail, err := r.machine.ReadRecord(t.Context(), first.recordID)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{detail.RequestedAt, detail.ExpiresAt} {
		if want := at.UTC().Format("2006-01-02 15:04:05 UTC"); !strings.Contains(got, want) {
			t.Errorf("the page does not print the instant %s:\n%s", want, got)
		}
	}
	if !strings.Contains(res.Body, `href="/approve/`+first.recordID+`?switch=1"`) {
		t.Errorf("the page has no switch link to the record:\n%s", res.Body)
	}
	for _, p := range []pending{first, second} {
		form := decisionForm(t, res.Body, p.recordID)
		if form == nil {
			t.Fatalf("the page offers no Approve/Deny form for %s:\n%s", p.recordID, res.Body)
		}
	}
	if _, label := approveAll(t, res.Body); label != "Approve all 2 shown" {
		t.Errorf("approve-all button = %q, want \"Approve all 2 shown\"", label)
	}
	if !strings.Contains(res.Body, `<button type="submit" name="decision" value="approve">Approve</button>`) ||
		!strings.Contains(res.Body, `<button type="submit" name="decision" value="deny" class="deny">Deny</button>`) {
		t.Errorf("the page has no Approve and Deny buttons:\n%s", res.Body)
	}
	if strings.Contains(strings.ToLower(res.Body), "<script") {
		t.Errorf("the page carries a script:\n%s", res.Body)
	}

	t.Run("a machine login links Dispatch's page and offers no button", func(t *testing.T) {
		machineRecord, _ := r.machineLogin(t, newKey(t), ada, "ada-laptop")
		res := b.Page(t, machineRecord)
		if !strings.Contains(pageText(t, res.Body), "Machine login for ada-laptop") ||
			!strings.Contains(res.Body, `href="`+brokertest.MachineLoginURL+`"`) {
			t.Errorf("the machine login's page does not link %s:\n%s", brokertest.MachineLoginURL, res.Body)
		}
		if decisionForm(t, res.Body, machineRecord) != nil {
			t.Errorf("the machine login's page offers a decision form:\n%s", res.Body)
		}
	})

	t.Run("another person's record names who decides it and offers only the switch", func(t *testing.T) {
		other := r.browser(t)
		other.SignIn(t, first.recordID, person(carol))
		res := other.Page(t, first.recordID)
		got := pageText(t, res.Body)
		if !strings.Contains(got, "Only "+ada+" may decide this request, and you are signed in as "+carol) {
			t.Errorf("carol's page does not say ada decides it:\n%s", got)
		}
		if decisionForm(t, res.Body, first.recordID) != nil || decisionForm(t, res.Body, second.recordID) != nil {
			t.Errorf("carol's page offers a decision form for ada's records:\n%s", res.Body)
		}
		if !strings.Contains(res.Body, `href="/approve/`+first.recordID+`?switch=1"`) {
			t.Errorf("carol's page has no switch link:\n%s", res.Body)
		}
	})

	t.Run("an unknown record is a 404 and a malformed id a 400", func(t *testing.T) {
		if res := b.Get(t, "/approve/"+strings.Repeat("0", 64)); res.Status != http.StatusNotFound {
			t.Errorf("unknown record = %d, want 404", res.Status)
		}
		if res := b.Get(t, "/approve/"+strings.ToUpper(first.recordID)); res.Status != http.StatusBadRequest {
			t.Errorf("malformed record id = %d, want 400", res.Status)
		}
	})

	t.Run("the stylesheet is served", func(t *testing.T) {
		res := b.Get(t, "/approve/assets/approve.css")
		if res.Status != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/css") {
			t.Errorf("stylesheet = %d %q, want 200 text/css", res.Status, res.Header.Get("Content-Type"))
		}
	})
}

// TestTheApproverApprovesWithOnePost pins A2: the approver's session approves with one POST, the
// requesting session then receives the value, and the page says the record is granted with no
// button left; another person's session, an expired session, a logged-out session's replayed
// cookie and no session each write nothing; a shared secret's request is approved by any signed-in
// member of the domain.
func TestTheApproverApprovesWithOnePost(t *testing.T) {
	r := newRig(t, true)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")

	t.Run("another person's session", func(t *testing.T) {
		approver := r.browser(t)
		approver.SignIn(t, p.recordID, person(ada))
		form := decisionForm(t, approver.Page(t, p.recordID).Body, p.recordID)
		other := r.browser(t)
		other.SignIn(t, p.recordID, person(carol))
		if res := other.FromPage(t, "/approve", decision(form, "approve")); res.Status != http.StatusForbidden {
			t.Errorf("carol posting ada's form = %d, want 403", res.Status)
		}
		r.requirePending(t, p.recordID)
	})

	t.Run("an expired session", func(t *testing.T) {
		b := r.browser(t)
		b.SignIn(t, p.recordID, person(ada))
		form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
		if _, err := r.st.Pool.Exec(t.Context(), `update browser_sessions set expires_at = now() - interval '1 second'`); err != nil {
			t.Fatal(err)
		}
		if res := b.FromPage(t, "/approve", decision(form, "approve")); res.Status != http.StatusForbidden {
			t.Errorf("POST with an expired session = %d, want 403", res.Status)
		}
		if res := b.Get(t, "/approve/"+p.recordID); res.Status != http.StatusFound {
			t.Errorf("the page with an expired session = %d, want 302 to sign in again", res.Status)
		}
		r.requirePending(t, p.recordID)
	})

	t.Run("a logged-out session's replayed cookie", func(t *testing.T) {
		b := r.browser(t)
		b.SignIn(t, p.recordID, person(ada))
		form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
		stolen := &http.Cookie{Name: "__Host-broker-session", Value: b.Cookie(t, "__Host-broker-session")}
		before := r.sessions(t)
		res := b.FromPage(t, "/signin/logout", url.Values{"next": {"/approve/" + p.recordID}})
		if res.Status != http.StatusOK || !strings.Contains(res.Body, `href="/approve/`+p.recordID+`"`) {
			t.Fatalf("logout = %d, want 200 linking back to the record:\n%s", res.Status, res.Body)
		}
		if c := res.Cookie("__Host-broker-session"); c == nil || c.MaxAge >= 0 {
			t.Errorf("logout's Set-Cookie = %v, want the session cookie cleared", c)
		}
		if after := r.sessions(t); after != before-1 {
			t.Errorf("sessions after logout = %d, want %d", after, before-1)
		}
		if res := r.browser(t).Post(t, "/approve", decision(form, "approve"), map[string]string{"Origin": r.url}, stolen); res.Status != http.StatusForbidden {
			t.Errorf("POST with the logged-out cookie = %d, want 403", res.Status)
		}
		r.requirePending(t, p.recordID)
	})

	t.Run("no session", func(t *testing.T) {
		approver := r.browser(t)
		approver.SignIn(t, p.recordID, person(ada))
		form := decisionForm(t, approver.Page(t, p.recordID).Body, p.recordID)
		if res := r.browser(t).FromPage(t, "/approve", decision(form, "approve")); res.Status != http.StatusForbidden {
			t.Errorf("POST with no session = %d, want 403", res.Status)
		}
		r.requirePending(t, p.recordID)
	})

	t.Run("the approver", func(t *testing.T) {
		b := r.browser(t)
		b.SignIn(t, p.recordID, person(ada))
		form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
		res := b.FromPage(t, "/approve", decision(form, "approve"))
		if res.Status != http.StatusSeeOther || res.Header.Get("Location") != "/approve/"+p.recordID+"?decided="+p.recordID {
			t.Fatalf("approve = %d to %q, want 303 to the record with its outcome:\n%s", res.Status, res.Header.Get("Location"), res.Body)
		}
		if state, grants := r.state(t, p.recordID), r.grants(t, p.recordID); state != "approved" || grants != 1 {
			t.Fatalf("record after the approval = %s with %d grants, want approved with one", state, grants)
		}
		status, body := r.session(t, key, enrollmentID, http.MethodGet, "/v1/requests/"+p.requestID, nil)
		grantID := decode[struct {
			GrantID *string `json:"grant_id"`
		}](t, body).GrantID
		if status != http.StatusOK || grantID == nil {
			t.Fatalf("GET /v1/requests/%s = %d %s, want its grant", p.requestID, status, body)
		}
		status, body = r.session(t, key, enrollmentID, http.MethodPost, "/v1/grants/"+*grantID+"/values", nil)
		if values := decode[struct {
			Values map[string]string `json:"values"`
		}](t, body).Values; status != http.StatusOK || values["TEST_SECRET"] != "test-v1" {
			t.Fatalf("values = %d %s, want TEST_SECRET=test-v1", status, body)
		}

		after := b.Get(t, res.Header.Get("Location"))
		got := pageText(t, after.Body)
		if !strings.Contains(got, "TEST_SECRET: granted (approved by "+ada+" at ") || !strings.Contains(got, "State approved by "+ada) {
			t.Errorf("the page after the approval does not say it is granted by ada:\n%s", got)
		}
		if decisionForm(t, after.Body, p.recordID) != nil {
			t.Errorf("the decided record still offers a decision form:\n%s", after.Body)
		}
		if res := b.FromPage(t, "/approve", decision(form, "approve")); res.Status != http.StatusSeeOther {
			t.Errorf("a replayed form = %d, want 303 deciding nothing", res.Status)
		}
		if grants, events := r.grants(t, p.recordID), r.terminalEvents(t, p.recordID); grants != 1 || events != 1 {
			t.Errorf("after a replay the record has %d grants and %d terminal events, want 1 and 1", grants, events)
		}
	})

	t.Run("a shared secret's request, by anyone signed in", func(t *testing.T) {
		shared := r.request(t, enrollmentID, key, "shared deploy", nil, "", "SHARED_SECRET")
		b := r.browser(t)
		auth := b.StartSignIn(t, "/approve/"+shared.recordID)
		if auth.Query().Has("login_hint") {
			t.Errorf("a shared secret's sign-in hints %q, want no login_hint", auth.Query().Get("login_hint"))
		}
		if res := b.Get(t, b.Authorize(t, auth, person(carol))); res.Status != http.StatusSeeOther {
			t.Fatalf("callback = %d, want 303", res.Status)
		}
		form := decisionForm(t, b.Page(t, shared.recordID).Body, shared.recordID)
		if form == nil {
			t.Fatal("carol is offered no form for a shared secret's request")
		}
		if res := b.FromPage(t, "/approve", decision(form, "approve")); res.Status != http.StatusSeeOther {
			t.Fatalf("carol's approval = %d, want 303", res.Status)
		}
		if state, grants := r.state(t, shared.recordID), r.grants(t, shared.recordID); state != "approved" || grants != 1 {
			t.Fatalf("shared record = %s with %d grants, want approved with one", state, grants)
		}
	})
}

// TestTheTokenBindsTheSessionAndThePrintedRecords pins A3: a page's token decides only the records
// that page printed, only for the session it was printed to, and only until it expires.
func TestTheTokenBindsTheSessionAndThePrintedRecords(t *testing.T) {
	r := newRig(t, true)
	enrollmentID, key := r.benSession(t, "ben-box")
	a := r.request(t, enrollmentID, key, "first", nil, "", "TEST_SECRET")
	b := r.browser(t)
	b.SignIn(t, a.recordID, person(ada))
	formA := decisionForm(t, b.Page(t, a.recordID).Body, a.recordID)

	t.Run("a record the page did not print", func(t *testing.T) {
		later := r.request(t, enrollmentID, key, "later", nil, "", "SECOND_SECRET")
		forged := decision(formA, "approve")
		forged.Set("record", later.recordID)
		if res := b.FromPage(t, "/approve", forged); res.Status != http.StatusForbidden {
			t.Errorf("A's form naming B = %d, want 403", res.Status)
		}
		forged.Add("printed", later.recordID)
		if res := b.FromPage(t, "/approve", forged); res.Status != http.StatusForbidden {
			t.Errorf("A's form with B added to its printed set = %d, want 403", res.Status)
		}
		r.requirePending(t, later.recordID)
		r.requirePending(t, a.recordID)
	})

	t.Run("another session's token", func(t *testing.T) {
		second := r.browser(t)
		second.SignIn(t, a.recordID, person(ada))
		if res := second.FromPage(t, "/approve", decision(formA, "approve")); res.Status != http.StatusForbidden {
			t.Errorf("one session's form posted with another session of the same person = %d, want 403", res.Status)
		}
		r.requirePending(t, a.recordID)
	})

	t.Run("an expired token", func(t *testing.T) {
		secret, err := base64.RawURLEncoding.DecodeString(b.Cookie(t, "__Host-broker-session"))
		if err != nil {
			t.Fatal(err)
		}
		at := func(expires time.Time) url.Values {
			form := decision(formA, "approve")
			form.Set("expires", strconv.FormatInt(expires.Unix(), 10))
			form.Set("token", approval.PageToken(secret, expires.Unix(), form["printed"]))
			return form
		}
		if res := b.FromPage(t, "/approve", at(time.Now().Add(-time.Second))); res.Status != http.StatusForbidden {
			t.Errorf("a token that expired a second ago = %d, want 403", res.Status)
		}
		r.requirePending(t, a.recordID)
		// The same token computed for an unexpired page is accepted, so the refusal above was its
		// expiry's.
		if res := b.FromPage(t, "/approve", at(time.Now().Add(time.Minute))); res.Status != http.StatusSeeOther {
			t.Fatalf("the token for an unexpired page = %d, want 303", res.Status)
		}
		if state := r.state(t, a.recordID); state != "approved" {
			t.Fatalf("record = %s, want approved", state)
		}
	})

	t.Run("approve all decides only what was printed", func(t *testing.T) {
		enrollmentID, key := r.benSession(t, "ben-box-2")
		one := r.request(t, enrollmentID, key, "one", nil, "", "TEST_SECRET")
		two := r.request(t, enrollmentID, key, "two", nil, "", "THIRD_SECRET")
		b := r.browser(t)
		b.SignIn(t, one.recordID, person(ada))
		page := b.Page(t, one.recordID)
		all, label := approveAll(t, page.Body)
		// SECOND_SECRET's request from the first session is still pending and ada's, so it is
		// printed too.
		if label != "Approve all 3 shown" {
			t.Fatalf("approve-all = %q, want \"Approve all 3 shown\":\n%s", label, pageText(t, page.Body))
		}
		after := r.request(t, enrollmentID, key, "after the page", nil, "", "SHARED_SECRET")
		res := b.FromPage(t, "/approve", all)
		if res.Status != http.StatusSeeOther {
			t.Fatalf("approve all = %d, want 303:\n%s", res.Status, res.Body)
		}
		for _, p := range []pending{one, two} {
			if state := r.state(t, p.recordID); state != "approved" {
				t.Errorf("printed record %s = %s, want approved", p.recordID, state)
			}
		}
		r.requirePending(t, after.recordID)
	})
}

// TestOnlyTheBrokersOriginDecides pins A4's server side: with a live session and a valid form, a
// POST from a sibling origin, Origin: null, Sec-Fetch-Site same-site or cross-site, or no origin
// information is refused and writes nothing; Sec-Fetch-Site same-origin with no Origin is
// accepted. A logout from another origin ends no session.
func TestOnlyTheBrokersOriginDecides(t *testing.T) {
	r := newRig(t, true)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	form := decision(decisionForm(t, b.Page(t, p.recordID).Body, p.recordID), "approve")
	broker, err := url.Parse(r.url)
	if err != nil {
		t.Fatal(err)
	}
	sibling := "https://dispatch." + broker.Hostname() + ".example"
	otherPort := "https://" + broker.Hostname() + ":1"

	for name, headers := range map[string]map[string]string{
		"a sibling origin":          {"Origin": sibling},
		"another port of the host":  {"Origin": otherPort},
		"Origin: null":              {"Origin": "null"},
		"Sec-Fetch-Site: same-site": {"Sec-Fetch-Site": "same-site"},
		"Sec-Fetch-Site: cross-site": {
			"Sec-Fetch-Site": "cross-site",
		},
		"no origin information":                     {},
		"a sibling origin claiming same-origin too": {"Origin": sibling, "Sec-Fetch-Site": "same-origin"},
	} {
		t.Run(name, func(t *testing.T) {
			if res := b.Post(t, "/approve", form, headers); res.Status != http.StatusForbidden {
				t.Errorf("POST /approve with %v = %d, want 403", headers, res.Status)
			}
			r.requirePending(t, p.recordID)
			sessions := r.sessions(t)
			if res := b.Post(t, "/signin/logout", url.Values{}, headers); res.Status != http.StatusForbidden {
				t.Errorf("POST /signin/logout with %v = %d, want 403", headers, res.Status)
			}
			if r.sessions(t) != sessions {
				t.Errorf("a refused logout ended a session")
			}
		})
	}

	if res := b.Post(t, "/approve", form, map[string]string{"Sec-Fetch-Site": "same-origin"}); res.Status != http.StatusSeeOther {
		t.Fatalf("POST with Sec-Fetch-Site same-origin and no Origin = %d, want 303", res.Status)
	}
	if state := r.state(t, p.recordID); state != "approved" {
		t.Fatalf("record = %s, want approved", state)
	}
}

// TestTheCallbackRefusesAndMakesNoSession pins A8: each way a callback can be wrong creates no
// session; a good one sets both cookies __Host-, Secure, HttpOnly, Path=/, SameSite=Lax, with no
// Domain.
func TestTheCallbackRefusesAndMakesNoSession(t *testing.T) {
	r := newRig(t, true)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
	page := "/approve/" + p.recordID

	// started is a browser with a sign-in started and completed at the provider as claims: its
	// sign-in cookie and the callback URL the provider returned.
	started := func(t *testing.T, claims map[string]any) (*brokertest.Browser, string) {
		b := r.browser(t)
		return b, b.Authorize(t, b.StartSignIn(t, page), claims)
	}
	refused := func(t *testing.T, res brokertest.Response, want int) {
		t.Helper()
		if res.Status != want {
			t.Errorf("callback = %d, want %d:\n%s", res.Status, want, res.Body)
		}
		if res.Cookie("__Host-broker-session") != nil {
			t.Errorf("a refused callback set a session cookie")
		}
		if n := r.sessions(t); n != 0 {
			t.Fatalf("a refused callback left %d sessions, want none", n)
		}
	}
	// withPending is callback sent with the sign-in cookie value given and no other.
	withPending := func(t *testing.T, callback, value string) brokertest.Response {
		return r.getWith(t, callback, &http.Cookie{Name: "__Host-broker-signin", Value: value})
	}

	t.Run("no sign-in cookie", func(t *testing.T) {
		_, callback := started(t, person(ada))
		refused(t, r.browser(t).Get(t, callback), http.StatusBadRequest)
	})
	t.Run("a state that is not the sign-in's", func(t *testing.T) {
		b, callback := started(t, person(ada))
		u, _ := url.Parse(callback)
		q := u.Query()
		q.Set("state", "not-the-state")
		u.RawQuery = q.Encode()
		refused(t, b.Get(t, u.String()), http.StatusBadRequest)
	})
	t.Run("a forged code", func(t *testing.T) {
		b, callback := started(t, person(ada))
		u, _ := url.Parse(callback)
		q := u.Query()
		q.Set("code", "forged-code")
		u.RawQuery = q.Encode()
		refused(t, b.Get(t, u.String()), http.StatusForbidden)
	})
	t.Run("a wrong PKCE verifier", func(t *testing.T) {
		b, callback := started(t, person(ada))
		fields := pendingCookie(t, b.Cookie(t, "__Host-broker-signin"))
		fields["verifier"] = strings.Repeat("v", 43)
		refused(t, withPending(t, callback, encodePending(t, fields)), http.StatusForbidden)
	})
	t.Run("a next outside /approve/", func(t *testing.T) {
		for _, next := range []string{"https://evil.example/approve/" + p.recordID, "//evil.example/", "/v1/pending", "/approve/" + p.recordID + "/../../v1"} {
			b, callback := started(t, person(ada))
			fields := pendingCookie(t, b.Cookie(t, "__Host-broker-signin"))
			fields["next"] = next
			res := withPending(t, callback, encodePending(t, fields))
			refused(t, res, http.StatusBadRequest)
			if res.Header.Get("Location") != "" {
				t.Errorf("next %q redirected to %q", next, res.Header.Get("Location"))
			}
		}
	})
	for name, spoil := range map[string]func(map[string]any){
		"a wrong nonce":    func(c map[string]any) { c["nonce"] = "another-nonce" },
		"a wrong audience": func(c map[string]any) { c["aud"] = "another-client" },
		"a wrong issuer":   func(c map[string]any) { c["iss"] = "https://issuer.example.com" },
	} {
		t.Run(name, func(t *testing.T) {
			r.issuer.SpoilIDTokens(spoil)
			t.Cleanup(func() { r.issuer.SpoilIDTokens(nil) })
			b, callback := started(t, person(ada))
			refused(t, b.Get(t, callback), http.StatusForbidden)
		})
	}
	t.Run("an account of another domain", func(t *testing.T) {
		claims := person(ada)
		claims["hd"] = "other.example"
		b, callback := started(t, claims)
		refused(t, b.Get(t, callback), http.StatusForbidden)
	})
	t.Run("an unverified email", func(t *testing.T) {
		claims := person(ada)
		claims["email_verified"] = false
		b, callback := started(t, claims)
		refused(t, b.Get(t, callback), http.StatusForbidden)
	})

	t.Run("a good sign-in, its cookies, and its replay", func(t *testing.T) {
		b := r.browser(t)
		start := b.Get(t, page)
		signInCookie := start.Cookie("__Host-broker-signin")
		callback := b.Authorize(t, start.Location(t), person(ada))
		res := b.Get(t, callback)
		if res.Status != http.StatusSeeOther || r.sessions(t) != 1 {
			t.Fatalf("callback = %d with %d sessions, want 303 and one", res.Status, r.sessions(t))
		}
		sessionCookie := res.Cookie("__Host-broker-session")
		if cleared := res.Cookie("__Host-broker-signin"); cleared == nil || cleared.MaxAge >= 0 {
			t.Errorf("the callback's Set-Cookie for the sign-in = %v, want it cleared", cleared)
		}
		for _, c := range []*http.Cookie{signInCookie, sessionCookie} {
			if c == nil {
				t.Fatal("a cookie was not set")
			}
			if !strings.HasPrefix(c.Name, "__Host-") || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteLaxMode {
				t.Errorf("cookie %s = %+v, want __Host-, Secure, HttpOnly, Path=/, SameSite=Lax and no Domain", c.Name, c)
			}
		}
		for _, raw := range append(start.Header.Values("Set-Cookie"), res.Header.Values("Set-Cookie")...) {
			if strings.Contains(strings.ToLower(raw), "domain=") {
				t.Errorf("Set-Cookie %q names a Domain", raw)
			}
		}
		secret, err := base64.RawURLEncoding.DecodeString(sessionCookie.Value)
		if err != nil || len(secret) != 32 {
			t.Fatalf("the session cookie holds %d bytes (%v), want a 32-byte secret", len(secret), err)
		}
		hash := sha256.Sum256(secret)
		if n := r.count(t, `select count(*) from browser_sessions where secret_hash = $1 and login = $2`, hash[:], ada); n != 1 {
			t.Errorf("Postgres holds %d sessions of ada under the SHA-256 of the cookie's secret, want 1", n)
		}

		// The browser's own replay has no sign-in cookie left; one replayed with the spent cookie
		// finds the code spent at the provider.
		if res := b.Get(t, callback); res.Status != http.StatusBadRequest || r.sessions(t) != 1 {
			t.Errorf("replayed callback = %d with %d sessions, want 400 and still one", res.Status, r.sessions(t))
		}
		if res := withPending(t, callback, signInCookie.Value); res.Status != http.StatusForbidden || r.sessions(t) != 1 {
			t.Errorf("replayed callback with its spent sign-in cookie = %d with %d sessions, want 403 and still one", res.Status, r.sessions(t))
		}
	})
}

// TestMarkupRendersInert pins A9: a reason and identifiers an agent wrote, carrying markup, render
// as text.
func TestMarkupRendersInert(t *testing.T) {
	r := newRig(t, true)
	reason := `<script>alert("reason")</script><b>bold</b> & more`
	runtimeID := `<img/src=x/onerror=alert(1)>`
	claim := `<i>claimed</i>`
	enrollmentID, key := r.enroll(t, "ben-box", runtimeID, `<em>enrolled</em>`)
	p := r.request(t, enrollmentID, key, reason, nil, claim, "TEST_SECRET")
	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	res := b.Page(t, p.recordID)
	for _, raw := range []string{"<script", "<b>bold", "<img", "<i>claimed", "<em>enrolled"} {
		if strings.Contains(res.Body, raw) {
			t.Errorf("the page carries %q unescaped:\n%s", raw, res.Body)
		}
	}
	got := pageText(t, res.Body)
	for _, want := range []string{reason, runtimeID, claim, `<em>enrolled</em>`} {
		if !strings.Contains(got, want) {
			t.Errorf("the page does not print %q as text:\n%s", want, got)
		}
	}
}

// TestABatchDecidesEachRecordOnItsOwn pins D6's batch: approve-all over one valid record, one that
// expired after the page rendered, and one whose requesting enrollment ended after it rendered
// grants exactly the valid one, leaves each record exactly one terminal event, and the page says
// what became of each from the records themselves.
func TestABatchDecidesEachRecordOnItsOwn(t *testing.T) {
	r := newRig(t, true)
	validEnrollment, validKey := r.benSession(t, "ben-valid")
	expiringEnrollment, expiringKey := r.benSession(t, "ben-expiring")
	endingEnrollment, endingKey := r.benSession(t, "ben-ending")
	valid := r.request(t, validEnrollment, validKey, "valid", nil, "", "TEST_SECRET")
	expiring := r.request(t, expiringEnrollment, expiringKey, "expiring", nil, "", "SECOND_SECRET")
	ending := r.request(t, endingEnrollment, endingKey, "ending", nil, "", "THIRD_SECRET")

	b := r.browser(t)
	b.SignIn(t, valid.recordID, person(ada))
	all, label := approveAll(t, b.Page(t, valid.recordID).Body)
	if label != "Approve all 3 shown" {
		t.Fatalf("approve-all = %q, want \"Approve all 3 shown\"", label)
	}
	// After the page rendered: one request passes its expiry and the sweeper expires it, and then
	// another's enrollment lapses before any sweep ends it.
	if _, err := r.st.Pool.Exec(t.Context(), `update requests set pending_expires_at = now() - interval '1 second' where id = $1`, expiring.requestID); err != nil {
		t.Fatal(err)
	}
	r.sweeper.Tick(t.Context())
	if _, err := r.st.Pool.Exec(t.Context(), `update enrollments set lease_expires_at = now() - interval '1 second' where id = $1`, endingEnrollment); err != nil {
		t.Fatal(err)
	}

	res := b.FromPage(t, "/approve", all)
	if res.Status != http.StatusSeeOther {
		t.Fatalf("approve all = %d, want 303:\n%s", res.Status, res.Body)
	}
	for _, c := range []struct {
		p      pending
		grants int
	}{{valid, 1}, {expiring, 0}, {ending, 0}} {
		if grants, events := r.grants(t, c.p.recordID), r.terminalEvents(t, c.p.recordID); grants != c.grants || events != 1 {
			t.Errorf("record %s has %d grants and %d terminal events, want %d and 1", c.p.recordID, grants, events, c.grants)
		}
	}
	got := pageText(t, b.Get(t, res.Header.Get("Location")).Body)
	for _, want := range []string{
		"TEST_SECRET: granted (approved by " + ada,
		"SECOND_SECRET: expired at ",
		"THIRD_SECRET: recorded as denied because the requesting session ended (approved by " + ada,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the outcomes do not say %q:\n%s", want, got)
		}
	}

	t.Run("an outcome is read from the record, never from the URL", func(t *testing.T) {
		got := pageText(t, b.Get(t, "/approve/"+valid.recordID+"?decided="+valid.recordID+",granted,"+strings.Repeat("f", 64)).Body)
		if strings.Count(got, "TEST_SECRET: granted") != 1 || strings.Contains(got, "SECOND_SECRET: ") {
			t.Errorf("forged decided ids changed the outcomes:\n%s", got)
		}
	})
}

// TestSignInOffAndAnUnreachableProvider pins D10: with sign-in off every route of the page answers
// 503 naming the four settings, the stylesheet still served; with sign-in on, a provider whose
// discovery fails is a 503 naming its issuer, and the next sign-in tries again.
func TestSignInOffAndAnUnreachableProvider(t *testing.T) {
	t.Run("sign-in off", func(t *testing.T) {
		r := newRig(t, false)
		enrollmentID, key := r.benSession(t, "ben-box")
		p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
		b := r.browser(t)
		for _, res := range []brokertest.Response{
			b.Get(t, "/approve/"+p.recordID),
			b.FromPage(t, "/approve", url.Values{"decision": {"approve"}, "record": {p.recordID}}),
			b.Get(t, "/signin/callback?code=x&state=y"),
			b.FromPage(t, "/signin/logout", url.Values{}),
		} {
			got := pageText(t, res.Body)
			if res.Status != http.StatusServiceUnavailable {
				t.Errorf("answer = %d, want 503", res.Status)
			}
			for _, setting := range []string{"BROKER_SIGNIN_ISSUER", "BROKER_SIGNIN_CLIENT_ID", "BROKER_SIGNIN_CLIENT_SECRET_FILE", "BROKER_SIGNIN_DOMAIN"} {
				if !strings.Contains(got, setting) {
					t.Errorf("the 503 does not name %s:\n%s", setting, got)
				}
			}
		}
		if res := b.Get(t, "/approve/assets/approve.css"); res.Status != http.StatusOK {
			t.Errorf("stylesheet with sign-in off = %d, want 200", res.Status)
		}
		r.requirePending(t, p.recordID)
	})

	t.Run("a provider whose discovery fails", func(t *testing.T) {
		r := newRig(t, true)
		enrollmentID, key := r.benSession(t, "ben-box")
		p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
		r.issuer.FailDiscovery(http.StatusInternalServerError)
		b := r.browser(t)
		res := b.Get(t, "/approve/"+p.recordID)
		if res.Status != http.StatusServiceUnavailable || !strings.Contains(pageText(t, res.Body), r.issuer.URL()) {
			t.Fatalf("sign-in with discovery failing = %d, want 503 naming %s:\n%s", res.Status, r.issuer.URL(), res.Body)
		}
		r.issuer.FailDiscovery(0)
		b.SignIn(t, p.recordID, person(ada))
	})
}

// TestSwitchingAccountsReplacesTheSession pins the switch link: it starts a sign-in with the
// provider's account chooser and the record's approver as login_hint, and the session it makes
// replaces the one the browser held, whose cookie then signs nothing in.
func TestSwitchingAccountsReplacesTheSession(t *testing.T) {
	r := newRig(t, true)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
	b := r.browser(t)
	b.SignIn(t, p.recordID, person(carol))
	old := &http.Cookie{Name: "__Host-broker-session", Value: b.Cookie(t, "__Host-broker-session")}

	auth := b.StartSignIn(t, "/approve/"+p.recordID+"?switch=1")
	if q := auth.Query(); q.Get("prompt") != "select_account" || q.Get("login_hint") != ada {
		t.Fatalf("the switch's sign-in asks prompt=%q login_hint=%q, want select_account and %s", q.Get("prompt"), q.Get("login_hint"), ada)
	}
	if res := b.Get(t, b.Authorize(t, auth, person(ada))); res.Status != http.StatusSeeOther || res.Header.Get("Location") != "/approve/"+p.recordID {
		t.Fatalf("callback = %d to %q, want 303 back to the record", res.Status, res.Header.Get("Location"))
	}
	if n := r.sessions(t); n != 1 {
		t.Fatalf("sessions after the switch = %d, want only the new one", n)
	}
	if got := pageText(t, b.Page(t, p.recordID).Body); !strings.Contains(got, "Signed in as "+ada) {
		t.Errorf("the page after the switch is not ada's:\n%s", got)
	}
	if res := r.getWith(t, "/approve/"+p.recordID, old); res.Status != http.StatusFound {
		t.Errorf("the page with carol's old cookie = %d, want 302 to sign in", res.Status)
	}
}
