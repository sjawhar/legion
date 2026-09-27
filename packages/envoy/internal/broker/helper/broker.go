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
// its credential cleared by a broker 401 LAUNCHER_INVALID (expired or revoked). It never
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

// loginState is a machine login in flight or just settled: the human-facing confirmation code,
// the opaque id the helper polls the broker with, and its current state.
type loginState struct {
	Code, PendingID, State string // State: pending|issued|denied|expired
}

// Broker is the helper's view of the secrets broker: the launcher routes (Enroll, Revoke)
// authenticate with an in-memory machineCredential minted by a human-approved machine login
// (Login), and Renew authenticates with the session's own proof, exactly as before.
type Broker struct {
	URL          string
	OperatorFile string // login_hint source; read per login, as before
	HTTP         *http.Client

	cred    atomic.Pointer[machineCredential]
	loginMu sync.Mutex                 // one login at a time
	login   atomic.Pointer[loginState] // pending login: code, pendingID, state
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
	b.login.Store(&loginState{Code: out.Code, PendingID: out.PendingID, State: "pending"})
	go b.pollLogin(key, out.PendingID, out.Code)
	return out.Code, nil
}

// LoginStatus reports the current (or most recently settled) machine login; the zero value means
// none has ever run.
func (b *Broker) LoginStatus() loginState {
	if ls := b.login.Load(); ls != nil {
		return *ls
	}
	return loginState{}
}

// pollLogin polls a pending machine login until a human decides it, backing off from 2s to 10s
// between attempts. On "issued" it installs the credential (key and id only — never written to
// disk); on "denied" or "expired" it records the terminal state and leaves cred untouched.
func (b *Broker) pollLogin(key *ecdsa.PrivateKey, pendingID, code string) {
	ctx := context.Background()
	delay := 2 * time.Second
	for {
		if state, credentialID, ok := b.readLoginStatus(ctx, pendingID); ok {
			switch state {
			case "issued":
				b.cred.Store(&machineCredential{key: key, id: credentialID})
				b.login.Store(&loginState{Code: code, PendingID: pendingID, State: "issued"})
				return
			case "denied", "expired":
				b.login.Store(&loginState{Code: code, PendingID: pendingID, State: state})
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
// errNoCredential if there is none yet.
func (b *Broker) launcherProof(method, url string) (string, error) {
	cred := b.cred.Load()
	if cred == nil {
		return "", errNoCredential
	}
	return proof.SignLauncher(cred.key, cred.id, method, url, time.Now())
}

// clearOnInvalid clears the machine credential and reports it missing whenever err is the
// broker's 401 LAUNCHER_INVALID — an expired or revoked credential. It never auto-relogins.
func (b *Broker) clearOnInvalid(err error) error {
	var be *BrokerError
	if errors.As(err, &be) && be.Status == http.StatusUnauthorized && be.Code == "LAUNCHER_INVALID" {
		b.cred.Store(nil)
		return errNoCredential
	}
	return err
}

// Enroll registers the session's key as kind host. 201 is a new enrollment; 200 is the live one
// for the same credential, kind, runtime_id and thumbprint (the contract's idempotent enroll).
func (b *Broker) Enroll(ctx context.Context, s *Session) (string, time.Time, error) {
	url := b.URL + "/v1/enrollments"
	compact, err := b.launcherProof(http.MethodPost, url)
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
		return "", time.Time{}, b.clearOnInvalid(err)
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
	compact, err := b.launcherProof(http.MethodPost, url)
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
		return "", time.Time{}, b.clearOnInvalid(err)
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
	compact, err := b.launcherProof(http.MethodDelete, url)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Proof", compact)
	if err := b.do(req, []int{http.StatusNoContent, http.StatusNotFound}, nil); err != nil {
		return b.clearOnInvalid(err)
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
