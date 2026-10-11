// Package agentsecrets is the daemon's side of the secrets broker's token-login and enrollment
// routes: it logs the daemon in with its own projected Kubernetes ServiceAccount token
// (POST /v1/launcher-credentials/token), which no person approves, holding the key the launcher
// credential is bound to in process memory only, logs in again before that credential expires,
// and enrolls a pod's key under it — authenticating every call with a launcher proof signed by the
// key, never a bearer token. Revoke tears the enrollment down when the pod is gone. No key, no
// token, no launcher credential id, and no grant or brokered secret value ever goes into an error,
// a log line, or the daemon's state; the pod's own `agent-secrets` client handles brokered secret
// values with the pod's own key.
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
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// launcherService is the authorization_details "service" this daemon's token logins always
// request: a Legion daemon's launcher credential is a service credential, never an operator's own
// machine credential.
const launcherService = "legion-daemon"

// loginRetryInitial and loginRetryMax bound the wait after a token login that failed (the broker
// refused it or did not answer, or the token file could not be read): the first wait is
// loginRetryInitial, each one after it doubles, up to loginRetryMax, and a login that succeeds
// resets it. No person sees a failed login, so nothing is asked of anyone while it retries.
// Package-level so tests can shrink them.
var (
	loginRetryInitial = 30 * time.Second
	loginRetryMax     = 5 * time.Minute
)

// Client speaks the broker's token-login and enrollment routes. URL is the broker's base URL with
// no path (also the request object's audience); HTTP is the client every call goes through, nil
// for http.DefaultClient; TokenFile is the path of the daemon's projected ServiceAccount token,
// read at every login so the kubelet's rotation is always picked up; Log is where the login loop
// reports, nil for slog's default.
//
// cred is the launcher credential the latest login won, cleared when the broker refuses it as
// invalid; login is the latest login's outcome. loginMu serializes logins. proofMu is held shared
// by every call a launcher proof signs (doProof) and exclusively by a login, which moves the
// broker's credential to a new key: no proof signed with the old key is in flight when the broker
// switches, so none comes back refused and clears a credential that was only renewed.
// failedLogins and nextLogin pace the next login after a failed one. changed wakes the login loop
// (Start) when a call finds no credential or the broker refuses it.
type Client struct {
	URL       string
	HTTP      *http.Client
	TokenFile string
	Log       *slog.Logger

	cred         atomic.Pointer[credential]
	login        atomic.Pointer[LoginState]
	loginMu      sync.Mutex
	proofMu      sync.RWMutex
	failedLogins int       // guarded by loginMu
	nextLogin    time.Time // guarded by loginMu: the login loop starts no login before it
	changed      chan struct{}
	changedOnce  sync.Once
}

// credential is a won launcher credential: the key it is bound to (which never leaves process
// memory), the id the broker answered, and when it expires.
type credential struct {
	key     *ecdsa.PrivateKey
	id      string
	expires time.Time
}

// LoginState is the daemon's token login as it knows it. State is one of "none" (no login has
// finished yet), "issued", or "failed" (the latest login did not win a credential). reason is why a
// "failed" login failed, for noCredentialError and the log; it is unexported so it never reaches
// GET /legion/v1/state.
type LoginState struct {
	State  string
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

// Permanent is a refusal a retry cannot change: every 4xx but 429 and POD_TOKEN_EXPIRED. A
// client-side NO_MACHINE_CREDENTIAL (Status 0) is never permanent — the caller retries once the
// daemon's next token login wins a credential — and neither is an expired pod token, which the
// pod's current token replaces under the same key (TokenExpired).
func (e *APIError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests && !e.TokenExpired()
}

// TokenExpired is the broker refusing the pod token presented because it has expired
// (POD_TOKEN_EXPIRED): the key stands, and the pod's current token enrolls it.
func (e *APIError) TokenExpired() bool { return e.Code == "POD_TOKEN_EXPIRED" }

// IsPermanent reports whether err is a permanent broker refusal. A transport failure, a 5xx, a
// 429, an expired pod token and a daemon with no credential yet are all transient.
func IsPermanent(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Permanent()
}

// Start logs the daemon in with its token now and keeps it logged in until ctx ends: it logs in
// again halfway through each credential's life, at once when a call finds no credential or the
// broker refuses it, and, after a login that failed, once loginRetryWait has passed. It returns
// after the first login, with that login's error. No step of it asks a person anything. Call it
// once.
func (c *Client) Start(ctx context.Context) error {
	err := c.Login(ctx)
	go c.keepLoggedIn(ctx)
	return err
}

// Login logs the daemon in once: it reads the token file, mints a fresh key, signs a request object
// with it naming os.Hostname() and launcherService, and posts both to
// /v1/launcher-credentials/token. On 200 the broker has moved the daemon's one launcher credential
// to the new key, which the client then holds; any other outcome records the login failed and
// lengthens the wait before the next. It holds proofMu exclusively while the broker switches keys.
func (c *Client) Login(ctx context.Context) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if err := c.tokenLogin(ctx); err != nil {
		c.failedLogins++
		c.nextLogin = time.Now().Add(loginRetryWait(c.failedLogins))
		c.login.Store(&LoginState{State: "failed", reason: err.Error()})
		return err
	}
	c.failedLogins, c.nextLogin = 0, time.Time{}
	c.login.Store(&LoginState{State: "issued"})
	return nil
}

