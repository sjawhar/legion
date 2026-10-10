package agentsecrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBroker is the broker's launcher-credential and enrollment routes (its HTTP API reference:
// https://sjawhar.github.io/legion/broker/reference/api/) as a fake: it captures every login request
// object's claims and every enrollment-route request's headers and body, and answers exactly
// what the test configures. It never honors an Authorization header — the whole point of this
// task is that one is never sent any more.
type fakeBroker struct {
	t *testing.T

	mu            sync.Mutex
	logins        []loginClaims           // one entry per accepted POST /v1/launcher-credentials, in order
	pending       map[string]pendingState // pendingID -> current answer
	loginSeq      int
	loginAttempts int                // every POST /v1/launcher-credentials, refused ones included
	refuseLogins  []enrollAnswer     // answered, in order, to the next logins instead of accepting them; status 0 drops the connection
	hangLogins    chan struct{}      // when set, every login waits for it to close (or its request to end) before answering
	holdEnrolls   chan chan struct{} // when set, every enrollment-route request sends a channel here and answers once the test closes it
	holdAnswers   chan struct{}      // when set, every login the broker accepts is recorded (its code exists) and then answered only once this closes, or not at all if its request ends first

	// enrollment-route behavior: answered in order, the last entry repeating once exhausted.
	enrollAnswers []enrollAnswer
	enrollSeq     int
	enrollReqs    []enrollRequest
}

type loginClaims struct {
	iss       string
	loginHint any
	host      string
	service   string
}

