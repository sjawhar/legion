package agentsecrets

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// jwkThumbprint is the RFC 7638 thumbprint of the ECDSA JWK a JWS header embeds.
func jwkThumbprint(t *testing.T, header map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(header["jwk"])
	if err != nil {
		t.Fatal(err)
	}
	var jwk jose.JSONWebKey
	if err := jwk.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	pub, ok := jwk.Key.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("embedded JWK is %T, want an ECDSA public key", jwk.Key)
	}
	sum, err := (&jose.JSONWebKey{Key: pub}).Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(sum)
}

// fakeBroker is the broker's token-login and enrollment routes (its HTTP API reference:
// https://sjawhar.github.io/legion/broker/reference/api/) as a fake: it records every token login's
// token and request object and every enrollment-route request's headers and body, and answers
// exactly what the test configures. It never honors an Authorization header — none is ever sent.
type fakeBroker struct {
	t *testing.T

	mu           sync.Mutex
	logins       []tokenLogin   // every POST /v1/launcher-credentials/token, refused ones included, in order
	refuseLogins []enrollAnswer // answered, in order, to the next logins instead of accepting them
	lifetime     time.Duration  // each accepted login's credential lives this long; an hour by default
	holdLogins   chan struct{}  // when set, every login waits for it to close before answering

	// enrollment-route behavior: answered in order, the last entry repeating once exhausted.
	enrollAnswers []enrollAnswer
	enrollSeq     int
	enrollReqs    []enrollRequest
}

type tokenLogin struct {
	token      string
	iss        string
	identifier string
	service    string
}

type enrollAnswer struct {
	status int
	body   string
}

type enrollRequest struct {
	method        string
	path          string
	proof         string
	authorization string
	body          map[string]any
}

func newFakeBroker(t *testing.T) (*fakeBroker, *httptest.Server) {
	b := &fakeBroker{t: t, lifetime: time.Hour}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/launcher-credentials/token", b.handleTokenLogin)
	mux.HandleFunc("POST /v1/enrollments", b.handleEnrollmentRoute)
	mux.HandleFunc("DELETE /v1/enrollments/{id}", b.handleEnrollmentRoute)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return b, server
}

