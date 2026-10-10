// Package agentsecrets is the daemon's side of the secrets broker's machine-login and enrollment
// routes (broker API v9): it holds a key in process memory, logs the machine in
// through the typed-code flow (POST /v1/launcher-credentials, polled until a human approves it),
// and enrolls a pod's key under the resulting launcher credential — authenticating every call
// with a launcher proof signed by the key, never a bearer token. Revoke tears the enrollment down
// when the pod is gone. No key, no launcher credential id, and no grant or brokered secret value
// ever goes into an error, a log line, or the daemon's state; the pod's own `agent-secrets`
// client handles brokered secret values with the pod's own key.
package agentsecrets

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// launcherService is the authorization_details "service" this daemon's machine logins always
// request: a Legion daemon's launcher credential is a service credential, never an operator's own
// machine credential.
const launcherService = "legion-daemon"

// pollInitialInterval and pollMaxInterval bound the machine-login poll goroutine's backoff:
// it starts at pollInitialInterval and doubles up to pollMaxInterval. Package-level so tests can
// shrink them instead of waiting out the real interval.
var (
	pollInitialInterval = 2 * time.Second
	pollMaxInterval     = 10 * time.Second
)

// loginRetryInitial and loginRetryMax bound the wait between machine logins that end without a
// credential (refused, unreachable, expired undecided, or denied): the first wait is
// loginRetryInitial, each one after it doubles, up to loginRetryMax, and an issued login resets it.
// The broker refills the one bucket every service's login shares at one login a minute
// (DefaultLauncherLimits in packages/envoy/internal/broker/api/limits.go), so a daemon retrying at
// its cap spends a fifth of that and cannot keep the bucket empty for other services. Package-level
// so tests can shrink them.
var (
	loginRetryInitial = 30 * time.Second
	loginRetryMax     = 5 * time.Minute
)

// Client speaks the broker's machine-login and enrollment routes. URL is the broker's base URL
// with no path (also the request object's audience); HTTP is the client every call
// goes through, nil for http.DefaultClient. cred is the launcher credential a login has won, if
// any; login is the most recent login's state, a login the broker never opened included
// ("failed"); failedLogins and nextLogin pace the next attempt after a login that ended without a
// credential (retryLogin). cred and login are set only by Login, its poll goroutine and settle,
// and cred cleared only when the broker refuses it as invalid. loginMu serializes Login's own
// check-then-start sequence and every login's outcome (settle): atomic.Pointer alone lets two
// concurrent callers both observe "not pending" and both mint a key and POST, silently discarding
// one credential's poll goroutine — loginMu makes "start at most one pending login" atomic, so
// every concurrent caller observes the same winner.
type Client struct {
	URL  string
	HTTP *http.Client

	cred         atomic.Pointer[credential]
	login        atomic.Pointer[LoginState]
	loginMu      sync.Mutex
	failedLogins int       // guarded by loginMu
	nextLogin    time.Time // guarded by loginMu: no retryLogin starts a login before it
}

// credential is a won launcher credential: the key it is bound to (which never leaves process
// memory) and the id the broker minted for it.
type credential struct {
	key *ecdsa.PrivateKey
	id  string
}

// LoginState is a machine login's current status as the daemon knows it. State is one of
// "none" (no login has ever been started), "pending", "issued", "denied", "expired", or "failed"
// (the broker never opened the login: it refused it, or did not answer). Code is the login's
// confirmation code, "" for "none" and "failed". reason is why a "failed" login failed, for
// noCredentialError; it is unexported so it never reaches GET /legion/v1/state.
type LoginState struct {
	State  string
	Code   string
	reason string
}

// PodEnrollment is one pod generation's identity as the daemon knows it: the pod UID the runtime
// recorded at spawn, the thumbprint and projected token the shim's hello carried, and the
// agent's session id — "" before the agent registered, which the broker records as null. The
// broker picks a request's approver at request time; the enrollment carries no issue.
// Slot names one of several pod enrollments when an issue pod holds role processes side by side
// (`<role>-g<generation>`); empty is the pod's one identity.
type PodEnrollment struct {
	PodUID, Slot, Thumbprint, PodToken, Session string
}

