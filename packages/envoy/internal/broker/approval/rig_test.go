package approval_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/approval"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// clientID and clientSecret are the broker's own client at the test's provider.
	clientID     = "broker-client"
	clientSecret = "broker-client-secret"
	// domain is the Workspace domain a person must belong to (BROKER_SIGNIN_DOMAIN).
	domain = "example.com"
	// ada owns every secret but the shared one, so she approves their requests.
	ada = "ada@example.com"
	// ben operates the machine every secret request comes from.
	ben = "ben@example.com"
	// carol is another person of the domain, who may decide only a shared secret's request.
	carol = "carol@example.com"
	// dispatchAccount and dispatchAudience are the listed caller of the account routes, which a
	// test uses only to approve a person's machine login, as Dispatch's server relays one.
	dispatchAccount  = "system:serviceaccount:example-ns:example-dispatch"
	dispatchAudience = "agent-secrets"
)

// rig is a live broker on a TLS test server, so the page's Secure cookies travel as they do in a
// browser, with every route of the real route table, its approval page signing people in at an
// oidctest provider with code flow, on a real Postgres schema of its own.
type rig struct {
	url     string
	srv     *httptest.Server
	st      *store.Store
	issuer  *oidctest.Issuer
	machine *requests.Machine
	sweeper *requests.Sweeper
	// accountToken is dispatchAccount's projected token, for the machine-login approval only.
	accountToken string
}

// newRig wires the broker as cmd/broker does. signIn false builds the page with sign-in off.
func newRig(t *testing.T, signIn bool) *rig {
	t.Helper()
	st := storetest.Open(t)
	local := secrets.NewLocal(
		policytest.Secret("TEST_SECRET", ada, policy.TierHuman, "test-v1"),
		policytest.Secret("SECOND_SECRET", ada, policy.TierHuman, "second-v1"),
		policytest.Secret("THIRD_SECRET", ada, policy.TierHuman, "third-v1"),
		policytest.Secret("SHARED_SECRET", policy.OwnerShared, policy.TierHuman, "shared-v1"),
	)
	current := policytest.Current(t, local)

	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "signing-key")
	issuer.EnableCodeFlow(key, clientID, clientSecret)
	// Google answers an online sign-in with no refresh token, and the broker keeps none.
	issuer.OmitRefreshTokens()
	accounts, err := oidc.New(context.Background(), issuer.URL(), dispatchAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}

	enr := &enroll.Service{Store: st, Lease: time.Hour}
	enr.Chain = enroll.NewChainVerifier(st, srv.URL, time.Minute)
	reqMachine := &requests.Machine{
		Store: st, Policy: current, Secrets: policytest.Store(local), PendingTTL: 12 * time.Hour,
		Audience: srv.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	reqMachine.Chain = requests.NewChainVerifier(st, srv.URL, time.Minute)
	provider := brokertest.NewOAuthProvider(t, st, issuer)
	mach := &machine.Service{
		Store: st, Enroll: enr, Policy: current,
		Audience: srv.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		OAuth: provider, Replay: enr.Replay,
	}
	var signInConfig *config.SignIn
	if signIn {
		signInConfig = &config.SignIn{Issuer: issuer.URL(), ClientID: clientID, ClientSecret: clientSecret, Domain: domain, SessionLifetime: time.Hour}
	}
	page, err := approval.New(approval.Config{
		SignIn: signInConfig, PublicURL: srv.URL, MachineLoginURL: brokertest.MachineLoginURL,
		Store: st, Machine: reqMachine,
	})
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}
	api.Register(mux, api.Deps{
		PublicURL:     srv.URL,
		APIAccounts:   map[string]string{dispatchAccount: "dispatch"},
		AccountTokens: enroll.K8sPodVerifier{Verifier: accounts},
		Enroll:        enr, Machine: reqMachine, MachineLogin: mach,
		OAuth: provider, DeviceLogin: mach.LoginDevice, TokenHooks: mach.TokenHooks(),
		Proof:  &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: current, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN,
		Approval: page,
	})

	claims := issuer.Claims(dispatchAccount, dispatchAudience)
	claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	return &rig{
		url: srv.URL, srv: srv, st: st, issuer: issuer, machine: reqMachine,
		sweeper:      &requests.Sweeper{Enrollments: enr, Machine: reqMachine, MachineLogins: mach, OAuth: oauthstore.New(st.Pool), Interval: time.Hour},
		accountToken: issuer.Mint(t, key, claims),
	}
}

// --- the broker's machine and session side, which makes the records a person decides ---

// call sends a JSON request to the broker with headers and answers its status and body.
func (r *rig) call(t *testing.T, method, path string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, r.url+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return res.StatusCode, out
}

