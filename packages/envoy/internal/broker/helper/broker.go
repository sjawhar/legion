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

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// noCredentialMsg is the exact instruction returned whenever the helper has no machine
// credential to authenticate the launcher routes with: never having logged in, and having dropped
// its credential (drop), because the broker refused it with 401 LAUNCHER_INVALID (clearOnInvalid),
// refused its refresh (refresh), or it reached its expiry without a refresh (watchExpiry). It
// never auto-relogins; a login is a human ceremony.
const noCredentialMsg = "no machine credential; run: agent-secrets machine login"

var errNoCredential = errors.New(noCredentialMsg)

// oauthClientID is the OAuth client the helper logs a machine in as, on the broker's device
// authorization and token endpoints.
const oauthClientID = "agent-secrets-helper"

// The refresh loop's defaults: a refresh a day, and after a refresh that did not reach the broker
// a retry a minute later, doubling to at most an hour.
const (
	defaultRefreshEvery = 24 * time.Hour
	defaultRefreshRetry = time.Minute
	maxRefreshRetry     = time.Hour
)

// machineCredential is the helper's launcher identity: the machine key, the launcher credential
// the broker minted once a human approved the machine login, when that credential lapses unless a
// refresh moves it, and the refresh token the next refresh spends. The same login is kept on disk
// (MachineFile) so a restart resumes it. A refresh replaces the held machineCredential with one
// carrying the new expiry and refresh token; gone closes when the helper stops holding it.
type machineCredential struct {
	key          *ecdsa.PrivateKey
	id           string
	expiresAt    time.Time
	refreshToken string
	gone         chan struct{}
}

// credential is the in-memory credential f describes.
func (f *MachineFile) credential() *machineCredential {
	return &machineCredential{key: f.Key, id: f.CredentialID, expiresAt: f.ExpiresAt.UTC(), refreshToken: f.RefreshToken, gone: make(chan struct{})}
}

// loginState is the machine login as login-status reports it: the most recent login's
// human-facing confirmation code and its state, beside whether the helper holds a launcher
// credential and when that credential expires. Those are separate facts, since a re-login that is
// denied, expires unapproved or is still pending leaves the credential an earlier login installed
// in place, and the helper keeps enrolling sessions with it. A login the helper resumed from its
// machine file at start reads "issued" with no code. Dropped is why the helper dropped the
// credential it held (dropRefused, dropRefreshRefused or dropExpired), set while no login has been
// recorded since, so the helper holds none; Refused is Dropped != "", the wire's login_refused.
// State then reads "expired" whatever the most recent login's was, unless that login is still
// pending. "expired" is the word the dotfiles launcher gate matches and the one an older client
// prints its refusal line for, and an older client must never read "issued" for a credential the
// helper no longer holds. A login pending at the drop that then settles is recorded after it, so it
// reports its own outcome, which `machine login` prints. "expired" is otherwise a pending login
// nobody approved in time.
type loginState struct {
	Code, State         string    // the most recent login; State: pending|issued|denied|expired
	CredentialHeld      bool      // the helper holds a launcher credential, whichever login installed it
	CredentialExpiresAt time.Time // when the held credential lapses without a refresh; zero while none is held
	Refused             bool      // Dropped != ""
	Dropped             string    // why the helper dropped the credential it held, while no login has been recorded since
}

