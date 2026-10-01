// packages/envoy/internal/broker/helper/broker.go
//go:build linux

package helper

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// noCredentialMsg is the exact instruction returned whenever the helper has no machine
// credential to authenticate the launcher routes with: never having logged in, and having had
// its credential cleared by a broker 401 LAUNCHER_INVALID (clearOnInvalid). It never
// auto-relogins; a login is a human ceremony.
const noCredentialMsg = "no machine credential; run: agent-secrets launcher login"

var errNoCredential = errors.New(noCredentialMsg)

// machineCredential is the helper's launcher identity, existing only in memory: the private key
// never touches disk, and id is the launcher credential the broker minted once a human approved
// the machine login that installed this.
type machineCredential struct {
	key *ecdsa.PrivateKey
	id  string
}

// loginState is the machine login as login-status reports it: the most recent login's
// human-facing confirmation code, the opaque id the helper polls the broker with, and its state,
// beside whether the helper holds a launcher credential. Those are separate facts, since a
// re-login that is denied, expires unapproved or is still pending leaves the credential an
// earlier login installed in place, and the helper keeps enrolling sessions with it. Refused says
// the broker refused the credential the helper held and no login has been recorded since, so the
// helper holds none; State then reads "expired" whatever the most recent login's was, unless that
// login is still pending. "expired" is the word the dotfiles launcher gate matches and the one an
// older client prints its refusal line for, and an older client must never read "issued" for a
// credential the helper no longer holds. A login pending at the refusal that then settles is
// recorded after it, so it reports its own outcome, which `launcher login` prints. "expired" is
// otherwise a pending login nobody approved in time.
type loginState struct {
	Code, PendingID, State string // the most recent login; State: pending|issued|denied|expired
	CredentialHeld         bool   // the helper holds a launcher credential, whichever login installed it
	Refused                bool   // the broker refused the credential the helper held, and no login has been recorded since
}

// Broker is the helper's view of the secrets broker: the launcher routes (Enroll, Revoke)
// authenticate with an in-memory machineCredential minted by a human-approved machine login
// (Login), and Renew authenticates with the session's own proof, exactly as before.
type Broker struct {
	URL          string
	OperatorFile string // login_hint source; read per login, as before
	HTTP         *http.Client
	// Log records every change of the launcher credential: a machine login installing one, and
	// a broker refusal clearing it. Its lines carry the credential id, the operator the login was
	// signed with (its login_hint) and the broker's refusal code, which are identifiers, never a
	// proof, a request object or key material. Nil logs nothing.
	Log *slog.Logger

	cred    atomic.Pointer[machineCredential]
	loginMu sync.Mutex                 // one login at a time
	login   atomic.Pointer[loginState] // most recent login: code, pendingID, state
	// stateMu is held by every write of cred, login and refused, and by LoginStatus while it reads
	// the three, so login-status never pairs one moment's login with another's credential, and a
	// refusal is recorded with the clear that caused it. cred and login stay atomics so a reader of
	// only one (HasCredential, launcherProof, Login's pending check) takes no lock.
	stateMu sync.Mutex
	refused bool // clearOnInvalid cleared cred, and no login has been recorded since; guarded by stateMu
	// testClearHook, when set, runs in clearOnInvalid between clearing the credential and
	// recording the refusal. It exists only so a test can read login-status there; no production
	// caller sets it.
	testClearHook func()

	installedMu sync.Mutex
	installed   chan struct{} // closed, then replaced, each time a credential is installed
}

// BrokerError is a non-success answer, with the contract's code.
type BrokerError struct {
	Status  int
	Code    string
	Message string
}

func (e *BrokerError) Error() string {
	return fmt.Sprintf("broker %d %s: %s", e.Status, e.Code, e.Message)
}

// readOperator reads OperatorFile, trimmed, per call: it names the login_hint for a machine
// login and the "operator" field of every enrollment.
func (b *Broker) readOperator() (string, error) {
	data, err := os.ReadFile(b.OperatorFile)
	if err != nil {
		return "", fmt.Errorf("%s: %w", b.OperatorFile, err)
	}
	operator := strings.TrimSpace(string(data))
	if operator == "" {
		return "", fmt.Errorf("%s is empty", b.OperatorFile)
	}
	return operator, nil
}

