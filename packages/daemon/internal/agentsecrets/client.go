// Package agentsecrets is the daemon's side of the secrets broker's machine-login, launcher-renewal
// and enrollment routes (broker API v9): it logs the machine in through the typed-code flow
// (POST /v1/launcher-credentials, polled until a human approves it), hands the key and the
// credential it won to its Keeper (the daemon's own database) so a restart takes them back rather
// than asking a person again, renews that credential before it expires
// (POST /v1/launcher-credentials/{id}/renew), and enrolls a pod's key under it — authenticating
// every call with a launcher proof signed by the key, never a bearer token. Revoke tears the
// enrollment down when the pod is gone. No key, and no grant or brokered secret value, ever goes
// into an error, a log line, or GET /legion/v1/state. A launcher credential's id, which
// authenticates nothing without its key and is listed on Dispatch's machine-login page, goes only
// into the ERROR a refusal logs (refused), so a person knows which login to revoke. The pod's own
// `agent-secrets` client handles brokered secret values with the pod's own key.
package agentsecrets

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// loginRetryInitial and loginRetryMax bound the wait between machine logins the broker never
// opened (refused, rate limited, or unreachable): the first wait is loginRetryInitial, each one
// after it doubles, up to loginRetryMax, and an issued login resets it. No person saw those, so the
// wait is short. The broker refills the one bucket every service's login shares at one login a
// minute (DefaultLauncherLimits in packages/envoy/internal/broker/api/limits.go), so a daemon
// retrying at its cap spends a fifth of that and cannot keep the bucket empty for other services.
// Package-level so tests can shrink them.
var (
	loginRetryInitial = 30 * time.Second
	loginRetryMax     = 5 * time.Minute
)

// unansweredRetryInitial and unansweredRetryMax bound the wait after a code a person was shown and
// did not approve (it expired undecided, or someone denied it): an hour, doubling to a day, so an
// unattended daemon asks every signed-in person again at most daily rather than every fifteen
// minutes. An issued login resets it. Package-level so tests can shrink them.
var (
	unansweredRetryInitial = time.Hour
	unansweredRetryMax     = 24 * time.Hour
)

// renewRetryInitial and renewRetryMax bound the wait before retrying a renewal that failed without
// the broker refusing the credential (unreachable, a 5xx, rate limited): a minute, doubling to an
// hour, never past the credential's expiry. keeperTimeout bounds each Keeper call the renewal loop
// and the poll goroutine make. Package-level so tests can shrink them.
var (
	renewRetryInitial = time.Minute
	renewRetryMax     = time.Hour
	keeperTimeout     = 10 * time.Second
)

// Client speaks the broker's machine-login, launcher-renewal and enrollment routes. URL is the
// broker's base URL with no path (also the request object's audience); HTTP is the client every
// call goes through, nil for http.DefaultClient. Keeper keeps the credential a login wins across a
// restart (nil keeps nothing: every boot logs in). Log is where the renewal loop reports, nil for
// slog's default. cred is the launcher credential a login has won or Start resumed, if any; login
// is the most recent login's state, a login the broker never opened included ("failed");
// failedLogins, unansweredLogins and nextLogin pace the next attempt after a login that ended
// without a credential (retryLogin). cred and login are set only by Login, its poll goroutine,
// settle, resume and a renewal, and cred cleared only when the broker refuses it as invalid.
// changed wakes the renewal loop when cred changes. loginMu serializes Login's own
// check-then-start sequence and every login's outcome (settle): atomic.Pointer alone lets two
// concurrent callers both observe "not pending" and both mint a key and POST, silently discarding
// one credential's poll goroutine — loginMu makes "start at most one pending login" atomic, so
// every concurrent caller observes the same winner. proofMu is held shared by every call a launcher
// proof signs (doProofWith) and exclusively by a renewal, which moves the broker's credential to a
// new key: no proof signed with the old key is in flight when the broker switches, so none comes
// back refused and clears a credential that was only renewed.
type Client struct {
	URL    string
	HTTP   *http.Client
	Keeper Keeper
	Log    *slog.Logger

	cred             atomic.Pointer[credential]
	login            atomic.Pointer[LoginState]
	loginMu          sync.Mutex
	proofMu          sync.RWMutex
	failedLogins     int       // guarded by loginMu
	unansweredLogins int       // guarded by loginMu
	nextLogin        time.Time // guarded by loginMu: no retryLogin starts a login before it
	changed          chan struct{}
	changedOnce      sync.Once
	// started is set once the daemon's boot login begins (Start, Login). Until then no enrollment
	// starts a login (retryLogin), so a pod the supervisor enrolls before the boot takes back its kept
	// credential is refused as "no machine login yet" and retried, never answered with a new code.
	started atomic.Bool
}

