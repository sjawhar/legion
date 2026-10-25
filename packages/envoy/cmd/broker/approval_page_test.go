package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The approval page's two cookies (internal/broker/approval): the session a sign-in makes, and
// the sign-in in progress between the redirect to the provider and its callback.
const (
	sessionCookieName = "__Host-broker-session"
	signInCookieName  = "__Host-broker-signin"
)

// approvalFixture is everything a broker started for the approval page shares: one local issuer
// standing in for both the cluster's ServiceAccount token issuer (BROKER_K8S_OIDC_ISSUER) and the
// person's sign-in provider (BROKER_SIGNIN_ISSUER, its code flow armed for the broker's own
// client), the fake secrets file holding ada's human-tier secret, and the accounts the binary
// lists: the service whose token login enrolls the pods, and Dispatch's server.
type approvalFixture struct {
	binary     string
	issuer     *oidctest.Issuer
	podKey     *oidctest.Key
	signInKey  *oidctest.Key
	secrets    string
	clientFile string
}

const (
	approvalPodAudience     = "example-broker-pod"
	approvalLauncherService = "example-launcher"
	approvalLauncherAccount = "system:serviceaccount:example-system:example-launcher"
	approvalPodAccount      = "system:serviceaccount:example:example-worker"
	approvalDispatchAccount = "system:serviceaccount:example-ns:example-dispatch"
	approvalSecretName      = "ADA_DEPLOY_KEY"
	approvalSecretValue     = "ada-deploy-value"
	ada                     = "ada@example.com"
	carol                   = "carol@example.com"
)