func (b *fakeBroker) handleTokenLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" || r.Header.Get("Proof") != "" {
		b.t.Errorf("token login carried a credential header, want none: %v", r.Header)
	}
	var body struct {
		Token   string `json:"token"`
		Request string `json:"request"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		b.t.Errorf("token login body: %v", err)
		return
	}
	_, payload, err := decodeCompactJWSRaw(body.Request)
	if err != nil {
		b.t.Errorf("token login request object: %v", err)
		return
	}
	detail := firstDetail(payload)
	b.mu.Lock()
	b.logins = append(b.logins, tokenLogin{
		token: body.Token, iss: fmt.Sprint(payload["iss"]),
		identifier: fmt.Sprint(detail["identifier"]), service: fmt.Sprint(detail["service"]),
	})
	seq := len(b.logins)
	held := b.holdLogins
	var refused *enrollAnswer
	if len(b.refuseLogins) > 0 {
		refused = &b.refuseLogins[0]
		b.refuseLogins = b.refuseLogins[1:]
	}
	lifetime := b.lifetime
	b.mu.Unlock()
	if held != nil {
		select {
		case <-held:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if refused != nil {
		w.WriteHeader(refused.status)
		_, _ = io.WriteString(w, refused.body)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"credential_id": "cred-1", "expires_at": time.Now().Add(lifetime).UTC().Format(time.RFC3339Nano),
		"seq": seq,
	})
}

func firstDetail(payload map[string]any) map[string]any {
	details, _ := payload["authorization_details"].([]any)
	if len(details) != 1 {
		return map[string]any{}
	}
	detail, _ := details[0].(map[string]any)
	return detail
}

func (b *fakeBroker) handleEnrollmentRoute(w http.ResponseWriter, r *http.Request) {
	req := enrollRequest{
		method: r.Method, path: r.URL.Path,
		proof: r.Header.Get("Proof"), authorization: r.Header.Get("Authorization"),
	}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req.body); err != nil {
				b.t.Errorf("enrollment request body is not JSON: %v", err)
			}
		}
	}
	b.mu.Lock()
	b.enrollReqs = append(b.enrollReqs, req)
	answer := enrollAnswer{status: http.StatusCreated, body: `{"enrollment_id":"enr-1","lease_expires_at":"2026-09-26T12:15:00Z"}`}
	if len(b.enrollAnswers) > 0 {
		idx := min(b.enrollSeq, len(b.enrollAnswers)-1)
		answer = b.enrollAnswers[idx]
		b.enrollSeq++
	}
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(answer.status)
	_, _ = io.WriteString(w, answer.body)
}

func (b *fakeBroker) loginCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.logins)
}

func (b *fakeBroker) login(i int) tokenLogin {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.logins[i]
}

func (b *fakeBroker) lastEnrollRequest() enrollRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.enrollReqs) == 0 {
		return enrollRequest{}
	}
	return b.enrollReqs[len(b.enrollReqs)-1]
}

// withLoginRetry sets the wait after a failed login, and the most it doubles to, for one test.
func withLoginRetry(t *testing.T, initial, most time.Duration) {
	t.Helper()
	oldInitial, oldMax := loginRetryInitial, loginRetryMax
	loginRetryInitial, loginRetryMax = initial, most
	t.Cleanup(func() { loginRetryInitial, loginRetryMax = oldInitial, oldMax })
}

// tokenFile writes token to a fresh file, as the kubelet projects the daemon's ServiceAccount
// token, and returns its path.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// eventually polls fn until it returns true or the deadline passes, failing the test otherwise.
func eventually(t *testing.T, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", msg)
}

// withCredential gives c an already-won credential directly, bypassing the login, for tests whose
// subject is Enroll/Revoke's request shape and error handling.
func withCredential(t *testing.T, c *Client, id string) {
	t.Helper()
	c.cred.Store(&credential{key: mustTestKey(t), id: id, expires: time.Now().Add(time.Hour)})
}

func newIssuedClient(t *testing.T) (*fakeBroker, *Client) {
	broker, server := newFakeBroker(t)
	c := &Client{URL: server.URL, HTTP: server.Client()}
	withCredential(t, c, "cred-1")
	return broker, c
}

var podEnrollment = PodEnrollment{
	PodUID:     "5f2c2d5e-8f9a-4a52-9f2f-0b4e1e3c9a11",
	Thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs", PodToken: "eyJhbGciOiJSUzI1NiJ9.e30.sig", Session: "ses_implementer",
}

// lockedLog is a log a test reads while the login loop writes it.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestLoginPostsTheTokenAndAKeyItThenSignsWith pins the token login: Login reads the token file and
// posts its token beside a request object naming legion-daemon, signed by a fresh key, to
// /v1/launcher-credentials/token, and answers the expiry it won; once it succeeds Enroll
// authenticates with a launcher proof — never Authorization — and enrolling before any login fails
// closed with NO_MACHINE_CREDENTIAL.
func TestLoginPostsTheTokenAndAKeyItThenSignsWith(t *testing.T) {
	broker, server := newFakeBroker(t)
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "the-daemon's-token")}
	if _, err := c.Enroll(context.Background(), podEnrollment); !isNoCredential(err, "no token login yet") {
		t.Fatalf("Enroll before any login = %v, want NO_MACHINE_CREDENTIAL naming no login yet", err)
	}
	if got := c.LoginStatus().State; got != "none" {
		t.Fatalf("LoginStatus before any login = %q, want none", got)
	}
	expires, err := c.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if until := time.Until(expires); until < 59*time.Minute || until > time.Hour {
		t.Fatalf("Login answered an expiry %v away, want the broker's hour", until)
	}
	if got := broker.login(0); got.token != "the-daemon's-token" || got.identifier != launcherService || got.service != launcherService || got.iss == "" {
		t.Fatalf("token login = %+v, want the file's token, identifier and service %s, and the key's thumbprint as iss", got, launcherService)
	}
	if got := c.LoginStatus().State; got != "issued" {
		t.Fatalf("LoginStatus after the login = %q, want issued", got)
	}
	if _, err := c.Enroll(context.Background(), podEnrollment); err != nil {
		t.Fatalf("Enroll after the login: %v", err)
	}
	req := broker.lastEnrollRequest()
	if req.authorization != "" || req.proof == "" {
		t.Fatalf("enrollment carried Authorization %q and Proof %q, want only a proof", req.authorization, req.proof)
	}
	header, payload, err := decodeCompactJWSRaw(req.proof)
	if err != nil {
		t.Fatal(err)
	}
	if payload["lid"] != "cred-1" || header["jwk"] == nil {
		t.Fatalf("proof header %v payload %v, want lid cred-1 and the key's JWK", header, payload)
	}
	if jwkThumbprint(t, header) != broker.login(0).iss {
		t.Fatal("the proof's key is not the key the login posted")
	}
}

// TestEachLoginReadsTheCurrentTokenAndProvesTheSameKey: a renewal presents the token the file now
// holds, as the kubelet rotates it, and proves the key the credential is already bound to, so the
// broker never moves it to another key; only once the broker has refused the credential does the
// next login bring a new key.
func TestEachLoginReadsTheCurrentTokenAndProvesTheSameKey(t *testing.T) {
	broker, server := newFakeBroker(t)
	broker.enrollAnswers = []enrollAnswer{{status: http.StatusUnauthorized, body: `{"code":"LAUNCHER_INVALID","error":"refused"}`}}
	path := tokenFile(t, "token-1")
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: path}
	if _, err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("token-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, second := broker.login(0), broker.login(1)
	if first.token != "token-1" || second.token != "token-2" || first.iss != second.iss {
		t.Fatalf("logins = %+v then %+v, want each token as the file held it, under one key", first, second)
	}
	if _, err := c.Enroll(context.Background(), podEnrollment); !isNoCredential(err, "refused") {
		t.Fatalf("Enroll the broker refuses = %v, want NO_MACHINE_CREDENTIAL", err)
	}
	if _, err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if third := broker.login(2); third.iss == first.iss {
		t.Fatal("the login after the broker refused the credential proved the refused key, want a new one")
	}
}

// TestALoginTheBrokerRefusesIsFailedAndAsksNoOne: a refused login (another ServiceAccount, an
// expired token) or one whose token file is missing records the login failed and keeps no
// credential; nothing in it names a code for anyone.
func TestALoginTheBrokerRefusesIsFailedAndAsksNoOne(t *testing.T) {
	broker, server := newFakeBroker(t)
	broker.refuseLogins = []enrollAnswer{
		{http.StatusForbidden, `{"code":"ACCOUNT_NOT_LISTED","error":"this ServiceAccount does not log in as this service"}`},
		{http.StatusUnauthorized, `{"code":"TOKEN_EXPIRED","error":"the ServiceAccount token has expired"}`},
	}
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "token")}
	for i, code := range []string{"ACCOUNT_NOT_LISTED", "TOKEN_EXPIRED"} {
		_, err := c.Login(context.Background())
		var api *APIError
		if !errors.As(err, &api) || api.Code != code {
			t.Fatalf("login %d = %v, want the broker's %s", i, err, code)
		}
		if got := c.LoginStatus().State; got != "failed" || c.cred.Load() != nil {
			t.Fatalf("after login %d: state %q, credential %v; want failed and none", i, got, c.cred.Load() != nil)
		}
		if _, err := c.Enroll(context.Background(), podEnrollment); !isNoCredential(err, code) {
			t.Fatalf("Enroll after login %d = %v, want NO_MACHINE_CREDENTIAL naming %s", i, err, code)
		}
	}
	missing := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: filepath.Join(t.TempDir(), "absent")}
	if _, err := missing.Login(context.Background()); err == nil || !strings.Contains(err.Error(), "read the ServiceAccount token") {
		t.Fatalf("a login with no token file = %v, want a failure naming the token file", err)
	}
	if got := broker.loginCount(); got != 2 {
		t.Fatalf("the broker saw %d logins, want the two refused ones and none from a missing token", got)
	}
}

// TestALoginAnswerWithNoExpiryNamesNoCredential: a 200 whose body lacks expires_at is a failed
// login, and its error never repeats the body, which carries the credential's id.
func TestALoginAnswerWithNoExpiryNamesNoCredential(t *testing.T) {
	broker, server := newFakeBroker(t)
	broker.refuseLogins = []enrollAnswer{{http.StatusOK, `{"credential_id":"secret-credential-id"}`}}
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "token")}
	_, err := c.Login(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret-credential-id") {
		t.Fatalf("Login = %v, want a failure that does not name the credential", err)
	}
	if _, err := c.Enroll(context.Background(), podEnrollment); err == nil || strings.Contains(err.Error(), "secret-credential-id") {
		t.Fatalf("Enroll = %v, want NO_MACHINE_CREDENTIAL that does not name the credential", err)
	}
}

// TestARefusedRenewalLeavesTheHeldCredentialIssued: a renewal the broker refuses while the
// credential it would replace has not expired leaves that credential working: the state stays
// issued and pods still enroll under it until it expires, after which the state reads failed.
func TestARefusedRenewalLeavesTheHeldCredentialIssued(t *testing.T) {
	withLoginRetry(t, time.Minute, time.Minute)
	broker, server := newFakeBroker(t)
	broker.lifetime = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "token"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	broker.mu.Lock()
	broker.refuseLogins = []enrollAnswer{{http.StatusServiceUnavailable, `{"code":"DATABASE","error":"down"}`}}
	broker.mu.Unlock()
	eventually(t, func() bool { return broker.loginCount() == 2 }, "the renewal to reach the broker")
	eventually(t, func() bool { return c.failure.Load() != nil }, "the refused renewal to be recorded")
	if got := c.LoginStatus().State; got != "issued" {
		t.Fatalf("state after a refused renewal under a live credential = %q, want issued", got)
	}
	if _, err := c.Enroll(ctx, podEnrollment); err != nil {
		t.Fatalf("Enroll under the still-live credential: %v", err)
	}
	eventually(t, func() bool { return c.LoginStatus().State == "failed" }, "the credential to expire")
	if _, err := c.Enroll(ctx, podEnrollment); !isNoCredential(err, "DATABASE") {
		t.Fatalf("Enroll once the credential expired = %v, want NO_MACHINE_CREDENTIAL naming the refusal", err)
	}
}

// TestRefusedRenewalsKeepTheirWaitPastTheCredentialsExpiry: renewals the broker keeps refusing are
// retried after loginRetryWait, before the held credential expires and after it, never in a loop
// that waits for nothing: two seconds of a broker that answers every renewal 503 at once see a
// handful of logins, not thousands.
func TestRefusedRenewalsKeepTheirWaitPastTheCredentialsExpiry(t *testing.T) {
	withLoginRetry(t, 200*time.Millisecond, 400*time.Millisecond)
	broker, server := newFakeBroker(t)
	broker.lifetime = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "token"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	refusals := make([]enrollAnswer, 10000)
	for i := range refusals {
		refusals[i] = enrollAnswer{http.StatusServiceUnavailable, `{"code":"DATABASE","error":"down"}`}
	}
	broker.mu.Lock()
	broker.refuseLogins = refusals
	broker.mu.Unlock()
	time.Sleep(2500 * time.Millisecond)
	if n := broker.loginCount(); n > 12 {
		t.Fatalf("%d logins in 2.5 s of refused renewals, want at most 12: a renewal waits 200 ms doubling to 400 ms after each refusal", n)
	}
	if got := c.LoginStatus().State; got != "failed" {
		t.Fatalf("state after the credential expired under refused renewals = %q, want failed", got)
	}
}

// TestARefusedProofLogsInAgain: when the broker refuses the held credential (401
// LAUNCHER_INVALID, as after its account left BROKER_SERVICE_LOGINS and returned), the call answers
// NO_MACHINE_CREDENTIAL and the login loop logs in again at once with the current token, after
// which the next enrollment succeeds — no person asked, no restart.
func TestARefusedProofLogsInAgain(t *testing.T) {
	broker, server := newFakeBroker(t)
	broker.enrollAnswers = []enrollAnswer{
		{status: http.StatusUnauthorized, body: `{"code":"LAUNCHER_INVALID","error":"refused"}`},
		{status: http.StatusCreated, body: `{"enrollment_id":"enr-2","lease_expires_at":"2026-09-26T12:15:00Z"}`},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "token"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enroll(ctx, podEnrollment); !isNoCredential(err, "refused") {
		t.Fatalf("Enroll refused by the broker = %v, want NO_MACHINE_CREDENTIAL", err)
	}
	eventually(t, func() bool { return broker.loginCount() == 2 && c.cred.Load() != nil }, "the login loop to log in again")
	if got, err := c.Enroll(ctx, podEnrollment); err != nil || got.ID != "enr-2" {
		t.Fatalf("Enroll after logging in again = %+v, %v; want enr-2", got, err)
	}
}

// TestTheLoginLoopRenewsHalfwayToExpiry: with credentials that live 200 ms, the loop logs in again
// before each expires, so a second later the daemon has logged in several times and still holds a
// live credential.
func TestTheLoginLoopRenewsHalfwayToExpiry(t *testing.T) {
	broker, server := newFakeBroker(t)
	broker.lifetime = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logged := &lockedLog{}
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "token"), Log: slog.New(slog.NewTextHandler(logged, nil))}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if n := broker.loginCount(); n < 5 {
		t.Fatalf("%d logins in a second of 200 ms credentials, want at least five", n)
	}
	if cred := c.cred.Load(); cred == nil || !cred.expires.After(time.Now()) {
		t.Fatal("no live credential after renewals, want one")
	}
	if got := strings.Count(logged.String(), "logged in with the daemon's ServiceAccount token"); got != broker.loginCount() && got != broker.loginCount()-1 {
		t.Fatalf("log = %s, want each login logged once", logged.String())
	}
}

// TestAFailedLoginIsRetriedAfterItsWait: a login the broker refuses is logged and retried by the
// loop once loginRetryWait has passed, and the first one that succeeds gives the client its
// credential.
func TestAFailedLoginIsRetriedAfterItsWait(t *testing.T) {
	withLoginRetry(t, 50*time.Millisecond, 100*time.Millisecond)
	broker, server := newFakeBroker(t)
	broker.refuseLogins = []enrollAnswer{
		{http.StatusServiceUnavailable, `{"code":"DATABASE","error":"down"}`},
		{http.StatusServiceUnavailable, `{"code":"DATABASE","error":"down"}`},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logged := &lockedLog{}
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "token"), Log: slog.New(slog.NewTextHandler(logged, nil))}
	if err := c.Start(ctx); err == nil {
		t.Fatal("the first login succeeded, want the refusal")
	}
	eventually(t, func() bool { return c.LoginStatus().State == "issued" }, "a retried login to succeed")
	if n := broker.loginCount(); n != 3 {
		t.Fatalf("%d logins, want the two refused and the third that succeeded", n)
	}
	if got := strings.Count(logged.String(), "the token login failed"); got != 2 {
		t.Fatalf("log = %s, want each refused login logged once", logged.String())
	}
}

// TestAProofDuringALoginPOSTReturnsWithinItsDeadline: an Enroll or a Revoke runs under its claim's
// machine lock with the RPC deadline, so it never waits for a login the broker is slow to answer:
// while a renewal's POST hangs, an enrollment under the held credential succeeds, and one the broker
// refuses 401 answers NO_MACHINE_CREDENTIAL, both well within their deadline.
func TestAProofDuringALoginPOSTReturnsWithinItsDeadline(t *testing.T) {
	broker, server := newFakeBroker(t)
	release := make(chan struct{})
	broker.holdLogins = release
	broker.enrollAnswers = []enrollAnswer{
		{status: http.StatusCreated, body: `{"enrollment_id":"enr-1","lease_expires_at":"2026-09-26T12:15:00Z"}`},
		{status: http.StatusUnauthorized, body: `{"code":"LAUNCHER_INVALID","error":"refused"}`},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "token"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	withCredential(t, c, "cred-0")
	started := make(chan error, 1)
	go func() { started <- c.Start(ctx) }()
	eventually(t, func() bool { return broker.loginCount() == 1 }, "the renewal to reach the broker")
	for i, want := range []string{"", "NO_MACHINE_CREDENTIAL"} {
		rpc, rpcCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, err := c.Enroll(rpc, podEnrollment)
		rpcCancel()
		switch {
		case want == "" && err != nil:
			t.Fatalf("Enroll %d during the renewal's POST = %v, want it enrolled under the held credential", i, err)
		case want != "" && !isNoCredential(err, "refused"):
			t.Fatalf("Enroll %d during the renewal's POST = %v, want %s, not its deadline", i, err, want)
		}
	}
	close(release)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
}

// isNoCredential reports whether err is the client's NO_MACHINE_CREDENTIAL, its message naming why.
func isNoCredential(err error, why string) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == 0 && api.Code == "NO_MACHINE_CREDENTIAL" && strings.Contains(api.Message, why) && !IsPermanent(err)
}

func TestEnrollSendsTheContractBodyAndReadsACreatedEnrollment(t *testing.T) {
	broker, c := newIssuedClient(t)
	got, err := c.Enroll(context.Background(), podEnrollment)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "enr-1" || !got.LeaseExpires.Equal(time.Date(2026, 9, 26, 12, 15, 0, 0, time.UTC)) {
		t.Fatalf("enrollment = %+v", got)
	}
	req := broker.lastEnrollRequest()
	if req.method != http.MethodPost || req.path != "/v1/enrollments" {
		t.Fatalf("request %s %s", req.method, req.path)
	}
	if req.authorization != "" {
		t.Fatalf("request carried Authorization %q, want none", req.authorization)
	}
	if req.proof == "" {
		t.Fatalf("request carried no Proof header")
	}
	want := map[string]any{
		"kind": "pod", "runtime_id": podEnrollment.PodUID, "operator": nil,
		"thumbprint": podEnrollment.Thumbprint, "session_id": "ses_implementer", "pod_token": podEnrollment.PodToken,
	}
	if gotJSON, wantJSON := mustJSON(t, req.body), mustJSON(t, want); gotJSON != wantJSON {
		t.Fatalf("body\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

func TestEnrollWithoutASessionSendsNull(t *testing.T) {
	broker, c := newIssuedClient(t)
	e := podEnrollment
	e.Session = ""
	if _, err := c.Enroll(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	req := broker.lastEnrollRequest()
	if v, present := req.body["session_id"]; !present || v != nil {
		t.Fatalf("session_id = %v (present %t), want an explicit null", v, present)
	}
}

func TestEnrollTreatsAnExistingEnrollmentAsTheSame(t *testing.T) {
	broker, c := newIssuedClient(t)
	broker.enrollAnswers = []enrollAnswer{{status: http.StatusOK, body: `{"enrollment_id":"enr-existing","lease_expires_at":"2026-09-26T12:15:00Z"}`}}
	got, err := c.Enroll(context.Background(), podEnrollment)
	if err != nil || got.ID != "enr-existing" {
		t.Fatalf("got %+v, %v; want the existing enrollment on 200", got, err)
	}
}

func TestEnrollWithAValidBodyMissingEnrollmentIDNamesItWithNoWrappedNil(t *testing.T) {
	broker, c := newIssuedClient(t)
	broker.enrollAnswers = []enrollAnswer{{status: http.StatusCreated, body: `{"lease_expires_at":"2026-09-26T12:15:00Z"}`}}
	_, err := c.Enroll(context.Background(), podEnrollment)
	if err == nil || !strings.Contains(err.Error(), "enrollment_id") {
		t.Fatalf("err %v, want a refusal naming enrollment_id", err)
	}
	if strings.Contains(err.Error(), "%!") {
		t.Fatalf("err %v, want no formatting artifact from a nil wrapped error", err)
	}
}

func TestEnrollRefusalsCarryTheCodeAndPermanence(t *testing.T) {
	for _, tc := range []struct {
		status       int
		code         string
		permanent    bool
		tokenExpired bool
	}{
		{http.StatusForbidden, "POD_IDENTITY_MISMATCH", true, false},
		// The same key enrolls with the pod's current token, so it is not permanent.
		{http.StatusForbidden, "POD_TOKEN_EXPIRED", false, true},
		{http.StatusConflict, "ALREADY_ENROLLED", true, false},
		{http.StatusTooManyRequests, "RATE_LIMITED", false, false},
		{http.StatusServiceUnavailable, "DATABASE", false, false},
	} {
		broker, c := newIssuedClient(t)
		broker.enrollAnswers = []enrollAnswer{{status: tc.status, body: `{"code":"` + tc.code + `","error":"why"}`}}
		_, err := c.Enroll(context.Background(), podEnrollment)
		var api *APIError
		if !errors.As(err, &api) {
			t.Fatalf("%d: err %v is not an *APIError", tc.status, err)
		}
		if api.Status != tc.status || api.Code != tc.code || api.Message != "why" || api.Permanent() != tc.permanent || IsPermanent(err) != tc.permanent ||
			api.TokenExpired() != tc.tokenExpired {
			t.Fatalf("%d %s: %+v permanent=%t tokenExpired=%t", tc.status, tc.code, api, api.Permanent(), api.TokenExpired())
		}
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound} {
		broker, c := newIssuedClient(t)
		broker.enrollAnswers = []enrollAnswer{{status: status, body: ""}}
		if err := c.Revoke(context.Background(), "enr-1"); err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		req := broker.lastEnrollRequest()
		if req.method != http.MethodDelete || req.path != "/v1/enrollments/enr-1" {
			t.Fatalf("%d: request %s %s", status, req.method, req.path)
		}
		if req.authorization != "" || req.proof == "" {
			t.Fatalf("%d: authorization %q, proof %q; want a proof and no authorization", status, req.authorization, req.proof)
		}
	}
	broker, c := newIssuedClient(t)
	broker.enrollAnswers = []enrollAnswer{{status: http.StatusServiceUnavailable, body: `{"code":"DATABASE","error":"down"}`}}
	if err := c.Revoke(context.Background(), "enr-1"); err == nil || IsPermanent(err) {
		t.Fatalf("503 revoke: err %v, want a transient failure", err)
	}
}

func TestAnUnreachableBrokerIsATransientError(t *testing.T) {
	c := &Client{URL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}}
	withCredential(t, c, "cred-1")
	_, err := c.Enroll(context.Background(), podEnrollment)
	if err == nil || IsPermanent(err) {
		t.Fatalf("err %v, want a transient failure", err)
	}
}

func TestARefusalBodyThatIsNotTheContractsShapeFallsBackToUNKNOWN(t *testing.T) {
	broker, c := newIssuedClient(t)
	broker.enrollAnswers = []enrollAnswer{{status: http.StatusBadGateway, body: "<html>Bad Gateway</html>"}}
	_, err := c.Enroll(context.Background(), podEnrollment)
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("err %v is not an *APIError", err)
	}
	if api.Code != "UNKNOWN" || api.Message != "<html>Bad Gateway</html>" {
		t.Fatalf("api = %+v, want code UNKNOWN and the raw body as the message", api)
	}
	if api.Permanent() || IsPermanent(err) {
		t.Fatalf("api = %+v permanent, want a 5xx to be transient", api)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