func (r *rig) session(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, method, path string, body any) (int, []byte) {
	t.Helper()
	p, err := proof.Sign(key, enrollmentID, method, r.url+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	return r.call(t, method, path, map[string]string{"Proof": p}, body)
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return v
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// machineLogin asks for a device login of person's machine on host and answers the record it
// opened and its code: a launcher_credential record, decided on Dispatch's machine-login page.
func (r *rig) machineLogin(t *testing.T, key *ecdsa.PrivateKey, person, host string) (recordID, code string) {
	t.Helper()
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	request, err := record.Sign(key, r.url, []record.AuthorizationDetail{{Type: record.KindLauncherCredential, Identifier: host}}, "", person, nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	cfg := &oauth2.Config{
		ClientID: oauthwire.ClientID, Scopes: []string{oauthwire.Scope},
		Endpoint: oauth2.Endpoint{DeviceAuthURL: r.url + "/v1/oauth/device_authorization", TokenURL: r.url + "/v1/oauth/token", AuthStyle: oauth2.AuthStyleInParams},
	}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: helper.NewDPoPTransport(key, r.srv.Client().Transport)})
	da, err := cfg.DeviceAuth(ctx, oauth2.SetAuthURLParam("request", request), oauth2.SetAuthURLParam("dpop_jkt", jkt))
	if err != nil {
		t.Fatalf("device authorization for %s: %v", person, err)
	}
	status, body := r.call(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]string{"Authorization": "Bearer " + r.accountToken}, map[string]any{"code": da.UserCode})
	if status != http.StatusOK {
		t.Fatalf("lookup = %d: %s", status, body)
	}
	return decode[struct {
		RecordID string `json:"record_id"`
	}](t, body).RecordID, da.UserCode
}

// enroll logs a machine of ben's on host in, approved by ben as Dispatch relays a machine login,
// and enrolls a session under it stating sessionID (none when empty) with runtimeID.
func (r *rig) enroll(t *testing.T, host, runtimeID, sessionID string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	machineKey := newKey(t)
	recordID, code := r.machineLogin(t, machineKey, ben, host)
	status, body := r.call(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
		map[string]string{"Authorization": "Bearer " + r.accountToken}, map[string]any{"approver": ben, "code": code})
	if status != http.StatusOK {
		t.Fatalf("approve ben's machine login = %d: %s", status, body)
	}
	credentialID := *decode[struct {
		CredentialID *string `json:"credential_id"`
	}](t, body).CredentialID
	sessionKey := newKey(t)
	thumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	enrollment := map[string]any{"kind": "session", "runtime_id": runtimeID, "thumbprint": thumbprint}
	if sessionID != "" {
		enrollment["session_id"] = sessionID
	}
	p, err := proof.SignLauncher(machineKey, credentialID, http.MethodPost, r.url+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatalf("proof.SignLauncher: %v", err)
	}
	status, body = r.call(t, http.MethodPost, "/v1/enrollments", map[string]string{"Proof": p}, enrollment)
	if status != http.StatusCreated {
		t.Fatalf("enroll ben's session = %d: %s", status, body)
	}
	return decode[struct {
		EnrollmentID string `json:"enrollment_id"`
	}](t, body).EnrollmentID, sessionKey
}

// pending is one pending agent_secret request and its record.
type pending struct{ requestID, recordID string }