// Enrollment is what the broker answered: the id every later call names, and the lease the pod's
// renewer keeps alive.
type Enrollment struct {
	ID           string
	LeaseExpires time.Time
}

// APIError is the broker's refusal: its status and the contract's `code`, which callers branch
// on. Status 0 (never a broker status) marks a refusal manufactured client-side: this client has
// no live launcher credential to authenticate with.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("broker answered %d %s: %s", e.Status, e.Code, e.Message)
}

// Permanent is a refusal a retry cannot change: every 4xx but 429. A client-side NO_MACHINE_CREDENTIAL
// (Status 0) is never permanent — the caller retries once the pending login is approved.
func (e *APIError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests
}

// IsPermanent reports whether err is a permanent broker refusal. A transport failure, a 5xx, a
// 429, and a pending machine login are all transient.
func IsPermanent(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Permanent()
}

// Login mints a fresh key, signs a machine-login request object naming os.Hostname() and
// "legion-daemon" as the launcher_credential it asks to hold (a service's login, which anyone
// signed in to Dispatch approves, so it names no approver), POSTs it to /v1/launcher-credentials, spawns the poll
// goroutine, and returns the confirmation code. Idempotent while a login is pending: a second
// call returns the same code without starting another one — loginMu holds this true even under
// real concurrency, so callers racing Login (doProof's automatic re-login, from concurrent
// Enroll/Revoke calls sharing one Client) never start more than one pending login. A login the
// broker does not open (a refusal, or no answer) settles as "failed", and the next retryLogin
// waits for it.
func (c *Client) Login(ctx context.Context) (string, error) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.loginLocked(ctx)
}

// loginLocked is Login with loginMu held. Its pending check is the one guard against a second
// login while one is pending, for Login and retryLogin alike.
func (c *Client) loginLocked(ctx context.Context) (string, error) {
	if state := c.LoginStatus(); state.State == "pending" {
		return state.Code, nil
	}
	code, err := c.open(ctx)
	if err != nil {
		c.settle(LoginState{State: "failed", reason: err.Error()}, nil)
		return "", err
	}
	return code, nil
}

// settle records how a login ended, with loginMu held: its state, the credential an issued login
// won, and the wait before the next login (retryLogin). An issued login resets the wait; every
// other outcome (a refusal, no answer, a denied code, an expired one) lengthens it.
func (c *Client) settle(state LoginState, cred *credential) {
	if state.State == "issued" {
		c.cred.Store(cred)
		c.failedLogins, c.nextLogin = 0, time.Time{}
	} else {
		c.failedLogins++
		c.nextLogin = time.Now().Add(loginRetryWait(c.failedLogins))
	}
	c.login.Store(&state)
}