// Operator reads OperatorFile for display only (the "operator" field on `sessions` and
// `register` socket responses): it returns "" on any read failure rather than an error, since
// this field is cosmetic and never security-relevant — unlike readOperator, whose callers use
// its value to build authenticated broker requests and so must see the error.
func (b *Broker) Operator() string {
	data, err := os.ReadFile(b.OperatorFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// launcherCredentialsRequest is POST /v1/launcher-credentials's exact contract v9 request shape
// (handlers_launcher.go's machineLoginBody): a signed request object under "request".
type launcherCredentialsRequest struct {
	Request string `json:"request"`
}

// launcherCredentialsResponse is that route's exact contract v9 response shape
// (handlers_launcher.go's machineLoginResponse): the opaque pending id and the human-facing
// confirmation code under "code".
type launcherCredentialsResponse struct {
	PendingID string `json:"pending_id"`
	Code      string `json:"code"`
}

// launcherCredentialPollResponse is GET /v1/launcher-credentials/{pending}'s reply: CredentialID
// is present only once State is "issued" — the credential is bound to the key that logged in, so
// the broker never has a bearer token to send back.
type launcherCredentialPollResponse struct {
	State        string `json:"state"`
	CredentialID string `json:"credential_id,omitempty"`
}

// Login starts (or reports the running) machine login: a fresh key, signed as a
// launcher_credential request object naming hostname and the OperatorFile's login as login_hint,
// posted with no credential of its own. It returns the human-facing confirmation code for the
// CLI to print, and a background goroutine polls the pending login (2s→10s backoff) until a
// human decides it, installing the credential on "issued" or recording denial/expiry — it never
// retries past a terminal state, and a later Login while one is still pending just reports it.
func (b *Broker) Login(ctx context.Context, hostname string) (string, error) {
	b.loginMu.Lock()
	defer b.loginMu.Unlock()
	if ls := b.login.Load(); ls != nil && ls.State == "pending" {
		return ls.Code, nil
	}
	key, err := proof.NewKey()
	if err != nil {
		return "", err
	}
	operator, err := b.readOperator()
	if err != nil {
		return "", err
	}
	details := []record.AuthorizationDetail{{Type: "launcher_credential", Identifier: hostname}}
	compact, err := record.Sign(key, b.URL, details, "", operator, time.Now())
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(launcherCredentialsRequest{Request: compact})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.URL+"/v1/launcher-credentials", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var out launcherCredentialsResponse
	if err := b.do(req, []int{http.StatusAccepted}, &out); err != nil {
		return "", err
	}
	if out.PendingID == "" || out.Code == "" {
		return "", fmt.Errorf("broker returned no pending_id/code")
	}
	b.recordLogin(&loginState{Code: out.Code, PendingID: out.PendingID, State: "pending"})
	go b.pollLogin(key, out.PendingID, out.Code, operator)
	return out.Code, nil
}

// LoginStatus reports the most recent machine login, current or settled, and whether the helper
// holds a launcher credential; the zero value means no login has ever run. A login that did not
// issue says nothing about the credential an earlier one installed: CredentialHeld does. Once the
// broker has refused the credential, and until a login is recorded (one starts, or one pending at
// the refusal settles), every state but pending reads "expired" and refused; an issued login is
// among them, since only clearOnInvalid ever clears a credential an issued login installed. The
// login, the credential and the refusal are read under stateMu, so the answer is all three of one
// moment.
func (b *Broker) LoginStatus() loginState {
	b.stateMu.Lock()
	ls, cred, refused := b.login.Load(), b.cred.Load(), b.refused
	b.stateMu.Unlock()
	if ls == nil {
		return loginState{}
	}
	out := *ls
	out.CredentialHeld = cred != nil
	if !out.CredentialHeld && refused {
		out.Refused = true
		if out.State != "pending" {
			out.State = "expired"
		}
	}
	return out
}

// recordLogin records ls as the current machine login. Whether the login starts or settles, its
// state is newer than any refusal recorded before it, so the same write ends that record and the
// login reports its own state.
func (b *Broker) recordLogin(ls *loginState) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	b.login.Store(ls)
	b.refused = false
}

// HasCredential reports whether the helper holds a launcher credential, the one thing every
// enrollment needs.
func (b *Broker) HasCredential() bool {
	return b.cred.Load() != nil
}

// CredentialInstalled returns a channel that closes the next time a machine login installs a
// credential, so a retry that failed for want of one can try again at once rather than when its
// backoff comes round.
func (b *Broker) CredentialInstalled() <-chan struct{} {
	b.installedMu.Lock()
	defer b.installedMu.Unlock()
	if b.installed == nil {
		b.installed = make(chan struct{})
	}
	return b.installed
}

// installCredential makes cred the helper's launcher credential and issued the login that minted
// it, in one write under stateMu that also ends any record of a refused credential, then wakes
// every waiter on CredentialInstalled.
func (b *Broker) installCredential(cred *machineCredential, issued *loginState) {
	b.stateMu.Lock()
	b.cred.Store(cred)
	b.login.Store(issued)
	b.refused = false
	b.stateMu.Unlock()
	b.installedMu.Lock()
	defer b.installedMu.Unlock()
	if b.installed != nil {
		close(b.installed)
	}
	b.installed = make(chan struct{})
}

// pollLogin polls a pending machine login until a human decides it, backing off from 2s to 10s
// between attempts. On "issued" it installs the credential (key and id only — never written to
// disk) and logs it with operator, the login_hint the login was signed with, whatever the
// operator file says by then; on "denied" or "expired" it records the terminal state and leaves
// cred untouched.
func (b *Broker) pollLogin(key *ecdsa.PrivateKey, pendingID, code, operator string) {
	ctx := context.Background()
	delay := 2 * time.Second
	for {
		if state, credentialID, ok := b.readLoginStatus(ctx, pendingID); ok {
			switch state {
			case "issued":
				b.installCredential(&machineCredential{key: key, id: credentialID}, &loginState{Code: code, PendingID: pendingID, State: "issued"})
				b.logger().Info("machine login issued; the helper holds a launcher credential", "credential_id", credentialID, "operator", operator)
				return
			case "denied", "expired":
				b.recordLogin(&loginState{Code: code, PendingID: pendingID, State: state})
				return
			}
		}
		time.Sleep(delay)
		if delay < 10*time.Second {
			delay *= 2
			if delay > 10*time.Second {
				delay = 10 * time.Second
			}
		}
	}
}

// readLoginStatus polls the pending login once. ok is false on any transport or protocol error
// (a transient broker outage), so pollLogin just backs off and tries again.
func (b *Broker) readLoginStatus(ctx context.Context, pendingID string) (state, credentialID string, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL+"/v1/launcher-credentials/"+pendingID, nil)
	if err != nil {
		return "", "", false
	}
	var out launcherCredentialPollResponse
	if err := b.do(req, []int{http.StatusOK}, &out); err != nil {
		return "", "", false
	}
	return out.State, out.CredentialID, true
}