// credential is a won launcher credential: the key it is bound to, the id the broker minted for it,
// and when it expires (expiresAt, the zero time when the broker named none). The key leaves process
// memory only for the Keeper. resumed marks a credential taken back from the Keeper at boot, which
// keepRenewed renews at once. next is a key a renewal asked the broker to move the credential to
// whose answer the client never read (the renewal's connection failed, or the daemon restarted
// before it saved the answer): until the client asks the broker which key it holds
// (resolveNext), either one may be the credential's.
type credential struct {
	key     *ecdsa.PrivateKey
	id      string
	resumed bool
	next    atomic.Pointer[ecdsa.PrivateKey]
	expires atomic.Int64 // unix nanoseconds; 0 when unknown
}

func (c *credential) setExpires(t time.Time) {
	if t.IsZero() {
		c.expires.Store(0)
		return
	}
	c.expires.Store(t.UnixNano())
}

func (c *credential) expiresAt() time.Time {
	if n := c.expires.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// Keeper is where the client keeps the launcher credential a login won, so a daemon that restarts
// takes it back instead of asking a person to approve a new login (the daemon's own Postgres:
// internal/daemon's credentialKeeper). Load answers false when nothing is kept. Save replaces what
// is kept. Forget drops what is kept only when it is the credential named, so dropping an older
// credential never drops a newer one.
type Keeper interface {
	Load(ctx context.Context) (Kept, bool, error)
	Save(ctx context.Context, kept Kept) error
	Forget(ctx context.Context, credentialID string) error
}

// RotationKeeper updates a renewed credential only when its complete prior durable state — the
// credential id, current key and candidate next key — still matches. A second daemon that lost the
// same renewal race must not write either its candidate or its old key over the broker winner's.
type RotationKeeper interface {
	Keeper
	CompareAndSave(ctx context.Context, expected Kept, kept Kept) (saved bool, err error)
}

// Kept is a launcher credential as a Keeper keeps it: the broker's id for it, its private key and,
// while a renewal's answer is unknown, the key that renewal asked the broker to move to, each as
// PKCS #8 DER (NextPrivateKey nil otherwise), and when it expires (the zero time when the broker
// named none).
type Kept struct {
	CredentialID   string
	PrivateKey     []byte
	NextPrivateKey []byte
	ExpiresAt      time.Time
}

// Start is the daemon's boot login. It takes back the credential the Keeper kept, when there is
// one that has not reached its expiry, and returns "", opening no machine login; otherwise it
// starts one and returns its code, as Login does. Either way it keeps the credential the client
// holds renewed for as long as ctx lives (keepRenewed): a resumed one at once, which also asks the
// broker whether anyone revoked it, and each one after a quarter of its remaining lifetime. Call it
// once.
func (c *Client) Start(ctx context.Context) (string, error) {
	resumed := c.resume(ctx)
	c.started.Store(true)
	go c.keepRenewed(ctx)
	if resumed {
		return "", nil
	}
	return c.Login(ctx)
}

// resume takes back the credential the Keeper kept, reporting whether it did. A kept credential
// past its expiry, or one whose key does not parse, is forgotten and not taken back; a Keeper that
// cannot be read is logged and not taken back, so the boot logs in as it would with nothing kept.
func (c *Client) resume(ctx context.Context) bool {
	if c.Keeper == nil {
		return false
	}
	kept, ok, err := c.Keeper.Load(ctx)
	if err != nil {
		c.log().Error("agent-secrets: could not read the kept launcher credential; starting a machine login instead", "error", err)
		return false
	}
	if !ok {
		return false
	}
	if !kept.ExpiresAt.IsZero() && !time.Now().Before(kept.ExpiresAt) {
		c.log().Info("agent-secrets: the kept launcher credential has expired; starting a machine login", "expired_at", formatExpiry(kept.ExpiresAt))
		c.forget(ctx, kept.CredentialID)
		return false
	}
	key, err := parseKey(kept.PrivateKey)
	var next *ecdsa.PrivateKey
	if err == nil && kept.NextPrivateKey != nil {
		next, err = parseKey(kept.NextPrivateKey)
	}
	if err != nil {
		c.log().Error("agent-secrets: the kept launcher credential's key does not parse; forgetting it and starting a machine login", "error", err)
		c.forget(ctx, kept.CredentialID)
		return false
	}
	cred := &credential{key: key, id: kept.CredentialID, resumed: true}
	cred.next.Store(next)
	cred.setExpires(kept.ExpiresAt)
	c.loginMu.Lock()
	c.cred.Store(cred)
	c.login.Store(&LoginState{State: "issued"})
	c.loginMu.Unlock()
	return true
}

// parseKey reads a PKCS #8 DER P-256 private key, as keep writes one.
func parseKey(der []byte) (*ecdsa.PrivateKey, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("a %T, not an ECDSA key", parsed)
	}
	return key, nil
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
	c.started.Store(true)
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.loginLocked(ctx)
}

