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

// fakeBroker is the launcher-credential and enrollment routes of the shared broker contract
// (dispatch://AGENTC-393/artifact/plan-overview-md) as a fake: it captures every login request
// object's claims and every enrollment-route request's headers and body, and answers exactly
// what the test configures. It never honors an Authorization header — the whole point of this
// task is that one is never sent any more.
type fakeBroker struct {
	t *testing.T

	mu       sync.Mutex
	logins   []loginClaims           // one entry per POST /v1/launcher-credentials, in order
	pending  map[string]pendingState // pendingID -> current answer
	loginSeq int

	// enrollment-route behavior: answered in order, the last entry repeating once exhausted.
	enrollAnswers []enrollAnswer
	enrollSeq     int
	enrollReqs    []enrollRequest
}

type loginClaims struct {
	iss       string
	loginHint string
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
	b.loginSeq++
	pendingID := fmt.Sprintf("pending-%d", b.loginSeq)
	code := fmt.Sprintf("CODE-%d", b.loginSeq)
	b.logins = append(b.logins, loginClaims{
		iss:       fmt.Sprint(payload["iss"]),
		loginHint: fmt.Sprint(payload["login_hint"]),
		host:      fmt.Sprint(detail["identifier"]),
		service:   fmt.Sprint(detail["service"]),
	})
	b.pending[pendingID] = pendingState{state: "pending"}
	b.mu.Unlock()

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
	c := &Client{URL: server.URL, Operator: "sjawhar", HTTP: server.Client()}
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
	c := &Client{URL: server.URL, Operator: "sjawhar", HTTP: server.Client()}

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
	if login.loginHint != "sjawhar" || login.service != "legion-daemon" || login.host != wantHost {
		t.Fatalf("login request = %+v, want host %q", login, wantHost)
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
	c := &Client{URL: server.URL, Operator: "sjawhar", HTTP: server.Client()}

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

// TestConcurrentLoginStartsExactlyOnePendingLogin pins Login's "idempotent while pending"
// invariant under real concurrency: many callers racing Login (as doProof's automatic
// re-login-on-401 can, from concurrent Enroll/Revoke calls sharing one Client) must observe
// exactly one pending login, not one each with a discarded loser's key and poll goroutine.
func TestConcurrentLoginStartsExactlyOnePendingLogin(t *testing.T) {
	withFastPolling(t)
	broker, server := newFakeBroker(t)
	c := &Client{URL: server.URL, Operator: "sjawhar", HTTP: server.Client()}

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
	c := &Client{URL: "http://127.0.0.1:1", Operator: "sjawhar", HTTP: &http.Client{Timeout: time.Second}}
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
	c := &Client{URL: server.URL, Operator: "sjawhar", HTTP: server.Client()}
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

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