// Broker is the helper's view of the secrets broker: the launcher routes (Enroll, Revoke)
// authenticate with a machineCredential minted by a human-approved machine login (Login, an OAuth
// device grant on the broker's own token endpoint), kept at MachineFile and refreshed daily
// (Start), and Renew authenticates with the session's own proof, exactly as before.
type Broker struct {
	URL          string
	OperatorFile string // login_hint source; read per login, as before
	// MachineFile is where the helper keeps its machine login across restarts (AGENT_SECRETS_MACHINE_FILE).
	MachineFile string
	HTTP        *http.Client
	// Log records every change of the launcher credential: a machine login installing one, a
	// restart resuming one, a refresh moving its expiry, the warning a day before it lapses, and
	// the helper dropping it, when the broker refuses it or its refresh or it reaches its expiry.
	// Its lines carry the credential id, its expiry, the operator the login was signed with (its
	// login_hint) and the broker's refusal code, which are identifiers, never a proof, a token, a
	// request object or key material. Nil logs nothing.
	Log *slog.Logger

	cred    atomic.Pointer[machineCredential]
	loginMu sync.Mutex                 // one login at a time
	login   atomic.Pointer[loginState] // most recent login: code, state
	// credMu is held by whatever changes the held login and its machine file together: a login's
	// save and install, a start's install, and a refresh from its request to its save and swap, so
	// a refresh never races a login's install and the file always names the credential in memory.
	credMu sync.Mutex
	// stateMu is held by every write of cred, login and dropped, and by LoginStatus while it reads
	// the three, so login-status never pairs one moment's login with another's credential, and a
	// drop is recorded with the clear that caused it. cred and login stay atomics so a reader of
	// only one (HasCredential, launcherProof, Login's pending check) takes no lock.
	stateMu sync.Mutex
	dropped string // why drop cleared cred (dropRefused, dropRefreshRefused, dropExpired); "" once a login is recorded since; guarded by stateMu
	// testClearHook, when set, runs in drop between clearing the credential and recording why.
	// It exists only so a test can read login-status there; no production caller sets it.
	testClearHook func()
	// expiryWarning is how long before the credential's expiry the helper warns that no refresh
	// has moved it; zero means a day. Only tests shorten it.
	expiryWarning time.Duration
	// refreshEvery is how long the refresh loop waits after a refresh that reached the broker, and
	// refreshRetry its first wait after one that did not (doubling to an hour); zero means a day
	// and a minute. Only tests shorten them.
	refreshEvery, refreshRetry time.Duration

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

// oauthConfig is the OAuth client the helper logs a machine in and refreshes its login with: the
// broker's device authorization and token endpoints, the client's id sent in the form.
func (b *Broker) oauthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID: oauthClientID, Scopes: []string{"machine"},
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: b.URL + "/v1/oauth/device_authorization",
			TokenURL:      b.URL + "/v1/oauth/token",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

// httpClient is b.HTTP, or http.DefaultClient when it is nil, as net/http reads a nil client.
func (b *Broker) httpClient() *http.Client {
	if b.HTTP == nil {
		return http.DefaultClient
	}
	return b.HTTP
}

// tokenContext is ctx carrying the client x/oauth2 sends token requests with: b.HTTP's transport
// and timeout, with a DPoP proof of key on every request, as the broker's token endpoint requires.
func (b *Broker) tokenContext(ctx context.Context, key *ecdsa.PrivateKey) context.Context {
	client := b.httpClient()
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: NewDPoPTransport(key, base), Timeout: client.Timeout})
}

// oauthBrokerError is err as a BrokerError when the broker answered it with an OAuth error: the
// device authorization endpoint's own refusals (invalid_request, described "<CODE>: <message>")
// carry the broker's code and message, any other OAuth error its error code and description.
func oauthBrokerError(err error) error {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return err
	}
	be := &BrokerError{Code: re.ErrorCode, Message: re.ErrorDescription}
	if re.Response != nil {
		be.Status = re.Response.StatusCode
	}
	if code, message, ok := strings.Cut(re.ErrorDescription, ": "); ok && re.ErrorCode == "invalid_request" {
		be.Code, be.Message = code, message
	}
	return be
}

// machineFileFrom is the machine login a token answer issues for key: the refresh token and the
// broker's extras naming the launcher credential and when it lapses without a refresh.
func machineFileFrom(key *ecdsa.PrivateKey, tok *oauth2.Token) (*MachineFile, error) {
	id, _ := tok.Extra("credential_id").(string)
	raw, _ := tok.Extra("credential_expires_at").(string)
	expiresAt, err := time.Parse(time.RFC3339, raw)
	if id == "" || err != nil || tok.RefreshToken == "" {
		return nil, errors.New("the broker's token answer lacks a credential_id, an RFC 3339 credential_expires_at or a refresh_token")
	}
	return &MachineFile{Key: key, CredentialID: id, ExpiresAt: expiresAt.UTC(), RefreshToken: tok.RefreshToken}, nil
}