func newApprovalFixture(t *testing.T) approvalFixture {
	t.Helper()
	issuer := oidctest.New(t)
	f := approvalFixture{
		binary: buildBroker(t), issuer: issuer,
		podKey:    issuer.PublishKey(t, "pod-key"),
		signInKey: issuer.PublishKey(t, "signin-key"),
		secrets:   filepath.Join(t.TempDir(), "fake-secrets.json"),
	}
	brokertest.SignIn(issuer, f.signInKey)
	content := `{"secrets": [
		{"name": "` + testSecretsPrefix + `ada-deploy-key", "kms_key_id": "` + testSecretsKMSKeyARN + `", "tags": {"owner": "` + ada + `", "tier": "human"}, "value": "` + approvalSecretValue + `"}
	]}`
	if err := os.WriteFile(f.secrets, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.clientFile = filepath.Join(t.TempDir(), "signin-client-secret")
	if err := os.WriteFile(f.clientFile, []byte(brokertest.SignInClientSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// baseEnv is a broker on databaseURL serving at publicURL (http://127.0.0.1:0 for its own bound
// address), with no sign-in configured.
func (f approvalFixture) baseEnv(databaseURL, publicURL string) []string {
	return []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=" + publicURL,
		"BROKER_FAKE_SECRETS_FILE=" + f.secrets,
		"BROKER_K8S_OIDC_ISSUER=" + f.issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + approvalPodAudience,
		"BROKER_SERVICE_LOGINS=" + approvalLauncherService + "=" + approvalLauncherAccount,
		"BROKER_API_ACCOUNTS=dispatch=" + approvalDispatchAccount,
		// A stopped broker exits at once rather than draining requests no row leaves in flight.
		"BROKER_DRAIN_SECONDS=0",
	}
}

// signInEnv is the four settings that turn the approval page's sign-in on, at the fixture's issuer.
func (f approvalFixture) signInEnv() []string {
	return []string{
		"BROKER_SIGNIN_ISSUER=" + f.issuer.URL(),
		"BROKER_SIGNIN_CLIENT_ID=" + brokertest.SignInClientID,
		"BROKER_SIGNIN_CLIENT_SECRET_FILE=" + f.clientFile,
		"BROKER_SIGNIN_DOMAIN=" + brokertest.SignInDomain,
	}
}

// start starts the binary with env on top of the base configuration on databaseURL.
func (f approvalFixture) start(t *testing.T, databaseURL, publicURL string, env ...string) *brokerProcess {
	t.Helper()
	return startBroker(t, brokerOptions{Binary: f.binary, Env: append(f.baseEnv(databaseURL, publicURL), env...)})
}

// pods is a brokertest.Pods on base, the one way every broker here is asked for a secret.
func (f approvalFixture) pods(t *testing.T, base string) brokertest.Pods {
	return brokertest.Pods{T: t, Base: base, Token: brokertest.IssuerTokens(t, f.issuer, f.podKey, approvalPodAudience)}
}

// dispatchToken is Dispatch's server's own projected ServiceAccount token, as it calls the broker.
func (f approvalFixture) dispatchToken(t *testing.T) string {
	claims := f.issuer.Claims(approvalDispatchAccount, approvalPodAudience)
	claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	return f.issuer.Mint(t, f.podKey, claims)
}

// requirePending fails pods' test unless request id, which pod p made, is still pending on its
// record.
func requirePending(pods brokertest.Pods, p brokertest.Pod, id, after string) {
	pods.T.Helper()
	if reply := pods.State(p, id); reply.State != "pending" || reply.GrantID != nil {
		pods.T.Fatalf("after %s the request is %+v, want still pending with no grant", after, reply)
	}
}

// accountCall sends body as JSON to path at base with headers, logs the exchange with no header
// shown, since each carries a credential, and answers the status and the body.
func accountCall(t *testing.T, base, method, path string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest(method, base+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("> %s %s %s\n< %d %s", method, path, payload, response.StatusCode, bytes.TrimSpace(answer))
	return response.StatusCode, answer
}

// errorBody is a refusal's JSON body.
type errorBody struct {
	Code       string `json:"code"`
	Error      string `json:"error"`
	ApproveURL string `json:"approve_url"`
}

func decodeError(t *testing.T, answer []byte) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(answer, &body); err != nil {
		t.Fatalf("decode %s: %v", answer, err)
	}
	return body
}

// forgedPageToken is the token a page would carry for printed, minted from a session's own cookie
// as plan D6 defines it (HMAC-SHA256 under the raw session secret over "approve-page", the expiry
// and the sorted record ids): what a person holding their own session cookie can compute for any
// record, whether or not any page of theirs printed it.
func forgedPageToken(t *testing.T, cookie string, expires int64, printed ...string) string {
	t.Helper()
	secret, err := base64.RawURLEncoding.DecodeString(cookie)
	if err != nil {
		t.Fatalf("the session cookie is not base64url: %v", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("approve-page\n" + strconv.FormatInt(expires, 10) + "\n" + strings.Join(slices.Sorted(slices.Values(printed)), "\n")))
	return hex.EncodeToString(mac.Sum(nil))
}

// requireSignInRedirect fails t unless res sends the browser to the issuer's authorization
// endpoint to sign in again.
func requireSignInRedirect(t *testing.T, issuer *oidctest.Issuer, res brokertest.Response, what string) {
	t.Helper()
	if res.Status != http.StatusFound || !strings.HasPrefix(res.Header.Get("Location"), issuer.URL()+"/authorize?") {
		t.Fatalf("%s = %d to %q, want 302 to the provider's sign-in:\n%s", what, res.Status, res.Header.Get("Location"), res.Body)
	}
}

// front is one public URL in front of whichever broker process a row routes it to, as a load
// balancer stands in front of replicas. Every process behind it is configured with its URL as
// BROKER_PUBLIC_URL, so each verifies records and proofs against the same audience, checks the
// same origin, and names the same redirect URI in a sign-in: a record one process wrote is one
// every other can read, which processes on public URLs of their own could not.
type front struct {
	*httptest.Server
	backend atomic.Pointer[url.URL]
}

func newFront(t *testing.T) *front {
	t.Helper()
	f := &front{}
	f.Server = httptest.NewServer(&httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(f.backend.Load())
		r.Out.Host = r.In.Host
	}})
	t.Cleanup(f.Close)
	return f
}

// route sends every request from now on to p.
func (f *front) route(t *testing.T, p *brokerProcess) {
	t.Helper()
	target, err := url.Parse("http://" + p.Addr)
	if err != nil {
		t.Fatal(err)
	}
	f.backend.Store(target)
}

// TestTheBinaryApprovesASecretOnItsOwnPage drives the REAL compiled broker on a real Postgres, over
// HTTP alone, with one local issuer standing in for the cluster's ServiceAccount token issuer and
// for the person's sign-in provider. A pod enrolled through a service's token login asks for ada's
// human-tier secret and is answered pending with the record's page; a browser opening that page is
// sent to the provider's sign-in with the broker's own client, PKCE, a nonce, a state and ada as
// the login hint, comes back to the page, and ada's one POST of its Approve grants the request,
// whose value the pod then reads. Against a second pending record, nothing Dispatch or another
// person holds decides it: carol's session, even with a page token minted from her own cookie;
// Dispatch's ServiceAccount token on the account routes; an ID token for ada minted for Dispatch's
// own OAuth client; a callback carrying a code the broker never asked for; a session past its
// expiry; and a logged-out session's cookie. The binary refuses a partial or over-long sign-in
// configuration, answers 503 with sign-in off, and finishes a sign-in begun on one process on
// another, whose session decides on a third start of the binary.
func TestTheBinaryApprovesASecretOnItsOwnPage(t *testing.T) {
	f := newApprovalFixture(t)
	databaseURL := storetest.URL(t)
	public := newFront(t)
	broker := f.start(t, databaseURL, public.URL, f.signInEnv()...)
	public.route(t, broker)
	base := public.URL
	if !strings.Contains(broker.Boot, "approval sign-in on") {
		t.Fatalf("the broker's boot log does not say sign-in is on:\n%s", broker.Boot)
	}

	var launch brokertest.Launcher
	var podA, podB brokertest.Pod
	var requestA, requestB, recordA, recordB string
	newBrowser := func(t *testing.T) *brokertest.Browser {
		return brokertest.NewBrowser(t, base, http.DefaultTransport, f.issuer)
	}

	t.Run("a pod's request for ada's secret waits on its record's page", func(t *testing.T) {
		pods := f.pods(t, base)
		launch = pods.LogIn(approvalLauncherService, approvalLauncherAccount)
		podA = pods.Enroll(launch, "pod-a", approvalPodAccount)
		reply := pods.Ask(podA, approvalSecretName)
		if reply.State != "pending" || reply.RecordID == nil || string(reply.Approver) != `"`+ada+`"` {
			t.Fatalf("pod-a's request = %+v (approver %s), want pending on a record ada approves", reply, reply.Approver)
		}
		requestA, recordA = reply.RequestID, *reply.RecordID
		if want := base + "/approve/" + recordA; reply.ApproveURL != want {
			t.Fatalf("pod-a's request approve_url = %q, want %q", reply.ApproveURL, want)
		}
		status, answer := accountCall(t, base, http.MethodGet, "/v1/pending?approver="+url.QueryEscape(ada),
			map[string]string{"Authorization": "Bearer " + f.dispatchToken(t)}, nil)
		if status != http.StatusOK || !bytes.Contains(answer, []byte(`"approve_url":"`+base+"/approve/"+recordA+`"`)) {
			t.Fatalf("Dispatch's GET /v1/pending for ada = %d %s, want the record with its approve_url", status, answer)
		}
	})

	t.Run("A1: the page sends the browser to sign in with the broker's own client and back to the record", func(t *testing.T) {
		b := newBrowser(t)
		res := b.Get(t, "/approve/"+recordA)
		if res.Status != http.StatusFound {
			t.Fatalf("GET /approve/%s with no session = %d, want 302 to the provider:\n%s", recordA, res.Status, res.Body)
		}
		if c := res.Cookie(signInCookieName); c == nil || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" {
			t.Fatalf("the sign-in in progress cookie = %+v, want a Secure HttpOnly host-only cookie at /", c)
		}
		authURL := res.Location(t)
		q := authURL.Query()
		t.Logf("the page sends the browser to %s://%s%s with client_id=%s redirect_uri=%s response_type=%s code_challenge_method=%s login_hint=%s scope=%q (state, nonce and code_challenge present: %t %t %t)",
			authURL.Scheme, authURL.Host, authURL.Path, q.Get("client_id"), q.Get("redirect_uri"), q.Get("response_type"),
			q.Get("code_challenge_method"), q.Get("login_hint"), q.Get("scope"), q.Get("state") != "", q.Get("nonce") != "", q.Get("code_challenge") != "")
		switch {
		case authURL.Scheme+"://"+authURL.Host+authURL.Path != f.issuer.URL()+"/authorize":
			t.Fatalf("the sign-in goes to %s, want the provider's authorization endpoint %s/authorize", authURL, f.issuer.URL())
		case q.Get("client_id") != brokertest.SignInClientID:
			t.Fatalf("client_id = %q, want the broker's own client %q", q.Get("client_id"), brokertest.SignInClientID)
		case q.Get("redirect_uri") != base+"/signin/callback":
			t.Fatalf("redirect_uri = %q, want the broker's own %s/signin/callback", q.Get("redirect_uri"), base)
		case q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
			t.Fatalf("response_type %q, PKCE %q %q; want code with an S256 challenge", q.Get("response_type"), q.Get("code_challenge_method"), q.Get("code_challenge"))
		case q.Get("nonce") == "" || q.Get("state") == "":
			t.Fatal("the sign-in carries no nonce or no state")
		case q.Get("login_hint") != ada:
			t.Fatalf("login_hint = %q, want the record's approver %s", q.Get("login_hint"), ada)
		case q.Has("access_type") || q.Get("prompt") == "consent":
			t.Fatalf("the online sign-in asks for offline access: access_type=%q prompt=%q", q.Get("access_type"), q.Get("prompt"))
		}

		callback := b.Authorize(t, authURL, brokertest.Person(ada))
		if !strings.HasPrefix(callback, base+"/signin/callback?") {
			t.Fatalf("the provider returns the browser to %q, want the broker's own callback", callback)
		}
		res = b.Get(t, callback)
		if res.Status != http.StatusSeeOther || res.Header.Get("Location") != "/approve/"+recordA {
			t.Fatalf("the callback = %d to %q, want 303 back to /approve/%s:\n%s", res.Status, res.Header.Get("Location"), recordA, res.Body)
		}
		if res.Cookie(sessionCookieName) == nil {
			t.Fatal("the callback set no session cookie")
		}
		page := b.Page(t, recordA)
		text := brokertest.PageText(t, page.Body)
		t.Logf("ada's page for %s reads: %s", recordA, text)
		for _, want := range []string{approvalSecretName, "a pod enrollment", "runtime pod-a", "pod-a needs " + approvalSecretName, "Signed in as " + ada} {
			if !strings.Contains(text, want) {
				t.Fatalf("ada's page does not print %q:\n%s", want, text)
			}
		}
		var buttons []string
		for _, form := range brokertest.Forms(t, page.Body) {
			if form.Action == "/approve" && form.Fields.Get("record") == recordA {
				for _, button := range form.Buttons {
					buttons = append(buttons, button.Text+"="+button.Value)
				}
			}
		}
		if !slices.Equal(buttons, []string{"Approve=approve", "Deny=deny"}) {
			t.Fatalf("the page's form for %s offers %v, want Approve and Deny", recordA, buttons)
		}

		t.Run("A2: ada's one POST of Approve grants it and the pod reads its value", func(t *testing.T) {
			form := brokertest.DecisionForm(t, page.Body, recordA)
			res := b.FromPage(t, "/approve", brokertest.Decision(form, "approve"))
			if want := "/approve/" + recordA + "?decided=" + recordA; res.Status != http.StatusSeeOther || res.Header.Get("Location") != want {
				t.Fatalf("ada's Approve = %d to %q, want 303 to %s:\n%s", res.Status, res.Header.Get("Location"), want, res.Body)
			}
			outcome := brokertest.PageText(t, b.Get(t, res.Header.Get("Location")).Body)
			if !strings.Contains(outcome, approvalSecretName+": granted (approved by "+ada) {
				t.Fatalf("the page after ada's Approve does not say it was granted:\n%s", outcome)
			}
			pods := f.pods(t, base)
			reply := pods.State(podA, requestA)
			if reply.State != "granted" || reply.GrantID == nil || reply.ApproveURL != "" {
				t.Fatalf("pod-a's request after ada's Approve = %+v, want granted with a grant and no approve_url", reply)
			}
			if values := pods.Values(podA, *reply.GrantID); values[approvalSecretName] != approvalSecretValue {
				t.Fatalf("pod-a's grant values = %v, want %s=%s", values, approvalSecretName, approvalSecretValue)
			}
		})
	})

	t.Run("a second pending record for the refusals", func(t *testing.T) {
		pods := f.pods(t, base)
		podB = pods.Enroll(launch, "pod-b", approvalPodAccount)
		reply := pods.Ask(podB, approvalSecretName)
		if reply.State != "pending" || reply.RecordID == nil || reply.ApproveURL != base+"/approve/"+*reply.RecordID {
			t.Fatalf("pod-b's request = %+v, want pending with its record's approve_url", reply)
		}
		requestB, recordB = reply.RequestID, *reply.RecordID
		t.Logf("given, each tried against record %s: carol's own signed-in session and a page token minted from her cookie; "+
			"Dispatch's ServiceAccount token (%s, audience %s) on the account approve and deny routes; "+
			"a valid ID token for %s from the same provider, minted for Dispatch's OAuth client; "+
			"a callback carrying a code the provider issued for a sign-in the broker did not start; "+
			"a session past its expiry; a logged-out session's cookie", recordB, approvalDispatchAccount, approvalPodAudience, ada)
	})

	t.Run("A2/A5: carol's session gets no button and her own valid page token decides nothing", func(t *testing.T) {
		b := newBrowser(t)
		b.SignIn(t, recordB, brokertest.Person(carol))
		page := b.Page(t, recordB)
		text := brokertest.PageText(t, page.Body)
		if brokertest.DecisionForm(t, page.Body, recordB) != nil || !strings.Contains(text, "Only "+ada+" may decide this request") {
			t.Fatalf("carol's page for ada's record offers a decision, or does not say only ada may:\n%s", text)
		}
		expires := time.Now().Add(time.Hour).Unix()
		forged := url.Values{
			"decision": {"approve"}, "record": {recordB}, "printed": {recordB},
			"token": {forgedPageToken(t, b.Cookie(t, sessionCookieName), expires, recordB)}, "expires": {strconv.FormatInt(expires, 10)},
		}
		// The token is good for carol's session, so the form passes the page's own checks and the
		// refusal is the decision's: ada's record is not carol's to decide.
		res := b.FromPage(t, "/approve", forged)
		t.Logf("carol's POST with a page token minted from her own cookie for %s = %d to %q", recordB, res.Status, res.Header.Get("Location"))
		if want := "/approve/" + recordB + "?decided=" + recordB; res.Status != http.StatusSeeOther || res.Header.Get("Location") != want {
			t.Fatalf("carol's POST with her own valid token = %d to %q, want 303 to %s reporting the outcome:\n%s", res.Status, res.Header.Get("Location"), want, res.Body)
		}
		if outcome := brokertest.PageText(t, b.Get(t, res.Header.Get("Location")).Body); !strings.Contains(outcome, approvalSecretName+": not yours to decide; only "+ada+" may") {
			t.Fatalf("the page after carol's forged POST does not say it was not hers to decide:\n%s", outcome)
		}
		requirePending(f.pods(t, base), podB, requestB, "carol's forged POST")
	})

	t.Run("A5: Dispatch's ServiceAccount token on the account routes is sent to the page", func(t *testing.T) {
		for _, route := range []string{"approve", "deny"} {
			status, answer := accountCall(t, base, http.MethodPost, "/v1/credential-requests/"+recordB+"/"+route,
				map[string]string{"Authorization": "Bearer " + f.dispatchToken(t)}, map[string]any{"approver": ada})
			body := decodeError(t, answer)
			if status != http.StatusForbidden || body.Code != "DECIDE_ON_BROKER_PAGE" || body.ApproveURL != base+"/approve/"+recordB {
				t.Fatalf("Dispatch's %s of ada's secret = %d %s, want 403 DECIDE_ON_BROKER_PAGE naming the page", route, status, answer)
			}
		}
		requirePending(f.pods(t, base), podB, requestB, "Dispatch's approve and deny")
	})

	t.Run("A5: an ID token for ada minted for Dispatch's OAuth client decides nothing", func(t *testing.T) {
		claims := brokertest.Person(ada)
		claims["iss"], claims["aud"] = f.issuer.URL(), "example-dispatch-web"
		claims["iat"], claims["exp"] = time.Now().Add(-5*time.Second).Unix(), time.Now().Add(time.Hour).Unix()
		idToken := f.issuer.Mint(t, f.signInKey, claims)
		b := newBrowser(t)
		res := b.Post(t, "/approve", url.Values{"decision": {"approve"}, "record": {recordB}, "printed": {recordB}},
			map[string]string{"Origin": base, "Authorization": "Bearer " + idToken})
		if res.Status != http.StatusForbidden || !strings.Contains(brokertest.PageText(t, res.Body), "nothing was decided") {
			t.Fatalf("POST /approve with ada's Dispatch ID token as a bearer = %d, want 403 deciding nothing:\n%s", res.Status, res.Body)
		}
		status, answer := accountCall(t, base, http.MethodPost, "/v1/credential-requests/"+recordB+"/approve",
			map[string]string{"Authorization": "Bearer " + idToken}, map[string]any{"approver": ada})
		if status != http.StatusUnauthorized || decodeError(t, answer).Code != "ACCOUNT_TOKEN_INVALID" {
			t.Fatalf("the account approve route with ada's Dispatch ID token = %d %s, want 401 ACCOUNT_TOKEN_INVALID", status, answer)
		}
		requirePending(f.pods(t, base), podB, requestB, "ada's Dispatch ID token")
	})

	t.Run("A5: a callback with a code from a sign-in the broker did not start makes no session", func(t *testing.T) {
		foreignCode := func(state string) string {
			t.Helper()
			verifier := oauth2.GenerateVerifier()
			q := url.Values{
				"client_id": {brokertest.SignInClientID}, "response_type": {"code"}, "redirect_uri": {base + "/signin/callback"},
				"scope": {"openid email"}, "state": {state}, "nonce": {"foreign-nonce"},
				"code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "code_challenge_method": {"S256"},
			}
			f.issuer.SignInAs(brokertest.Person(ada))
			code, _ := f.issuer.Authorize(t, f.issuer.URL()+"/authorize?"+q.Encode())
			if code == "" {
				t.Fatal("the provider issued no code for the foreign sign-in")
			}
			return code
		}

		b := newBrowser(t)
		res := b.Get(t, "/signin/callback?"+url.Values{"code": {foreignCode("foreign-state")}, "state": {"foreign-state"}}.Encode())
		if res.Status != http.StatusBadRequest || res.Cookie(sessionCookieName) != nil {
			t.Fatalf("the foreign callback with no sign-in in progress = %d (session cookie %v), want 400 and no session:\n%s",
				res.Status, res.Cookie(sessionCookieName), res.Body)
		}
		requireSignInRedirect(t, f.issuer, b.Get(t, "/approve/"+recordB), "the page after the foreign callback")

		// The same code grafted onto a sign-in this browser did start, with that sign-in's own
		// state: the code was not issued for its PKCE challenge or its nonce, so the exchange fails.
		started := b.StartSignIn(t, "/approve/"+recordB)
		res = b.Get(t, "/signin/callback?"+url.Values{"code": {foreignCode("foreign-state")}, "state": {started.Query().Get("state")}}.Encode())
		if res.Status != http.StatusForbidden || res.Cookie(sessionCookieName) != nil {
			t.Fatalf("the foreign code on the browser's own sign-in = %d (session cookie %v), want 403 and no session:\n%s",
				res.Status, res.Cookie(sessionCookieName), res.Body)
		}
		requireSignInRedirect(t, f.issuer, b.Get(t, "/approve/"+recordB), "the page after the grafted callback")
		requirePending(f.pods(t, base), podB, requestB, "the foreign callbacks")
	})

	t.Run("A2: a session past its expiry decides nothing and signs in again", func(t *testing.T) {
		// A second process on the same database and public URL, whose sessions last two seconds.
		short := f.start(t, databaseURL, public.URL, append(f.signInEnv(), "BROKER_SIGNIN_SESSION_SECONDS=2")...)
		public.route(t, short)
		defer public.route(t, broker)
		b := newBrowser(t)
		b.SignIn(t, recordB, brokertest.Person(ada))
		form := brokertest.DecisionForm(t, b.Page(t, recordB).Body, recordB)
		if form == nil {
			t.Fatalf("ada's page for %s on the short-session broker offers no decision", recordB)
		}
		old := &http.Cookie{Name: sessionCookieName, Value: b.Cookie(t, sessionCookieName)}
		// The one fixed wait: past the session's two-second absolute expiry.
		time.Sleep(3 * time.Second)
		replay := newBrowser(t)
		res := replay.Post(t, "/approve", brokertest.Decision(form, "approve"), map[string]string{"Origin": base}, old)
		if res.Status != http.StatusForbidden || !strings.Contains(brokertest.PageText(t, res.Body), "Not signed in") {
			t.Fatalf("Approve with the expired session's cookie = %d, want 403 not signed in:\n%s", res.Status, res.Body)
		}
		requireSignInRedirect(t, f.issuer, replay.Get(t, "/approve/"+recordB, old), "the page with the expired session's cookie")
		requirePending(f.pods(t, base), podB, requestB, "the expired session's POST")
	})

	t.Run("A2: a logged-out session's replayed cookie decides nothing", func(t *testing.T) {
		b := newBrowser(t)
		b.SignIn(t, recordB, brokertest.Person(ada))
		form := brokertest.DecisionForm(t, b.Page(t, recordB).Body, recordB)
		if form == nil {
			t.Fatalf("ada's page for %s offers no decision", recordB)
		}
		old := &http.Cookie{Name: sessionCookieName, Value: b.Cookie(t, sessionCookieName)}
		if res := b.FromPage(t, "/signin/logout", url.Values{"next": {"/approve/" + recordB}}); res.Status != http.StatusOK {
			t.Fatalf("POST /signin/logout = %d, want 200:\n%s", res.Status, res.Body)
		}
		replay := newBrowser(t)
		res := replay.Post(t, "/approve", brokertest.Decision(form, "approve"), map[string]string{"Origin": base}, old)
		if res.Status != http.StatusForbidden || !strings.Contains(brokertest.PageText(t, res.Body), "Not signed in") {
			t.Fatalf("Approve with the logged-out session's cookie = %d, want 403 not signed in:\n%s", res.Status, res.Body)
		}
		requirePending(f.pods(t, base), podB, requestB, "the logged-out session's POST")
	})

	t.Run("A10: sign-in is all or none, and none boots with approvals off", func(t *testing.T) {
		env := f.baseEnv(databaseURL, "http://127.0.0.1:0")
		signIn := f.signInEnv()
		for _, c := range []struct {
			name string
			set  []string
			want string
		}{
			{"the issuer alone", signIn[:1], "missing BROKER_SIGNIN_CLIENT_ID, BROKER_SIGNIN_CLIENT_SECRET_FILE, BROKER_SIGNIN_DOMAIN"},
			{"all but the domain", signIn[:3], "missing BROKER_SIGNIN_DOMAIN"},
			{"all four with a session over a day", append(slices.Clone(signIn), "BROKER_SIGNIN_SESSION_SECONDS=86401"),
				"BROKER_SIGNIN_SESSION_SECONDS must be a whole number between 1 and 86400"},
		} {
			out := runRefusedBroker(t, f.binary, append(slices.Clone(env), c.set...)...)
			t.Logf("%s refuses boot: %s", c.name, strings.TrimSpace(out))
			if !strings.Contains(out, c.want) {
				t.Fatalf("%s: the broker refused boot without naming %q:\n%s", c.name, c.want, out)
			}
		}

		off := f.start(t, databaseURL, "http://127.0.0.1:0")
		offBase := "http://" + off.Addr
		if !strings.Contains(off.Boot, "approval sign-in off: BROKER_SIGNIN_* unset; no person can approve a secret request") {
			t.Fatalf("the broker with no sign-in did not log that approvals are off:\n%s", off.Boot)
		}
		res := brokertest.NewBrowser(t, offBase, http.DefaultTransport, f.issuer).Get(t, "/approve/"+recordB)
		text := brokertest.PageText(t, res.Body)
		if res.Status != http.StatusServiceUnavailable {
			t.Fatalf("GET /approve/%s with sign-in off = %d, want 503:\n%s", recordB, res.Status, text)
		}
		for _, name := range []string{"BROKER_SIGNIN_ISSUER", "BROKER_SIGNIN_CLIENT_ID", "BROKER_SIGNIN_CLIENT_SECRET_FILE", "BROKER_SIGNIN_DOMAIN"} {
			if !strings.Contains(text, name) {
				t.Fatalf("the sign-in-off page does not name %s:\n%s", name, text)
			}
		}
		status, answer := accountCall(t, offBase, http.MethodPost, "/v1/credential-requests/"+recordB+"/approve",
			map[string]string{"Authorization": "Bearer " + f.dispatchToken(t)}, map[string]any{"approver": ada})
		if status != http.StatusForbidden || decodeError(t, answer).Code != "DECIDE_ON_BROKER_PAGE" {
			t.Fatalf("Dispatch's approve with sign-in off = %d %s, want 403 DECIDE_ON_BROKER_PAGE", status, answer)
		}
		requirePending(f.pods(t, base), podB, requestB, "the sign-in-off broker")
	})

	t.Run("A11: a sign-in begun on one process finishes on another and decides on a third", func(t *testing.T) {
		// Three processes on a database of their own behind one public URL, as replicas sit
		// behind a load balancer, and the row routes each step to the process it names.
		shared := storetest.URL(t)
		replicas := newFront(t)
		first := f.start(t, shared, replicas.URL, f.signInEnv()...)
		second := f.start(t, shared, replicas.URL, f.signInEnv()...)

		replicas.route(t, first)
		pods := f.pods(t, replicas.URL)
		p := pods.Enroll(pods.LogIn(approvalLauncherService, approvalLauncherAccount), "pod-c", approvalPodAccount)
		reply := pods.Ask(p, approvalSecretName)
		if reply.State != "pending" || reply.RecordID == nil || reply.ApproveURL != replicas.URL+"/approve/"+*reply.RecordID {
			t.Fatalf("pod-c's request = %+v, want pending with its record's page on the shared URL", reply)
		}
		recordC := *reply.RecordID

		b := brokertest.NewBrowser(t, replicas.URL, http.DefaultTransport, f.issuer)
		res := b.Get(t, "/approve/"+recordC)
		if res.Status != http.StatusFound || res.Cookie(signInCookieName) == nil {
			t.Fatalf("process #1's page = %d (sign-in cookie %v), want 302 setting the sign-in in progress:\n%s", res.Status, res.Cookie(signInCookieName), res.Body)
		}
		callback := b.Authorize(t, res.Location(t), brokertest.Person(ada))

		replicas.route(t, second)
		res = b.Get(t, callback)
		if res.Status != http.StatusSeeOther || res.Header.Get("Location") != "/approve/"+recordC || res.Cookie(sessionCookieName) == nil {
			t.Fatalf("process #2's callback = %d to %q, want 303 to /approve/%s with a session:\n%s", res.Status, res.Header.Get("Location"), recordC, res.Body)
		}
		const signedIn = "approval sign-in login=" + ada
		if !strings.Contains(second.Log(), signedIn) || strings.Contains(first.Log(), signedIn) {
			t.Fatalf("process #2 logged the sign-in %t, process #1 %t; want #2 alone", strings.Contains(second.Log(), signedIn), strings.Contains(first.Log(), signedIn))
		}

		for _, process := range []*brokerProcess{first, second} {
			process.Terminate(t)
			if err := process.AwaitExit(t, stopTimeout); err != nil {
				t.Fatalf("a broker exited with %v, want 0:\n%s", err, process.Log())
			}
		}
		third := f.start(t, shared, replicas.URL, f.signInEnv()...)
		replicas.route(t, third)
		b.Decide(t, recordC, true)
		if !strings.Contains(third.Log(), "approval page decision login="+ada+" record="+recordC) {
			t.Fatalf("process #3 did not log ada's decision of %s:\n%s", recordC, third.Log())
		}
		granted := pods.State(p, reply.RequestID)
		if granted.State != "granted" || granted.GrantID == nil {
			t.Fatalf("pod-c's request after ada's Approve on process #3 = %+v, want granted", granted)
		}
		if values := pods.Values(p, *granted.GrantID); values[approvalSecretName] != approvalSecretValue {
			t.Fatalf("pod-c's grant values = %v, want %s=%s", values, approvalSecretName, approvalSecretValue)
		}
	})
}