// loginLocked is Login with loginMu held. It starts no login while a credential is live or a
// login is pending, so neither Login nor retryLogin opens one beside them.
func (c *Client) loginLocked(ctx context.Context) (string, error) {
	if c.cred.Load() != nil {
		return c.LoginStatus().Code, nil
	}
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
// won, and the wait before the next login (retryLogin). An issued login resets the wait. A login
// the broker never opened ("failed": a refusal, no answer) waits loginRetryWait, since no person
// saw it; a code a person was shown and did not approve (expired, denied) waits unansweredWait, so
// nobody is shown a new code every few minutes.
func (c *Client) settle(state LoginState, cred *credential) {
	switch state.State {
	case "issued":
		c.cred.Store(cred)
		c.failedLogins, c.unansweredLogins, c.nextLogin = 0, 0, time.Time{}
	case "failed":
		c.failedLogins++
		c.nextLogin = time.Now().Add(loginRetryWait(c.failedLogins))
	default:
		c.failedLogins = 0
		c.unansweredLogins++
		c.nextLogin = time.Now().Add(unansweredWait(c.unansweredLogins))
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
// backing off from pollInitialInterval to pollMaxInterval between reads. On "issued" it persists
// the credential before publishing it to Enroll or the state route: a daemon that exits in that
// gap must take back this same key at its next boot, not ask a person again.
func (c *Client) poll(ctx context.Context, key *ecdsa.PrivateKey, pendingID, code string) {
	wait := pollInitialInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		state, credentialID, expiresAt, err := c.readLauncherCredential(ctx, pendingID)
		if err != nil || state == "pending" {
			if wait *= 2; wait > pollMaxInterval {
				wait = pollMaxInterval
			}
			continue
		}
		won := &credential{key: key, id: credentialID}
		won.setExpires(expiresAt)
		if state == "issued" {
			if err := c.keep(ctx, won); err != nil {
				c.loginMu.Lock()
				c.settle(LoginState{State: "failed", reason: "keep issued launcher credential: " + err.Error()}, nil)
				c.loginMu.Unlock()
				return
			}
		}
		c.loginMu.Lock()
		c.settle(LoginState{State: state, Code: code}, won)
		c.loginMu.Unlock()
		if state == "issued" {
			c.wake()
		}
		return
	}
}

func (c *Client) readLauncherCredential(ctx context.Context, pendingID string) (state, credentialID string, expiresAt time.Time, err error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/v1/launcher-credentials/"+url.PathEscape(pendingID), nil, nil)
	if err != nil {
		return "", "", time.Time{}, err
	}
	if status != http.StatusOK {
		return "", "", time.Time{}, refusal(status, raw)
	}
	var answer struct {
		State        string     `json:"state"`
		CredentialID string     `json:"credential_id"`
		ExpiresAt    *time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", "", time.Time{}, fmt.Errorf("broker answered %d with a body that is not the contract's shape: %w", status, err)
	}
	if answer.ExpiresAt != nil {
		expiresAt = *answer.ExpiresAt
	}
	return answer.State, answer.CredentialID, expiresAt, nil
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
// the Proof header — never Authorization, which no route on this client uses any more. It holds
// proofMu shared from reading the credential to handling the answer, so no renewal moves the
// broker to another key while this call's proof is in flight. With no credential it starts a fresh
// login when none is pending and the retry wait has passed (retryLogin), and answers
// NO_MACHINE_CREDENTIAL naming the login's state.
//
// A 401 (LAUNCHER_INVALID) refuses the credential that failed (refused): it is cleared, the renewal
// loop woken and a login started, and the call answers NO_MACHINE_CREDENTIAL naming the login's
// state. The Keeper keeps the refused credential until a new login replaces it or it expires: a 401
// also answers a proof the broker could not verify for a passing reason (clock skew), and the next
// boot tries the kept credential again rather than ask a person. A 401 while a renewal's answer is
// unknown (credential.next) first retries the call with the next key, since the broker may hold
// that one: an answer other than 401 takes the next key up (kept, and renewed at once, since its
// expiry is unknown), and a second 401 drops it and refuses the credential.
func (c *Client) doProof(ctx context.Context, method, path string, body any) (int, []byte, error) {
	c.proofMu.RLock()
	defer c.proofMu.RUnlock()
	cred := c.cred.Load()
	if cred == nil {
		c.retryLogin(ctx)
		return 0, nil, c.noCredentialError()
	}
	status, raw, err := c.signed(ctx, cred.key, cred.id, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	if status != http.StatusUnauthorized {
		return status, raw, nil
	}
	if next := cred.next.Load(); next != nil {
		status, raw, err = c.signed(ctx, next, cred.id, method, path, body)
		if err != nil {
			return 0, nil, err
		}
		if status != http.StatusUnauthorized {
			expected, err := keptCredential(cred)
			if err != nil {
				return 0, nil, err
			}
			moved := &credential{key: next, id: cred.id}
			if err := c.keepRotation(ctx, expected, moved); err != nil {
				c.log().Error("agent-secrets: took up the broker's renewed key but did not replace its durable key; another daemon may hold the durable winner", "error", err)
				return 0, nil, err
			}
			if c.cred.CompareAndSwap(cred, moved) {
				c.wake()
			}
			return status, raw, nil
		}
		cred.next.Store(nil)
	}
	c.refused(ctx, cred)
	return 0, nil, c.noCredentialError()
}

// signed sends one call carrying a launcher proof signed with key for credential id, and answers the
// broker's status and body.
func (c *Client) signed(ctx context.Context, key *ecdsa.PrivateKey, id, method, path string, body any) (int, []byte, error) {
	proof, err := signLauncherProof(key, id, method, c.URL+path, time.Now())
	if err != nil {
		return 0, nil, err
	}
	return c.do(ctx, method, path, body, func(req *http.Request) {
		req.Header.Set("Proof", proof)
	})
}

// retryLogin starts a fresh machine login when the client holds no credential and the wait after
// the last login that ended without a credential has passed: the supervisor's next observation of
// a pod then retries the login the daemon's boot started, with no restart. loginLocked returns a
// pending login's code instead of starting another, settle records how the new login ends, and
// noCredentialError reports it. A held loginMu means a login is already opening or settling, so
// retryLogin returns at once rather than hold its caller (an Enroll or Revoke, under its claim's
// machine lock) behind that login's POST. The login it starts runs in a goroutine on ctx's values
// but not its deadline, bounded by HTTP's own timeout (30 s for the daemon's client), and holds
// loginMu until it settles. retryLogin waits for it only until ctx ends: a quick answer names the
// fresh code in the caller's refusal, and a deadline that ends mid-POST returns the caller then
// while the login goes on to collect the code the broker committed, so no code is left that the
// client never holds.
func (c *Client) retryLogin(ctx context.Context) {
	if !c.started.Load() || !c.loginMu.TryLock() {
		return
	}
	if c.cred.Load() != nil || time.Now().Before(c.nextLogin) {
		c.loginMu.Unlock()
		return
	}
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		defer c.loginMu.Unlock()
		_, _ = c.loginLocked(context.WithoutCancel(ctx))
	}()
	select {
	case <-settled:
	case <-ctx.Done():
	}
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

// unansweredWait is the wait after consecutive codes a person was shown and did not approve:
// unansweredRetryInitial, doubled for each one after the first, never more than unansweredRetryMax.
func unansweredWait(unanswered int) time.Duration {
	wait := unansweredRetryInitial
	for i := 1; i < unanswered && wait < unansweredRetryMax; i++ {
		wait *= 2
	}
	return min(wait, unansweredRetryMax)
}

// renewalBody is POST /v1/launcher-credentials/{id}/renew's body: a machine request object the new
// key signed, naming this daemon's service as a login's does (signRequestObject).
type renewalBody struct {
	Request string `json:"request"`
}

// renewalAnswer is POST /v1/launcher-credentials/{id}/renew's answer, and the part of
// GET /v1/launcher-credentials/self's this client reads.
type renewalAnswer struct {
	ExpiresAt time.Time `json:"expires_at"`
}

var (
	// errNoRenewalRoute is a renewal's answer from a broker that has no renewal route (a release
	// before it, which answers 404): the credential lasts until its expiry and a person approves a new
	// login then.
	errNoRenewalRoute = errors.New("the secrets broker has no launcher-credential renewal route")
	// errKeyRefused is a renewal the broker refused the credential's own key for (401): the
	// credential is cleared and a login started.
	errKeyRefused = errors.New("the secrets broker refused the launcher credential")
	// errReplaced is a renewal of a credential the client no longer holds: a login replaced it or a
	// refusal cleared it while the renewal waited for proofMu.
	errReplaced = errors.New("the launcher credential was replaced before its renewal")
	// errDurableCredentialChanged is a successful broker renewal whose local CAS found another
	// process had already changed the durable credential. The broker winner stays untouched.
	errDurableCredentialChanged = errors.New("the durable launcher credential changed during its renewal")
)

// renew moves held to a new key and a new expiry (rotate), first settling which key the broker holds
// when an earlier renewal's answer is unknown (resolveNext). It holds proofMu exclusively, so no
// proof signed with the old key is in flight when the broker switches keys. It answers the
// credential the client holds afterwards.
func (c *Client) renew(ctx context.Context, held *credential) (*credential, error) {
	c.proofMu.Lock()
	defer c.proofMu.Unlock()
	if c.cred.Load() != held {
		return nil, errReplaced
	}
	if held.next.Load() != nil {
		resolved, err := c.resolveNext(ctx, held)
		if err != nil {
			return nil, err
		}
		held = resolved
	}
	return c.rotate(ctx, held)
}

// resolveNext asks the broker, with held's next key, for the credential
// (GET /v1/launcher-credentials/self), which changes nothing: 200 means an earlier renewal moved the
// credential to that key, which the client then holds and keeps; 401 means it never did, and the
// next key is dropped. The caller holds proofMu exclusively.
func (c *Client) resolveNext(ctx context.Context, held *credential) (*credential, error) {
	expected, err := keptCredential(held)
	if err != nil {
		return nil, err
	}
	next := held.next.Load()
	status, raw, err := c.signed(ctx, next, held.id, http.MethodGet, "/v1/launcher-credentials/self", nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var answer renewalAnswer
		if err := json.Unmarshal(raw, &answer); err != nil {
			return nil, fmt.Errorf("broker answered %d with a body that is not the contract's shape: %w", status, err)
		}
		moved := &credential{key: next, id: held.id}
		moved.setExpires(answer.ExpiresAt)
		if err := c.keepRotation(ctx, expected, moved); err != nil {
			c.log().Error("agent-secrets: took up the broker's renewed key but did not replace its durable key; another daemon may hold the durable winner", "error", err)
			return nil, err
		}
		c.cred.CompareAndSwap(held, moved)
		return moved, nil
	case http.StatusUnauthorized:
		held.next.Store(nil)
		if err := c.keepCurrent(ctx, expected, held); err != nil {
			c.log().Error("agent-secrets: did not clear a stale next key from the durable credential; another daemon may hold the durable winner", "error", err)
		}
		return held, nil
	default:
		return nil, refusal(status, raw)
	}
}

// rotate is POST /v1/launcher-credentials/{id}/renew for held: signed with held's key and carrying a
// request object a new key signed, it moves the broker's credential to that key and a full lifetime
// from now, which the client then holds and keeps. It keeps the new key beside the current one
// before it asks, and asks only once the Keeper has it, so a restart, or an answer that never
// arrives, still holds whichever key the broker ends on (credential.next, resolved at the next
// renewal or by doProof). A 401 is errKeyRefused: held is refused (refused). An older broker's 404
// is errNoRenewalRoute; any other refusal an *APIError. The caller holds proofMu exclusively.
func (c *Client) rotate(ctx context.Context, held *credential) (*credential, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("agent-secrets renewal: generate key: %w", err)
	}
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("agent-secrets renewal: hostname: %w", err)
	}
	request, err := signRequestObject(key, c.URL, host, time.Now())
	if err != nil {
		return nil, fmt.Errorf("agent-secrets renewal: sign request object: %w", err)
	}
	before, err := keptCredential(held)
	if err != nil {
		return nil, err
	}
	held.next.Store(key)
	if err := c.keepCurrent(ctx, before, held); err != nil {
		held.next.Store(nil)
		return nil, fmt.Errorf("keep the next key before asking the broker to move to it: %w", err)
	}
	candidate, err := keptCredential(held)
	if err != nil {
		return nil, err
	}
	status, raw, err := c.signed(ctx, held.key, held.id, http.MethodPost, "/v1/launcher-credentials/"+url.PathEscape(held.id)+"/renew", renewalBody{Request: request})
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusOK:
		var answer renewalAnswer
		if err := json.Unmarshal(raw, &answer); err != nil || answer.ExpiresAt.IsZero() {
			return nil, fmt.Errorf("broker answered %d with a body that is not the contract's shape: %s", status, raw)
		}
		renewed := &credential{key: key, id: held.id}
		renewed.setExpires(answer.ExpiresAt)
		if err := c.keepRotation(ctx, candidate, renewed); err != nil {
			c.cred.CompareAndSwap(held, renewed)
			c.log().Error("agent-secrets: renewed the broker credential but did not replace its durable key; another daemon may hold the durable winner", "error", err)
			c.wake()
			return nil, err
		}
		c.cred.CompareAndSwap(held, renewed)
		c.log().Info("agent-secrets: renewed the launcher credential onto a new key", "expires_at", formatExpiry(answer.ExpiresAt))
		return renewed, nil
	case status >= http.StatusInternalServerError:
		// The broker may have committed before it failed: the answer is unknown, and next stays.
		return nil, refusal(status, raw)
	}
	held.next.Store(nil)
	if err := c.keepCurrent(ctx, candidate, held); err != nil {
		c.log().Error("agent-secrets: did not clear a losing next key from the durable credential; another daemon may hold the durable winner", "error", err)
	}
	switch status {
	case http.StatusUnauthorized:
		c.refused(ctx, held)
		return nil, errKeyRefused
	case http.StatusNotFound:
		return nil, errNoRenewalRoute
	default:
		return nil, refusal(status, raw)
	}
}

// keepRenewed renews the credential the client holds until ctx ends: one Start resumed, or one whose
// last renewal's answer is unknown, at once (firstRenewal), since a restart cannot tell whether
// anyone revoked it meanwhile; every other one once a quarter of the time to its expiry has passed,
// so a daemon stopped for up to three quarters of a lifetime still comes back to a live credential.
// A renewal the broker refuses has cleared the credential and started a login (rotate, refused), so
// the loop waits for the next credential. An older broker's 404, or a refusal a retry cannot change,
// ends renewals of that credential: it lapses at its expiry, when the broker refuses it and a login
// starts. Any other failure is retried after renewRetryInitial, doubling to renewRetryMax, never
// past the expiry. Every outcome is logged, a success at INFO, with the expiry and never the key.
func (c *Client) keepRenewed(ctx context.Context) {
	var held *credential
	var next time.Time
	var retry time.Duration
	renewing := false
	for {
		if cred := c.cred.Load(); cred != held {
			held, retry, renewing = cred, 0, cred != nil
			if held != nil {
				next = firstRenewal(held, time.Now())
			}
		}
		var due <-chan time.Time
		var timer *time.Timer
		if renewing {
			timer = time.NewTimer(time.Until(next))
			due = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-c.changes():
			if timer != nil {
				timer.Stop()
			}
			if held != nil && held.next.Load() != nil {
				next = time.Now()
			}
			continue
		case <-due:
		}
		renewed, err := c.renew(ctx, held)
		switch {
		case err == nil:
			held, retry, next = renewed, 0, quarterway(time.Now(), renewed.expiresAt())
		case errors.Is(err, errReplaced):
		case errors.Is(err, errKeyRefused):
			// refused logged it, naming the credential to revoke.
		case errors.Is(err, errNoRenewalRoute):
			renewing = false
			c.log().Warn("agent-secrets: the secrets broker cannot renew the launcher credential (a release before its renewal route); a person approves a new machine login when it expires",
				"expires_at", formatExpiry(held.expiresAt()))
		case IsPermanent(err):
			renewing = false
			c.log().Error("agent-secrets: the secrets broker refused to renew the launcher credential; it lapses at its expiry",
				"error", err, "expires_at", formatExpiry(held.expiresAt()))
		default:
			retry = nextRetry(retry)
			next = time.Now().Add(retry)
			if expires := held.expiresAt(); !expires.IsZero() && next.After(expires) {
				next = expires
			}
			c.log().Warn("agent-secrets: renewing the launcher credential failed; retrying",
				"error", err, "retry_in", retry.String(), "expires_at", formatExpiry(held.expiresAt()))
		}
	}
}

// firstRenewal is when keepRenewed first renews cred: at once for a credential Start resumed, one
// whose last renewal's answer is unknown, or one whose expiry the broker never named; else a quarter
// of the way to its expiry.
func firstRenewal(cred *credential, now time.Time) time.Time {
	expires := cred.expiresAt()
	if cred.resumed || cred.next.Load() != nil || expires.IsZero() {
		return now
	}
	return quarterway(now, expires)
}

// quarterway is the moment a quarter of the time from now to expires has passed; now when expires
// has, or is unknown.
func quarterway(now, expires time.Time) time.Time {
	if !expires.After(now) {
		return now
	}
	return now.Add(expires.Sub(now) / 4)
}

// nextRetry is the wait before the next renewal after one more failure: renewRetryInitial, then
// doubling, never more than renewRetryMax.
func nextRetry(last time.Duration) time.Duration {
	if last <= 0 {
		return renewRetryInitial
	}
	return min(last*2, renewRetryMax)
}

func formatExpiry(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// keptCredential serializes cred as a Keeper row.
func keptCredential(cred *credential) (Kept, error) {
	kept := Kept{CredentialID: cred.id, ExpiresAt: cred.expiresAt()}
	var err error
	if kept.PrivateKey, err = x509.MarshalPKCS8PrivateKey(cred.key); err == nil {
		if next := cred.next.Load(); next != nil {
			kept.NextPrivateKey, err = x509.MarshalPKCS8PrivateKey(next)
		}
	}
	return kept, err
}

// keep hands cred to the Keeper, its key and any next key as PKCS #8 DER, and answers whether it
// could. A failure is also logged: the credential still works in this process, and the next restart
// logs in.
func (c *Client) keep(ctx context.Context, cred *credential) error {
	if c.Keeper == nil {
		return nil
	}
	kept, err := keptCredential(cred)
	if err != nil {
		c.log().Error("agent-secrets: could not encode the launcher credential's key to keep it; the next restart starts a machine login", "error", err)
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keeperTimeout)
	defer cancel()
	if err := c.Keeper.Save(ctx, kept); err != nil {
		c.log().Error("agent-secrets: could not keep the launcher credential; the next restart starts a machine login", "error", err)
		return err
	}
	return nil
}

// keepCurrent applies desired only when the Keeper still holds expected. It is used before a
// renewal stores its candidate next key and while a loser clears one; neither write may restore an
// old key or replace a candidate another daemon saved after the caller observed the row.
func (c *Client) keepCurrent(ctx context.Context, expected Kept, desired *credential) error {
	if c.Keeper == nil {
		return nil
	}
	keeper, ok := c.Keeper.(RotationKeeper)
	if !ok {
		return c.keep(ctx, desired)
	}
	kept, err := keptCredential(desired)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keeperTimeout)
	defer cancel()
	saved, err := keeper.CompareAndSave(ctx, expected, kept)
	if err != nil {
		return err
	}
	if !saved {
		return errDurableCredentialChanged
	}
	return nil
}

// keepRotation stores renewed only if the Keeper still holds the complete state held had when the
// broker moved it. The broker has already made renewed its winner, and a losing daemon must never
// overwrite that durable winner. A Keeper without the optional CAS operation is a unit-test
// implementation; production's credentialKeeper implements it.
func (c *Client) keepRotation(ctx context.Context, expected Kept, renewed *credential) error {
	return c.keepCurrent(ctx, expected, renewed)
}

// refused clears cred, which the broker refused (401), wakes the renewal loop and starts a login;
// only the caller whose swap cleared it does, so concurrent refusals of one credential start one
// login and a refusal of a credential already replaced starts none. It logs, at ERROR, the
// credential's id and expiry: the broker answers 401 for a revoked or expired credential and for a
// proof it could not verify, but also when someone who copied the key renewed it first, which only a
// person can end, by revoking that login on Dispatch's machine-login page. The Keeper keeps the
// refused credential until a new login replaces it or it expires (doProof says why).
func (c *Client) refused(ctx context.Context, cred *credential) {
	if !c.cred.CompareAndSwap(cred, nil) {
		return
	}
	c.log().Error("agent-secrets: the secrets broker refused the daemon's launcher credential: it was revoked or expired, a proof could not be verified, or someone renewed it with a copy of its key. If nobody revoked it, revoke this login on Dispatch's machine-login page before approving the new machine login",
		"credential_id", cred.id, "expires_at", formatExpiry(cred.expiresAt()))
	c.wake()
	c.retryLogin(ctx)
}

// forget drops the credential named from the Keeper, once it expired or its key no longer parses. A
// failure is logged: the next boot finds it, and forgets it then.
func (c *Client) forget(ctx context.Context, credentialID string) {
	if c.Keeper == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keeperTimeout)
	defer cancel()
	if err := c.Keeper.Forget(ctx, credentialID); err != nil {
		c.log().Error("agent-secrets: could not forget the kept launcher credential", "error", err)
	}
}

// changes is the channel wake signals: one pending signal at most, so a burst of changes wakes the
// renewal loop once.
func (c *Client) changes() chan struct{} {
	c.changedOnce.Do(func() { c.changed = make(chan struct{}, 1) })
	return c.changed
}

// wake tells the renewal loop the credential changed.
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