// enrollBody is POST /v1/enrollments's contract v9 shape (the v8 "approver" field is gone: the
// broker's rules pick a request's approver, never the enrollment).
type enrollBody struct {
	Kind       string  `json:"kind"`
	RuntimeID  string  `json:"runtime_id"`
	Operator   string  `json:"operator"`
	Thumbprint string  `json:"thumbprint"`
	SessionID  *string `json:"session_id"`
	PodToken   *string `json:"pod_token"`
}

type leaseReply struct {
	EnrollmentID   string    `json:"enrollment_id"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// launcherProof signs a launcher proof with the current machine credential, or fails with
// errNoCredential if there is none yet. It returns the credential it signed with, so a caller
// whose call the broker then refuses clears exactly that one (clearOnInvalid).
func (b *Broker) launcherProof(method, url string) (string, *machineCredential, error) {
	cred := b.cred.Load()
	if cred == nil {
		return "", nil, errNoCredential
	}
	compact, err := proof.SignLauncher(cred.key, cred.id, method, url, time.Now())
	return compact, cred, err
}

// clearOnInvalid clears the machine credential and reports it missing whenever err is the
// broker's 401 LAUNCHER_INVALID. The broker answers that for any launcher proof it rejects: an
// expired or revoked credential, and also a proof it cannot verify, such as one signed outside
// its clock-skew window or for a URL other than its own (an AGENT_SECRETS_URL that does not match
// the broker's public URL). It never auto-relogins. cred is the credential the refused call was
// signed with, and only that one is cleared: a refusal that arrives after another login has
// installed a new credential leaves the new one alone. The clear and the record that the broker
// refused it are one write under stateMu, so no LoginStatus reads a credential gone without its
// refusal: until a login is recorded, it reports the credential refused, whatever the most recent
// login's state, and a login recorded meanwhile ends that record after it, never before.
// login-status — the probe every launcher decides on — then stops reporting a credential the
// helper no longer holds.
func (b *Broker) clearOnInvalid(cred *machineCredential, err error) error {
	var be *BrokerError
	if !errors.As(err, &be) || be.Status != http.StatusUnauthorized || be.Code != "LAUNCHER_INVALID" {
		return err
	}
	b.stateMu.Lock()
	cleared := b.cred.CompareAndSwap(cred, nil)
	if cleared {
		if b.testClearHook != nil {
			b.testClearHook()
		}
		b.refused = true
	}
	b.stateMu.Unlock()
	if cleared {
		b.logger().Warn("launcher credential refused; cleared", "credential_id", cred.id, "code", be.Code)
	}
	return errNoCredential
}

// logger is b.Log, or a logger that writes nowhere when there is none.
func (b *Broker) logger() *slog.Logger {
	if b.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return b.Log
}

// Enroll registers the session's key as kind host. 201 is a new enrollment; 200 is the live one
// for the same credential, kind, runtime_id and thumbprint (the contract's idempotent enroll).
func (b *Broker) Enroll(ctx context.Context, s *Session) (string, time.Time, error) {
	url := b.URL + "/v1/enrollments"
	compact, cred, err := b.launcherProof(http.MethodPost, url)
	if err != nil {
		return "", time.Time{}, err
	}
	operator, err := b.readOperator()
	if err != nil {
		return "", time.Time{}, err
	}
	body, err := json.Marshal(enrollBody{Kind: "host", RuntimeID: s.RuntimeID, Operator: operator, Thumbprint: s.Thumbprint})
	if err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Proof", compact)
	req.Header.Set("Content-Type", "application/json")
	var out leaseReply
	if err := b.do(req, []int{http.StatusOK, http.StatusCreated}, &out); err != nil {
		return "", time.Time{}, b.clearOnInvalid(cred, err)
	}
	if out.EnrollmentID == "" {
		return "", time.Time{}, fmt.Errorf("broker returned no enrollment_id")
	}
	return out.EnrollmentID, out.LeaseExpiresAt, nil
}

// EnrollBox registers a box's key as kind box: a pass-through broker call with no backing
// registry Session (a box enrollment is not a registry session), so it takes the box's
// runtime_id, thumbprint and optional session_id directly instead of a *Session. Otherwise
// identical to Enroll: same fail-closed check when there is no machine credential yet, same
// launcher proof, same error-propagating readOperator() (never the cosmetic Operator()), same
// idempotent 200/201 handling, same 401 LAUNCHER_INVALID handling.
func (b *Broker) EnrollBox(ctx context.Context, runtimeID, thumbprint string, sessionID *string) (string, time.Time, error) {
	url := b.URL + "/v1/enrollments"
	compact, cred, err := b.launcherProof(http.MethodPost, url)
	if err != nil {
		return "", time.Time{}, err
	}
	operator, err := b.readOperator()
	if err != nil {
		return "", time.Time{}, err
	}
	body, err := json.Marshal(enrollBody{Kind: "box", RuntimeID: runtimeID, Operator: operator, Thumbprint: thumbprint, SessionID: sessionID})
	if err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Proof", compact)
	req.Header.Set("Content-Type", "application/json")
	var out leaseReply
	if err := b.do(req, []int{http.StatusOK, http.StatusCreated}, &out); err != nil {
		return "", time.Time{}, b.clearOnInvalid(cred, err)
	}
	if out.EnrollmentID == "" {
		return "", time.Time{}, fmt.Errorf("broker returned no enrollment_id")
	}
	return out.EnrollmentID, out.LeaseExpiresAt, nil
}

// Renew extends the lease with a proof signed by the session key. Unchanged: it always
// authenticates with the session's own key, never the launcher credential.
func (b *Broker) Renew(ctx context.Context, s *Session) (time.Time, error) {
	id := s.EnrollmentID()
	url := b.URL + "/v1/enrollments/" + id + "/renew"
	compact, err := proof.Sign(s.Key, id, http.MethodPost, url, time.Now())
	if err != nil {
		return time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Proof", compact)
	var out leaseReply
	if err := b.do(req, []int{http.StatusOK}, &out); err != nil {
		return time.Time{}, err
	}
	return out.LeaseExpiresAt, nil
}

// Revoke deletes the enrollment and every grant under it. 204 and 404 both mean done.
func (b *Broker) Revoke(ctx context.Context, enrollmentID string) error {
	url := b.URL + "/v1/enrollments/" + enrollmentID
	compact, cred, err := b.launcherProof(http.MethodDelete, url)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Proof", compact)
	if err := b.do(req, []int{http.StatusNoContent, http.StatusNotFound}, nil); err != nil {
		return b.clearOnInvalid(cred, err)
	}
	return nil
}

// UnenrollBox deletes a box enrollment and every grant under it — Revoke under another name,
// exported because it's called directly by the box-enrollment socket op rather than through the
// session-revoke path. 204 and 404 both mean done.
func (b *Broker) UnenrollBox(ctx context.Context, enrollmentID string) error {
	return b.Revoke(ctx, enrollmentID)
}

func (b *Broker) do(req *http.Request, ok []int, out any) error {
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if !slices.Contains(ok, resp.StatusCode) {
		var e struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return &BrokerError{Status: resp.StatusCode, Code: e.Code, Message: e.Error}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
