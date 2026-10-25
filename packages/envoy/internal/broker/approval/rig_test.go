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
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
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
	// clientID is the broker's own client at the test's provider, and domain the Workspace domain
	// a person must belong to (BROKER_SIGNIN_DOMAIN).
	clientID = brokertest.SignInClientID
	domain   = brokertest.SignInDomain
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
	signInConfig := brokertest.SignIn(issuer, key)
	if !signIn {
		signInConfig = nil
	}
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
	api.Register(mux, api.Deps{
		PublicURL:     srv.URL,
		APIAccounts:   map[string]string{dispatchAccount: "dispatch"},
		AccountTokens: enroll.K8sPodVerifier{Verifier: accounts},
		Enroll:        enr, Machine: reqMachine, MachineLogin: mach,
		OAuth: provider, DeviceLogin: mach.LoginDevice, TokenHooks: mach.TokenHooks(),
		Proof:  &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: current, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN,
		Approval: brokertest.NewApprovalPage(t, signInConfig, srv.URL, st, reqMachine),
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

// --- the browser: brokertest.Browser, every answer of the broker held to the page's headers ---

// browser is a person's browser on the rig's broker, trusting its TLS test server.
func (r *rig) browser(t *testing.T) *brokertest.Browser {
	t.Helper()
	b := brokertest.NewBrowser(t, r.url, r.srv.Client().Transport, r.issuer)
	b.Check = checkPageHeaders
	return b
}

// pageHeaders are the headers every answer of the approval page must carry.
var pageHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
	"X-Frame-Options":         "DENY",
	"Referrer-Policy":         "same-origin",
	"Cache-Control":           "no-store",
	"X-Content-Type-Options":  "nosniff",
}

// checkPageHeaders checks an answer of the broker carries the page's headers and no CORS header.
func checkPageHeaders(t *testing.T, req *http.Request, res brokertest.Response) {
	t.Helper()
	for name, want := range pageHeaders {
		if got := res.Header.Get(name); got != want {
			t.Errorf("%s %s answered %d with %s %q, want %q", req.Method, req.URL.Path, res.Status, name, got, want)
		}
	}
	for name := range res.Header {
		if strings.HasPrefix(name, "Access-Control-") {
			t.Errorf("%s %s answered the CORS header %s", req.Method, req.URL.Path, name)
		}
	}
}

// --- reading the page ---

var (
	person       = brokertest.Person
	pageText     = brokertest.PageText
	decisionForm = brokertest.DecisionForm
	decision     = brokertest.Decision
)

// approveAll is the page's "Approve all N shown" form and its button's label, nil when it offers
// none.
func approveAll(t *testing.T, body string) (url.Values, string) {
	t.Helper()
	for _, f := range brokertest.Forms(t, body) {
		for _, b := range f.Buttons {
			if strings.HasPrefix(b.Text, "Approve all") {
				return f.Fields, b.Text
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

// getWith GETs target with exactly the cookies given, from a browser holding none.
func (r *rig) getWith(t *testing.T, target string, cookies ...*http.Cookie) brokertest.Response {
	t.Helper()
	return r.browser(t).Get(t, target, cookies...)
}