// Login starts (or reports the running) machine login, an OAuth device grant: a fresh key, signed
// as a launcher_credential request object naming hostname and the OperatorFile's login as
// login_hint, sent to the device authorization endpoint with the key's thumbprint (dpop_jkt). It
// returns the user code for the CLI to print, and a background goroutine polls the token endpoint
// until a human decides it (pollLogin), keeping and installing the login on approval or recording
// denial/expiry. It never retries past a terminal state, and a later Login while one is still
// pending just reports it. A login while a credential is held mints a new key and a new login,
// which replaces the held one only once it is issued.
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
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		return "", err
	}
	operator, err := b.readOperator()
	if err != nil {
		return "", err
	}
	details := []record.AuthorizationDetail{{Type: record.KindLauncherCredential, Identifier: hostname}}
	compact, err := record.Sign(key, b.URL, details, "", operator, nil, time.Now())
	if err != nil {
		return "", err
	}
	cfg := b.oauthConfig()
	da, err := cfg.DeviceAuth(context.WithValue(ctx, oauth2.HTTPClient, b.httpClient()),
		oauth2.SetAuthURLParam("request", compact), oauth2.SetAuthURLParam("dpop_jkt", thumbprint))
	if err != nil {
		return "", oauthBrokerError(err)
	}
	if da.DeviceCode == "" || da.UserCode == "" {
		return "", errors.New("broker returned no device_code/user_code")
	}
	b.recordLogin(&loginState{Code: da.UserCode, State: "pending"})
	go b.pollLogin(key, cfg, da, operator)
	return da.UserCode, nil
}

// LoginStatus reports the most recent machine login, current or settled, whether the helper holds
// a launcher credential and when that credential lapses without a refresh; the zero value means no
// login has run or been resumed. A login that did not issue says nothing about the credential an
// earlier one installed: CredentialHeld does. Once the helper has dropped the credential (drop: the
// broker refused it or its refresh, or it reached its expiry), and until a login is recorded (one
// starts, or one pending at the drop settles), every state but pending reads "expired", refused and
// why it was dropped; an issued login is among them, since only drop ever clears a credential an
// issued login installed. The login, the credential and the drop are read under stateMu, so the
// answer is all three of one moment.
func (b *Broker) LoginStatus() loginState {
	b.stateMu.Lock()
	ls, cred, dropped := b.login.Load(), b.cred.Load(), b.dropped
	b.stateMu.Unlock()
	if ls == nil {
		return loginState{}
	}
	out := *ls
	out.CredentialHeld = cred != nil
	if cred != nil {
		out.CredentialExpiresAt = cred.expiresAt
	}
	if !out.CredentialHeld && dropped != "" {
		out.Refused, out.Dropped = true, dropped
		if out.State != "pending" {
			out.State = "expired"
		}
	}
	return out
}

// noCredentialReason says why the helper holds no launcher credential, for the error lines that
// say a session cannot enroll: NoCredentialReason of this moment's login-status.
func (b *Broker) noCredentialReason() string {
	ls := b.LoginStatus()
	return NoCredentialReason(ls.State, ls.Refused, ls.Dropped)
}

// recordLogin records ls as the current machine login. Whether the login starts or settles, its
// state is newer than any drop recorded before it, so the same write ends that record and the
// login reports its own state.
func (b *Broker) recordLogin(ls *loginState) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	b.login.Store(ls)
	b.dropped = ""
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
// it, in one write under stateMu that also ends any record of a dropped credential, then wakes
// every waiter on CredentialInstalled and watches cred's expiry (watchExpiry). A credential it
// replaces is gone from then on, so its expiry watch says nothing more about it.
func (b *Broker) installCredential(cred *machineCredential, issued *loginState) {
	b.stateMu.Lock()
	old := b.cred.Swap(cred)
	b.login.Store(issued)
	b.dropped = ""
	b.stateMu.Unlock()
	if old != nil {
		close(old.gone)
	}
	go b.watchExpiry(cred)
	b.installedMu.Lock()
	defer b.installedMu.Unlock()
	if b.installed != nil {
		close(b.installed)
	}
	b.installed = make(chan struct{})
}