type pendingState struct {
	state        string // pending|issued|denied|expired
	credentialID string
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
	b := &fakeBroker{t: t, pending: map[string]pendingState{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/launcher-credentials", b.handleLoginRequest)
	mux.HandleFunc("GET /v1/launcher-credentials/{pending}", b.handleLoginPoll)
	mux.HandleFunc("POST /v1/enrollments", b.handleEnrollmentRoute)
	mux.HandleFunc("DELETE /v1/enrollments/{id}", b.handleEnrollmentRoute)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return b, server
}

func (b *fakeBroker) handleLoginRequest(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" || r.Header.Get("Proof") != "" {
		b.t.Errorf("login request carried a credential header, want none: %v", r.Header)
	}
	var body struct {
		Request string `json:"request"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		b.t.Errorf("login request body: %v", err)
		return
	}
	_, payload, err := decodeCompactJWSRaw(body.Request)
	if err != nil {
		b.t.Errorf("login request object: %v", err)
		return
	}
	detail := firstDetail(payload)

	b.mu.Lock()
	b.loginAttempts++
	hang := b.hangLogins
	b.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-r.Context().Done():
			return
		}
	}
	b.mu.Lock()
	if len(b.refuseLogins) > 0 {
		refused := b.refuseLogins[0]
		b.refuseLogins = b.refuseLogins[1:]
		b.mu.Unlock()
		if refused.status == 0 {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				b.t.Errorf("hijack the login connection: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(refused.status)
		_, _ = io.WriteString(w, refused.body)
		return
	}
	b.loginSeq++
	pendingID := fmt.Sprintf("pending-%d", b.loginSeq)
	code := fmt.Sprintf("CODE-%d", b.loginSeq)
	b.logins = append(b.logins, loginClaims{
		iss:       fmt.Sprint(payload["iss"]),
		loginHint: payload["login_hint"],
		host:      fmt.Sprint(detail["identifier"]),
		service:   fmt.Sprint(detail["service"]),
	})
	b.pending[pendingID] = pendingState{state: "pending"}
	holdAnswer := b.holdAnswers
	b.mu.Unlock()
	if holdAnswer != nil {
		select {
		case <-holdAnswer:
		case <-r.Context().Done():
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"pending_id": pendingID, "code": code})
}

func firstDetail(payload map[string]any) map[string]any {
	details, _ := payload["authorization_details"].([]any)
	if len(details) != 1 {
		return map[string]any{}
	}
	detail, _ := details[0].(map[string]any)
	return detail
}

// setPending lets the test drive a pending login's state directly, keyed by its 1-based sequence
// number (the order Login was called in).
func (b *fakeBroker) setPending(seq int, state, credentialID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending[fmt.Sprintf("pending-%d", seq)] = pendingState{state: state, credentialID: credentialID}
}

func (b *fakeBroker) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" || r.Header.Get("Proof") != "" {
		b.t.Errorf("login poll carried a credential header, want none: %v", r.Header)
	}
	pending := r.PathValue("pending")
	b.mu.Lock()
	st, ok := b.pending[pending]
	b.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	answer := map[string]any{"state": st.state}
	if st.state == "issued" {
		answer["credential_id"] = st.credentialID
	}
	_ = json.NewEncoder(w).Encode(answer)
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
	held := b.holdEnrolls
	b.mu.Unlock()
	if held != nil {
		release := make(chan struct{})
		select {
		case held <- release:
		case <-r.Context().Done():
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
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

func (b *fakeBroker) lastLogin() loginClaims {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.logins) == 0 {
		return loginClaims{}
	}
	return b.logins[len(b.logins)-1]
}

func (b *fakeBroker) loginCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.logins)
}

func (b *fakeBroker) loginAttemptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loginAttempts
}

func (b *fakeBroker) lastEnrollRequest() enrollRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.enrollReqs) == 0 {
		return enrollRequest{}
	}
	return b.enrollReqs[len(b.enrollReqs)-1]
}

// withFastPolling overrides the poll goroutine's backoff for the duration of one test so it
// never has to wait out the real 2s-10s production interval.
func withFastPolling(t *testing.T) {
	t.Helper()
	oldInitial, oldMax := pollInitialInterval, pollMaxInterval
	pollInitialInterval, pollMaxInterval = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { pollInitialInterval, pollMaxInterval = oldInitial, oldMax })
}

// withLoginRetry sets the delay doProof waits after a login that did not end issued, before it
// starts the next one, and the most that delay doubles to, for the duration of one test.
func withLoginRetry(t *testing.T, initial, most time.Duration) {
	t.Helper()
	oldInitial, oldMax := loginRetryInitial, loginRetryMax
	loginRetryInitial, loginRetryMax = initial, most
	t.Cleanup(func() { loginRetryInitial, loginRetryMax = oldInitial, oldMax })
}

// enrollUntil calls Enroll until done reports true, failing the test at the deadline, and returns
// the last call's error.
func enrollUntil(t *testing.T, c *Client, done func(error) bool, msg string) error {
	t.Helper()
	var err error
	eventually(t, func() bool {
		_, err = c.Enroll(context.Background(), podEnrollment)
		return done(err)
	}, msg)
	return err
}

// eventually polls fn until it returns true or the deadline passes, failing the test otherwise.
func eventually(t *testing.T, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", msg)
}

// withCredential gives c an already-issued credential directly, bypassing the login/poll flow
// entirely, for tests whose subject is Enroll/Revoke's request shape and error handling rather
// than the login flow itself (that flow has its own dedicated tests below).
func withCredential(t *testing.T, c *Client, id string) {
	t.Helper()
	c.cred.Store(&credential{key: mustTestKey(t), id: id})
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

// TestLoginThenEnrollSignsProofs pins the whole replacement: Login signs a machine request
// object naming this host and "legion-daemon", the broker's answer is polled until issued, and
// once issued Enroll authenticates with a launcher proof header — never Authorization — anywhere
// in the flow. Enrolling before the login is issued fails closed with NO_MACHINE_CREDENTIAL
// naming the pending code, so the caller retries later instead of erroring hard.
func TestLoginThenEnrollSignsProofs(t *testing.T) {
	withFastPolling(t)
	broker, server := newFakeBroker(t)
	c := &Client{URL: server.URL, HTTP: server.Client()}

	code, err := c.Login(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code != "CODE-1" {
		t.Fatalf("code = %q, want CODE-1", code)
	}
	wantHost, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	login := broker.lastLogin()
	if login.loginHint != nil || login.service != "legion-daemon" || login.host != wantHost {
		t.Fatalf("login request = %+v, want host %q, service legion-daemon and no login_hint: a service's login names no approver", login, wantHost)
	}

	// The login is still pending: Enroll must fail closed, naming the code, not error hard.
	_, err = c.Enroll(context.Background(), podEnrollment)
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if api.Code != "NO_MACHINE_CREDENTIAL" || !strings.Contains(api.Message, code) {
		t.Fatalf("api = %+v, want NO_MACHINE_CREDENTIAL naming %q", api, code)
	}
	if IsPermanent(err) {
		t.Fatalf("NO_MACHINE_CREDENTIAL must be non-permanent (retryable), got permanent")
	}

	// Now the human approves: the broker starts answering "issued".
	broker.setPending(1, "issued", "cred-1")
	eventually(t, func() bool { return c.LoginStatus().State == "issued" }, "login to become issued")
	if got := c.LoginStatus(); got.Code != code {
		t.Fatalf("LoginStatus = %+v, want code %q preserved", got, code)
	}

	if _, err := c.Enroll(context.Background(), podEnrollment); err != nil {
		t.Fatalf("Enroll after issue: %v", err)
	}
	req := broker.lastEnrollRequest()
	if req.authorization != "" {
		t.Fatalf("enroll request carried Authorization %q, want none ever", req.authorization)
	}
	if req.proof == "" {
		t.Fatalf("enroll request carried no Proof header")
	}
	_, proofPayload := decodeCompactJWS(t, req.proof)
	if proofPayload["lid"] != "cred-1" {
		t.Fatalf("proof lid = %v, want the issued credential id cred-1", proofPayload["lid"])
	}
}

// TestExpiredCredentialTriggersAFreshLoginWithANewKey pins the fail-open-to-a-new-login path: a
// 401 on an enrollment-route call (LAUNCHER_INVALID) clears the held credential and starts a
// fresh login signed by a freshly minted key — never the old, now-refused one.
func TestExpiredCredentialTriggersAFreshLoginWithANewKey(t *testing.T) {
	withFastPolling(t)
	broker, server := newFakeBroker(t)
	broker.enrollAnswers = []enrollAnswer{
		{status: http.StatusUnauthorized, body: `{"code":"LAUNCHER_INVALID","error":"expired"}`},
	}
	c := &Client{URL: server.URL, HTTP: server.Client()}

	if _, err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	broker.setPending(1, "issued", "cred-1")
	eventually(t, func() bool { return c.LoginStatus().State == "issued" }, "first login to become issued")
	firstIssuer := broker.lastLogin().iss

	_, err := c.Enroll(context.Background(), podEnrollment)
	var api *APIError
	if !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" {
		t.Fatalf("err = %v, want NO_MACHINE_CREDENTIAL after a 401", err)
	}

	eventually(t, func() bool { return broker.loginCount() == 2 }, "a second login to be sent")
	secondIssuer := broker.lastLogin().iss
	if secondIssuer == "" || secondIssuer == firstIssuer {
		t.Fatalf("second login iss = %q, first was %q; want a different key", secondIssuer, firstIssuer)
	}
	if got := c.LoginStatus(); got.State != "pending" || got.Code == "CODE-1" {
		t.Fatalf("LoginStatus = %+v, want a fresh pending code distinct from CODE-1", got)
	}

	// Resolve the second login before returning: an unresolved pending login's poll goroutine
	// would otherwise keep running past this test (context.Background(), no terminal state),
	// racing the next test's withFastPolling override of the shared backoff variables.
	broker.setPending(2, "issued", "cred-2")
	eventually(t, func() bool { return c.LoginStatus().State == "issued" }, "second login to become issued")
}

// TestReLoginPollSurvivesCallerContextCancellation pins the fix for the wedged-re-login bug: a
// 401 on an enrollment call starts a fresh login (doProof) on the *same* ctx the caller passed
// in, and production callers (supervise/secrets.go's ensureEnrolled/revokeAttempt) wrap every
// such call in context.WithTimeout(parent, RPC) and unconditionally cancel() the instant the call
// returns. Before the fix, Login's poll goroutine ran on that same ctx, so the cancel landed
// before poll ever read the pending login even once, and LoginStatus never left "pending" --
// permanently wedging re-login after any credential invalidation until a daemon restart. The fix
// (context.WithoutCancel) detaches the poll goroutine's lifetime from the triggering call's own,
// so it keeps polling and reaches "issued" once approved, however long after the caller's own
// context died.
func TestReLoginPollSurvivesCallerContextCancellation(t *testing.T) {
	withFastPolling(t)
	broker, c := newIssuedClient(t)
	broker.enrollAnswers = []enrollAnswer{
		{status: http.StatusUnauthorized, body: `{"code":"LAUNCHER_INVALID","error":"expired"}`},
	}

	// Mirror the production callsite exactly: a short-lived context the caller cancels
	// unconditionally the instant the call returns, before this test ever inspects LoginStatus.
	func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := c.Enroll(ctx, podEnrollment)
		var api *APIError
		if !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" {
			t.Fatalf("err = %v, want NO_MACHINE_CREDENTIAL after a 401", err)
		}
	}()

	// The caller's context is now cancelled. The re-login's poll goroutine must still be running
	// on its own detached lifetime and must still resolve to "issued" once approved -- pre-fix,
	// the poll goroutine observes the same cancellation and returns without ever polling, so
	// LoginStatus stays "pending" forever and this times out.
	broker.setPending(1, "issued", "cred-2")
	eventually(t, func() bool { return c.LoginStatus().State == "issued" },
		"re-login to survive the caller's context cancellation and resolve to issued")
}

// held401 gives c a live credential the broker answers 401 to, holding each enrollment-route
// request at the broker until the test releases it, so several can be in flight on that one
// credential at once.
func held401(t *testing.T) (*fakeBroker, *Client, chan chan struct{}) {
	t.Helper()
	broker, c := newIssuedClient(t)
	held := make(chan chan struct{})
	broker.holdEnrolls = held
	broker.enrollAnswers = []enrollAnswer{{status: http.StatusUnauthorized, body: `{"code":"LAUNCHER_INVALID","error":"expired"}`}}
	return broker, c, held
}

// enrollAsync runs Enroll in the background and answers its error on the returned channel.
func enrollAsync(ctx context.Context, c *Client) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := c.Enroll(ctx, podEnrollment)
		done <- err
	}()
	return done
}

// TestConcurrent401sStartOneLogin: five Enrolls that each get a 401 on the same credential send
// one login, under the wait like any other: only the call that cleared the credential retries it,
// and a refused retry waits before the next.
func TestConcurrent401sStartOneLogin(t *testing.T) {
	withFastPolling(t)
	withLoginRetry(t, time.Hour, 4*time.Hour)
	broker, c, held := held401(t)
	refused := enrollAnswer{http.StatusTooManyRequests, `{"code":"RATE_LIMITED","error":"too many"}`}
	broker.refuseLogins = []enrollAnswer{refused, refused, refused, refused, refused}
	var done []<-chan error
	var releases []chan struct{}
	for range 5 {
		done = append(done, enrollAsync(context.Background(), c))
		releases = append(releases, <-held)
	}
	for _, release := range releases {
		close(release)
	}
	for _, d := range done {
		if err := <-d; err == nil {
			t.Fatal("an Enroll answered 401 succeeded")
		}
	}
	if got := broker.loginAttemptCount(); got != 1 {
		t.Fatalf("broker saw %d logins from 5 Enrolls answered 401 on one credential, want 1", got)
	}
}

// TestA401DuringAnotherLoginPOSTReturnsWithinItsDeadline: an Enroll answered 401 on a credential
// another call already cleared, while that call's fresh login POST hangs, answers
// NO_MACHINE_CREDENTIAL within its 100 ms deadline instead of waiting behind that POST. This Enroll
// loses the CompareAndSwap, so it pins the CAS gate and retryLogin's TryLock together: it fails
// only with both gone. Each alone has a test of its own
// (TestOnlyTheCallThatClearedTheCredentialRetries, TestAnEnrollDuringALoginPOSTReturnsWithinItsDeadline).
func TestA401DuringAnotherLoginPOSTReturnsWithinItsDeadline(t *testing.T) {
	withFastPolling(t)
	withLoginRetry(t, 0, 0)
	broker, c, held := held401(t)
	hang := make(chan struct{})
	broker.hangLogins = hang
	down := enrollAnswer{http.StatusServiceUnavailable, `{"code":"UNAVAILABLE","error":"down"}`}
	broker.refuseLogins = []enrollAnswer{down, down}
	first := enrollAsync(context.Background(), c)
	releaseFirst := <-held
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	second := enrollAsync(ctx, c)
	releaseSecond := <-held
	t.Cleanup(func() {
		close(hang)
		<-first
	})
	close(releaseFirst)
	eventually(t, func() bool { return broker.loginAttemptCount() == 1 }, "the first 401's fresh login POST to reach the broker")
	close(releaseSecond)
	select {
	case err := <-second:
		var api *APIError
		if elapsed := time.Since(start); elapsed > time.Second || !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" {
			t.Fatalf("second 401'd Enroll = %v after %v, want NO_MACHINE_CREDENTIAL within its 100ms deadline", err, elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second 401'd Enroll had not returned after 2s, want NO_MACHINE_CREDENTIAL within its 100ms deadline")
	}
}

// TestALate401OnTheOldCredentialStartsNoLogin: an Enroll answered 401 on the old credential after
// the fresh login it led to was approved starts no login and leaves the new credential and the
// "issued" state alone.
func TestALate401OnTheOldCredentialStartsNoLogin(t *testing.T) {
	withFastPolling(t)
	withLoginRetry(t, 0, 0)
	broker, c, held := held401(t)
	first := enrollAsync(context.Background(), c)
	releaseFirst := <-held
	late := enrollAsync(context.Background(), c)
	releaseLate := <-held
	close(releaseFirst)
	<-first
	eventually(t, func() bool { return broker.loginCount() == 1 }, "the first 401's fresh login")
	broker.setPending(1, "issued", "cred-2")
	eventually(t, func() bool { return c.LoginStatus().State == "issued" }, "the fresh login to be issued")
	broker.mu.Lock()
	broker.refuseLogins = []enrollAnswer{{http.StatusTooManyRequests, `{"code":"RATE_LIMITED","error":"too many"}`}}
	broker.mu.Unlock()
	close(releaseLate)
	<-late
	if got := broker.loginAttemptCount(); got != 1 {
		t.Fatalf("broker saw %d logins, want only the first 401's: a late 401 on the old credential must start none", got)
	}
	if got, cred := c.LoginStatus(), c.cred.Load(); got.State != "issued" || cred == nil || cred.id != "cred-2" {
		t.Fatalf("after a late 401 on the old credential: state %+v, credential %v; want issued and cred-2 held", got, cred)
	}
}

// TestOnlyTheCallThatClearedTheCredentialRetries: of several Enrolls answered 401 on one
// credential, only the one whose 401 cleared it retries the login. The others, answered one at a
// time after each retry was refused and with no wait set, find the credential already gone and
// start none.
func TestOnlyTheCallThatClearedTheCredentialRetries(t *testing.T) {
	withFastPolling(t)
	withLoginRetry(t, 0, 0)
	broker, c, held := held401(t)
	down := enrollAnswer{http.StatusServiceUnavailable, `{"code":"UNAVAILABLE","error":"down"}`}
	broker.refuseLogins = []enrollAnswer{down, down, down}
	var done []<-chan error
	var releases []chan struct{}
	for range 3 {
		done = append(done, enrollAsync(context.Background(), c))
		releases = append(releases, <-held)
	}
	for i, release := range releases {
		close(release)
		if err := <-done[i]; err == nil {
			t.Fatal("an Enroll answered 401 succeeded")
		}
	}
	if got := broker.loginAttemptCount(); got != 1 {
		t.Fatalf("broker saw %d logins from 3 Enrolls answered 401 on one credential, one at a time, want 1: only the call that cleared it retries", got)
	}
}

// TestLoginWithALiveCredentialStartsNone: Login while the client holds a live credential starts
// no login, so the state never reads "failed" or "pending" beside a credential that works.
func TestLoginWithALiveCredentialStartsNone(t *testing.T) {
	withFastPolling(t)
	broker, c := newIssuedClient(t)
	broker.refuseLogins = []enrollAnswer{{http.StatusTooManyRequests, `{"code":"RATE_LIMITED","error":"too many"}`}}
	c.login.Store(&LoginState{State: "issued", Code: "CODE-0"})
	code, err := c.Login(context.Background())
	if err != nil || code != "CODE-0" {
		t.Fatalf("Login with a live credential = %q, %v; want the issued login's code CODE-0 and no error", code, err)
	}
	if got := broker.loginAttemptCount(); got != 0 {
		t.Fatalf("broker saw %d logins, want none while a credential is live", got)
	}
	if got := c.LoginStatus(); got.State != "issued" || c.cred.Load() == nil {
		t.Fatalf("after Login with a live credential: state %+v, credential held %v; want issued and held", got, c.cred.Load() != nil)
	}
}

// TestConcurrentLoginStartsExactlyOnePendingLogin pins Login's "idempotent while pending"
// invariant under real concurrency: many callers racing Login (as doProof's automatic
// re-login-on-401 can, from concurrent Enroll/Revoke calls sharing one Client) must observe
// exactly one pending login, not one each with a discarded loser's key and poll goroutine.
func TestConcurrentLoginStartsExactlyOnePendingLogin(t *testing.T) {
	withFastPolling(t)
	broker, server := newFakeBroker(t)
	c := &Client{URL: server.URL, HTTP: server.Client()}

	const n = 20
	codes := make([]string, n)
	errs := make([]error, n)
	var start, done sync.WaitGroup
	start.Add(1)
	done.Add(n)
	for i := range n {
		go func(i int) {
			defer done.Done()
			start.Wait()
			codes[i], errs[i] = c.Login(context.Background())
		}(i)
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	want := codes[0]
	for i, code := range codes {
		if code != want {
			t.Fatalf("caller %d got code %q, want %q: every concurrent Login must observe the same winning login", i, code, want)
		}
	}
	if got := broker.loginCount(); got != 1 {
		t.Fatalf("broker saw %d POST /v1/launcher-credentials, want exactly 1", got)
	}

	// Resolve the one winning login before returning: an unresolved pending login's poll
	// goroutine would otherwise keep running past this test (context.Background(), no terminal
	// state), racing a later test's withFastPolling override of the shared backoff variables --
	// the same leaked-goroutine hazard fixed in the contract test (TestExpiredCredentialTriggers
	// AFreshLoginWithANewKey follows the identical pattern).
	broker.setPending(1, "issued", "cred-concurrent")
	eventually(t, func() bool { return c.LoginStatus().State == "issued" }, "the winning login to become issued")
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
		status    int
		code      string
		permanent bool
	}{
		{http.StatusForbidden, "POD_IDENTITY_MISMATCH", true},
		{http.StatusConflict, "ALREADY_ENROLLED", true},
		{http.StatusTooManyRequests, "RATE_LIMITED", false},
		{http.StatusServiceUnavailable, "DATABASE", false},
	} {
		broker, c := newIssuedClient(t)
		broker.enrollAnswers = []enrollAnswer{{status: tc.status, body: `{"code":"` + tc.code + `","error":"why"}`}}
		_, err := c.Enroll(context.Background(), podEnrollment)
		var api *APIError
		if !errors.As(err, &api) {
			t.Fatalf("%d: err %v is not an *APIError", tc.status, err)
		}
		if api.Status != tc.status || api.Code != tc.code || api.Message != "why" || api.Permanent() != tc.permanent || IsPermanent(err) != tc.permanent {
			t.Fatalf("%d: %+v permanent=%t", tc.status, api, api.Permanent())
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

// TestEnrollWithNoCredentialNamesTheCode pins the fail-closed contract on its own, independent of
// the full login flow: with a login pending and no live credential, Enroll never touches the
// network and names the pending code.
func TestEnrollWithNoCredentialNamesTheCode(t *testing.T) {
	_, server := newFakeBroker(t)
	c := &Client{URL: server.URL, HTTP: server.Client()}
	c.login.Store(&LoginState{State: "pending", Code: "CODE-9"})
	_, err := c.Enroll(context.Background(), podEnrollment)
	var api *APIError
	if !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" || !strings.Contains(api.Message, "CODE-9") {
		t.Fatalf("err = %v, want NO_MACHINE_CREDENTIAL naming CODE-9", err)
	}
	if IsPermanent(err) {
		t.Fatalf("NO_MACHINE_CREDENTIAL must be non-permanent")
	}
}

// approveAndEnroll approves the login the broker numbered seq, waits for the client to hold its
// credential, and enrolls the pod under it.
func approveAndEnroll(t *testing.T, broker *fakeBroker, c *Client, seq int) {
	t.Helper()
	broker.setPending(seq, "issued", fmt.Sprintf("cred-%d", seq))
	eventually(t, func() bool { return c.LoginStatus().State == "issued" }, "the retried login to become issued")
	if _, err := c.Enroll(context.Background(), podEnrollment); err != nil {
		t.Fatalf("Enroll after the retried login was approved: %v", err)
	}
	if req := broker.lastEnrollRequest(); req.proof == "" {
		t.Fatalf("the enrollment carried no launcher proof")
	}
}

// TestABootLoginTheBrokerRefusedIsRetriedByTheNextEnrollment: a boot login the broker answers
// with a 500 (a broker from before #1868), a 429 (the shared service bucket empty), or no answer at
// all leaves the daemon with no credential and no login pending. The next Enroll starts a fresh
// login, which once approved enrolls the pod, with no restart.
func TestABootLoginTheBrokerRefusedIsRetriedByTheNextEnrollment(t *testing.T) {
	for name, refused := range map[string]enrollAnswer{
		"500 from a broker before #1868": {http.StatusInternalServerError, `{"code":"INTERNAL","error":"machine login failed"}`},
		"429 from the shared bucket":     {http.StatusTooManyRequests, `{"code":"RATE_LIMITED","error":"too many"}`},
		"a dropped connection":           {status: 0},
	} {
		t.Run(name, func(t *testing.T) {
			withFastPolling(t)
			withLoginRetry(t, 0, 0)
			broker, server := newFakeBroker(t)
			broker.refuseLogins = []enrollAnswer{refused}
			c := &Client{URL: server.URL, HTTP: server.Client()}
			if _, err := c.Login(context.Background()); err == nil {
				t.Fatal("boot Login succeeded, want the broker's refusal")
			}
			err := enrollUntil(t, c, func(error) bool { return broker.loginCount() == 1 }, "an Enroll to start a fresh login")
			var api *APIError
			if !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" || !strings.Contains(api.Message, "CODE-1") {
				t.Fatalf("Enroll = %v, want NO_MACHINE_CREDENTIAL naming the fresh login's code CODE-1", err)
			}
			approveAndEnroll(t, broker, c, 1)
		})
	}
}

// TestALoginThatExpiredOrWasDeniedIsRetriedByTheNextEnrollment: a login whose code expired
// undecided, or that a person denied, leaves no credential, and Enroll names that state rather than
// a pending login. The next Enroll starts a fresh login with a new code, which once approved
// enrolls the pod.
func TestALoginThatExpiredOrWasDeniedIsRetriedByTheNextEnrollment(t *testing.T) {
	for _, ended := range []string{"expired", "denied"} {
		t.Run(ended, func(t *testing.T) {
			withFastPolling(t)
			withLoginRetry(t, 0, 0)
			broker, server := newFakeBroker(t)
			c := &Client{URL: server.URL, HTTP: server.Client()}
			if _, err := c.Login(context.Background()); err != nil {
				t.Fatal(err)
			}
			broker.setPending(1, ended, "")
			eventually(t, func() bool { return c.LoginStatus().State == ended }, "the boot login to end "+ended)
			err := enrollUntil(t, c, func(error) bool { return broker.loginCount() == 2 }, "an Enroll to start a fresh login")
			var api *APIError
			if !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" || !strings.Contains(api.Message, "CODE-2") {
				t.Fatalf("Enroll = %v, want NO_MACHINE_CREDENTIAL naming the fresh login's code CODE-2", err)
			}
			approveAndEnroll(t, broker, c, 2)
		})
	}
}

// TestNoCredentialErrorNamesTheLoginsState: with no credential, the refusal names the login's
// actual state: pending with its code, expired or denied with its code, a login the broker never
// opened with why, and no login yet. Each stays non-permanent, so the supervisor keeps retrying.
func TestNoCredentialErrorNamesTheLoginsState(t *testing.T) {
	for _, tc := range []struct {
		state LoginState
		want  string
	}{
		{LoginState{State: "pending", Code: "CODE-9"}, "machine login pending; code CODE-9"},
		{LoginState{State: "expired", Code: "CODE-9"}, "machine login expired; code CODE-9"},
		{LoginState{State: "denied", Code: "CODE-9"}, "machine login denied; code CODE-9"},
		{LoginState{State: "failed", reason: "broker answered 429 RATE_LIMITED: too many"}, "machine login failed: broker answered 429 RATE_LIMITED: too many"},
		{LoginState{State: "none"}, "no machine login yet"},
	} {
		c := &Client{URL: "http://127.0.0.1:1"}
		c.login.Store(&tc.state)
		err := c.noCredentialError()
		var api *APIError
		if !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" || api.Message != tc.want || IsPermanent(err) {
			t.Errorf("state %+v: noCredentialError = %v, want non-permanent NO_MACHINE_CREDENTIAL %q", tc.state, err, tc.want)
		}
	}
}

// TestALoginTheBrokerNeverOpenedReadsFailedUntilTheNextOne: a login the broker refused reads
// "failed" on LoginStatus, never "none", and the no-credential refusal names why. The next login
// the broker opens replaces it, so neither that login's pending code nor how it ends reads as the
// earlier refusal.
func TestALoginTheBrokerNeverOpenedReadsFailedUntilTheNextOne(t *testing.T) {
	withFastPolling(t)
	broker, server := newFakeBroker(t)
	broker.refuseLogins = []enrollAnswer{{http.StatusTooManyRequests, `{"code":"RATE_LIMITED","error":"too many"}`}}
	c := &Client{URL: server.URL, HTTP: server.Client()}
	if _, err := c.Login(context.Background()); err == nil {
		t.Fatal("Login succeeded, want the broker's 429")
	}
	if got := c.LoginStatus(); got.State != "failed" || got.Code != "" {
		t.Fatalf("LoginStatus after a refused login = %+v, want failed with no code", got)
	}
	wantNoCredential(t, c, "machine login failed: broker answered 429 RATE_LIMITED: too many")

	code, err := c.Login(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.LoginStatus(); got.State != "pending" || got.Code != code {
		t.Fatalf("LoginStatus after the next login opened = %+v, want pending with code %q", got, code)
	}
	wantNoCredential(t, c, "machine login pending; code "+code)
	broker.setPending(1, "denied", "")
	eventually(t, func() bool { return c.LoginStatus().State == "denied" }, "the login to end denied")
	wantNoCredential(t, c, "machine login denied; code "+code)
}

// wantNoCredential checks the refusal Enroll and Revoke return with no credential: non-permanent
// NO_MACHINE_CREDENTIAL, its message exactly want.
func wantNoCredential(t *testing.T, c *Client, want string) {
	t.Helper()
	err := c.noCredentialError()
	var api *APIError
	if !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" || api.Message != want || IsPermanent(err) {
		t.Fatalf("noCredentialError = %v, want non-permanent NO_MACHINE_CREDENTIAL %q", err, want)
	}
}

// TestEnrollmentsDuringAPendingLoginStartNoSecondLogin: while a login is pending, every Enroll,
// however many and however concurrent, answers NO_MACHINE_CREDENTIAL without sending another
// login, so the broker never sees a second code nobody asked for.
func TestEnrollmentsDuringAPendingLoginStartNoSecondLogin(t *testing.T) {
	withFastPolling(t)
	withLoginRetry(t, 0, 0)
	broker, server := newFakeBroker(t)
	c := &Client{URL: server.URL, HTTP: server.Client()}
	if _, err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	var done sync.WaitGroup
	for range 20 {
		done.Add(1)
		go func() {
			defer done.Done()
			for range 10 {
				_, _ = c.Enroll(context.Background(), podEnrollment)
			}
		}()
	}
	done.Wait()
	if got := broker.loginAttemptCount(); got != 1 {
		t.Fatalf("broker saw %d logins during one pending login, want exactly 1", got)
	}
	broker.setPending(1, "issued", "cred-1")
	eventually(t, func() bool { return c.LoginStatus().State == "issued" }, "the login to become issued")
}

// TestALoginWhoseCallerEndsMidPOSTStillCollectsItsCode: an Enroll's retry login runs past that
// Enroll's own deadline. The broker commits the code, the caller's context ends before the answer
// arrives, and the login still collects that code, records it pending and, once approved, holds
// its credential. Exactly one code results: the next enrollment starts no second one.
func TestALoginWhoseCallerEndsMidPOSTStillCollectsItsCode(t *testing.T) {
	withFastPolling(t)
	withLoginRetry(t, 0, 0)
	broker, server := newFakeBroker(t)
	answer := make(chan struct{})
	broker.holdAnswers = answer
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(answer) }) })
	c := &Client{URL: server.URL, HTTP: server.Client()}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	enrolled := make(chan error, 1)
	go func() { enrolled <- enrollNoCredential(ctx, c) }()
	select {
	case err := <-enrolled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Enroll had not returned 2s after its 100ms deadline: it waited for the login's answer")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Enroll took %v, want it to answer within its 100ms deadline", elapsed)
	}
	eventually(t, func() bool { return broker.loginCount() == 1 }, "the broker to commit the retry's code")
	<-ctx.Done()
	release.Do(func() { close(answer) })
	eventually(t, func() bool { return c.LoginStatus().State == "pending" }, "the login to collect its code after its caller's deadline")
	if got := c.LoginStatus(); got.Code != "CODE-1" {
		t.Fatalf("LoginStatus = %+v, want pending with the committed code CODE-1", got)
	}
	if err := enrollNoCredential(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := broker.loginAttemptCount(); got != 1 {
		t.Fatalf("broker saw %d logins, want exactly one code for the one retry", got)
	}
	broker.setPending(1, "issued", "cred-1")
	eventually(t, func() bool { return c.LoginStatus().State == "issued" && c.cred.Load() != nil }, "the committed code, once approved, to give the client its credential")
}

// enrollNoCredential enrolls the pod and answers an error unless the call is refused with
// NO_MACHINE_CREDENTIAL.
func enrollNoCredential(ctx context.Context, c *Client) error {
	_, err := c.Enroll(ctx, podEnrollment)
	var api *APIError
	if !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" {
		return fmt.Errorf("Enroll = %v, want NO_MACHINE_CREDENTIAL", err)
	}
	return nil
}

// TestAnEnrollDuringALoginPOSTReturnsWithinItsDeadline: while a login's POST hangs (the boot
// login, on a background context), an Enroll with no credential answers NO_MACHINE_CREDENTIAL
// within its own deadline instead of waiting behind that POST, since its caller holds the claim's
// machine lock for the whole call.
func TestAnEnrollDuringALoginPOSTReturnsWithinItsDeadline(t *testing.T) {
	withFastPolling(t)
	withLoginRetry(t, 0, 0)
	broker, server := newFakeBroker(t)
	hang := make(chan struct{})
	broker.hangLogins = hang
	// Released at cleanup, the hung login is refused, so no poll goroutine outlives the test.
	broker.refuseLogins = []enrollAnswer{{http.StatusServiceUnavailable, `{"code":"UNAVAILABLE","error":"down"}`}}
	c := &Client{URL: server.URL, HTTP: server.Client()}
	booted := make(chan struct{})
	go func() {
		defer close(booted)
		_, _ = c.Login(context.Background())
	}()
	t.Cleanup(func() {
		close(hang)
		<-booted
	})
	eventually(t, func() bool { return broker.loginAttemptCount() == 1 }, "the boot login's POST to reach the broker")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	enrolled := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := c.Enroll(ctx, podEnrollment)
		enrolled <- err
	}()
	select {
	case err := <-enrolled:
		var api *APIError
		if elapsed := time.Since(start); elapsed > time.Second || !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" {
			t.Fatalf("Enroll during a hung login POST = %v after %v, want NO_MACHINE_CREDENTIAL within its 100ms deadline", err, elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Enroll during a hung login POST had not returned after 2s, want NO_MACHINE_CREDENTIAL within its 100ms deadline")
	}
	if got := broker.loginAttemptCount(); got != 1 {
		t.Fatalf("broker saw %d logins, want only the hung boot login", got)
	}
}

// TestALoginThatFailedWaitsBeforeTheNext: after a login ends without a credential, enrollments
// inside the wait start no login, so a supervisor observing every few seconds does not hammer the
// broker; the wait doubles after each failed login, up to its cap.
func TestALoginThatFailedWaitsBeforeTheNext(t *testing.T) {
	withFastPolling(t)
	withLoginRetry(t, time.Hour, 4*time.Hour)
	broker, server := newFakeBroker(t)
	broker.refuseLogins = []enrollAnswer{{http.StatusTooManyRequests, `{"code":"RATE_LIMITED","error":"too many"}`}}
	c := &Client{URL: server.URL, HTTP: server.Client()}
	if _, err := c.Login(context.Background()); err == nil {
		t.Fatal("boot Login succeeded, want the broker's 429")
	}
	for range 50 {
		_, err := c.Enroll(context.Background(), podEnrollment)
		var api *APIError
		if !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" {
			t.Fatalf("Enroll inside the wait = %v, want NO_MACHINE_CREDENTIAL", err)
		}
	}
	if got := broker.loginAttemptCount(); got != 1 {
		t.Fatalf("broker saw %d logins, want only the boot login: an Enroll inside the wait must start none", got)
	}
	for failed, want := range map[int]time.Duration{1: time.Hour, 2: 2 * time.Hour, 3: 4 * time.Hour, 4: 4 * time.Hour, 40: 4 * time.Hour} {
		if got := loginRetryWait(failed); got != want {
			t.Errorf("loginRetryWait(%d failed logins) = %v, want %v", failed, got, want)
		}
	}
}

// TestADeniedOrExpiredCodeWaitsBeforeTheNextLogin: a code a person denied, or one that expired
// undecided, starts the wait as a refused login does, so enrollments inside it start no login and
// the broker never sees a fresh code on every observation.
func TestADeniedOrExpiredCodeWaitsBeforeTheNextLogin(t *testing.T) {
	for _, ended := range []string{"denied", "expired"} {
		t.Run(ended, func(t *testing.T) {
			withFastPolling(t)
			withLoginRetry(t, time.Hour, 4*time.Hour)
			broker, server := newFakeBroker(t)
			c := &Client{URL: server.URL, HTTP: server.Client()}
			if _, err := c.Login(context.Background()); err != nil {
				t.Fatal(err)
			}
			broker.setPending(1, ended, "")
			eventually(t, func() bool { return c.LoginStatus().State == ended }, "the boot login to end "+ended)
			for range 50 {
				wantEnrollNoCredential(t, c)
			}
			if got := broker.loginAttemptCount(); got != 1 {
				t.Fatalf("broker saw %d logins after the code was %s, want only the boot login: an Enroll inside the wait must start none", got, ended)
			}
		})
	}
}

// TestAnApprovedLoginResetsTheWait: a refused login, then an approved one, then a refused one
// waits loginRetryInitial again, not the doubled wait of a second failure in a row.
func TestAnApprovedLoginResetsTheWait(t *testing.T) {
	withFastPolling(t)
	withLoginRetry(t, time.Hour, 4*time.Hour)
	refused := enrollAnswer{http.StatusTooManyRequests, `{"code":"RATE_LIMITED","error":"too many"}`}
	broker, server := newFakeBroker(t)
	broker.refuseLogins = []enrollAnswer{refused}
	c := &Client{URL: server.URL, HTTP: server.Client()}
	if _, err := c.Login(context.Background()); err == nil {
		t.Fatal("boot Login succeeded, want the broker's 429")
	}
	if _, err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	broker.setPending(1, "issued", "cred-1")
	eventually(t, func() bool { return c.LoginStatus().State == "issued" }, "the second login to become issued")

	// The broker refuses the credential, and the fresh login doProof starts at once is refused too.
	broker.mu.Lock()
	broker.enrollAnswers = []enrollAnswer{{status: http.StatusUnauthorized, body: `{"code":"LAUNCHER_INVALID","error":"expired"}`}}
	broker.refuseLogins = []enrollAnswer{refused}
	broker.mu.Unlock()
	if _, err := c.Enroll(context.Background(), podEnrollment); err == nil {
		t.Fatal("Enroll succeeded, want the refused credential's fresh login to fail")
	}
	if got := c.LoginStatus(); got.State != "failed" {
		t.Fatalf("LoginStatus = %+v, want failed", got)
	}
	c.loginMu.Lock()
	failed, wait := c.failedLogins, time.Until(c.nextLogin)
	c.loginMu.Unlock()
	if failed != 1 || wait <= loginRetryInitial-time.Minute || wait > loginRetryInitial {
		t.Fatalf("after failure, approval, failure: %d failed logins, next login in %v; want 1 and about %v", failed, wait, loginRetryInitial)
	}
}

// wantEnrollNoCredential enrolls the pod and requires the no-credential refusal.
func wantEnrollNoCredential(t *testing.T, c *Client) {
	t.Helper()
	_, err := c.Enroll(context.Background(), podEnrollment)
	var api *APIError
	if !errors.As(err, &api) || api.Code != "NO_MACHINE_CREDENTIAL" {
		t.Fatalf("Enroll = %v, want NO_MACHINE_CREDENTIAL", err)
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