// open sends one machine login and, once the broker opens it, records it pending and starts its
// poll goroutine.
func (c *Client) open(ctx context.Context) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("agent-secrets login: generate key: %w", err)
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("agent-secrets login: hostname: %w", err)
	}
	request, err := signRequestObject(key, c.URL, host, time.Now())
	if err != nil {
		return "", fmt.Errorf("agent-secrets login: sign request object: %w", err)
	}
	status, raw, err := c.do(ctx, http.MethodPost, "/v1/launcher-credentials", map[string]string{"request": request}, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusAccepted {
		return "", refusal(status, raw)
	}
	var answer struct {
		PendingID string `json:"pending_id"`
		Code      string `json:"code"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", fmt.Errorf("broker answered %d with a body that is not the contract's shape: %w", status, err)
	}
	c.login.Store(&LoginState{State: "pending", Code: answer.Code})
	// The poll goroutine outlives this call by design (see the package doc): it keeps running,
	// backed off, until the login resolves, however long that takes. ctx here may be the RPC-
	// scoped context of the Enroll/Revoke call whose 401 triggered this automatic re-login
	// (doProof), which the caller cancels the instant that call returns -- context.WithoutCancel
	// keeps whatever values ctx carries but detaches the poll goroutine from that cancellation, so
	// a short-lived RPC context can no longer kill the re-login before it ever polls once.
	go c.poll(context.WithoutCancel(ctx), key, answer.PendingID, answer.Code)
	return answer.Code, nil
}

// LoginStatus reports the most recent login's status: {"none", ""} when Login has never been
// called.
func (c *Client) LoginStatus() LoginState {
	if s := c.login.Load(); s != nil {
		return *s
	}
	return LoginState{State: "none"}
}

// poll reads the pending login until it reaches a terminal state (issued, denied, or expired),
// backing off from pollInitialInterval to pollMaxInterval between reads, and settles it: on
// "issued" with the credential key won by Login alongside the broker's credential_id.
func (c *Client) poll(ctx context.Context, key *ecdsa.PrivateKey, pendingID, code string) {
	wait := pollInitialInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		state, credentialID, err := c.readLauncherCredential(ctx, pendingID)
		if err != nil || state == "pending" {
			if wait *= 2; wait > pollMaxInterval {
				wait = pollMaxInterval
			}
			continue
		}
		c.loginMu.Lock()
		c.settle(LoginState{State: state, Code: code}, &credential{key: key, id: credentialID})
		c.loginMu.Unlock()
		return
	}
}

func (c *Client) readLauncherCredential(ctx context.Context, pendingID string) (state, credentialID string, err error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/v1/launcher-credentials/"+url.PathEscape(pendingID), nil, nil)
	if err != nil {
		return "", "", err
	}
	if status != http.StatusOK {
		return "", "", refusal(status, raw)
	}
	var answer struct {
		State        string `json:"state"`
		CredentialID string `json:"credential_id"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", "", fmt.Errorf("broker answered %d with a body that is not the contract's shape: %w", status, err)
	}
	return answer.State, answer.CredentialID, nil
}

type enrollmentBody struct {
	Kind       string  `json:"kind"`
	RuntimeID  string  `json:"runtime_id"`
	Slot       string  `json:"slot,omitempty"`
	Operator   *string `json:"operator"`
	Thumbprint string  `json:"thumbprint"`
	SessionID  *string `json:"session_id"`
	PodToken   string  `json:"pod_token"`
}