// request has the session ask for names with reason, lifetime (nil until the session ends) and
// the request's own session claim (none when empty), and answers the pending request it opened.
func (r *rig) request(t *testing.T, enrollmentID string, key *ecdsa.PrivateKey, reason string, lifetime *int, sessionClaim string, names ...string) pending {
	t.Helper()
	details := make([]record.AuthorizationDetail, len(names))
	for i, n := range names {
		details[i] = record.AuthorizationDetail{Type: record.KindAgentSecret, Identifier: n, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(key, r.url, details, reason, "", lifetime, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	body := map[string]any{"request": compact, "session_id": nil}
	if sessionClaim != "" {
		body["session_id"] = sessionClaim
	}
	status, out := r.session(t, key, enrollmentID, http.MethodPost, "/v1/requests", body)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests %v = %d: %s", names, status, out)
	}
	created := decode[struct {
		RequestID string  `json:"request_id"`
		State     string  `json:"state"`
		RecordID  *string `json:"record_id"`
	}](t, out)
	if created.State != "pending" || created.RecordID == nil {
		t.Fatalf("POST /v1/requests %v = %s, want a pending request with a record", names, out)
	}
	return pending{requestID: created.RequestID, recordID: *created.RecordID}
}

// benSession is a session of ben's machine on host, stating no session id.
func (r *rig) benSession(t *testing.T, host string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	return r.enroll(t, host, host+"-session", "")
}

// --- reading the broker's own state ---

// state is the record's state as the broker reads it.
func (r *rig) state(t *testing.T, recordID string) string {
	t.Helper()
	detail, err := r.machine.ReadRecord(context.Background(), recordID)
	if err != nil {
		t.Fatalf("ReadRecord(%s): %v", recordID, err)
	}
	return detail.State
}

// count answers a count(*) query.
func (r *rig) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := r.st.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (r *rig) sessions(t *testing.T) int {
	t.Helper()
	return r.count(t, `select count(*) from browser_sessions`)
}

func (r *rig) grants(t *testing.T, recordID string) int {
	t.Helper()
	return r.count(t, `select count(*) from grants g join requests q on q.id = g.request_id where q.record_id = $1`, recordID)
}

func (r *rig) terminalEvents(t *testing.T, recordID string) int {
	t.Helper()
	return r.count(t, `select count(*) from credential_request_events where record_id = $1 and event in ('approved','denied','expired','cancelled')`, recordID)
}

// requirePending fails t unless the record is still pending with no grant.
func (r *rig) requirePending(t *testing.T, recordID string) {
	t.Helper()
	if state, grants := r.state(t, recordID), r.grants(t, recordID); state != "pending" || grants != 0 {
		t.Fatalf("record %s is %s with %d grants, want still pending with none", recordID, state, grants)
	}
}

// --- the browser ---

// response is one answer as a browser received it.
type response struct {
	status int
	header http.Header
	body   string
}

func (res response) location(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(res.header.Get("Location"))
	if err != nil || res.header.Get("Location") == "" {
		t.Fatalf("answer %d has no Location: %v\n%s", res.status, err, res.body)
	}
	return u
}

// cookie is the Set-Cookie of the answer naming name, or nil.
func (res response) cookie(name string) *http.Cookie {
	for _, c := range (&http.Response{Header: res.header}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// browser is a person's browser: a cookie jar, following no redirect by itself so a test sees
// each hop.
type browser struct {
	rig    *rig
	jar    *cookiejar.Jar
	client *http.Client
}

func (r *rig) browser(t *testing.T) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &browser{rig: r, jar: jar, client: r.client(jar)}
}

// client is an HTTP client trusting the test server, following no redirect, with jar (none when
// nil).
func (r *rig) client(jar http.CookieJar) *http.Client {
	return &http.Client{
		Transport:     r.srv.Client().Transport,
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// pageHeaders are the headers every answer of the approval page must carry.
var pageHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
	"X-Frame-Options":         "DENY",
	"Referrer-Policy":         "same-origin",
	"Cache-Control":           "no-store",
	"X-Content-Type-Options":  "nosniff",
}

// send sends req with client, and checks every answer of the broker carries the page's headers and
// no CORS header.
func (r *rig) send(t *testing.T, client *http.Client, req *http.Request) response {
	t.Helper()
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", req.Method, req.URL, err)
	}
	if strings.HasPrefix(req.URL.String(), r.url) {
		for name, want := range pageHeaders {
			if got := res.Header.Get(name); got != want {
				t.Errorf("%s %s answered %d with %s %q, want %q", req.Method, req.URL.Path, res.StatusCode, name, got, want)
			}
		}
		for name := range res.Header {
			if strings.HasPrefix(name, "Access-Control-") {
				t.Errorf("%s %s answered the CORS header %s", req.Method, req.URL.Path, name)
			}
		}
	}
	return response{status: res.StatusCode, header: res.Header, body: string(body)}
}

// target is path on the broker, or an absolute URL as given.
func (r *rig) target(path string) string {
	if strings.HasPrefix(path, "http") {
		return path
	}
	return r.url + path
}

func (b *browser) get(t *testing.T, path string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, b.rig.target(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	return b.rig.send(t, b.client, req)
}

// post posts form to path with headers; fromPage adds the Origin the broker's own page sends.
func (b *browser) post(t *testing.T, path string, form url.Values, headers map[string]string) response {
	t.Helper()
	return b.rig.post(t, b.client, path, form, headers)
}

func (b *browser) fromPage(t *testing.T, path string, form url.Values) response {
	t.Helper()
	return b.post(t, path, form, map[string]string{"Origin": b.rig.url})
}

func (r *rig) post(t *testing.T, client *http.Client, path string, form url.Values, headers map[string]string, cookies ...*http.Cookie) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.target(path), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return r.send(t, client, req)
}

// cookie is the value of the jar's cookie name for the broker, or "".
func (b *browser) cookie(t *testing.T, name string) string {
	t.Helper()
	u, err := url.Parse(b.rig.url)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range b.jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// person is the claims of a domain member with a verified email, as the provider signs them in.
func person(email string) map[string]any {
	return map[string]any{"sub": "sub-" + email, "email": email, "email_verified": true, "hd": domain}
}

// startSignIn opens path with no live session and answers the provider URL the broker sends the
// browser to.
func (b *browser) startSignIn(t *testing.T, path string) *url.URL {
	t.Helper()
	res := b.get(t, path)
	if res.status != http.StatusFound {
		t.Fatalf("GET %s = %d, want 302 to the provider:\n%s", path, res.status, res.body)
	}
	return res.location(t)
}

// authorize signs in at the provider as claims and answers the callback URL it returns to.
func (b *browser) authorize(t *testing.T, authURL *url.URL, claims map[string]any) string {
	t.Helper()
	b.rig.issuer.SignInAs(claims)
	res := b.get(t, authURL.String())
	if res.status != http.StatusFound {
		t.Fatalf("the provider answered %d, want 302 to the callback: %s", res.status, res.body)
	}
	return res.header.Get("Location")
}

// signIn signs the browser in as claims through recordID's page, the way a person arrives.
func (b *browser) signIn(t *testing.T, recordID string, claims map[string]any) {
	t.Helper()
	res := b.get(t, b.authorize(t, b.startSignIn(t, "/approve/"+recordID), claims))
	if res.status != http.StatusSeeOther || res.header.Get("Location") != "/approve/"+recordID {
		t.Fatalf("callback = %d to %q, want 303 to /approve/%s:\n%s", res.status, res.header.Get("Location"), recordID, res.body)
	}
}

// page opens recordID's page with a live session.
func (b *browser) page(t *testing.T, recordID string) response {
	t.Helper()
	res := b.get(t, "/approve/"+recordID)
	if res.status != http.StatusOK {
		t.Fatalf("GET /approve/%s = %d, want the page:\n%s", recordID, res.status, res.body)
	}
	return res
}

// --- reading the page ---

// htmlForm is one form of a page: its action, its hidden fields and its buttons.
type htmlForm struct {
	action  string
	fields  url.Values
	buttons []htmlButton
}

type htmlButton struct{ name, value, text string }

func parse(t *testing.T, body string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse the page: %v", err)
	}
	return doc
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// text is every text node under n, with runs of whitespace as one space. The templates put a
// newline between block elements, so their texts stay apart.
func text(n *html.Node) string {
	var sb strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			sb.WriteString(d.Data)
		}
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

func pageText(t *testing.T, body string) string {
	t.Helper()
	return text(parse(t, body))
}

func forms(t *testing.T, body string) []htmlForm {
	t.Helper()
	var out []htmlForm
	for n := range parse(t, body).Descendants() {
		if n.Type != html.ElementNode || n.Data != "form" {
			continue
		}
		f := htmlForm{action: attr(n, "action"), fields: url.Values{}}
		for d := range n.Descendants() {
			switch {
			case d.Type == html.ElementNode && d.Data == "input" && attr(d, "type") == "hidden":
				f.fields.Add(attr(d, "name"), attr(d, "value"))
			case d.Type == html.ElementNode && d.Data == "button":
				f.buttons = append(f.buttons, htmlButton{name: attr(d, "name"), value: attr(d, "value"), text: text(d)})
			}
		}
		out = append(out, f)
	}
	return out
}

// decisionForm is the hidden fields of the page's Approve/Deny form for recordID, nil when the
// page offers none.
func decisionForm(t *testing.T, body, recordID string) url.Values {
	t.Helper()
	for _, f := range forms(t, body) {
		if f.action == "/approve" && len(f.fields["record"]) == 1 && f.fields.Get("record") == recordID {
			return f.fields
		}
	}
	return nil
}

// decision is form with decision set, a copy.
func decision(form url.Values, value string) url.Values {
	out := url.Values{}
	for k, v := range form {
		out[k] = append([]string(nil), v...)
	}
	out.Set("decision", value)
	return out
}

// approveAll is the page's "Approve all N shown" form and its button's label, nil when it offers
// none.
func approveAll(t *testing.T, body string) (url.Values, string) {
	t.Helper()
	for _, f := range forms(t, body) {
		for _, b := range f.buttons {
			if strings.HasPrefix(b.text, "Approve all") {
				return f.fields, b.text
			}
		}
	}
	return nil, ""
}

// pendingCookie decodes a sign-in in progress from the browser's cookie, for a test that forges
// one of its fields.
func pendingCookie(t *testing.T, value string) map[string]string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("decode the sign-in cookie: %v", err)
	}
	var fields map[string]string
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode the sign-in cookie: %v", err)
	}
	return fields
}

func encodePending(t *testing.T, fields map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// getWith GETs target with exactly the cookies given and no jar.
func (r *rig) getWith(t *testing.T, target string, cookies ...*http.Cookie) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.target(target), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return r.send(t, r.client(nil), req)
}