// tokenLogin is one POST /v1/launcher-credentials/token, with loginMu held; on 200 it stores the
// credential the broker answered.
func (c *Client) tokenLogin(ctx context.Context) error {
	raw, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return fmt.Errorf("agent-secrets login: read the ServiceAccount token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return fmt.Errorf("agent-secrets login: the ServiceAccount token file %s is empty", c.TokenFile)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("agent-secrets login: generate key: %w", err)
	}
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("agent-secrets login: hostname: %w", err)
	}
	request, err := signRequestObject(key, c.URL, host, time.Now())
	if err != nil {
		return fmt.Errorf("agent-secrets login: sign request object: %w", err)
	}
	c.proofMu.Lock()
	defer c.proofMu.Unlock()
	status, body, err := c.do(ctx, http.MethodPost, "/v1/launcher-credentials/token", map[string]string{"token": token, "request": request}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return refusal(status, body)
	}
	var answer struct {
		CredentialID string    `json:"credential_id"`
		ExpiresAt    time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.CredentialID == "" || answer.ExpiresAt.IsZero() {
		return fmt.Errorf("broker answered %d with a body that is not the contract's shape: %s", status, body)
	}
	c.cred.Store(&credential{key: key, id: answer.CredentialID, expires: answer.ExpiresAt})
	return nil
}

// LoginStatus reports the latest login's outcome: {"none"} before the first login finishes.
func (c *Client) LoginStatus() LoginState {
	if s := c.login.Load(); s != nil {
		return *s
	}
	return LoginState{State: "none"}
}

// keepLoggedIn is Start's loop: it logs in again when the next login is due (nextDue) and logs
// each outcome, the expiry and never the key, the token or the credential's id.
func (c *Client) keepLoggedIn(ctx context.Context) {
	for {
		timer := time.NewTimer(time.Until(c.nextDue(time.Now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.changes():
			timer.Stop()
			if !c.due(time.Now()) {
				continue
			}
		case <-timer.C:
		}
		if err := c.Login(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log().Error("agent-secrets: the token login failed; retrying", "error", err, "retry_in", c.retryIn().String())
			continue
		}
		c.log().Info("agent-secrets: logged in with the daemon's ServiceAccount token", "expires_at", c.cred.Load().expires.UTC().Format(time.RFC3339))
	}
}

// nextDue is when the next login is due: halfway from now to the held credential's expiry, or,
// with no credential, once the wait after the last failed login has passed.
func (c *Client) nextDue(now time.Time) time.Time {
	c.loginMu.Lock()
	next := c.nextLogin
	c.loginMu.Unlock()
	if cred := c.cred.Load(); cred != nil {
		if !cred.expires.After(now) {
			return now
		}
		return now.Add(cred.expires.Sub(now) / 2)
	}
	if next.After(now) {
		return next
	}
	return now
}

// due reports whether a login woken by a call (a missing or refused credential) may start now: only
// with no credential held and the wait after the last failed login passed.
func (c *Client) due(now time.Time) bool {
	if c.cred.Load() != nil {
		return false
	}
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return !now.Before(c.nextLogin)
}

// retryIn is the wait before the login loop's next attempt after a failure.
func (c *Client) retryIn() time.Duration {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return time.Until(c.nextLogin).Round(time.Second)
}

// changes is the channel wake signals: one pending signal at most, so a burst of refusals wakes
// the login loop once.
func (c *Client) changes() chan struct{} {
	c.changedOnce.Do(func() { c.changed = make(chan struct{}, 1) })
	return c.changed
}

// wake tells the login loop a call found no credential or the broker refused it.
func (c *Client) wake() {
	select {
	case c.changes() <- struct{}{}:
	default:
	}
}

func (c *Client) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
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
// contract adopted); both are the enrollment to record. With no live launcher credential — not yet
// won, or just refused — it returns a non-permanent *APIError{Code: "NO_MACHINE_CREDENTIAL"}, so
// the caller retries after the next token login. Any other broker answer is an *APIError, a
// transport failure a plain error.
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
// the Proof header — never Authorization, which no route on this client uses. It holds proofMu
// shared from reading the credential to handling the answer, so no login moves the broker to
// another key while this call's proof is in flight. With no credential it wakes the login loop and
// answers NO_MACHINE_CREDENTIAL. A 401 (LAUNCHER_INVALID) clears the credential that failed,
// wakes the login loop, and answers NO_MACHINE_CREDENTIAL; a 401 on a credential a login already
// replaced clears nothing.
func (c *Client) doProof(ctx context.Context, method, path string, body any) (int, []byte, error) {
	c.proofMu.RLock()
	defer c.proofMu.RUnlock()
	cred := c.cred.Load()
	if cred == nil {
		c.wake()
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
		c.wake()
		return 0, nil, c.noCredentialError()
	}
	return status, raw, nil
}

// loginRetryWait is the wait after failed consecutive logins: loginRetryInitial, doubled for each
// one after the first, never more than loginRetryMax.
func loginRetryWait(failed int) time.Duration {
	wait := loginRetryInitial
	for i := 1; i < failed && wait < loginRetryMax; i++ {
		wait *= 2
	}
	return min(wait, loginRetryMax)
}

// noCredentialError is the fail-closed refusal Enroll/Revoke return with no live launcher
// credential: non-permanent, naming why — the latest login's failure, or that none has finished —
// never the key, the token or the credential id.
func (c *Client) noCredentialError() error {
	state := c.LoginStatus()
	message := "no token login yet"
	switch state.State {
	case "failed":
		message = "token login failed: " + state.reason
	case "issued":
		message = "the broker refused the daemon's launcher credential; logging in again"
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