type enrollmentAnswer struct {
	EnrollmentID   string    `json:"enrollment_id"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// Enroll is POST /v1/enrollments for a pod, authenticated with a launcher proof. A 201 is a fresh
// enrollment and a 200 the live one with the same thumbprint (the idempotent re-enrollment the
// contract adopted); both are the enrollment to record. With no live launcher credential — never
// obtained, or just invalidated — it returns a non-permanent *APIError{Code: "NO_MACHINE_CREDENTIAL"}
// naming the pending login's code, so the caller retries once a human approves it. Any other
// broker answer is an *APIError, a transport failure a plain error.
func (c *Client) Enroll(ctx context.Context, e PodEnrollment) (Enrollment, error) {
	body := enrollmentBody{
		Kind: "pod", RuntimeID: e.PodUID, Slot: e.Slot, Operator: nil,
		Thumbprint: e.Thumbprint, PodToken: e.PodToken,
	}
	if e.Session != "" {
		session := e.Session
		body.SessionID = &session
	}
	status, raw, err := c.doProof(ctx, http.MethodPost, "/v1/enrollments", body)
	if err != nil {
		return Enrollment{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return Enrollment{}, refusal(status, raw)
	}
	var answer enrollmentAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return Enrollment{}, fmt.Errorf("broker answered %d with a body that is not the contract's shape: %w", status, err)
	}
	if answer.EnrollmentID == "" {
		return Enrollment{}, fmt.Errorf("broker answered %d with no enrollment_id", status)
	}
	return Enrollment{ID: answer.EnrollmentID, LeaseExpires: answer.LeaseExpiresAt}, nil
}

// Revoke is DELETE /v1/enrollments/{id}, authenticated with a launcher proof: 204 and 404 are
// both "revoked" (the route is idempotent).
func (c *Client) Revoke(ctx context.Context, id string) error {
	status, raw, err := c.doProof(ctx, http.MethodDelete, "/v1/enrollments/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return refusal(status, raw)
}

// doProof signs a launcher proof over this call with the held credential's key and sends it as
// the Proof header — never Authorization, which no route on this client uses any more. With no
// credential it starts a fresh login when none is pending and the retry wait has passed
// (retryLogin), and answers NO_MACHINE_CREDENTIAL naming the login's state. A 401
// (LAUNCHER_INVALID) clears the credential that failed and starts a fresh login at once before
// answering the same way.
func (c *Client) doProof(ctx context.Context, method, path string, body any) (int, []byte, error) {
	cred := c.cred.Load()
	if cred == nil {
		c.retryLogin(ctx)
		return 0, nil, c.noCredentialError()
	}
	proof, err := signLauncherProof(cred.key, cred.id, method, c.URL+path, time.Now())
	if err != nil {
		return 0, nil, err
	}
	status, raw, err := c.do(ctx, method, path, body, func(req *http.Request) {
		req.Header.Set("Proof", proof)
	})
	if err != nil {
		return 0, nil, err
	}
	if status == http.StatusUnauthorized {
		c.cred.CompareAndSwap(cred, nil)
		if _, loginErr := c.Login(ctx); loginErr != nil {
			return 0, nil, fmt.Errorf("agent-secrets: launcher credential invalid, and a fresh login failed: %w", loginErr)
		}
		return 0, nil, c.noCredentialError()
	}
	return status, raw, nil
}

// retryLogin starts a fresh machine login when the client holds no credential, no login is
// pending, and the wait after the last login that ended without a credential has passed: the
// supervisor's next observation of a pod then retries the login the daemon's boot started, with
// no restart. settle records how the new login ends, and noCredentialError reports it. A held
// loginMu means a login is already opening or settling, so retryLogin returns at once rather than
// hold its caller (an Enroll or Revoke, under its claim's machine lock) behind that login's POST.
func (c *Client) retryLogin(ctx context.Context) {
	if !c.loginMu.TryLock() {
		return
	}
	defer c.loginMu.Unlock()
	if c.cred.Load() != nil || c.LoginStatus().State == "pending" || time.Now().Before(c.nextLogin) {
		return
	}
	_, _ = c.loginLocked(ctx)
}

// loginRetryWait is the wait after failed consecutive logins that ended without a credential:
// loginRetryInitial, doubled for each one after the first, never more than loginRetryMax.
func loginRetryWait(failed int) time.Duration {
	wait := loginRetryInitial
	for i := 1; i < failed && wait < loginRetryMax; i++ {
		wait *= 2
	}
	return min(wait, loginRetryMax)
}

// noCredentialError is the fail-closed refusal Enroll/Revoke return with no live launcher
// credential: non-permanent, naming the login's state — pending, expired or denied with its code,
// why the broker never opened the last login, or that none has started — so the caller knows what
// to tell a human, never the key or the credential id.
func (c *Client) noCredentialError() error {
	state := c.LoginStatus()
	message := "machine login " + state.State + "; code " + state.Code
	switch state.State {
	case "failed":
		message = "machine login failed: " + state.reason
	case "none":
		message = "no machine login yet"
	}
	return &APIError{Code: "NO_MACHINE_CREDENTIAL", Message: message}
}

func (c *Client) do(ctx context.Context, method, path string, body any, mutate func(*http.Request)) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, payload)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(req)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("broker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return 0, nil, fmt.Errorf("broker %s %s: read the answer: %w", method, path, err)
	}
	return resp.StatusCode, raw, nil
}

// refusal is the broker's error body as an *APIError; a body that is not the contract's shape
// still yields one, with the status and the raw text as its message.
func refusal(status int, raw []byte) error {
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Code == "" {
		return &APIError{Status: status, Code: "UNKNOWN", Message: string(raw)}
	}
	return &APIError{Status: status, Code: body.Code, Message: body.Error}
}