// pollLogin polls the token endpoint for a pending machine login until a human decides it, at the
// interval the broker named. On approval it keeps the login (keepLogin). access_denied records
// "denied"; expired_token, a device code the broker no longer knows (invalid_grant) and the
// code's own lifetime passing record "expired". A poll that does not reach the broker, or that it
// answers with anything else, polls again.
func (b *Broker) pollLogin(key *ecdsa.PrivateKey, cfg *oauth2.Config, da *oauth2.DeviceAuthResponse, operator string) {
	ctx := b.tokenContext(context.Background(), key)
	settle := func(state string) { b.recordLogin(&loginState{Code: da.UserCode, State: state}) }
	for {
		tok, err := cfg.DeviceAccessToken(ctx, da)
		if err == nil {
			b.keepLogin(key, tok, da.UserCode, operator)
			return
		}
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			switch re.ErrorCode {
			case "access_denied":
				settle("denied")
				return
			case "expired_token", "invalid_grant":
				settle("expired")
				return
			}
		}
		if !da.Expiry.IsZero() && !time.Now().Before(da.Expiry) {
			settle("expired")
			return
		}
	}
}

// keepLogin keeps the login an approved device grant issued at MachineFile and only then installs
// it, under credMu, so the file names the login the helper holds and no refresh runs between the
// two; it logs the credential with operator, the login_hint the login was signed with, whatever
// the operator file says by then. An approval the helper cannot keep (a token answer without the
// credential's extras, or a machine file it cannot write) is logged at ERROR and recorded
// "expired", leaving cred untouched.
func (b *Broker) keepLogin(key *ecdsa.PrivateKey, tok *oauth2.Token, code, operator string) {
	f, err := machineFileFrom(key, tok)
	if err == nil {
		b.credMu.Lock()
		if err = f.Save(b.MachineFile); err == nil {
			b.installCredential(f.credential(), &loginState{Code: code, State: "issued"})
		}
		b.credMu.Unlock()
	}
	if err != nil {
		b.logger().Error("machine login approved, but the helper could not keep it; run: agent-secrets machine login again", "error", err)
		b.recordLogin(&loginState{Code: code, State: "expired"})
		return
	}
	b.logger().Info("machine login issued; the helper holds a launcher credential",
		"credential_id", f.CredentialID, "operator", operator, "expires_at", f.ExpiresAt.Format(time.RFC3339))
}

// Start resumes the machine login kept at MachineFile, if there is one: it installs it as an
// issued login with no code and refreshes it at once. It then refreshes whatever login the helper
// holds every day (refreshEvery) until ctx ends. A file that is there but is not a machine login is
// an error naming it; serve runs Start before it listens.
func (b *Broker) Start(ctx context.Context) error {
	f, err := LoadMachineFile(b.MachineFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		go b.refreshLoop(ctx, false)
		return nil
	case err != nil:
		return err
	}
	b.credMu.Lock()
	b.installCredential(f.credential(), &loginState{State: "issued"})
	b.credMu.Unlock()
	b.logger().Info("the helper resumed the machine login it kept", "credential_id", f.CredentialID,
		"expires_at", f.ExpiresAt.Format(time.RFC3339), "machine_file", b.MachineFile)
	go b.refreshLoop(ctx, true)
	return nil
}

// refreshLoop refreshes the held machine login, at once when now is set, then a day after each
// refresh that reached the broker; one that did not is retried a minute later, the wait doubling
// to an hour. It ends with ctx.
func (b *Broker) refreshLoop(ctx context.Context, now bool) {
	every, retry := b.refreshEvery, b.refreshRetry
	if every == 0 {
		every = defaultRefreshEvery
	}
	if retry == 0 {
		retry = defaultRefreshRetry
	}
	wait, backoff := every, retry
	if now {
		wait = 0
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := b.refresh(ctx); err != nil {
			wait, backoff = backoff, min(2*backoff, maxRefreshRetry)
			continue
		}
		wait, backoff = every, retry
	}
}

// refresh spends the held login's refresh token for a new one and the credential's new expiry,
// under credMu, so it never races a login's install. It saves the new login to MachineFile, then
// swaps it in for the credential it refreshed. The broker's invalid_grant (the login was revoked,
// lapsed, or its refresh token was reused) drops the credential (dropRefreshRefused) and deletes
// the machine file; any other failure keeps both and is returned, so the loop retries. The old
// refresh token is spent once the broker answers, so a new login the file cannot take is still
// swapped in, and the failed save is logged and returned to be retried with the next refresh.
// Holding no credential, it does nothing.
func (b *Broker) refresh(ctx context.Context) error {
	b.credMu.Lock()
	defer b.credMu.Unlock()
	cred := b.cred.Load()
	if cred == nil {
		return nil
	}
	tok, err := b.oauthConfig().TokenSource(b.tokenContext(ctx, cred.key),
		&oauth2.Token{RefreshToken: cred.refreshToken, Expiry: time.Unix(1, 0)}).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
			b.drop(cred, dropRefreshRefused, "code", re.ErrorCode)
			if err := RemoveMachineFile(b.MachineFile); err != nil {
				b.logger().Error("removing the refused machine login's file", "machine_file", b.MachineFile, "error", err)
			}
			return nil
		}
		b.logger().Warn("the machine login's refresh did not reach the broker; retrying", "credential_id", cred.id, "error", err)
		return err
	}
	f, err := machineFileFrom(cred.key, tok)
	if err != nil {
		b.logger().Error("the machine login's refresh answer cannot be kept; retrying", "credential_id", cred.id, "error", err)
		return err
	}
	saveErr := f.Save(b.MachineFile)
	if saveErr != nil {
		b.logger().Error("the helper could not keep the machine login's refresh on disk; a restart before the next refresh loses the login",
			"credential_id", f.CredentialID, "machine_file", b.MachineFile, "error", saveErr)
	}
	next := f.credential()
	b.stateMu.Lock()
	swapped := b.cred.CompareAndSwap(cred, next)
	b.stateMu.Unlock()
	if swapped {
		close(cred.gone)
		go b.watchExpiry(next)
		b.logger().Info("machine login refreshed", "credential_id", f.CredentialID, "expires_at", f.ExpiresAt.Format(time.RFC3339))
	}
	return saveErr
}

// watchExpiry warns a day (expiryWarning) before cred lapses, at once when less than that is left,
// and drops cred when it reaches its expiry (dropExpired), as a broker refusal would: the login
// renews itself only while a refresh reaches the broker, and from then on no session can enroll
// until a human approves a new machine login. It ends early, silently, once the helper stops
// holding cred (a refusal, a refresh, or a newer login).
func (b *Broker) watchExpiry(cred *machineCredential) {
	lead := b.expiryWarning
	if lead == 0 {
		lead = 24 * time.Hour
	}
	select {
	case <-time.After(time.Until(cred.expiresAt.Add(-lead))):
		b.logger().Warn(expiresSoonMsg,
			"credential_id", cred.id, "expires_at", cred.expiresAt.Format(time.RFC3339), "in", time.Until(cred.expiresAt).Round(time.Second))
	case <-cred.gone:
		return
	}
	select {
	case <-time.After(time.Until(cred.expiresAt)):
		b.drop(cred, dropExpired, "expires_at", cred.expiresAt.Format(time.RFC3339))
	case <-cred.gone:
	}
}

// expiresSoonMsg is the WARN watchExpiry logs a day before the held credential lapses.
const expiresSoonMsg = "the launcher credential lapses soon: no refresh has moved its expiry; the machine login renews itself while the helper reaches the broker, otherwise run: agent-secrets machine login, and have a human approve it"

// enrollBody is POST /v1/enrollments's shape in the shared broker contract (the v8 "approver"
// field is gone: the broker's policy picks a request's approver, never the enrollment).
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

// clearOnInvalid drops the machine credential (dropRefused) and reports it missing whenever err
// is the broker's 401 LAUNCHER_INVALID. The broker answers that for any launcher proof it
// rejects: an expired or revoked credential, and also a proof it cannot verify, such as one
// signed outside its clock-skew window or for a URL other than its own (an AGENT_SECRETS_URL that
// does not match the broker's public URL). It never auto-relogins. cred is the credential the
// refused call was signed with, and only that one is dropped. A refusal of a credential a newer
// login has already replaced drops nothing and is returned as it came: the helper holds a
// credential, and the caller's retry, which that login woke, uses it at once.
func (b *Broker) clearOnInvalid(cred *machineCredential, err error) error {
	var be *BrokerError
	if !errors.As(err, &be) || be.Status != http.StatusUnauthorized || be.Code != "LAUNCHER_INVALID" {
		return err
	}
	if !b.drop(cred, dropRefused, "code", be.Code) && b.HasCredential() {
		return err
	}
	return errNoCredential
}

// drop stops holding cred, if the helper still does, records why, logs at ERROR that it did,
// naming why and the credential (with attrs), and reports whether it did: a refusal or an expiry
// that arrives after another login has installed a new credential leaves the new one alone and
// says nothing. The clear and the record of why are one write under stateMu, so no LoginStatus
// reads a credential gone without why: until a login is recorded, it reports the credential
// dropped, whatever the most recent login's state, and a login recorded meanwhile ends that
// record after it, never before. login-status — the probe every launcher decides on — then stops
// reporting a credential the helper no longer holds.
func (b *Broker) drop(cred *machineCredential, why string, attrs ...any) bool {
	b.stateMu.Lock()
	cleared := b.cred.CompareAndSwap(cred, nil)
	if cleared {
		if b.testClearHook != nil {
			b.testClearHook()
		}
		b.dropped = why
	}
	b.stateMu.Unlock()
	if !cleared {
		return false
	}
	close(cred.gone)
	b.logger().Error(why+"; cleared: no session can enroll until a human approves a new machine login (run: agent-secrets machine login)",
		append([]any{"credential_id", cred.id}, attrs...)...)
	return true
}

// logger is b.Log, or a logger that writes nowhere when there is none.
func (b *Broker) logger() *slog.Logger {
	if b.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return b.Log
}

// Enroll registers the session's key. 201 is a new enrollment; 200 is the live one for the same
// credential, kind, runtime_id and thumbprint (the contract's idempotent enroll).
func (b *Broker) Enroll(ctx context.Context, s *Session) (string, time.Time, error) {
	return b.enroll(ctx, s.RuntimeID, s.Thumbprint, nil, "host")
}

// EnrollSessionKey registers a key the helper does not hold, one a launcher enrolls for a session
// on this machine (a box's, a container's): a pass-through broker call with no registry Session,
// so it takes the key's runtime_id, thumbprint and optional session_id directly. Otherwise it is
// Enroll: the same fail-closed check when there is no machine credential yet, the same launcher
// proof, the same 200/201 handling and the same 401 LAUNCHER_INVALID handling.
func (b *Broker) EnrollSessionKey(ctx context.Context, runtimeID, thumbprint string, sessionID *string) (string, time.Time, error) {
	return b.enroll(ctx, runtimeID, thumbprint, sessionID, "box")
}

// enroll enrolls a key as kind session. A broker from before that kind (every release before
// the one that records one kind of machine session) answers it 400 INVALID_KIND, and enroll asks
// it once more with legacyKind, the kind that broker knew the enrollment by: host for the
// helper's own sessions, box for a key a launcher brings. That retry goes once the production
// broker records the session kind.
func (b *Broker) enroll(ctx context.Context, runtimeID, thumbprint string, sessionID *string, legacyKind string) (string, time.Time, error) {
	id, lease, err := b.enrollAs(ctx, "session", runtimeID, thumbprint, sessionID)
	var be *BrokerError
	if errors.As(err, &be) && be.Status == http.StatusBadRequest && be.Code == "INVALID_KIND" {
		return b.enrollAs(ctx, legacyKind, runtimeID, thumbprint, sessionID)
	}
	return id, lease, err
}

// enrollAs is one POST /v1/enrollments naming kind, signed with the current machine credential;
// it reads the operator with readOperator, which propagates its error, never the cosmetic
// Operator().
func (b *Broker) enrollAs(ctx context.Context, kind, runtimeID, thumbprint string, sessionID *string) (string, time.Time, error) {
	url := b.URL + "/v1/enrollments"
	compact, cred, err := b.launcherProof(http.MethodPost, url)
	if err != nil {
		return "", time.Time{}, err
	}
	operator, err := b.readOperator()
	if err != nil {
		return "", time.Time{}, err
	}
	body, err := json.Marshal(enrollBody{Kind: kind, RuntimeID: runtimeID, Operator: operator, Thumbprint: thumbprint, SessionID: sessionID})
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
